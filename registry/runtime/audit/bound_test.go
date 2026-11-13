package audit

import (
	"encoding/json"
	"math/big"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
)

// TestBounds proves the bounds of the canonical form and of the patch that the security review of PR #201 found
// loose (follow-ups L1, L2 and L3).
func TestBounds(t *testing.T) {
	t.Run("ST-SUP-RP05-LC05-T06 A patch under keys that JSON escapes stays within the bound of the event", func(t *testing.T) {
		// A key of 1000 control characters is 1000 bytes raw and 6000 bytes in JSON.
		key := strings.Repeat("\x01", 1000)
		members := make([]string, 20)
		for i := range members {
			members[i] = `"` + strconv.Itoa(i) + `":1`
		}
		keyJSON, _ := json.Marshal(key)
		before := json.RawMessage(`{` + string(keyJSON) + `:{}}`)
		after := json.RawMessage(`{` + string(keyJSON) + `:{` + strings.Join(members, ",") + `}}`)
		e := sampleEvent("t1", "update")
		if err := e.SetStates(Settings{Level: LevelChange}, Fields{}, map[string]any{"doc": before}, map[string]any{"doc": after}); err != nil {
			t.Fatalf("SetStates with escaped keys: %v", err)
		}
		// The budget of a field counts each path in its JSON form, as the net of the event does.
		d := differ{segments: []string{"doc", escapePointer(key)}, budget: 1 << 30}
		d.spend("0")
		if want := opOverhead + 1 + (3 + 1) + (6000 + 1); d.used != want {
			t.Errorf("one operation under the escaped key counts %d bytes, want %d", d.used, want)
		}
		input := len(e.Before.JSON()) + len(e.After.JSON())
		if n := len(e.Patch.JSON()); n > 3*input+4096*3 {
			t.Errorf("the patch is %d bytes for states of %d bytes", n, input)
		}
		p, err := ParsePatch(e.Patch.JSON())
		if err != nil {
			t.Fatal(err)
		}
		got, err := Apply(mustDecode(t, e.Before.JSON()), p)
		if err != nil {
			t.Fatal(err)
		}
		if string(encodeTree(got)) != string(encodeTree(mustDecode(t, e.After.JSON()))) {
			t.Error("the patch applied to before does not give after")
		}
	})
	t.Run("ST-SUP-RP05-LC05-T07 The whole-field form of a patch holds no secret and no raw pii value", func(t *testing.T) {
		f := Fields{"profile.email": PII, "profile.api_token": Secret}
		before := map[string]any{"profile": map[string]any{"email": "old@example.com", "api_token": "tok-old-1", "city": "Oslo"}}
		after := map[string]any{"profile": map[string]any{"email": "new@example.com", "api_token": "tok-new-2", "city": "Bergen"},
			"plan": "gold"}
		p, err := diff(before, after, f, true)
		if err != nil {
			t.Fatal(err)
		}
		text := string(encodeTree(p.tree()))
		for _, value := range []string{"old@example.com", "new@example.com", "tok-old-1", "tok-new-2", "api_token"} {
			if strings.Contains(text, value) {
				t.Errorf("the whole-field patch %s holds %s", text, value)
			}
		}
		for _, want := range []string{`"path":"/profile"`, `"path":"/plan"`, RedactionMarker, "Bergen"} {
			if !strings.Contains(text, want) {
				t.Errorf("the whole-field patch %s does not hold %s", text, want)
			}
		}
		for _, o := range p {
			if strings.Count(o.Path, "/") > 1 {
				t.Errorf("the whole-field patch has the operation %s %s below a field", o.Op, o.Path)
			}
		}
		// A patch over the net of the event falls back to this form, and is not an error.
		big := Patch{{Op: "add", Path: "/profile/x", Value: strings.Repeat("x", 100_000)}}
		encoded, err := boundedPatch(big, before, after, f, 200)
		if err != nil || string(encoded) != text {
			t.Errorf("a patch over the net gave %s and %v, want the whole-field patch %s", encoded, err, text)
		}
	})
	t.Run("ST-SUP-RP05-LC04-T10 The canonical form of a number is never longer than the bound, and only decimal text is a number", func(t *testing.T) {
		// 700 digits and the exponent -400 pass the first bound; the form -0.000... has 1102 characters.
		long := "-1." + strings.Repeat("1", 699) + "e-400"
		if n, err := canonicalNumber("n", long); err == nil || !strings.Contains(err.Error(), "n") {
			t.Errorf("700 digits at -400 gave %d characters and %v, want a refusal that names the field", len(n), err)
		}
		// 698 digits give 1100 characters, which the bound takes.
		if n, err := canonicalNumber("n", "-1."+strings.Repeat("1", 697)+"e-400"); err != nil || len(n) != maxNumberText {
			t.Errorf("698 digits at -400 gave %d characters and %v, want %d", len(n), err, maxNumberText)
		}
		for _, text := range []string{"0x1p4", "0x1p2000000", "1/3", "Inf", "NaN", "1e5.5", "+1", ".5", "1.", "0b101"} {
			if n, err := canonicalNumber("n", text); err == nil {
				t.Errorf("the text %q gave the number %s, want a refusal", text, n)
			} else if strings.Contains(err.Error(), text) {
				t.Errorf("the refusal of %q holds the text: %v", text, err)
			}
		}
	})
	t.Run("ST-SUP-RP05-LC04-T11 The canonical form of a random JSON number has the exact value of its text", func(t *testing.T) {
		r := rand.New(rand.NewPCG(201, 11))
		digits := func(n int) string {
			b := make([]byte, n)
			for i := range b {
				b[i] = byte('0' + r.IntN(10))
			}
			return string(b)
		}
		for range 20000 {
			text := ""
			if r.IntN(2) == 0 {
				text = "-"
			}
			whole := digits(1 + r.IntN(20))
			if len(whole) > 1 {
				whole = string(rune('1'+r.IntN(9))) + whole[1:]
			}
			text += whole
			if r.IntN(2) == 0 {
				text += "." + digits(1+r.IntN(20))
			}
			if r.IntN(2) == 0 {
				text += []string{"e", "E", "e+", "e-", "E-"}[r.IntN(5)] + strconv.Itoa(r.IntN(60))
			}
			got, err := canonicalNumber("n", text)
			if err != nil {
				t.Fatalf("%s: %v", text, err)
			}
			want, ok := new(big.Rat).SetString(text)
			if !ok {
				t.Fatalf("big.Rat does not read %s", text)
			}
			value, ok := new(big.Rat).SetString(string(got))
			if !ok || value.Cmp(want) != 0 {
				t.Fatalf("%s gave %s, which is not the same value", text, got)
			}
			if strings.ContainsAny(string(got), "eE+") || (strings.Contains(string(got), ".") && strings.HasSuffix(string(got), "0")) || got == "-0" {
				t.Fatalf("%s gave %s, which is not the shortest plain form", text, got)
			}
		}
	})
}

func mustDecode(t *testing.T, data []byte) any {
	t.Helper()
	v, err := decodeJSON("state", data)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
