package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func ssoFixture(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(map[string]any{"keys": []any{map[string]any{"kid": "test", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(file, document, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JWK_CERT_FILE", file)
	t.Setenv("JWK_CA_FILE", "")
	t.Setenv("STEGO_AUTH_ISSUER", "https://issuer.example/realm")
	t.Setenv("STEGO_AUTH_AUDIENCE", "service")
	t.Setenv("AUTH_ENABLED", "")
	return key
}

func ssoClaims() jwt.MapClaims {
	return jwt.MapClaims{"iss": "https://issuer.example/realm", "aud": "service", "sub": "caller", "iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Minute).Unix()}
}

func ssoToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims, method jwt.SigningMethod) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = "test"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTrustClaims(t *testing.T) {
	key := ssoFixture(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewJWTHandler()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	called := 0
	next := h.Build()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(204) }))
	for _, name := range []string{"valid", "wrong-issuer", "wrong-audience", "missing-expiry", "missing-issuer", "missing-audience", "missing-subject", "missing-issue-time", "future-issue-time", "expired", "wrong-signature", "wrong-algorithm", "duplicate-header", "oversize-token"} {
		t.Run(name, func(t *testing.T) {
			claims := ssoClaims()
			method := jwt.SigningMethodRS256
			signingKey := key
			switch name {
			case "wrong-issuer":
				claims["iss"] = "https://other.example/realm"
			case "wrong-audience":
				claims["aud"] = "other-service"
			case "missing-expiry":
				delete(claims, "exp")
			case "missing-issuer":
				delete(claims, "iss")
			case "missing-audience":
				delete(claims, "aud")
			case "missing-subject":
				delete(claims, "sub")
			case "missing-issue-time":
				delete(claims, "iat")
			case "future-issue-time":
				claims["iat"] = time.Now().Add(time.Hour).Unix()
			case "expired":
				claims["exp"] = time.Now().Add(-time.Second).Unix()
			case "wrong-signature":
				signingKey = other
			case "wrong-algorithm":
				method = jwt.SigningMethodRS512
			}
			raw := ssoToken(t, signingKey, claims, method)
			if name == "oversize-token" {
				raw = strings.Repeat("x", maxTokenBytes+1)
			}
			r := httptest.NewRequest("GET", "/private", nil)
			r.Header.Set("Authorization", "Bearer "+raw)
			if name == "duplicate-header" {
				r.Header.Add("Authorization", "Bearer "+raw)
			}
			w := httptest.NewRecorder()
			next.ServeHTTP(w, r)
			want := 401
			if name == "valid" {
				want = 204
			}
			if w.Code != want {
				t.Fatalf("got %d, want %d", w.Code, want)
			}
			if want == 401 && (strings.Contains(w.Body.String(), raw) || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("WWW-Authenticate") == "") {
				t.Fatal("unsafe authentication error response")
			}
		})
	}
	if called != 1 {
		t.Fatal("a denied request reached the application")
	}
	for _, p := range []string{"/healthcheck", "/metrics", "/openapi", "/healthcheck/private", "/metrics/private"} {
		w := httptest.NewRecorder()
		next.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		want := 204
		if strings.HasSuffix(p, "/private") {
			want = 401
		}
		if w.Code != want {
			t.Fatal("public path match was not exact", p, w.Code)
		}
	}
	h.Stop()
	h.Stop()
	r := httptest.NewRequest("GET", "/private", nil)
	r.Header.Set("Authorization", "Bearer "+ssoToken(t, key, ssoClaims(), jwt.SigningMethodRS256))
	w := httptest.NewRecorder()
	next.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("stopped handler accepted token")
	}
}

func TestSSOPayloadMapping(t *testing.T) {
	key := ssoFixture(t)
	h, err := NewJWTHandler()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	for _, mode := range []string{"sso", "oidc", "fallback"} {
		claims := ssoClaims()
		claims["email"] = "person@example.com"
		claims["clientId"] = "service-client"
		username, first, last := "caller", "Example", "Person"
		switch mode {
		case "sso":
			claims["username"] = "account"
			claims["preferred_username"] = "ignored"
			claims["first_name"] = first
			claims["last_name"] = last
			username = "account"
		case "oidc":
			claims["preferred_username"] = "preferred"
			claims["given_name"] = first
			claims["family_name"] = last
			username = "preferred"
		case "fallback":
			claims["name"] = first + " " + last
		}
		called := false
		next := h.Build()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			p := GetAuthPayload(r)
			id := IdentityFromContext(r.Context())
			if p.Username != username || p.FirstName != first || p.LastName != last || p.Email != "person@example.com" || p.ClientID != "service-client" || p.Issuer != "https://issuer.example/realm" {
				t.Fatal("SSO mapping changed", mode)
			}
			if id.UserID != username || id.Role != "" || id.ExpiresAt.IsZero() || id.Attributes["client_id"] != p.ClientID || GetUsernameFromContext(r.Context()) != username || !TokenFromContext(r.Context()).Valid {
				t.Fatal("verified context mapping changed")
			}
			w.WriteHeader(204)
		}))
		r := httptest.NewRequest("GET", "/private", nil)
		r.Header.Set("Authorization", "Bearer "+ssoToken(t, key, claims, jwt.SigningMethodRS256))
		next.ServeHTTP(httptest.NewRecorder(), r)
		if !called {
			t.Fatal("valid payload rejected")
		}
	}
}

func TestSSOStartupRequiresTrust(t *testing.T) {
	_ = ssoFixture(t)
	for _, setting := range []struct{ name, value string }{{"STEGO_AUTH_ISSUER", ""}, {"STEGO_AUTH_AUDIENCE", ""}, {"AUTH_ENABLED", "false"}, {"AUTH_ENABLED", "FALSE"}, {"AUTH_ENABLED", "invalid"}, {"JWK_CERT_FILE", "/missing-keys"}} {
		t.Run(setting.name+setting.value, func(t *testing.T) {
			t.Setenv(setting.name, setting.value)
			if h, err := NewJWTHandler(); err == nil {
				h.Stop()
				t.Fatal("unsafe startup accepted")
			}
		})
	}
}

func TestSSOStartupContext(t *testing.T) {
	ssoFixture(t)
	if h, err := NewJWTHandlerWithContext(nil); err == nil {
		h.Stop()
		t.Fatal("nil startup context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h, err := NewJWTHandlerWithContext(ctx); err == nil {
		h.Stop()
		t.Fatal("canceled startup context accepted")
	}
	h, err := NewJWTHandlerWithContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h.Stop()
}
