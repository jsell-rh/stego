package sample

import (
	"context"
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

	client "example.com/http-test/out/application/client"
)

func secureClient(t *testing.T, server *httptest.Server) *client.Client {
	t.Helper()
	name := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(name, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(client.Options{BaseURL: server.URL, CAFile: name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func TestBoundedHTTPSClient(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "https://untrusted.example/secret", 307)
		case "/large":
			w.Write([]byte(strings.Repeat("x", client.MaxResponseBytes+1)))
		case "/slow":
			<-r.Context().Done()
		case "/fail":
			http.Error(w, "sensitive-provider-body", 503)
		default:
			w.Write([]byte("ok"))
		}
	}))
	defer server.Close()
	c := secureClient(t, server)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	mismatch, err := client.New(client.Options{BaseURL: strings.Replace(server.URL, "127.0.0.1", "localhost", 1), CAFile: caPath})
	if err != nil {
		t.Fatal(err)
	}
	defer mismatch.Close()
	if _, err := mismatch.Do(context.Background(), "GET", "/record", nil, nil); err == nil {
		t.Fatal("accepted wrong TLS server identity")
	}

	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	response, err := c.Do(context.Background(), "POST", "/record", http.Header{"Authorization": []string{"Bearer secret"}}, []byte(`{"title":"sample"}`))
	if err != nil || string(response.Body) != "ok" {
		t.Fatalf("valid request: %v", err)
	}
	before := calls.Load()
	for _, path := range []string{"//untrusted.example/path", "https://untrusted.example/path", "/../other", "/%2e%2e/other", "/%2fother", "/path#fragment"} {
		if _, err := c.Do(context.Background(), "GET", path, nil, nil); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	if _, err := c.Do(context.Background(), "POST", "/record", nil, make([]byte, client.MaxRequestBytes+1)); err == nil {
		t.Fatal("accepted large request")
	}
	if calls.Load() != before {
		t.Fatal("invalid request reached provider")
	}
	for _, path := range []string{"/redirect", "/large"} {
		if _, err := c.Do(context.Background(), "GET", path, nil, nil); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	before = calls.Load()
	response, err = c.Do(context.Background(), "POST", "/fail", nil, []byte("secret"))
	if err != nil || response.StatusCode != 503 || calls.Load() != before+1 {
		t.Fatal("failed mutation was replayed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Do(ctx, "GET", "/slow", nil, nil); err == nil {
		t.Fatal("ignored deadline")
	}
	c.Close()
	if _, err := c.Do(context.Background(), "GET", "/record", nil, nil); err == nil {
		t.Fatal("accepted request after close")
	}
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted server reached") }))
	defer other.Close()
	wrong := secureClient(t, other)
	// A request cannot replace the configured origin.
	if _, err := wrong.Do(context.Background(), "GET", server.URL, nil, nil); err == nil {
		t.Fatal("accepted foreign origin")
	}
}
func TestHTTPSCapacity(t *testing.T) {
	entered := make(chan struct{}, client.MaxConcurrentRequests)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	c := secureClient(t, server)
	var wg sync.WaitGroup
	for range client.MaxConcurrentRequests {
		wg.Go(func() {
			if _, err := c.Do(context.Background(), "GET", "/record", nil, nil); err != nil {
				t.Error(err)
			}
		})
	}
	for range client.MaxConcurrentRequests {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("requests did not reach service")
		}
	}
	_, err := c.Do(context.Background(), "GET", "/record", nil, nil)
	close(release)
	wg.Wait()
	if err == nil {
		t.Fatal("accepted excess request")
	}
}
func TestHTTPSCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadPrivateFile(path); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0644)
	if _, err := client.ReadPrivateFile(path); err == nil {
		t.Fatal("accepted public secret")
	}
	for _, base := range []string{"http://example.test", "https://user:secret@example.test", "https://example.test?query", "https://example.test/#fragment", "https://example.test/../"} {
		if _, err := client.New(client.Options{BaseURL: base, CAFile: path}); err == nil {
			t.Fatal("accepted invalid base")
		}
	}
}
