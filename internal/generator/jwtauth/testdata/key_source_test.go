package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func keyDocument(t *testing.T, id string, key *rsa.PrivateKey) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"keys": []any{map[string]any{"kid": id, "kty": "RSA", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestKeySourceTLSRotationAndFailure(t *testing.T) {
	first, second := keyForTest(t), keyForTest(t)
	var document atomic.Value
	document.Store(keyDocument(t, "first", first))
	var requests atomic.Int32
	var fail atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if fail.Load() {
			http.Error(w, "private-provider-error", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(document.Load().([]byte))
	}))
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	config := JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}, URL: server.URL}
	if v, err := NewJWKSVerifier(context.Background(), config); err == nil {
		v.Stop()
		t.Fatal("untrusted TLS certificate accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("untrusted server received an HTTP request")
	}
	config.CAFile = ca
	v, err := NewJWKSVerifier(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Stop()
	now := time.Now()
	v.now = func() time.Time { return now }
	a := signed(t, validClaims(), jwt.SigningMethodRS256, first, map[string]any{"kid": "first"})
	b := signed(t, validClaims(), jwt.SigningMethodRS256, second, map[string]any{"kid": "second"})
	if _, err := v.Verify(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	initial := requests.Load()
	for i := 0; i < 3; i++ {
		if _, err := v.Verify(context.Background(), b); err == nil {
			t.Fatal("unknown key accepted")
		}
	}
	if requests.Load() != initial {
		t.Fatal("unknown key bypassed the refresh interval")
	}
	now = now.Add(31 * time.Second)
	document.Store(keyDocument(t, "second", second))
	if _, err := v.Verify(context.Background(), b); err != nil {
		t.Fatal("rotated key rejected", err)
	}
	if _, err := v.Verify(context.Background(), a); err == nil {
		t.Fatal("removed key accepted")
	}
	if requests.Load() != initial+1 {
		t.Fatal("rotation did not make one bounded request")
	}
	fail.Store(true)
	now = now.Add(6 * time.Minute)
	if _, err := v.Verify(context.Background(), b); err != nil {
		t.Fatal("known key failed inside the outage limit", err)
	}
	now = now.Add(10 * time.Minute)
	if _, err := v.Verify(context.Background(), b); err == nil {
		t.Fatal("expired cache survived provider failure")
	}
	fail.Store(false)
	now = now.Add(31 * time.Second)
	if _, err := v.Verify(context.Background(), b); err != nil {
		t.Fatal("provider recovery failed", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	count := requests.Load()
	if _, err := v.Verify(canceled, b); err == nil {
		t.Fatal("canceled request accepted")
	}
	if requests.Load() != count {
		t.Fatal("canceled request contacted provider")
	}
	v.Stop()
	v.Stop()
	if _, err := v.Verify(context.Background(), b); err == nil {
		t.Fatal("stopped verifier accepted request")
	}
}

func TestKeySourceInputLimitsAndRedirect(t *testing.T) {
	key := keyForTest(t)
	file := filepath.Join(t.TempDir(), "keys.json")
	config := JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}, File: file}
	for _, data := range [][]byte{nil, []byte(`{"keys":[]}`), []byte(strings.Repeat(" ", maxKeyFileBytes+1)), []byte(`{"keys":[],"keys":[]}`)} {
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		if v, err := NewJWKSVerifier(context.Background(), config); err == nil {
			v.Stop()
			t.Fatal("invalid key file accepted")
		}
	}
	if err := os.WriteFile(file, keyDocument(t, "key", key), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := NewJWKSVerifier(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Stop()
	if _, err := v.Verify(context.Background(), strings.Repeat("x", maxTokenBytes+1)); err == nil {
		t.Fatal("oversize token accepted")
	}
	config.Trust.Audience = ""
	if bad, err := NewJWKSVerifier(context.Background(), config); err == nil {
		bad.Stop()
		t.Fatal("missing audience accepted")
	}
	var targetRequests atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetRequests.Add(1) }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redirect.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	config = JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}, URL: redirect.URL, CAFile: ca}
	if bad, err := NewJWKSVerifier(context.Background(), config); err == nil {
		bad.Stop()
		t.Fatal("redirect accepted")
	}
	if targetRequests.Load() != 0 {
		t.Fatal("key loader followed redirect")
	}
	config.URL = "http://example.com/keys"
	if bad, err := NewJWKSVerifier(context.Background(), config); err == nil {
		bad.Stop()
		t.Fatal("plaintext key source accepted")
	}
}

func TestKeySourceStopCancelsRefresh(t *testing.T) {
	key := keyForTest(t)
	data := keyDocument(t, "key", key)
	var wait atomic.Bool
	entered := make(chan struct{})
	var once sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wait.Load() {
			once.Do(func() { close(entered) })
			<-r.Context().Done()
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := NewJWKSVerifier(context.Background(), JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}, URL: server.URL, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Stop()
	future := time.Now().Add(6 * time.Minute)
	v.now = func() time.Time { return future }
	raw := signed(t, validClaims(), jwt.SigningMethodRS256, key, map[string]any{"kid": "key"})
	wait.Store(true)
	done := make(chan error, 1)
	go func() { _, err := v.Verify(context.Background(), raw); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not start")
	}
	v.Stop()
	v.Stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stopped refresh accepted token")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel key retrieval")
	}
}

func TestEmptyKeySourceFailsClosed(t *testing.T) {
	var v JWKSVerifier
	if _, err := v.Verify(context.Background(), "token"); err == nil {
		t.Fatal("empty verifier accepted token")
	}
	v.Stop()
	v.Stop()
}

func TestRemoteKeyInputLimits(t *testing.T) {
	for _, mode := range []string{"body", "headers", "status"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "body":
					_, _ = w.Write([]byte(strings.Repeat("x", maxKeyFileBytes+1)))
				case "headers":
					w.Header().Set("X-Excess", strings.Repeat("x", 16384))
					w.WriteHeader(200)
				case "status":
					http.Error(w, "private-provider-error", 503)
				}
			}))
			defer server.Close()
			ca := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			v, err := NewJWKSVerifier(context.Background(), JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}, URL: server.URL, CAFile: ca})
			if err == nil {
				v.Stop()
				t.Fatal("invalid remote input accepted")
			}
			if strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "private-provider-error") {
				t.Fatal("provider details reached the startup error")
			}
		})
	}
}
