package process

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProcessHealthTransitions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready, stopping := make(chan struct{}), make(chan struct{})
	handler := processHealth(ctx, ready, stopping)
	check := func(method, path string, want int) {
		t.Helper()
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(method, path, nil))
		if r.Code != want || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("wrong probe response", path, r.Code)
		}
	}
	check("GET", "/livez", 200)
	check("GET", "/readyz", 503)
	check("POST", "/livez", 405)
	check("GET", "/metrics", 404)
	close(ready)
	check("GET", "/readyz", 200)
	close(stopping)
	check("GET", "/readyz", 503)
	check("GET", "/livez", 503)
	other := processHealth(ctx, ready, make(chan struct{}))
	cancel()
	r := httptest.NewRecorder()
	other.ServeHTTP(r, httptest.NewRequest("GET", "/readyz", nil))
	if r.Code != 503 {
		t.Fatal("canceled process remained ready")
	}
}

func TestProcessProbeBoundaries(t *testing.T) {
	for _, address := range []string{"localhost:9082", "0.0.0.0:9082", "127.0.0.1:0", "127.0.0.1:09082", "127.0.0.1:65536", "[::1%eth0]:9082", "127.0.0.1:9082/path"} {
		if monitorAddress(address) == nil {
			t.Fatal("unsafe monitor address accepted")
		}
	}
	for _, address := range []string{"127.0.0.1:9082", "[::1]:9082"} {
		if monitorAddress(address) != nil {
			t.Fatal("loopback rejected")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") }))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	open := func(context.Context) (Application, error) { t.Error("probe opened domain resources"); return nil, nil }
	for _, arg := range []string{"--stego-probe=live", "--stego-probe=ready"} {
		if processCommand(context.Background(), []string{arg}, address, open) != 0 {
			t.Fatal("probe failed")
		}
	}
	for _, args := range [][]string{{"private-argument"}, {"--stego-probe=live", "extra"}, {"--stego-probe=metrics"}} {
		if processCommand(context.Background(), args, address, open) != 2 {
			t.Fatal("invalid argument accepted")
		}
	}
	for _, mode := range []string{"long", "redirect", "status", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "long":
					io.WriteString(w, "ok\nprivate")
				case "redirect":
					http.Redirect(w, r, server.URL, 302)
				case "status":
					w.WriteHeader(503)
					io.WriteString(w, "ok\n")
				case "timeout":
					<-r.Context().Done()
				}
			}))
			defer s.Close()
			start := time.Now()
			if processProbe(context.Background(), strings.TrimPrefix(s.URL, "http://"), "/readyz") == nil || time.Since(start) > 2*time.Second {
				t.Fatal("probe did not fail within its bound")
			}
		})
	}
}

func TestMonitorBindFailureDoesNotOpen(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	open := func(context.Context) (Application, error) {
		t.Error("bind failure opened the application")
		return nil, nil
	}
	if processCommand(context.Background(), nil, listener.Addr().String(), open) != 1 {
		t.Fatal("bind failure did not fail the process")
	}
}
