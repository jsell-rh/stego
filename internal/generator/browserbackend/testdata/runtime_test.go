package browser

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoginProxyRestartLogout(t *testing.T) {
	f := setup(t)
	active, csrf := login(t, f)
	headers := mutationHeaders(csrf)
	headers.Set("Cookie", "malicious=ignored")
	headers.Set("X-Forwarded-Host", "private.example")
	headers.Set("Proxy-Authorization", "private")
	headers.Set("X-Untrusted", "private")
	// Cookies from the browser are consumed by the backend, never by the API.
	delete(headers, "Cookie")
	w := send(f.backend, "POST", apiPrefix+"/records", `{"name":"one"}`, []*http.Cookie{active}, headers)
	require(t, w.Code == 201 && w.Body.String() == `{"id":"record-1","name":"one"}`, "API contract changed")
	require(t, w.Header().Get("ETag") == `"record-version"` && w.Header().Get("Set-Cookie") == "" && w.Header().Get("X-Private") == "", "unsafe response headers")
	f.mu.Lock()
	sent := f.requests[0]
	f.mu.Unlock()
	require(t, sent.Get("Authorization") == "Bearer initial-access-value", "server access token was not sent")
	for _, name := range []string{"Cookie", "X-Forwarded-Host", "Proxy-Authorization", "X-Untrusted", CSRFHeader, "Origin"} {
		require(t, sent.Get(name) == "", "private header reached the API: "+name)
	}
	f.backend.Close()
	f.backend = f.start(t)
	w = send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, nil)
	require(t, w.Code == 201, "session did not survive backend restart")
	w = send(f.backend, "POST", "/auth/logout", "", []*http.Cookie{active}, mutationHeaders(csrf))
	require(t, w.Code == 204 && cookie(t, w, SessionCookie).MaxAge == -1, "logout failed")
	w = send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, nil)
	require(t, w.Code == 401, "logout did not remove API access")
	_, _, revokes := f.oidc.counts()
	require(t, revokes == 1, "logout did not revoke provider token")
}
func TestDeniedRequestsDoNotReachAPI(t *testing.T) {
	f := setup(t)
	active, csrf := login(t, f)
	cases := []struct {
		name, method, path, body string
		cookies                  []*http.Cookie
		headers                  http.Header
		status                   int
	}{
		{name: "missing session", method: "GET", path: apiPrefix + "/records", status: 401},
		{name: "missing csrf", method: "POST", path: apiPrefix + "/records", cookies: []*http.Cookie{active}, headers: http.Header{"Origin": {origin}}, status: 403},
		{name: "missing origin", method: "POST", path: apiPrefix + "/records", cookies: []*http.Cookie{active}, headers: http.Header{CSRFHeader: {csrf}}, status: 403},
		{name: "wrong origin", method: "POST", path: apiPrefix + "/records", cookies: []*http.Cookie{active}, headers: http.Header{"Origin": {"https://sibling.example.test"}, CSRFHeader: {csrf}}, status: 403},
		{name: "same site is not same origin", method: "GET", path: apiPrefix + "/records", cookies: []*http.Cookie{active}, headers: http.Header{"Sec-Fetch-Site": {"same-site"}}, status: 403},
		{name: "browser bearer", method: "GET", path: apiPrefix + "/records", cookies: []*http.Cookie{active}, headers: http.Header{"Authorization": {"Bearer from-browser"}}, status: 400},
		{name: "duplicate cookie", method: "GET", path: apiPrefix + "/records", cookies: []*http.Cookie{active, active}, status: 401},
		{name: "duplicate origin", method: "POST", path: apiPrefix + "/records", cookies: []*http.Cookie{active}, headers: http.Header{"Origin": {origin, origin}, CSRFHeader: {csrf}}, status: 403},
		{name: "duplicate csrf", method: "POST", path: apiPrefix + "/records", cookies: []*http.Cookie{active}, headers: http.Header{"Origin": {origin}, CSRFHeader: {csrf, csrf}}, status: 403},
		{name: "get logout", method: "GET", path: "/auth/logout", cookies: []*http.Cookie{active}, status: 405},
		{name: "bad method", method: "TRACE", path: apiPrefix + "/records", cookies: []*http.Cookie{active}, status: 405},
		{name: "request body limit", method: "POST", path: apiPrefix + "/records", body: strings.Repeat("x", (1<<20)+1), cookies: []*http.Cookie{active}, headers: mutationHeaders(csrf), status: 413},
		{name: "GET body", method: "GET", path: apiPrefix + "/records", body: "data", cookies: []*http.Cookie{active}, status: 400},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			w := send(f.backend, test.method, test.path, test.body, test.cookies, test.headers)
			if w.Code != test.status {
				t.Fatalf("status %d, want %d", w.Code, test.status)
			}
		})
	}
	f.mu.Lock()
	count := len(f.requests)
	f.mu.Unlock()
	require(t, count == 0, "denied request reached API")
}
func TestProviderDenialDoesNotEndValidSession(t *testing.T) {
	f := setup(t)
	active, _ := login(t, f)
	f.mu.Lock()
	f.status = 403
	f.mu.Unlock()
	w := send(f.backend, "GET", apiPrefix+"/records", "", []*http.Cookie{active}, nil)
	require(t, w.Code == 403, "API denial changed")
	w = send(f.backend, "GET", "/auth/session", "", []*http.Cookie{active}, nil)
	require(t, strings.Contains(w.Body.String(), `"authenticated":true`), "403 removed session")
	f.mu.Lock()
	f.status = 401
	f.mu.Unlock()
	w = send(f.backend, "GET", apiPrefix+"/records", "", []*http.Cookie{active}, nil)
	require(t, w.Code == 401, "API authentication failure changed")
	w = send(f.backend, "GET", "/auth/session", "", []*http.Cookie{active}, nil)
	require(t, strings.Contains(w.Body.String(), `"authenticated":false`), "401 retained session")
}
func TestCallbackStateAndReplay(t *testing.T) {
	f := setup(t)
	c, target := startLogin(t, f)
	u, _ := url.Parse(target)
	q := u.Query()
	q.Set("state", "wrong")
	u.RawQuery = q.Encode()
	w := send(f.backend, "GET", u.RequestURI(), "", []*http.Cookie{c}, nil)
	require(t, w.Code == 400, "wrong state accepted")
	exchanges, _, _ := f.oidc.counts()
	require(t, exchanges == 0, "wrong state exchanged code")
	b2 := f.start(t)
	results := make(chan int, 2)
	var group sync.WaitGroup
	for _, b := range []*Backend{f.backend, b2} {
		group.Add(1)
		go func() { defer group.Done(); results <- send(b, "GET", target, "", []*http.Cookie{c}, nil).Code }()
	}
	group.Wait()
	close(results)
	successes, denials := 0, 0
	for code := range results {
		if code == 303 {
			successes++
		} else if code == 400 {
			denials++
		} else {
			t.Fatalf("unexpected concurrent callback status %d", code)
		}
	}
	require(t, successes == 1 && denials == 1, "callback was not consumed exactly once")
	exchanges, _, _ = f.oidc.counts()
	require(t, exchanges == 1, "code exchanged more than once")
}
func TestInvalidIdentityClaims(t *testing.T) {
	f := setup(t)
	for _, mode := range []string{"nonce", "issuer", "audience", "expired", "old", "auth-time", "roles", "at-hash"} {
		t.Run(mode, func(t *testing.T) {
			f.oidc.setMode(mode)
			c, target := startLogin(t, f)
			w := send(f.backend, "GET", target, "", []*http.Cookie{c}, nil)
			require(t, w.Code == 502, "invalid identity accepted")
			for _, c := range w.Result().Cookies() {
				require(t, c.Name != SessionCookie, "invalid identity got a session")
			}
		})
	}
	_, _, revokes := f.oidc.counts()
	require(t, revokes == 8, "rejected identity tokens were not revoked")
}
func TestRefreshAcrossInstances(t *testing.T) {
	f := setup(t)
	active, _ := login(t, f)
	expireAccess(t, f, active)
	b2 := f.start(t)
	entered, release := f.oidc.pauseRefresh()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- send(f.backend, "GET", "/auth/session", "", []*http.Cookie{active}, nil) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("refresh did not start")
	}
	w := send(b2, "GET", "/auth/session", "", []*http.Cookie{active}, nil)
	require(t, w.Code == 503 && w.Header().Get("Retry-After") == "1", "second instance did not wait for refresh")
	close(release)
	select {
	case w = <-result:
		require(t, w.Code == 200, "refresh failed")
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not finish")
	}
	w = send(b2, "GET", apiPrefix+"/records", "", []*http.Cookie{active}, nil)
	require(t, w.Code == 201, "refreshed session unavailable to second instance")
	f.mu.Lock()
	token := f.requests[len(f.requests)-1].Get("Authorization")
	f.mu.Unlock()
	require(t, token == "Bearer refreshed-access-value", "old access token used")
	_, refreshes, _ := f.oidc.counts()
	require(t, refreshes == 1, "refresh token rotated twice")
}
func TestLogoutCannotBeUndoneByRefresh(t *testing.T) {
	f := setup(t)
	active, csrf := login(t, f)
	expireAccess(t, f, active)
	b2 := f.start(t)
	entered, release := f.oidc.pauseRefresh()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- send(f.backend, "GET", "/auth/session", "", []*http.Cookie{active}, nil) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("refresh did not start")
	}
	w := send(b2, "POST", "/auth/logout", "", []*http.Cookie{active}, mutationHeaders(csrf))
	if w.Code != 204 {
		close(release)
		t.Fatalf("logout status %d", w.Code)
	}
	close(release)
	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not finish")
	}
	w = send(b2, "GET", apiPrefix+"/records", "", []*http.Cookie{active}, nil)
	require(t, w.Code == 401, "refresh restored logged-out session")
	_, refreshes, revokes := f.oidc.counts()
	require(t, refreshes == 1 && revokes == 2, "old and rotated tokens were not revoked")
}
func TestUncertainRefreshRequiresLogin(t *testing.T) {
	for _, mode := range []string{"lost-response", "stale-claim"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			active, _ := login(t, f)
			expireAccess(t, f, active)
			if mode == "lost-response" {
				f.oidc.setMode("lost-refresh")
			} else {
				_, err := f.backend.store.claimRefresh(context.Background(), active.Value)
				if err != nil {
					t.Fatal(err)
				}
				hash, _ := sessionHash(active.Value)
				if _, err := f.db.Exec("UPDATE stego_browser_sessions SET changed_at=CURRENT_TIMESTAMP-INTERVAL '31 seconds' WHERE id_hash=$1", hash); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				w := send(f.backend, "GET", apiPrefix+"/records", "", []*http.Cookie{active}, nil)
				require(t, w.Code == 401, "uncertain refresh retained access")
			}
			_, refreshes, _ := f.oidc.counts()
			want := 1
			if mode == "stale-claim" {
				want = 0
			}
			require(t, refreshes == want, "uncertain refresh was retried")
		})
	}
}
func TestStorageEncryptionAndMigration(t *testing.T) {
	db := database(t)
	ctx := context.Background()
	key := bytes.Repeat([]byte{7}, 32)
	if _, err := newStore(ctx, db, key); err == nil {
		t.Fatal("missing migration accepted")
	}
	migrate(t, db)
	s, err := newStore(ctx, db, key)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := randomValue()
	value := session{Kind: "login", Expires: time.Now().Add(time.Minute).Unix(), Access: "private-access-token", Refresh: "private-refresh-token"}
	if err := s.create(ctx, id, value); err != nil {
		t.Fatal(err)
	}
	var hash, payload []byte
	if err := db.QueryRow("SELECT id_hash,payload FROM stego_browser_sessions").Scan(&hash, &payload); err != nil {
		t.Fatal(err)
	}
	require(t, len(hash) == 32 && !bytes.Contains(payload, []byte(value.Access)) && !bytes.Contains(payload, []byte(value.Refresh)) && !bytes.Contains(payload, []byte(id)), "private session data stored in plaintext")
	other, _ := randomValue()
	if _, err := s.open(other, payload); err == nil {
		t.Fatal("ciphertext was not bound to cookie ID")
	}
	payload[len(payload)-1] ^= 1
	if _, err := s.open(id, payload); err == nil {
		t.Fatal("modified ciphertext accepted")
	}
	require(t, s.remove(ctx, id) == nil, "delete failed")
	require(t, errors.Is(func() error { _, _, _, err := s.read(ctx, id); return err }(), errSession), "removed session exists")
}
func TestStaticRoutesAndBoundaries(t *testing.T) {
	f := setup(t)
	for _, path := range []string{"/", "/records/record-1", "/assets/main.js"} {
		w := send(f.backend, "GET", path, "", nil, nil)
		require(t, w.Code == 200, "declared asset route failed")
		require(t, w.Header().Get("Content-Security-Policy") != "" && w.Header().Get("X-Content-Type-Options") == "nosniff", "missing response controls")
	}
	for _, path := range []string{"/unknown", "/records/a/b", "/assets/secret.pem"} {
		w := send(f.backend, "GET", path, "", nil, nil)
		require(t, w.Code == 404, "unknown route returned SPA")
	}
	for _, path := range []string{"https://evil.example/x", "//evil.example/x", "/unknown", "/records/%2F%2Fevil.example", "/records/a#fragment"} {
		require(t, f.backend.returnPath(path) == "/", "unsafe return route accepted")
	}
	for _, address := range []string{"http://console.example.test/", "https://other.example.test/"} {
		r := httptest.NewRequest("GET", address, nil)
		w := httptest.NewRecorder()
		f.backend.ServeHTTP(w, r)
		require(t, w.Code == 400, "wrong host or plaintext accepted")
	}
	w := send(f.backend, "GET", "/assets/main.js", "", nil, nil)
	again := send(f.backend, "GET", "/assets/main.js", "", nil, http.Header{"If-None-Match": {w.Header().Get("ETag")}})
	require(t, again.Code == 304 && again.Body.Len() == 0, "asset cache validation failed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require(t, f.backend.Run(ctx) == nil, "canceled backend did not stop")
}
func TestStrictJSON(t *testing.T) {
	for _, text := range []string{`{"a":1,"a":2}`, `{"a":"\ud800"}`, `{"a":"\udc00"}`, `{"a":"\ud800x"}`, `{} {}`, strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34), string([]byte{34, 255, 34})} {
		require(t, !validJSON([]byte(text)), "invalid JSON accepted")
	}
	for _, text := range []string{`{"a":"\ud83d\ude00"}`, `{"a":"\\ud800"}`, `{"a":[1,null,true]}`} {
		require(t, validJSON([]byte(text)), "valid JSON rejected")
	}
}
func TestBackendConfiguration(t *testing.T) {
	f := setup(t)
	for _, upstream := range []string{origin, origin + ":443", origin + "/api", "http://api.example.test", "https://api.example.test/path", "https://api.example.test?query=x"} {
		o := f.options
		o.Upstream = upstream
		if b, err := newBackend(context.Background(), f.db, o); err == nil {
			b.Close()
			t.Fatal("invalid upstream accepted")
		}
	}
}

func TestAPINotModified(t *testing.T) {
	f := setup(t)
	active, _ := login(t, f)
	f.mu.Lock()
	f.status = 304
	f.mu.Unlock()
	w := send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, http.Header{"If-None-Match": {`"record-version"`}})
	require(t, w.Code == 304 && w.Body.Len() == 0 && w.Header().Get("ETag") == `"record-version"`, "API cache response changed")
}

func TestStorageFailureIsNotLogout(t *testing.T) {
	f := setup(t)
	active, _ := login(t, f)
	f.db.Close()
	w := send(f.backend, "GET", "/auth/session", "", []*http.Cookie{active}, nil)
	require(t, w.Code == 503 && w.Header().Get("Retry-After") == "1", "storage failure did not report temporary unavailability")
	require(t, len(w.Result().Cookies()) == 0, "storage failure deleted browser cookie")
}
func TestPendingLoginCapacity(t *testing.T) {
	f := setup(t)
	id, _ := randomValue()
	value := session{Kind: "login", Expires: time.Now().Add(time.Minute).Unix()}
	payload, err := f.backend.store.seal(id, value)
	if err != nil {
		t.Fatal(err)
	}
	// One bounded insert reaches the fixed capacity without a request load test.
	_, err = f.db.Exec("INSERT INTO stego_browser_sessions(id_hash,payload,state,expires_at) SELECT decode(md5(i::text)||md5('fixture-'||i::text),'hex'),$1,'login',CURRENT_TIMESTAMP+INTERVAL '1 minute' FROM generate_series(1,1000) AS i", payload)
	if err != nil {
		t.Fatal(err)
	}
	require(t, errors.Is(f.backend.store.create(context.Background(), id, value), errStore), "pending login limit was not enforced")
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM stego_browser_sessions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	require(t, count == 1000, "capacity failure left an extra session")
}

func TestCrossSiteLandingDoesNotExposeSession(t *testing.T) {
	f := setup(t)
	active, _ := login(t, f)
	for _, site := range []string{"cross-site", "same-site"} {
		headers := http.Header{"Sec-Fetch-Site": {site}}
		w := send(f.backend, "GET", "/", "", []*http.Cookie{active}, headers)
		require(t, w.Code == 200, "external link cannot open the public application page")
		for _, path := range []string{"/auth/session", "/auth/login", apiPrefix + "/records"} {
			w = send(f.backend, "GET", path, "", []*http.Cookie{active}, headers)
			require(t, w.Code == 403, "cross-origin request obtained a private response")
		}
	}
}
