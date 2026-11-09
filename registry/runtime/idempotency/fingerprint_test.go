package idempotency

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// orderBody is the body of the golden fingerprint, and goldenFingerprint its recorded value for POST /v1/orders.
const (
	orderBody         = `{ "qty": 2, "item": "book", "meta": {"z": [1.50, true, null], "a": "é\n"} }`
	goldenFingerprint = "843d86171a754be29de2ef7dc37e668a7c9cf2b223188d31102a134a117bdcaf"
)

// lengthPrefixed computes in the test SHA-256 over the parts, each prefixed by its length in bytes as a 4-byte
// big-endian unsigned integer.
func lengthPrefixed(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

var lowerHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

const fingerprintChild = "STEGO_IDEMPOTENCY_FINGERPRINT_CHILD"

func TestFingerprint(t *testing.T) {
	if os.Getenv(fingerprintChild) == "1" {
		fp, err := Fingerprint("POST", "/v1/orders", []byte(orderBody))
		if err != nil {
			fmt.Println("ERROR", err)
			os.Exit(1)
		}
		fmt.Println("FINGERPRINT=" + fp)
		os.Exit(0)
	}

	t.Run("ST-SUP-RP07-LC05-T01 The fingerprint is SHA-256 over method, path and canonical body", func(t *testing.T) {
		got, err := Fingerprint("POST", "/v1/orders", []byte(orderBody))
		if err != nil {
			t.Fatal(err)
		}
		canonical := `{"item":"book","meta":{"a":"` + "é" + `\n","z":[1.5,true,null]},"qty":2}`
		if c, err := Canonical([]byte(orderBody)); err != nil || string(c) != canonical {
			t.Fatalf("Canonical = %s, %v; want %s", c, err, canonical)
		}
		want := lengthPrefixed("POST", "/v1/orders", canonical)
		if got != want || !lowerHex64.MatchString(got) {
			t.Fatalf("Fingerprint = %s, want %s as 64 lower-case hexadecimal digits", got, want)
		}
		if got != goldenFingerprint {
			t.Fatalf("Fingerprint = %s, want the golden value %s", got, goldenFingerprint)
		}
	})

	t.Run("ST-SUP-RP07-LC05-T02 Bodies that differ only in key order, whitespace or number form have one fingerprint", func(t *testing.T) {
		equivalent := []string{
			`{"a":1,"b":{"c":2,"d":[1,2]}}`,
			`{"b":{"d":[1,2],"c":2},"a":1}`,
			"{ \"b\" : { \"d\" : [ 1 , 2 ] ,\n\t\"c\" : 2 } , \"a\" : 1 }",
			`{"a":1.0,"b":{"c":2.000,"d":[1e0,0.2e1]}}`,
			`{"a":10e-1,"b":{"c":200E-2,"d":[1,2]}}`,
		}
		first, err := Fingerprint("POST", "/v1/orders", []byte(equivalent[0]))
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range equivalent[1:] {
			if got, err := Fingerprint("POST", "/v1/orders", []byte(body)); err != nil || got != first {
				t.Fatalf("Fingerprint(%s) = %s, %v; want %s", body, got, err, first)
			}
		}
		changed, err := Fingerprint("POST", "/v1/orders", []byte(`{"a":1,"b":{"c":3,"d":[1,2]}}`))
		if err != nil || changed == first {
			t.Fatalf("a changed value gave %s, %v; want another fingerprint than %s", changed, err, first)
		}
		for n, want := range map[string]string{"1": "1", "-0.0": "0", "1.50": "1.5", "100": "100", "1e20": "100000000000000000000", "1e21": "1e+21",
			"1e22": "1e+22", "0.000001": "0.000001", "0.0000001": "1e-7", "123456789012345678901234567890": "1.2345678901234567890123456789e+29",
			"-12.5e-1": "-1.25"} {
			if got, err := canonicalNumber(n); err != nil || got != want {
				t.Fatalf("canonicalNumber(%s) = %s, %v; want %s", n, got, err, want)
			}
		}
		for _, bad := range []string{`{"a":}`, `{"a":1} {"b":2}`, `[1,]`} {
			if _, err := Fingerprint("POST", "/v1/orders", []byte(bad)); err == nil {
				t.Fatalf("Fingerprint(%s) took a body that is not one JSON value", bad)
			}
		}
	})

	t.Run("ST-SUP-RP07-LC05-T03 The fingerprint is the same in separate processes", func(t *testing.T) {
		var got []string
		for _, procs := range []string{"1", "7"} {
			cmd := exec.Command(os.Args[0], "-test.run=^TestFingerprint$", "-test.count=1")
			// Each child process has its own random map seed; GOMAXPROCS differs.
			cmd.Env = append(os.Environ(), fingerprintChild+"=1", "GOMAXPROCS="+procs)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("child with GOMAXPROCS=%s: %v\n%s", procs, err, out)
			}
			_, fp, ok := strings.Cut(strings.TrimSpace(string(out)), "FINGERPRINT=")
			if !ok {
				t.Fatalf("child with GOMAXPROCS=%s printed %q", procs, out)
			}
			got = append(got, strings.Fields(fp)[0])
		}
		if got[0] != got[1] || got[0] != goldenFingerprint {
			t.Fatalf("children gave %v, want the golden value %s from both", got, goldenFingerprint)
		}
	})
}

func TestDerive(t *testing.T) {
	t.Run("ST-SUP-RP07-LC06-T01 An outgoing key is SHA-256 over the tenant, the principal, the inbound key, the destination and the effect", func(t *testing.T) {
		got := Derive("t1", "p1", "k1", "billing", "charge")
		want := lengthPrefixed("t1", "p1", "k1", "billing", "charge")
		if got != want || !lowerHex64.MatchString(got) {
			t.Fatalf("Derive = %s, want %s as 64 lower-case hexadecimal digits", got, want)
		}
	})

	t.Run("ST-SUP-RP07-LC06-T02 A retry derives the same outgoing key, and another tenant, principal, destination or effect a different one", func(t *testing.T) {
		first := Derive("t1", "p1", "k1", "billing", "charge")
		if again := Derive("t1", "p1", "k1", "billing", "charge"); again != first {
			t.Fatalf("a retry derived %s, want %s", again, first)
		}
		seen := map[string]string{first: "first"}
		for name, key := range map[string]string{
			"tenant":      Derive("t2", "p1", "k1", "billing", "charge"),
			"principal":   Derive("t1", "p2", "k1", "billing", "charge"),
			"destination": Derive("t1", "p1", "k1", "mail", "charge"),
			"effect":      Derive("t1", "p1", "k1", "billing", "refund"),
		} {
			if other, ok := seen[key]; ok {
				t.Fatalf("another %s derived the same key as the %s", name, other)
			}
			seen[key] = name
		}
	})

	t.Run("ST-SUP-RP07-LC06-T03 Two pairs of key and destination that join to the same bytes derive different keys", func(t *testing.T) {
		if Derive("t1", "p1", "ab", "c", "charge") == Derive("t1", "p1", "a", "bc", "charge") {
			t.Fatal("ab with c and a with bc derived one key")
		}
	})
}
