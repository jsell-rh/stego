package browser

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const origin = "https://console.example.test"
const secret = "browser-fixture-secret-value"
const clientID = "browser-client"
const apiPrefix = "/api/records/v1"

func database(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("database fixture is required")
		}
		t.Skip("database fixture is not configured")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid database fixture configuration")
	}
	cfg.ConnectTimeout = 3 * time.Second
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("stego_browser_%x", suffix)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	t.Cleanup(func() {
		db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	return db
}
func migrate(t *testing.T, db *sql.DB) {
	t.Helper()
	data, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, string(data)); err != nil {
		t.Fatal(err)
	}
}
func privateFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func trust(t *testing.T, server *httptest.Server) string {
	t.Helper()
	return privateFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
}
func require(t *testing.T, condition bool, message string) {
	t.Helper()
	if !condition {
		t.Fatal(message)
	}
}

type authorization struct{ nonce, challenge, redirect string }
type fakeOIDC struct {
	t                             *testing.T
	server                        *httptest.Server
	key                           *ecdsa.PrivateKey
	mu                            sync.Mutex
	codes                         map[string]authorization
	mode                          string
	exchanges, refreshes, revokes int
	entered, release              chan struct{}
}

func provider(t *testing.T) *fakeOIDC {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOIDC{t: t, key: key, codes: map[string]authorization{}}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.handle))
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}
func (f *fakeOIDC) sign(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "key-1", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	message := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(message))
	r, s, err := ecdsa.Sign(rand.Reader, f.key, digest[:])
	if err != nil {
		panic(err)
	}
	signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return message + "." + base64.RawURLEncoding.EncodeToString(signature)
}
func (f *fakeOIDC) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		f.mu.Lock()
		mode := f.mode
		f.mu.Unlock()
		authorizationEndpoint := f.server.URL + "/authorize"
		if mode == "cookie-host" {
			authorizationEndpoint = origin + ":8443/authorize"
		}
		json.NewEncoder(w).Encode(map[string]any{"issuer": f.server.URL, "authorization_endpoint": authorizationEndpoint, "token_endpoint": f.server.URL + "/token", "jwks_uri": f.server.URL + "/keys", "revocation_endpoint": f.server.URL + "/revoke", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"ES256"}, "code_challenge_methods_supported": []string{"S256"}})
	case "/keys":
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": "key-1", "x": base64.RawURLEncoding.EncodeToString(f.key.X.FillBytes(make([]byte, 32))), "y": base64.RawURLEncoding.EncodeToString(f.key.Y.FillBytes(make([]byte, 32)))}}})
	case "/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != clientID || q.Get("response_type") != "code" || q.Get("redirect_uri") != origin+"/auth/callback" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" || len(q.Get("code_challenge")) != 43 {
			http.Error(w, "invalid authorization request", 400)
			return
		}
		code, _ := randomValue()
		f.mu.Lock()
		f.codes[code] = authorization{q.Get("nonce"), q.Get("code_challenge"), q.Get("redirect_uri")}
		f.mu.Unlock()
		target := q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}, "iss": {f.server.URL}}.Encode()
		http.Redirect(w, r, target, 302)
	case "/token", "/revoke":
		id, password, ok := r.BasicAuth()
		if !ok || id != url.QueryEscape(clientID) || password != url.QueryEscape(secret) || r.Method != "POST" {
			http.Error(w, "invalid client authentication", 401)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		if r.URL.Path == "/revoke" {
			f.revokes++
			f.mu.Unlock()
			w.WriteHeader(200)
			return
		}
		mode := f.mode
		if r.Form.Get("grant_type") == "refresh_token" {
			f.refreshes++
			entered, release := f.entered, f.release
			f.mu.Unlock()
			if entered != nil {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			if mode == "lost-refresh" {
				w.WriteHeader(503)
				return
			}
			json.NewEncoder(w).Encode(oauthTokens{Access: "refreshed-access-value", Refresh: "rotated-refresh-value", Type: "Bearer", Expires: 300})
			return
		}
		auth, ok := f.codes[r.Form.Get("code")]
		delete(f.codes, r.Form.Get("code"))
		f.exchanges++
		f.mu.Unlock()
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != auth.redirect || base64.RawURLEncoding.EncodeToString(challenge[:]) != auth.challenge {
			w.WriteHeader(400)
			return
		}
		now := time.Now().Unix()
		claims := map[string]any{"iss": f.server.URL, "aud": clientID, "sub": "owner-a", "iat": now, "exp": now + 300, "auth_time": now, "nonce": auth.nonce, "roles": []string{"user"}, "name": "Owner A"}
		switch mode {
		case "nonce":
			claims["nonce"] = "wrong"
		case "issuer":
			claims["iss"] = "https://wrong.example"
		case "audience":
			claims["aud"] = "other-client"
		case "expired":
			claims["exp"] = now - 120
		case "old":
			claims["iat"] = now - 600
		case "auth-time":
			claims["auth_time"] = "yesterday"
		case "roles":
			claims["roles"] = true
		case "at-hash":
			claims["at_hash"] = "invalid"
		}
		json.NewEncoder(w).Encode(oauthTokens{Access: "initial-access-value", Refresh: "initial-refresh-value", ID: f.sign(claims), Type: "Bearer", Expires: 300})
	default:
		w.WriteHeader(404)
	}
}
func (f *fakeOIDC) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges, f.refreshes, f.revokes
}
func (f *fakeOIDC) setMode(mode string) { f.mu.Lock(); defer f.mu.Unlock(); f.mode = mode }
func (f *fakeOIDC) pauseRefresh() (chan struct{}, chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entered = make(chan struct{}, 1)
	f.release = make(chan struct{})
	return f.entered, f.release
}

type fixture struct {
	db       *sql.DB
	oidc     *fakeOIDC
	backend  *Backend
	options  options
	upstream *httptest.Server
	mu       sync.Mutex
	requests []http.Header
	paths    []string
	status   int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	db := database(t)
	migrate(t, db)
	f := &fixture{db: db, oidc: provider(t), status: 201}
	f.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Header.Clone())
		f.paths = append(f.paths, r.URL.RequestURI())
		status := f.status
		f.mu.Unlock()
		w.Header().Set("Set-Cookie", "upstream=secret")
		w.Header().Set("X-Private", "private")
		w.Header().Set("ETag", `"record-version"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, `{"id":"record-1","name":"one"}`)
	}))
	t.Cleanup(f.upstream.Close)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	f.options = options{Origin: origin, Upstream: f.upstream.URL, UpstreamCA: trust(t, f.upstream), Issuer: f.oidc.server.URL, IssuerCA: trust(t, f.oidc.server), ClientID: clientID, SecretFile: privateFile(t, "secret", []byte(secret)), KeyFile: privateFile(t, "key", []byte(base64.StdEncoding.EncodeToString(key)))}
	f.backend = f.start(t)
	return f
}
func (f *fixture) start(t *testing.T) *Backend {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := newBackend(ctx, f.db, f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}
func send(b *Backend, method, target, body string, cookies []*http.Cookie, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, origin+target, strings.NewReader(body))
	for _, c := range cookies {
		r.AddCookie(c)
	}
	for key, values := range headers {
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	return w
}
func cookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("missing %s cookie", name)
	return nil
}
func startLogin(t *testing.T, f *fixture) (*http.Cookie, string) {
	t.Helper()
	w := send(f.backend, "GET", "/auth/login?return_to=%2Frecords%2Frecord-1", "", nil, http.Header{"Sec-Fetch-Site": {"none"}})
	require(t, w.Code == 302, "login did not redirect")
	c := cookie(t, w, LoginCookie)
	require(t, c.Secure && c.HttpOnly && c.SameSite == http.SameSiteLaxMode && c.Path == "/" && c.Domain == "", "unsafe login cookie")
	client := f.oidc.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Get(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	require(t, response.StatusCode == 302, "authorization failed")
	target, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return c, target.RequestURI()
}
func login(t *testing.T, f *fixture) (*http.Cookie, string) {
	t.Helper()
	c, target := startLogin(t, f)
	w := send(f.backend, "GET", target, "", []*http.Cookie{c}, http.Header{"Sec-Fetch-Site": {"cross-site"}})
	require(t, w.Code == 303, "callback failed")
	require(t, w.Header().Get("Location") == "/records/record-1", "return route changed")
	active := cookie(t, w, SessionCookie)
	require(t, active.Secure && active.HttpOnly && active.SameSite == http.SameSiteStrictMode && active.Path == "/" && active.Domain == "", "unsafe active cookie")
	w = send(f.backend, "GET", "/auth/session", "", []*http.Cookie{active}, nil)
	require(t, w.Code == 200, "session unavailable")
	var result struct {
		Authenticated bool   `json:"authenticated"`
		CSRF          string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	require(t, result.Authenticated && result.CSRF != "", "missing authenticated session")
	require(t, !strings.Contains(w.Body.String(), "access-value") && !strings.Contains(w.Body.String(), "refresh-value"), "tokens reached the browser")
	return active, result.CSRF
}
func expireAccess(t *testing.T, f *fixture, c *http.Cookie) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	value, _, _, err := f.backend.store.read(ctx, c.Value)
	if err != nil {
		t.Fatal(err)
	}
	value.AccessExpires = time.Now().Add(-time.Minute).Unix()
	payload, err := f.backend.store.seal(c.Value, value)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := sessionHash(c.Value)
	if _, err := f.db.ExecContext(ctx, "UPDATE stego_browser_sessions SET payload=$2 WHERE id_hash=$1", hash, payload); err != nil {
		t.Fatal(err)
	}
}
func mutationHeaders(csrf string) http.Header {
	return http.Header{"Origin": {origin}, CSRFHeader: {csrf}, "Content-Type": {"application/json"}}
}
