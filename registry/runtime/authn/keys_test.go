package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"stego.dev/runtime/authn/authntest"
)

// modulus makes an odd number of exactly the bits, as the public part of an RSA key that the key set reads. The
// key set reads only the public numbers, so no key of 8192 bits or more needs to be generated.
func modulus(t *testing.T, bits int) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), uint(bits-1)))
	if err != nil {
		t.Fatal(err)
	}
	n.SetBit(n, bits-1, 1)
	n.SetBit(n, 0, 1)
	return n
}

func fakeJWK(t *testing.T, id string, bits, exponent int) map[string]any {
	t.Helper()
	return authntest.RSAJWK(id, &rsa.PublicKey{N: modulus(t, bits), E: exponent})
}

// remoteVerifier gives a verifier of the key server's key set, with a fake clock.
func remoteVerifier(t *testing.T, server *authntest.Server, c *clock) (*Verifier, *KeySet) {
	t.Helper()
	set, err := newKeySet(context.Background(), KeySource{URL: server.URL(), Roots: server.Roots()},
		keyOptions{now: c.now})
	if err != nil {
		t.Fatalf("newKeySet: %v", err)
	}
	v, err := newVerifier(Settings{Issuer: testIssuer, Audience: testAudience, Keys: set}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Stop)
	return v, set
}

func tokenOf(t *testing.T, key *authntest.Key, kid string) string {
	t.Helper()
	return authntest.Mint(t, authntest.Header(kid), authntest.Claims(testIssuer, testAudience, "user-1", time.Now()), key)
}

func TestKeysBounds(t *testing.T) {
	t.Run("ST-SUP-RP13-LC04-T01 Key size and exponent are bounded", func(t *testing.T) {
		for _, bits := range []int{MinKeyBits, MaxKeyBits} {
			if _, err := ParseKeys(authntest.Document(fakeJWK(t, "k1", bits, 65537))); err != nil {
				t.Errorf("a key of %d bits was refused: %v", bits, err)
			}
		}
		cases := []struct {
			name string
			key  map[string]any
			want error
		}{
			{"1024 bits", fakeJWK(t, "k1", 1024, 65537), errKeySize},
			{"2047 bits", fakeJWK(t, "k1", MinKeyBits-1, 65537), errKeySize},
			{"8200 bits", fakeJWK(t, "k1", 8200, 65537), errKeySize},
			{"exponent 3", fakeJWK(t, "k1", MinKeyBits, 3), errExponent},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				set, err := ParseKeys(authntest.Document(c.key))
				if set != nil || !errors.Is(err, c.want) || !errors.Is(err, ErrKeys) {
					t.Errorf("ParseKeys gave %v, %v, want %q", set, err, c.want)
				}
			})
		}
	})

	t.Run("ST-SUP-RP13-LC07-T01 The key is chosen by exact key ID only", func(t *testing.T) {
		key := authntest.NewKey(t, "Key-1")
		set, err := ParseKeys(authntest.Document(key.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		v, err := New(Settings{Issuer: testIssuer, Audience: testAudience, Keys: set})
		if err != nil {
			t.Fatal(err)
		}
		expectAccepted(t, v, tokenOf(t, key, "Key-1"))
		for _, kid := range []string{"key-1", "KEY-1", "Key-1 ", "Key-"} {
			id, err := v.Verify(context.Background(), tokenOf(t, key, kid))
			expectRefused(t, id, err, errUnknownKey)
		}

		// A document of exactly MaxDocumentBytes is read; one byte more is refused.
		sized := func(size int) []byte {
			document := authntest.Document(key.JWK())
			pad := size - len(document) - len(`,"pad":""`)
			return append(document[:len(document)-1], []byte(`,"pad":"`+strings.Repeat("x", pad)+`"}`)...)
		}
		if _, err := ParseKeys(sized(MaxDocumentBytes)); err != nil {
			t.Errorf("a document of %d bytes was refused: %v", MaxDocumentBytes, err)
		}
		keys := func(n int) []map[string]any {
			out := make([]map[string]any, n)
			for i := range out {
				out[i] = fakeJWK(t, fmt.Sprintf("k%d", i), MinKeyBits, 65537)
			}
			return out
		}
		if _, err := ParseKeys(authntest.Document(keys(MaxKeys)...)); err != nil {
			t.Errorf("a document of %d keys was refused: %v", MaxKeys, err)
		}
		cases := []struct {
			name     string
			document []byte
			want     error
		}{
			{"64 KiB plus one byte", sized(MaxDocumentBytes + 1), errDocumentSize},
			{"a repeated key ID", authntest.Document(fakeJWK(t, "k1", MinKeyBits, 65537), fakeJWK(t, "k1", MinKeyBits, 65537)), errDocumentKID},
			{"33 keys", authntest.Document(keys(MaxKeys + 1)...), errKeyCount},
			{"no RSA signing key", authntest.Document(map[string]any{"kid": "e1", "kty": "EC"}), errNoKey},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				set, err := ParseKeys(c.document)
				if set != nil || !errors.Is(err, c.want) {
					t.Errorf("ParseKeys gave %v, %v, want %q", set, err, c.want)
				}
			})
		}
	})
}

func TestKeysSource(t *testing.T) {
	t.Run("ST-SUP-RP13-LC08-T01 The remote source refuses http, redirects, proxies and unverified TLS", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		server := authntest.NewServer(t)
		server.Publish(key)
		if set, err := NewKeySet(context.Background(), KeySource{URL: server.URL(), Roots: server.Roots()}); err != nil {
			t.Fatalf("the control load failed: %v", err)
		} else {
			set.Stop()
		}
		other := authntest.NewServer(t)
		other.Publish(key)

		refused := func(t *testing.T, source KeySource, want error) {
			t.Helper()
			set, err := NewKeySet(context.Background(), source)
			if set != nil || !errors.Is(err, want) {
				t.Errorf("NewKeySet gave %v, %v, want %q", set, err, want)
			}
		}
		t.Run("http", func(t *testing.T) {
			refused(t, KeySource{URL: strings.Replace(server.URL(), "https://", "http://", 1), Roots: server.Roots()}, ErrSettings)
		})
		t.Run("redirect", func(t *testing.T) {
			// The redirect points at a URL of the same trusted server that serves a valid document, so only the
			// refusal of the redirect stops the load.
			if set, err := NewKeySet(context.Background(), KeySource{URL: server.OtherURL(), Roots: server.Roots()}); err != nil {
				t.Fatalf("the redirect target does not load: %v", err)
			} else {
				set.Stop()
			}
			server.Redirect(server.OtherURL())
			defer server.Publish(key)
			before := server.Requests()
			refused(t, KeySource{URL: server.URL(), Roots: server.Roots()}, ErrKeys)
			if n := server.Requests() - before; n != 1 {
				t.Errorf("the load made %d requests, want 1 and no redirect", n)
			}
		})
		t.Run("untrusted certificate", func(t *testing.T) {
			refused(t, KeySource{URL: other.URL(), Roots: server.Roots()}, ErrKeys)
			refused(t, KeySource{URL: other.URL()}, ErrKeys)
		})
		t.Run("proxy", func(t *testing.T) {
			// net/http reads the proxy settings of the process once, so the check runs in a child process that
			// starts with HTTPS_PROXY set to a listener of this process.
			proxy, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer proxy.Close()
			var connections atomic.Int64
			go func() {
				for {
					conn, err := proxy.Accept()
					if err != nil {
						return
					}
					connections.Add(1)
					_ = conn.Close()
				}
			}()
			child := exec.Command(os.Args[0], "-test.run=^TestKeysProxyChild$", "-test.count=1", "-test.v")
			address := "http://" + proxy.Addr().String()
			child.Env = append(os.Environ(), proxyChild+"=1", "HTTPS_PROXY="+address, "https_proxy="+address,
				"NO_PROXY=", "no_proxy=")
			output, err := child.CombinedOutput()
			if err != nil || !strings.Contains(string(output), "--- PASS: TestKeysProxyChild/ST-SUP-RP13-LC08-T01") {
				t.Errorf("the child process failed: %v\n%s", err, output)
			}
			if n := connections.Load(); n != 0 {
				t.Errorf("the proxy got %d connections, want none", n)
			}
		})
	})

	t.Run("ST-SUP-RP13-LC09-T01 A known key ID makes no request, an unknown one refreshes once", func(t *testing.T) {
		server := authntest.NewServer(t)
		k1, k2, k3 := authntest.NewKey(t, "k1"), authntest.NewKey(t, "k2"), authntest.NewKey(t, "k3")
		server.Publish(k1)
		c := newClock()
		v, _ := remoteVerifier(t, server, c)
		requests := server.Requests()
		// Past the cooldown of the first load, so only the cache keeps the ten tokens from a request.
		c.add(Cooldown)
		for range 10 {
			expectAccepted(t, v, tokenOf(t, k1, "k1"))
		}
		if n := server.Requests() - requests; n != 0 {
			t.Errorf("ten tokens of a cached key ID made %d requests, want none", n)
		}
		server.Publish(k1, k2)
		expectAccepted(t, v, tokenOf(t, k2, "k2"))
		if n := server.Requests() - requests; n != 1 {
			t.Errorf("the first unknown key ID made %d requests, want 1", n)
		}
		server.Publish(k1, k2, k3)
		c.add(Cooldown - time.Second)
		id, err := v.Verify(context.Background(), tokenOf(t, k3, "k3"))
		expectRefused(t, id, err, errUnknownKey)
		if n := server.Requests() - requests; n != 1 {
			t.Errorf("an unknown key ID in the cooldown made a request: %d requests, want 1", n)
		}
		// After the cooldown the key set refreshes again; a key ID that is still unknown is denied.
		c.add(time.Second)
		id, err = v.Verify(context.Background(), tokenOf(t, k1, "k4"))
		expectRefused(t, id, err, errUnknownKey)
		if n := server.Requests() - requests; n != 2 {
			t.Errorf("an unknown key ID after the cooldown made %d requests in all, want 2", n)
		}
		expectAccepted(t, v, tokenOf(t, k3, "k3"))
	})

	t.Run("ST-SUP-RP13-LC10-T01 An outage denies after 15 minutes, a removed key at once", func(t *testing.T) {
		server := authntest.NewServer(t)
		k1, k2 := authntest.NewKey(t, "k1"), authntest.NewKey(t, "k2")
		server.Publish(k1)
		c := newClock()
		v, _ := remoteVerifier(t, server, c)
		token := tokenOf(t, k1, "k1")
		server.Fail(http.StatusServiceUnavailable)
		c.add(14 * time.Minute)
		requests := server.Requests()
		expectAccepted(t, v, token)
		if server.Requests() == requests {
			t.Error("a cache older than MaxAge made no refresh")
		}
		c.add(2 * time.Minute)
		id, err := v.Verify(context.Background(), token)
		expectRefused(t, id, err, errOutage)
		c.add(10 * time.Second)
		id, err = v.Verify(context.Background(), token)
		expectRefused(t, id, err, errOutage)
		// The server is back with a document that no longer holds k1.
		server.Publish(k2)
		c.add(Cooldown)
		id, err = v.Verify(context.Background(), token)
		expectRefused(t, id, err, errUnknownKey)
		expectAccepted(t, v, tokenOf(t, k2, "k2"))

		// In a fresh cache, a load for an unknown key ID that drops k1 stops k1 at once.
		server.Publish(k1)
		c2 := newClock()
		v2, _ := remoteVerifier(t, server, c2)
		expectAccepted(t, v2, token)
		server.Publish(k2)
		c2.add(Cooldown)
		expectAccepted(t, v2, tokenOf(t, k2, "k2"))
		id, err = v2.Verify(context.Background(), token)
		expectRefused(t, id, err, errUnknownKey)
	})

	t.Run("ST-SUP-RP13-LC11-T01 Stop and the zero value fail closed", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		token := tokenOf(t, key, "k1")
		var zero Verifier
		id, err := zero.Verify(context.Background(), token)
		expectRefused(t, id, err, errNotReady)
		var none *Verifier
		id, err = none.Verify(context.Background(), token)
		expectRefused(t, id, err, errNotReady)
		none.Stop()
		zero.Stop()
		var zeroSet KeySet
		zeroSet.Stop()
		zeroSet.Stop()
		withZeroSet, err := New(Settings{Issuer: testIssuer, Audience: testAudience, Keys: &KeySet{}})
		if err != nil {
			t.Fatal(err)
		}
		id, err = withZeroSet.Verify(context.Background(), token)
		expectRefused(t, id, err, errNotReady)

		server := authntest.NewServer(t)
		server.Publish(key)
		c := newClock()
		v, _ := remoteVerifier(t, server, c)
		expectAccepted(t, v, token)
		v.Stop()
		v.Stop()
		id, err = v.Verify(context.Background(), token)
		expectRefused(t, id, err, errStopped)

		// Stop while a load waits on a server that does not answer.
		c2 := newClock()
		v2, _ := remoteVerifier(t, server, c2)
		for len(server.Arrived()) > 0 {
			<-server.Arrived()
		}
		server.Hang()
		c2.add(Cooldown)
		done := make(chan error, 1)
		go func() {
			_, err := v2.Verify(context.Background(), tokenOf(t, key, "k2"))
			done <- err
		}()
		select {
		case <-server.Arrived():
		case <-time.After(3 * time.Second):
			t.Fatal("the key load did not reach the server")
		}
		v2.Stop()
		select {
		case err := <-done:
			if !errors.Is(err, errStopped) {
				t.Errorf("the request that started the load gave %v, want %q", err, errStopped)
			}
		case <-time.After(time.Second):
			t.Fatal("Stop did not end the request that started the load")
		}
		deadline := time.Now().Add(time.Second)
		for server.Abandoned() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if server.Abandoned() != 1 {
			t.Errorf("the server saw %d abandoned loads, want the one that Stop cancelled", server.Abandoned())
		}
	})
}

const proxyChild = "STEGO_AUTHN_PROXY_CHILD"

// TestKeysProxyChild is the child process of the proxy case of ST-SUP-RP13-LC08-T01: it runs only when that case
// starts it with HTTPS_PROXY set.
func TestKeysProxyChild(t *testing.T) {
	t.Run("ST-SUP-RP13-LC08-T01 The key load sends no request through the proxy", proxyChildCase)
}

func proxyChildCase(t *testing.T) {
	if os.Getenv(proxyChild) == "" {
		t.Skip("runs only as the child process of ST-SUP-RP13-LC08-T01")
	}
	key := authntest.NewKey(t, "k1")
	server := authntest.NewServer(t)
	server.Publish(key)
	// The key server's name resolves nowhere: the dialer refuses it, so only a proxy could reach the server.
	_, port, _ := net.SplitHostPort(server.Addr())
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if strings.HasPrefix(address, authntest.Host+":") {
			return nil, errors.New("no such host")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	set, err := newKeySet(context.Background(), KeySource{URL: "https://" + authntest.Host + ":" + port + "/keys",
		Roots: server.Roots()}, keyOptions{now: time.Now, dial: dial})
	if set != nil || !errors.Is(err, ErrKeys) {
		t.Errorf("newKeySet gave %v, %v, want a failed load", set, err)
	}
}
