package keycloak

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, Options) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	ca, secret := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "secret")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("private-admin-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	options := Options{ServerURL: server.URL, Realm: "tenant", ClientID: "operator", SecretFile: secret, CAFile: ca}
	c, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, options
}
func authRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/realms/tenant/protocol/openid-connect/token" {
		return false
	}
	if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "operator" || r.Form.Get("client_secret") != "private-admin-secret" {
		http.Error(w, "unexpected authentication", 400)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"access_token":"admin-token","expires_in":300,"token_type":"Bearer"}`))
	return true
}

func TestTypedReadsAndRealmBoundary(t *testing.T) {
	var calls atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if authRequest(w, r) {
			return
		}
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			t.Error("administrator credential missing")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/admin/realms/tenant/clients":
			if r.URL.Query().Get("clientId") != "" {
				if r.URL.Query().Get("clientId") != "catalog & reports" || r.URL.Query().Get("max") != "2" || r.URL.Query().Get("search") != "false" {
					t.Error("lookup query differs")
				}
			} else if r.URL.Query().Get("first") != "100" || r.URL.Query().Get("max") != "5" || len(r.URL.Query()) != 2 {
				t.Error("page query differs")
			}
			_, _ = w.Write([]byte(`[{"id":"provider-id","clientId":"catalog & reports","attributes":{"product.owner":"object-1"}}]`))
		case "/admin/realms/tenant/clients/provider-id":
			_, _ = w.Write([]byte(`{"id":"provider-id","clientId":"catalog & reports","attributes":{"product.owner":"object-1"}}`))
		default:
			t.Error("unexpected provider path", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	ctx := context.Background()
	if got, err := c.FindClient(ctx, "catalog & reports"); err != nil || got.ID != "provider-id" {
		t.Fatal("typed lookup failed", err)
	}
	if got, err := c.ListClients(ctx, Page{First: 100, Size: 5}); err != nil || len(got) != 1 {
		t.Fatal("typed page failed", err)
	}
	if _, err := c.InspectClient(ctx, ClientBinding{ID: "provider-id", ClientID: "catalog & reports", Attributes: map[string]string{"product.owner": "object-1"}}); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	for _, id := range []string{"", "../other", "a/b", "a%2fb", "other?x=y", "other#x", "a\x00b"} {
		if _, err := c.GetClient(ctx, id); err == nil {
			t.Fatal("unsafe identifier accepted")
		}
	}
	for _, page := range []Page{{Size: 0}, {First: -1, Size: 1}, {Size: 101}, {First: 9999, Size: 2}} {
		if _, err := c.ListClients(ctx, page); err == nil {
			t.Fatal("invalid page accepted")
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid arguments caused a network request")
	}
	if calls.Load() != 4 {
		t.Fatal("administrator token was not cached", calls.Load())
	}
}

func TestOwnershipDisableAndDelete(t *testing.T) {
	for _, policy := range []struct{ clientID, key, value string }{{"catalog", "product.owner", "object-1"}, {"batch-worker", "pipeline.job", "run-2"}} {
		t.Run(policy.clientID, func(t *testing.T) {
			enabled, deleted := true, false
			writes := 0
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if authRequest(w, r) {
					return
				}
				if r.URL.Path != "/admin/realms/tenant/clients/provider-id" {
					t.Error("unexpected resource")
					w.WriteHeader(500)
					return
				}
				if deleted {
					w.WriteHeader(404)
					return
				}
				switch r.Method {
				case "GET":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "provider-id", "clientId": policy.clientID, "enabled": enabled, "attributes": map[string]string{policy.key: policy.value}, "futureProviderField": map[string]string{"keep": "yes"}})
				case "PUT":
					writes++
					var record map[string]json.RawMessage
					if json.NewDecoder(r.Body).Decode(&record) != nil || string(record["enabled"]) != "false" || !strings.Contains(string(record["futureProviderField"]), "yes") {
						t.Error("disablement lost provider settings")
						w.WriteHeader(400)
						return
					}
					enabled = false
					w.WriteHeader(204)
				case "DELETE":
					writes++
					deleted = true
					w.WriteHeader(204)
				default:
					t.Error("unexpected method")
					w.WriteHeader(500)
				}
			})
			b := ClientBinding{ID: "provider-id", ClientID: policy.clientID, Attributes: map[string]string{policy.key: policy.value}}
			foreign := b
			foreign.Attributes = map[string]string{policy.key: "another-owner"}
			for _, operation := range []func(context.Context, ClientBinding) error{c.DisableClient, c.DeleteClient} {
				if err := operation(context.Background(), foreign); !errors.Is(err, ErrOwnership) {
					t.Fatal("foreign ownership accepted", err)
				}
			}
			if writes != 0 {
				t.Fatal("foreign resource was changed")
			}
			if err := c.DisableClient(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			if err := c.DisableClient(context.Background(), b); err != nil || writes != 1 {
				t.Fatal("correct state was rewritten", err, writes)
			}
			if err := c.DeleteClient(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			if err := c.DeleteClient(context.Background(), b); err != nil || writes != 2 {
				t.Fatal("deletion was not idempotent", err, writes)
			}
		})
	}
}

func TestMutationRequiresObservedResult(t *testing.T) {
	for _, method := range []string{"disable", "delete"} {
		t.Run(method, func(t *testing.T) {
			writes := 0
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if authRequest(w, r) {
					return
				}
				if r.Method != "GET" {
					writes++
					w.WriteHeader(204)
					return
				}
				_, _ = w.Write([]byte(`{"id":"owned","clientId":"service","enabled":true,"attributes":{"owner":"one"}}`))
			})
			b := ClientBinding{ID: "owned", ClientID: "service", Attributes: map[string]string{"owner": "one"}}
			operation := c.DisableClient
			if method == "delete" {
				operation = c.DeleteClient
			}
			if err := operation(context.Background(), b); err == nil || writes != 1 {
				t.Fatal("unconfirmed mutation passed", err, writes)
			}
		})
	}
}

func TestTokenRotationAndUnauthorizedResponse(t *testing.T) {
	var auth, admin atomic.Int32
	c, options := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			count := auth.Add(1)
			_ = r.ParseForm()
			want := "private-admin-secret"
			if count > 1 {
				want = "rotated-secret"
			}
			if r.Form.Get("client_secret") != want {
				t.Error("secret rotation was not read")
			}
			_, _ = fmt.Fprintf(w, `{"access_token":"token-%d","expires_in":300,"token_type":"Bearer"}`, count)
			return
		}
		count := admin.Add(1)
		if count == 2 {
			w.WriteHeader(401)
			_, _ = w.Write([]byte("private-error-body"))
			return
		}
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer token-%d", auth.Load()) {
			t.Error("old token reused")
		}
		_, _ = w.Write([]byte(`{"id":"owned","clientId":"service"}`))
	})
	if _, err := c.GetClient(context.Background(), "owned"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options.SecretFile, []byte("rotated-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetClient(context.Background(), "owned"); err == nil || strings.Contains(err.Error(), "private-error-body") {
		t.Fatal("unauthorized response was replayed or exposed", err)
	}
	if admin.Load() != 2 || auth.Load() != 2 {
		t.Fatal("unexpected authentication or replay")
	}
	if _, err := c.GetClient(context.Background(), "owned"); err != nil {
		t.Fatal(err)
	}
	if auth.Load() != 3 {
		t.Fatal("unauthorized token remained cached")
	}
}

func TestTokenWaitCancellationAndClose(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	first := make(chan error, 1)
	go func() { _, err := c.GetClient(context.Background(), "owned"); first <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("authentication did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetClient(ctx, "owned"); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled caller was not released", err)
	}
	// The token gate is held by the first request. A waiting caller must leave
	// when its context expires, even though the first authentication is pending.
	waiting, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if _, err := c.GetClient(waiting, "owned"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("token wait ignored its deadline", err)
	}
	c.Close()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("close did not cancel authentication", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close left an active request")
	}
	if _, err := c.GetClient(context.Background(), "owned"); !errors.Is(err, ErrClosed) {
		t.Fatal("closed provider accepted work", err)
	}
}

func TestPrivateCredentialAndServiceAccountBinding(t *testing.T) {
	var reads atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if authRequest(w, r) {
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/client-secret"):
			reads.Add(1)
			_, _ = w.Write([]byte(`{"value":"private-client-secret"}`))
		case strings.HasSuffix(r.URL.Path, "/service-account-user"):
			_, _ = w.Write([]byte(`{"id":"subject","serviceAccountClientId":"service"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"owned","clientId":"service","protocol":"openid-connect","publicClient":false,"serviceAccountsEnabled":true,"attributes":{"owner":"one"}}`))
		}
	})
	b := ClientBinding{ID: "owned", ClientID: "service", Attributes: map[string]string{"owner": "one"}}
	wrong := b
	wrong.ClientID = "other"
	if _, err := c.GetClientSecret(context.Background(), wrong); !errors.Is(err, ErrOwnership) || reads.Load() != 0 {
		t.Fatal("foreign credential was read", err)
	}
	secret, err := c.GetClientSecret(context.Background(), b)
	if err != nil || secret.Reveal() != "private-client-secret" {
		t.Fatal("credential read failed", err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", secret, secret, secret), secret.Reveal()) {
		t.Fatal("credential formatting exposed a secret")
	}
	if _, err := json.Marshal(secret); err == nil {
		t.Fatal("credential serialized without an explicit reveal")
	}
	if user, err := c.ResolveServiceAccountUser(context.Background(), b); err != nil || user.ID != "subject" {
		t.Fatal("service account binding failed", err)
	}
}

func TestMalformedProviderResponses(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"id":"different","clientId":"service"}`, `{"id":"owned","id":"owned","clientId":"service"}`, `{"id":"owned","clientId":"\ud800"}`, `{"id":"owned","clientId":"service"} {}`, `{"id":"owned","clientId":"service","attributes":{"owner":"one","owner":"two"}}`} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if authRequest(w, r) {
					return
				}
				_, _ = w.Write([]byte(body))
			})
			if _, err := c.GetClient(context.Background(), "owned"); !errors.Is(err, ErrResponse) {
				t.Fatal("invalid provider response accepted", err)
			}
		})
	}
	for _, body := range []string{`null`, `[{"id":"x","clientId":"service"},{"id":"x","clientId":"service"}]`, `[{"id":"x","clientId":"other"}]`, `[{"id":"","clientId":"service"}]`} {
		t.Run("lookup-"+fmt.Sprint(len(body)), func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if authRequest(w, r) {
					return
				}
				_, _ = w.Write([]byte(body))
			})
			if _, err := c.FindClient(context.Background(), "service"); !errors.Is(err, ErrResponse) {
				t.Fatal("ambiguous lookup accepted", err)
			}
		})
	}
}

func TestProviderTLSAndRedirects(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	c, options := testClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL+"/private", 302) })
	if _, err := c.GetClient(context.Background(), "owned"); err == nil || redirected.Load() != 0 {
		t.Fatal("authentication followed a redirect", err)
	}
	options.ServerURL = strings.Replace(options.ServerURL, "https:", "http:", 1)
	if other, err := New(options); err == nil {
		other.Close()
		t.Fatal("plaintext provider accepted")
	}
}

func TestDisableRequiresAnExplicitState(t *testing.T) {
	for _, state := range []string{"", `,"enabled":null`} {
		writes := 0
		c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if authRequest(w, r) {
				return
			}
			if r.Method != "GET" {
				writes++
				w.WriteHeader(500)
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":"owned","clientId":"service","attributes":{"owner":"one"}%s}`, state)
		})
		if err := c.DisableClient(context.Background(), ClientBinding{ID: "owned", ClientID: "service", Attributes: map[string]string{"owner": "one"}}); !errors.Is(err, ErrResponse) || writes != 0 {
			t.Fatal("missing state was treated as disabled", err)
		}
	}
}

func TestInvalidAdministratorGrantCannotAuthorizeARequest(t *testing.T) {
	for _, body := range []string{
		`{"access_token":"token","expires_in":300}`,
		`{"access_token":"token","expires_in":300,"token_type":"MAC"}`,
		`{"access_token":"token","expires_in":0,"token_type":"Bearer"}`,
		`{"access_token":"token","expires_in":3601,"token_type":"Bearer"}`,
		`{"access_token":"token","access_token":"second","expires_in":300,"token_type":"Bearer"}`,
	} {
		var requests atomic.Int32
		c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/token") {
				_, _ = w.Write([]byte(body))
				return
			}
			requests.Add(1)
			w.WriteHeader(500)
		})
		if _, err := c.GetClient(context.Background(), "owned"); !errors.Is(err, ErrResponse) || requests.Load() != 0 {
			t.Fatal("invalid administrator grant reached the API", err)
		}
	}
}

func TestProtectedValueFormattingUsesEveryVerb(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{Secret{value: "private-test-credential"}, "[credential redacted]"},
		{&Client{token: Secret{value: "private-admin-token"}}, "KeycloakClient{credentials redacted}"},
		{ClientOwnershipMigration{fingerprint: "private-checkpoint-fingerprint"}, "ClientOwnershipMigration{checkpoint redacted}"},
	} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%d", "%x", "%s", "%q"} {
			if fmt.Sprintf(verb, tc.value) != tc.want {
				t.Fatalf("protected formatting failed for %T with %s", tc.value, verb)
			}
		}
	}
}
