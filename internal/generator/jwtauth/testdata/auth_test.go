package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func keyForTest(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": "https://issuer.example", "aud": "example-api", "sub": "alice",
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		"role": "admin", "attributes": map[string]string{"team": "engineering"},
	}
}

func signed(t *testing.T, claims jwt.MapClaims, method jwt.SigningMethod, key any, header map[string]any) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	for name, value := range header {
		token.Header[name] = value
	}
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func verifierForTest(t *testing.T, key *rsa.PrivateKey) *Verifier {
	t.Helper()
	verifier, err := NewVerifier(Config{Issuer: "https://issuer.example", Audience: "example-api", PublicKey: &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func TestAuthenticationRequests(t *testing.T) {
	key, otherKey := keyForTest(t), keyForTest(t)
	verifier := verifierForTest(t, key)
	for _, test := range []struct {
		name   string
		change func(jwt.MapClaims)
		method jwt.SigningMethod
		key    any
		header map[string]any
		want   int
	}{
		{name: "valid", want: http.StatusNoContent},
		{name: "wrong signature", key: otherKey},
		{name: "none", method: jwt.SigningMethodNone, key: jwt.UnsafeAllowNoneSignatureType},
		{name: "symmetric key confusion", method: jwt.SigningMethodHS256, key: []byte("public key bytes are not a shared secret")},
		{name: "unconfigured algorithm", method: jwt.SigningMethodRS384},
		{name: "expired", change: func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Second).Unix() }},
		{name: "missing expiry", change: func(c jwt.MapClaims) { delete(c, "exp") }},
		{name: "missing issue time", change: func(c jwt.MapClaims) { delete(c, "iat") }},
		{name: "future issue time", change: func(c jwt.MapClaims) { c["iat"] = time.Now().Add(time.Minute).Unix() }},
		{name: "not active", change: func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Minute).Unix() }},
		{name: "wrong issuer", change: func(c jwt.MapClaims) { c["iss"] = "https://other.example" }},
		{name: "missing issuer", change: func(c jwt.MapClaims) { delete(c, "iss") }},
		{name: "wrong audience", change: func(c jwt.MapClaims) { c["aud"] = "other-api" }},
		{name: "missing audience", change: func(c jwt.MapClaims) { delete(c, "aud") }},
		{name: "missing subject", change: func(c jwt.MapClaims) { delete(c, "sub") }},
		{name: "empty subject", change: func(c jwt.MapClaims) { c["sub"] = " " }},
		{name: "wrong token type", header: map[string]any{"typ": "unexpected"}},
		{name: "untrusted key URL", header: map[string]any{"jku": "https://attacker.example/keys"}},
		{name: "embedded key", header: map[string]any{"jwk": map[string]string{"kty": "RSA"}}},
		{name: "unknown critical header", header: map[string]any{"crit": []string{"unknown"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := validClaims()
			if test.change != nil {
				test.change(claims)
			}
			method := test.method
			if method == nil {
				method = jwt.SigningMethodRS256
			}
			signingKey := test.key
			if signingKey == nil {
				signingKey = key
			}
			raw := signed(t, claims, method, signingKey, test.header)
			called := false
			handler := verifier.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				id := IdentityFromContext(r.Context())
				if id.UserID != "alice" || id.Role != "admin" || id.Attributes["team"] != "engineering" {
					t.Errorf("wrong identity: %+v", id)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodGet, "/widgets", nil)
			request.Header.Set("Authorization", "Bearer "+raw)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			want := test.want
			if want == 0 {
				want = http.StatusUnauthorized
			}
			if response.Code != want || called != (want == http.StatusNoContent) {
				t.Fatalf("status %d, called %v; want %d", response.Code, called, want)
			}
			if want == http.StatusUnauthorized {
				if response.Header().Get("WWW-Authenticate") == "" || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("missing authentication response headers")
				}
				if strings.Contains(response.Body.String(), raw) {
					t.Fatal("response exposes the token")
				}
				id, err := verifier.Verify(raw)
				if err == nil || id.UserID != "" || id.Role != "" || id.Attributes != nil {
					t.Fatal("unverified claims escaped the verifier")
				}
			}
		})
	}
}

func signRaw(t *testing.T, key *rsa.PrivateKey, header, payload []byte) string {
	t.Helper()
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	hash := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestAmbiguousAndMalformedTokens(t *testing.T) {
	key := keyForTest(t)
	verifier := verifierForTest(t, key)
	payload, err := json.Marshal(validClaims())
	if err != nil {
		t.Fatal(err)
	}
	validHeader := []byte(`{"alg":"RS256","typ":"JWT"}`)
	for _, raw := range []string{
		"", "not-a-token", "x.y.z", strings.Repeat("x", maxTokenBytes+1),
		signRaw(t, key, validHeader, []byte(`{"sub":"attacker",`+string(payload[1:]))),
		signRaw(t, key, []byte(`{"alg":"RS256","alg":"RS256","typ":"JWT"}`), payload),
		signRaw(t, key, validHeader, []byte("[]")),
		signRaw(t, key, validHeader, []byte(`{"nested":`+strings.Repeat("[", 34)+"0"+strings.Repeat("]", 34)+"}")),
	} {
		if _, err := verifier.Verify(raw); err == nil {
			t.Fatal("accepted an ambiguous or malformed token")
		}
	}
	valid := signed(t, validClaims(), jwt.SigningMethodRS256, key, nil)
	for _, values := range [][]string{nil, {valid}, {"Basic " + valid}, {"Bearer " + valid, "Bearer " + valid}} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, value := range values {
			request.Header.Add("Authorization", value)
		}
		response := httptest.NewRecorder()
		verifier.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid header reached the handler") })).ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("invalid header status = %d", response.Code)
		}
	}
}

func TestConfigurationAndKeyValidation(t *testing.T) {
	key := keyForTest(t)
	for _, config := range []Config{
		{},
		{Issuer: "http://issuer.example", Audience: "api", PublicKey: &key.PublicKey},
		{Issuer: "https://issuer.example", PublicKey: &key.PublicKey},
		{Issuer: "https://issuer.example", Audience: "api", PublicKey: &rsa.PublicKey{N: big.NewInt(3), E: 65537}},
		{Issuer: "https://issuer.example", Audience: "api", PublicKey: &rsa.PublicKey{N: key.N, E: 3}},
	} {
		if _, err := NewVerifier(config); err == nil {
			t.Fatal("accepted invalid authentication configuration")
		}
	}
	path := filepath.Join(t.TempDir(), "public.pem")
	data, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: data}), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEGO_AUTH_PUBLIC_KEY_FILE", path)
	t.Setenv("STEGO_AUTH_ISSUER", "https://issuer.example")
	t.Setenv("STEGO_AUTH_AUDIENCE", "example-api")
	if _, err := NewAuthMiddleware(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEGO_AUTH_AUDIENCE", "")
	if _, err := NewAuthMiddleware(); err == nil {
		t.Fatal("startup accepted a missing audience")
	}
	t.Setenv("STEGO_AUTH_AUDIENCE", "example-api")
	if err := os.WriteFile(path, []byte("invalid key"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuthMiddleware(); err == nil {
		t.Fatal("startup accepted a malformed key")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxKeyFileBytes+1)), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuthMiddleware(); err == nil {
		t.Fatal("startup accepted an oversized key file")
	}
}

func TestConcurrentVerification(t *testing.T) {
	key := keyForTest(t)
	verifier := verifierForTest(t, key)
	raw := signed(t, validClaims(), jwt.SigningMethodRS256, key, nil)
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for range 20 {
				id, err := verifier.Verify(raw)
				if err != nil || id.UserID != "alice" {
					t.Errorf("concurrent verification failed: %v", err)
				}
			}
		})
	}
	workers.Wait()
}

func BenchmarkVerify(b *testing.B) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		b.Fatal(err)
	}
	verifier, err := NewVerifier(Config{Issuer: "https://issuer.example", Audience: "example-api", PublicKey: &key.PublicKey})
	if err != nil {
		b.Fatal(err)
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, validClaims()).SignedString(key)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := verifier.Verify(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func TestVerifiedProfileAndConfiguredRoles(t *testing.T) {
	key := keyForTest(t)
	config := Config{Issuer: "https://issuer.example", Audience: "example-api", PublicKey: &key.PublicKey, RolesClaim: "realm_access.roles"}
	verifier, err := NewVerifier(config)
	if err != nil {
		t.Fatal(err)
	}
	claims := validClaims()
	claims["preferred_username"] = "alice-name"
	claims["email"] = "alice@example.test"
	claims["given_name"] = "Alice"
	claims["family_name"] = "Example"
	claims["roles"] = []string{"unselected-role"}
	claims["realm_access"] = map[string]any{"roles": []string{"creator", "viewer"}}
	raw := signed(t, claims, jwt.SigningMethodRS256, key, nil)
	ctx, err := verifier.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	id := IdentityFromContext(ctx)
	if id.UserID != "alice" || id.Username != "alice-name" || id.Email != "alice@example.test" || id.GivenName != "Alice" || id.FamilyName != "Example" || !slices.Equal(id.Roles, []string{"creator", "viewer"}) {
		t.Fatalf("verified profile: %+v", id)
	}
	httpIdentity := Identity{}
	handler := verifier.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpIdentity = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest("GET", "/records", nil)
	request.Header.Set("Authorization", "Bearer "+raw)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !reflect.DeepEqual(id, httpIdentity) {
		t.Fatal("HTTP and shared authentication returned different identities")
	}
	delete(claims, "realm_access")
	raw = signed(t, claims, jwt.SigningMethodRS256, key, nil)
	id, err = verifier.Verify(raw)
	if err != nil || len(id.Roles) != 0 {
		t.Fatal("an absent configured claim used another role source")
	}
	for _, value := range []any{nil, "creator", map[string]any{}, []any{nil}, []any{42}, []string{""}, []string{" creator"}, []string{strings.Repeat("r", 257)}, make([]string, 129)} {
		claims["realm_access"] = map[string]any{"roles": value}
		id, err = verifier.Verify(signed(t, claims, jwt.SigningMethodRS256, key, nil))
		if err == nil || id.UserID != "" || len(id.Roles) != 0 {
			t.Fatalf("malformed roles escaped: %v", value)
		}
	}
	claims["realm_access"] = "not an object"
	if _, err := verifier.Verify(signed(t, claims, jwt.SigningMethodRS256, key, nil)); err == nil {
		t.Fatal("malformed claim parent was accepted")
	}
	for _, path := range []string{".roles", "realm_access..roles", "roles.", "roles/other", "realm_access. roles", strings.Repeat("x", 129)} {
		config.RolesClaim = path
		if _, err := NewVerifier(config); err == nil {
			t.Fatalf("invalid claim path: %q", path)
		}
	}
	if _, err := verifier.Authenticate(nil, raw); err == nil {
		t.Fatal("nil authentication context was accepted")
	}
}
