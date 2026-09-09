package command

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type oidcFixture struct {
	t                       *testing.T
	server                  *httptest.Server
	key                     *rsa.PrivateKey
	ca, directory           string
	mode                    atomic.Int32
	refresh, polls, revokes atomic.Int32
	mu                      sync.Mutex
	nonce, challenge        string
}

func oidcServer(t *testing.T) *oidcFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &oidcFixture{t: t, key: key, directory: t.TempDir()}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.ca = filepath.Join(f.directory, "ca.pem")
	if err := os.WriteFile(f.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}), 0644); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *oidcFixture) session() oauthSession {
	return oauthSession{Issuer: f.server.URL + "/tenant", ClientID: "record-cli", CAFile: f.ca}
}
func (f *oidcFixture) sign(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"one"}`))
	body, _ := json.Marshal(claims)
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}
func (f *oidcFixture) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/tenant/.well-known/openid-configuration":
		issuer := f.server.URL + "/tenant"
		endpoint := f.server.URL + "/exchange"
		if f.mode.Load() == 1 {
			issuer += "/wrong"
		}
		if f.mode.Load() == 2 {
			endpoint = "http://untrusted.invalid/token"
		}
		methods := []string{"S256"}
		if f.mode.Load() == 12 {
			methods = nil
		}
		json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": endpoint, "jwks_uri": f.server.URL + "/keys", "device_authorization_endpoint": f.server.URL + "/device", "revocation_endpoint": f.server.URL + "/revoke", "code_challenge_methods_supported": methods, "id_token_signing_alg_values_supported": []string{"RS256"}})
	case "/keys":
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "one", "n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()), "e": "AQAB"}}})
	case "/device":
		if r.ParseForm() != nil || r.Form.Get("scope") != "openid" {
			f.t.Error("device login requested extra scopes")
		}
		f.mu.Lock()
		f.challenge = r.Form.Get("code_challenge")
		f.mu.Unlock()
		if f.mode.Load() != 12 && (r.Form.Get("code_challenge_method") != "S256" || r.Form.Get("code_challenge") == "") {
			f.t.Error("device login omitted PKCE")
		}
		json.NewEncoder(w).Encode(map[string]any{"device_code": "device-private", "user_code": "ABCD-EFGH", "verification_uri": f.server.URL + "/verify", "expires_in": 60, "interval": 1})
	case "/revoke":
		if r.ParseForm() != nil || r.Form.Get("token") != "refresh-new" || r.Form.Get("client_id") != "record-cli" {
			f.t.Error("incorrect revocation request")
		}
		f.revokes.Add(1)
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	case "/exchange":
		if r.ParseForm() != nil || r.Form.Get("client_id") != "record-cli" {
			f.t.Error("invalid token request")
		}
		nonce := ""
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			f.mu.Lock()
			nonce = f.nonce
			challenge := f.challenge
			f.mu.Unlock()
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "one-time-code" || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
				f.t.Error("invalid code or PKCE proof")
			}
		case "refresh_token":
			f.refresh.Add(1)
			nonce = "session-nonce"
			if r.Form.Get("refresh_token") != "refresh-old" {
				f.t.Error("refresh token was reused after rotation")
			}
			if f.mode.Load() == 9 {
				os.Chmod(f.directory, 0500)
			}
		case "urn:ietf:params:oauth:grant-type:device_code":
			f.mu.Lock()
			challenge := f.challenge
			f.mu.Unlock()
			verifier := r.Form.Get("code_verifier")
			digest := sha256.Sum256([]byte(verifier))
			if f.mode.Load() == 12 {
				if verifier != "" {
					f.t.Error("unexpected device PKCE proof")
				}
			} else if verifier == "" || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
				f.t.Error("invalid device PKCE proof")
			}
			n := f.polls.Add(1)
			if r.Form.Get("device_code") != "device-private" {
				f.t.Error("incorrect device code")
			}
			if f.mode.Load() == 10 {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"slow_down"}`))
				return
			}
			if n == 1 {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
		default:
			f.t.Error("unexpected grant type")
		}
		claims := map[string]any{"iss": f.server.URL + "/tenant", "sub": "person-one", "aud": "record-cli", "azp": "record-cli", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "nonce": nonce, "auth_time": 1000}
		switch f.mode.Load() {
		case 3:
			claims["nonce"] = "wrong"
		case 4:
			claims["aud"] = "another-client"
		case 5:
			claims["sub"] = "person-two"
		case 6:
			claims["exp"] = time.Now().Add(-time.Minute).Unix()
		case 7:
			claims["azp"] = "another-client"
		case 8:
			claims["at_hash"] = "wrong"
		case 11:
			claims["iss"] = f.server.URL + "/another-issuer"
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "access-new", "refresh_token": "refresh-new", "id_token": f.sign(claims), "token_type": "Bearer", "expires_in": 60})
	default:
		w.WriteHeader(404)
	}
}
func (f *oidcFixture) browser(t *testing.T, p *oauthProvider) (*oauthSession, error) {
	t.Helper()
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := p.browser(ctx, &output, func(ctx context.Context, address string) {
		u, err := url.Parse(address)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if q.Get("scope") != "openid" {
			t.Fatal("browser login requested extra scopes")
		}
		if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" || q.Get("state") == "" {
			t.Fatal("missing browser flow bindings")
		}
		f.mu.Lock()
		f.nonce = q.Get("nonce")
		f.challenge = q.Get("code_challenge")
		f.mu.Unlock()
		callback, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || callback.Hostname() != "127.0.0.1" {
			t.Fatal("callback is not loopback")
		}
		call := func(state string, want int) {
			t.Helper()
			callback.RawQuery = url.Values{"state": {state}, "code": {"one-time-code"}, "iss": {f.server.URL + "/tenant"}}.Encode()
			response, err := http.Get(callback.String())
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != want || response.Header.Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("callback validation failed")
			}
		}
		call("incorrect", 400)
		call(q.Get("state"), 200)
		call(q.Get("state"), 409)
	})
	if strings.Contains(output.String(), "access-new") || strings.Contains(output.String(), "refresh-new") {
		t.Fatal("login exposed a token")
	}
	return session, err
}
func TestOIDCBrowserVerification(t *testing.T) {
	f := oidcServer(t)
	for _, mode := range []int32{0, 1, 2, 3, 4, 6, 7, 8, 11} {
		f.mode.Store(mode)
		p, err := discover(context.Background(), f.session())
		if mode == 1 || mode == 2 {
			if err == nil {
				p.close()
				t.Fatal("invalid issuer or endpoint accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		session, err := f.browser(t, p)
		p.close()
		if mode == 0 {
			if err != nil || session.Subject != "person-one" || session.RefreshToken != "refresh-new" {
				t.Fatal("browser login failed", err)
			}
		} else if err == nil {
			t.Fatal("invalid ID token accepted")
		}
	}
}
func TestOIDCDevicePolling(t *testing.T) {
	f := oidcServer(t)
	p, err := discover(context.Background(), f.session())
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := p.device(ctx, &output)
	if err != nil || session.Subject != "person-one" || f.polls.Load() != 2 {
		t.Fatal("device authorization failed", err)
	}
	if !strings.Contains(output.String(), "ABCD-EFGH") || strings.Contains(output.String(), "device-private") || strings.Contains(output.String(), "access-new") {
		t.Fatal("device display exposed a private value")
	}
	f.mode.Store(10)
	f.polls.Store(0)
	ctx, cancel = context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := p.device(ctx, &output); err == nil || f.polls.Load() != 1 {
		t.Fatal("slow_down did not delay the next poll")
	}
}
func expiredSession(f *oidcFixture) oauthSession {
	s := f.session()
	s.AccessToken = "access-old"
	s.RefreshToken = "refresh-old"
	s.Subject = "person-one"
	s.Nonce = "session-nonce"
	s.AuthTime = 1000
	s.ExpiresAt = time.Now().Add(-time.Second)
	s.RefreshAt = time.Now().Add(-2 * time.Second)
	return s
}
func saveTestSession(t *testing.T, app Application, s oauthSession) {
	t.Helper()
	root, name, err := configRoot(app, true)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := saveConfiguration(root, name, configuration{URL: s.Issuer, CAFile: s.CAFile, OAuth: &s}); err != nil {
		t.Fatal(err)
	}
}
func TestRefreshProcessHelper(t *testing.T) {
	if os.Getenv("CLI_REFRESH_HELPER") != "1" {
		t.Skip("child process helper")
	}
	_, token, err := requestCredentials(context.Background(), definition())
	if err != nil || token != "access-new" {
		t.Fatal("child could not load refreshed session")
	}
}
func TestOIDCRefreshAcrossProcessesAndLogout(t *testing.T) {
	f := oidcServer(t)
	app := definition()
	t.Setenv(app.ConfigEnv, filepath.Join(f.directory, "session.json"))
	saveTestSession(t, app, expiredSession(f))
	var commands []*exec.Cmd
	for i := 0; i < 4; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRefreshProcessHelper$")
		cmd.Env = append(os.Environ(), "CLI_REFRESH_HELPER=1", "GORACE=atexit_sleep_ms=0")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal("refresh child failed", err)
		}
	}
	if f.refresh.Load() != 1 {
		t.Fatal("concurrent processes repeated refresh")
	}
	saved, err := load(app)
	if err != nil || saved.OAuth.RefreshToken != "refresh-new" || saved.OAuth.RefreshPending {
		t.Fatal("rotated session was not saved")
	}
	if err := logout(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if f.revokes.Load() != 1 {
		t.Fatal("logout did not revoke the provider session")
	}
	if _, err := os.Stat(os.Getenv(app.ConfigEnv)); !os.IsNotExist(err) {
		t.Fatal("logout retained local credentials")
	}
}
func TestOIDCRefreshFailureRemainsPending(t *testing.T) {
	for _, mode := range []int32{5, 9} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			f := oidcServer(t)
			t.Cleanup(func() { os.Chmod(f.directory, 0700) })
			app := definition()
			t.Setenv(app.ConfigEnv, filepath.Join(f.directory, "session.json"))
			saveTestSession(t, app, expiredSession(f))
			f.mode.Store(mode)
			if _, _, err := requestCredentials(context.Background(), app); err == nil {
				t.Fatal("failed refresh returned a credential")
			}
			os.Chmod(f.directory, 0700)
			f.mode.Store(0)
			if _, _, err := requestCredentials(context.Background(), app); err == nil || f.refresh.Load() != 1 {
				t.Fatal("uncertain refresh was retried")
			}
			saved, err := load(app)
			if err != nil || !saved.OAuth.RefreshPending {
				t.Fatal("failed refresh lost its pending record")
			}
		})
	}
}

func TestOIDCLogoutRemovesLocalSessionAfterProviderFailure(t *testing.T) {
	f := oidcServer(t)
	app := definition()
	t.Setenv(app.ConfigEnv, filepath.Join(f.directory, "session.json"))
	saveTestSession(t, app, expiredSession(f))
	f.mode.Store(1)
	if err := logout(context.Background(), app); err == nil {
		t.Fatal("logout hid the provider failure")
	}
	if _, err := os.Stat(os.Getenv(app.ConfigEnv)); !os.IsNotExist(err) {
		t.Fatal("provider failure retained local credentials")
	}
}
func TestOIDCConfigLockCancellation(t *testing.T) {
	f := oidcServer(t)
	app := definition()
	t.Setenv(app.ConfigEnv, filepath.Join(f.directory, "session.json"))
	saveTestSession(t, app, expiredSession(f))
	_, _, release, err := configGuard(context.Background(), app, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := requestCredentials(ctx, app); err == nil {
		t.Fatal("busy lock ignored cancellation")
	}
	if f.refresh.Load() != 0 {
		t.Fatal("busy lock permitted token use")
	}
}

func TestOIDCOutputReservationPrecedesRefresh(t *testing.T) {
	f := oidcServer(t)
	app := definition()
	t.Setenv(app.ConfigEnv, filepath.Join(f.directory, "session.json"))
	saveTestSession(t, app, expiredSession(f))
	target := filepath.Join(f.directory, "existing.json")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), app, []string{"get", "record", "one", "--output-file", target}, &output); err == nil {
		t.Fatal("existing output was accepted")
	}
	if f.refresh.Load() != 0 {
		t.Fatal("invalid output caused a token request")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "keep" || output.Len() != 0 {
		t.Fatal("failed output reservation changed output")
	}
}

func TestOIDCDeviceWithoutPKCEAdvertisement(t *testing.T) {
	f := oidcServer(t)
	f.mode.Store(12)
	p, err := discover(context.Background(), f.session())
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var output bytes.Buffer
	if _, err := p.device(ctx, &output); err != nil {
		t.Fatal(err)
	}
}
