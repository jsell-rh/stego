package browser

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Exercise the generated handler and form with native browser navigation.
// The fixture only installs a session cookie and presses the submit button.
// It does not construct the POST body, Origin, Referer, or fetch metadata.
func TestNativeLogoutBrowser(t *testing.T) {
	if os.Getenv("STEGO_REQUIRE_BROWSER_LOGOUT") != "1" {
		t.Skip("native browser check runs in CI")
	}
	require(t, os.Getenv("GITHUB_ACTIONS") == "true", "run browser checks in CI, not on the developer workstation")
	browser, err := exec.LookPath("google-chrome")
	if err != nil {
		browser, err = exec.LookPath("chromium")
	}
	require(t, err == nil, "a test browser is required")
	output := os.Getenv("STEGO_BROWSER_LOGOUT_ARTIFACTS")
	require(t, output != "", "browser evidence directory is required")
	require(t, os.MkdirAll(output, 0700) == nil, "cannot create browser evidence directory")
	for _, oldPolicy := range []bool{true, false} {
		name := "generated-policy"
		if oldPolicy {
			name = "no-referrer-control"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			active, _ := login(t, f)
			type observation struct {
				Policy string `json:"policy"`
				Origin string `json:"origin"`
				Mode   string `json:"mode"`
				Status int    `json:"status"`
			}
			posts := make(chan observation, 1)
			providerRequests := make(chan bool, 1)
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clean := r.Method == "GET" && r.URL.RequestURI() == "/logout" && r.Header.Get("Referer") == "" && r.Header.Get("Cookie") == ""
				w.WriteHeader(200)
				select {
				case providerRequests <- clean:
				default:
				}
			}))
			defer provider.Close()
			f.backend.logoutOrigin = provider.URL
			f.backend.logoutTarget = provider.URL + "/logout"
			var policy atomic.Value
			policy.Store("")
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/fixture-start":
					setCookie(w, SessionCookie, active.Value, 60)
					http.Redirect(w, r, "/auth/logout", 303)
					return
				case "/fixture-submit.js":
					w.Header().Set("Content-Type", "text/javascript")
					_, _ = w.Write([]byte(`document.querySelector('button[type="submit"]').click();`))
					return
				}
				recorded := httptest.NewRecorder()
				f.backend.ServeHTTP(recorded, r)
				body := recorded.Body.String()
				if r.Method == "GET" && r.URL.Path == "/auth/logout" && recorded.Code == 200 {
					if oldPolicy {
						recorded.Header().Set("Referrer-Policy", "no-referrer")
					}
					policy.Store(recorded.Header().Get("Referrer-Policy"))
					body = strings.Replace(body, "</body>", `<script src="/fixture-submit.js"></script></body>`, 1)
				}
				for key, values := range recorded.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorded.Code)
				_, _ = w.Write([]byte(body))
				if r.Method == "POST" && r.URL.Path == "/auth/logout" {
					select {
					case posts <- observation{policy.Load().(string), r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Mode"), recorded.Code}:
					default:
					}
				}
			}))
			server.Config.ReadHeaderTimeout = 3 * time.Second
			// Publish the complete handler state before StartTLS starts serving.
			f.backend.origin, err = url.Parse("https://" + server.Listener.Addr().String())
			require(t, err == nil, "invalid fixture origin")
			server.StartTLS()
			defer server.Close()
			// Trust only the fixture certificate's public key, not all certificates.
			key := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
			log, err := os.Create(filepath.Join(output, name+".browser.log"))
			require(t, err == nil, "cannot create browser log")
			defer log.Close()
			cmd := exec.Command(browser, "--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage", "--disable-background-networking", "--no-first-run", "--user-data-dir="+t.TempDir(), "--ignore-certificate-errors-spki-list="+base64.StdEncoding.EncodeToString(key[:]), server.URL+"/fixture-start")
			cmd.Stdout, cmd.Stderr = log, log
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			require(t, cmd.Start() == nil, "cannot start test browser")
			wait := make(chan error, 1)
			go func() { wait <- cmd.Wait() }()
			defer func() {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
				select {
				case <-wait:
				case <-time.After(5 * time.Second):
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					select {
					case <-wait:
					case <-time.After(5 * time.Second):
						t.Error("browser did not exit")
					}
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var observed observation
			select {
			case observed = <-posts:
			case <-ctx.Done():
				t.Fatal("native form POST deadline expired")
			}
			data, _ := json.MarshalIndent(observed, "", "  ")
			require(t, os.WriteFile(filepath.Join(output, name+".json"), append(data, '\n'), 0600) == nil, "cannot retain browser result")
			require(t, observed.Mode == "navigate", "test did not use native form navigation")
			_, _, revokes := f.oidc.counts()
			_, _, _, sessionErr := f.backend.store.read(ctx, active.Value)
			if oldPolicy {
				require(t, observed.Policy == "no-referrer" && observed.Origin == "null" && observed.Status == 403, "old policy did not reproduce the rejected native form")
				require(t, revokes == 0 && sessionErr == nil, "denied form removed the session")
			} else {
				require(t, observed.Policy == "same-origin" && observed.Origin == server.URL && observed.Status == 303, "generated native sign-out failed")
				require(t, revokes == 1 && errors.Is(sessionErr, errSession), "sign-out did not remove and revoke the session")
				select {
				case clean := <-providerRequests:
					require(t, clean, "provider received an unsafe sign-out request")
				case <-ctx.Done():
					t.Fatal("provider sign-out deadline expired")
				}
			}
		})
	}
}
