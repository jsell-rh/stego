package browser

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func applicationFixture(t *testing.T) *fixture {
	t.Helper()
	db := database(t)
	migrate(t, db)
	f := &fixture{db: db, oidc: provider(t), documentLogin: true}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	f.options = options{Origin: origin, Issuer: f.oidc.server.URL, IssuerCA: trust(t, f.oidc.server), ClientID: clientID, SecretFile: privateFile(t, "secret", []byte(secret)), KeyFile: privateFile(t, "key", []byte(base64.StdEncoding.EncodeToString(key)))}
	f.backend = f.start(t)
	return f
}
func applicationLogin(t *testing.T, f *fixture) *http.Cookie {
	t.Helper()
	c, _ := login(t, f)
	return c
}
func TestApplicationIdentityAndRestart(t *testing.T) {
	f := applicationFixture(t)
	c := applicationLogin(t, f)
	for _, backend := range []*Backend{f.backend, f.start(t)} {
		targets := []string{apiPrefix + "/auth/whoami"}
		if len(backend.config.Assets) == 0 {
			targets = append(targets, "/", "/records/record-1", "/assets/app.js")
		}
		for _, target := range targets {
			w := send(backend, "GET", target, "", []*http.Cookie{c}, http.Header{"X-Forwarded-Access-Token": {"attacker"}, "X-Forwarded-User": {"attacker"}, "X-Auth-Request-User": {"attacker"}, "Forwarded": {"host=attacker"}})
			require(t, w.Code == 200, "authenticated application request failed")
			var result struct {
				Path    string
				Headers http.Header
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			require(t, result.Path == target, "application path changed")
			require(t, result.Headers.Get("Authorization") == "Bearer initial-access-value", "server identity was not forwarded")
			for _, name := range []string{"Cookie", "X-Forwarded-Access-Token", "X-Forwarded-User", "X-Auth-Request-User", "Forwarded"} {
				require(t, result.Headers.Get(name) == "", "untrusted header reached application")
			}
			require(t, w.Header().Get("Set-Cookie") == "" && w.Header().Get("X-Private") == "", "application response leaked private headers")
		}
	}
}
func TestApplicationWritesRequireOrigin(t *testing.T) {
	f := applicationFixture(t)
	c := applicationLogin(t, f)
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, headers := range []http.Header{nil, {"Origin": {"https://evil.example"}}, {"Origin": {origin, origin}}, {"Origin": {origin}, "Sec-Fetch-Site": {"same-site"}}, {"Origin": {origin}, "Sec-Fetch-Site": {"none"}}, {"Origin": {origin}, "Sec-Fetch-Site": {"unknown"}}, {"Origin": {origin}, "Sec-Fetch-Site": {"same-origin", "same-origin"}}} {
			w := send(f.backend, method, apiPrefix+"/workspaces", `{"name":"one"}`, []*http.Cookie{c}, headers)
			require(t, w.Code == 403 || w.Code == 400, "unsafe application write was accepted")
		}
		for _, headers := range []http.Header{{"Origin": {origin}}, {"Origin": {origin}, "Sec-Fetch-Site": {"same-origin"}}} {
			w := send(f.backend, method, apiPrefix+"/workspaces", `{"name":"one"}`, []*http.Cookie{c}, headers)
			require(t, w.Code == 200, "same-origin application write failed")
		}
	}
}
func TestApplicationLoginAndDeniedRequests(t *testing.T) {
	f := applicationFixture(t)
	assetPath := "/assets/app.js"
	if len(f.backend.config.Assets) != 0 {
		assetPath = "/assets/main.js"
	}
	for _, target := range []string{apiPrefix + "/auth/whoami", assetPath} {
		require(t, send(f.backend, "GET", target, "", nil, nil).Code == 401, "anonymous request reached application")
	}
	w := send(f.backend, "GET", "/records/record-1", "", nil, http.Header{"Sec-Fetch-Site": {"none"}})
	require(t, w.Code == 303 && strings.HasPrefix(w.Header().Get("Location"), "/auth/login?"), "document did not start login")
	w = send(f.backend, "GET", "/", "", nil, http.Header{"Sec-Fetch-Site": {"cross-site"}})
	require(t, w.Code == 200 && strings.Contains(w.Body.String(), "Sign in to continue"), "cross-site landing did not require sign-in")
	require(t, w.Header().Get("Set-Cookie") == "", "cross-site landing cleared an existing browser cookie")
	c := applicationLogin(t, f)
	require(t, send(f.backend, "GET", apiPrefix+"/auth/whoami", "", []*http.Cookie{c}, http.Header{"Authorization": {"Bearer browser-token"}}).Code == 400, "browser bearer was accepted")
	require(t, send(f.backend, "GET", "/unknown", "", []*http.Cookie{c}, nil).Code == 404, "undeclared UI route was accepted")
	require(t, send(f.backend, "POST", "/records/record-1", "", []*http.Cookie{c}, http.Header{"Origin": {origin}}).Code == 405, "write reached a UI route")
	require(t, send(f.backend, "GET", apiPrefix+"/terminal", "", []*http.Cookie{c}, http.Header{"Upgrade": {"websocket"}, "Sec-Websocket-Key": {"a-key"}}).Code == 403, "upgrade without an origin was accepted")
	require(t, send(f.backend, "GET", apiPrefix+"/unauthorized", "", []*http.Cookie{c}, nil).Code == 401, "upstream denial was lost")
	require(t, send(f.backend, "GET", apiPrefix+"/auth/whoami", "", []*http.Cookie{c}, nil).Code == 401, "denied session remained active")
}
func TestApplicationOriginGuard(t *testing.T) {
	b := &Backend{}
	// This check needs no database. It covers the exact-origin boundary.
	r := httptest.NewRequest("POST", origin+"/api/v1/workspaces", nil)
	b.origin = r.URL
	b.origin.Path = ""
	for _, site := range []string{"same-site", "cross-site", "none", "invalid"} {
		r.Header.Set("Origin", origin)
		r.Header.Set("Sec-Fetch-Site", site)
		require(t, !b.applicationCSRF(r), "untrusted fetch site accepted")
	}
	r.Header.Del("Sec-Fetch-Site")
	r.Header.Del("Origin")
	require(t, !b.applicationCSRF(r), "missing origin accepted")
	r.Header.Set("Origin", origin)
	require(t, b.applicationCSRF(r), "exact origin rejected")
}

func TestApplicationRefreshAndLogout(t *testing.T) {
	f := applicationFixture(t)
	c, csrf := login(t, f)
	expireAccess(t, f, c)
	second := f.start(t)
	w := send(second, "GET", apiPrefix+"/auth/whoami", "", []*http.Cookie{c}, nil)
	require(t, w.Code == 200, "application session did not refresh")
	var result struct{ Headers http.Header }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	require(t, result.Headers.Get("Authorization") == "Bearer refreshed-access-value", "application received an expired token")
	w = send(f.backend, "POST", "/auth/logout", "", []*http.Cookie{c}, http.Header{"Origin": {origin}})
	require(t, w.Code == 403, "application mode bypassed logout CSRF")
	require(t, send(second, "GET", apiPrefix+"/auth/whoami", "", []*http.Cookie{c}, nil).Code == 200, "denied logout ended the session")
	w = send(f.backend, "POST", "/auth/logout", "", []*http.Cookie{c}, http.Header{"Origin": {origin}, CSRFHeader: {csrf}})
	require(t, w.Code == 204, "application logout failed")
	require(t, send(second, "GET", apiPrefix+"/auth/whoami", "", []*http.Cookie{c}, nil).Code == 401, "application session survived logout on another backend")
}
func TestApplicationAddressIsCompilerOwned(t *testing.T) {
	for _, o := range []options{{Origin: origin, Upstream: "http://127.0.0.1:8000"}, {Origin: origin, Upstream: "https://other.example"}, {Origin: origin, UpstreamCA: "unused.pem"}} {
		_, err := newBackend(context.Background(), nil, o)
		require(t, err != nil && strings.Contains(err.Error(), "compiler-owned"), "runtime application address override accepted")
	}
}
