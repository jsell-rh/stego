package httpclient

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

//go:embed testdata/local_test.go
var localTests []byte

func TestLocalApplicationRendering(t *testing.T) {
	for _, port := range []int{-1, 0, 80, 1023, 65536} {
		if _, err := RenderLocalApplication("client", "", port); err == nil {
			t.Fatalf("accepted port %d", port)
		}
	}
	for _, port := range []int{1024, 65535} {
		first, err := RenderLocalApplication("client", "example.com/app/tracing", port)
		if err != nil {
			t.Fatal(err)
		}
		second, err := RenderLocalApplication("client", "example.com/app/tracing", port)
		if err != nil || !bytes.Equal(first.Bytes(), second.Bytes()) {
			t.Fatal("local client generation changed", err)
		}
	}
	ordinary, err := Render("client", "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ordinary.Bytes(), []byte("NewLocalApplication")) {
		t.Fatal("ordinary client contains the local transport")
	}
}

func TestLocalApplicationTransport(t *testing.T) {
	var redirects, waiting atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/whoami":
			if r.Header.Get("Authorization") != "Bearer server-session-token" || r.Header.Get("Accept-Encoding") != "" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"user":"owner"}`)
		case "/api/v1/workspaces":
			body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
			if err != nil || r.Method != "POST" || string(body) != `{"name":"workspace"}` {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(201)
			w.Write(body)
		case "/redirect":
			http.Redirect(w, r, "/redirect-target", 302)
		case "/redirect-target":
			redirects.Add(1)
		case "/stream":
			io.WriteString(w, "first\nsecond\n")
		case "/wait":
			waiting.Add(1)
			defer waiting.Add(-1)
			<-r.Context().Done()
		case "/waiting":
			io.WriteString(w, strconv.Itoa(int(waiting.Load())))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	_, rawPort, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	file, err := RenderLocalApplication("client", "", port)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, data := range map[string][]byte{"go.mod": []byte("module example.com/local-test\ngo 1.26.8\n"), "client.go": file.Bytes(), "local_test.go": localTests} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-p=1", "-race", "-count=1", "-mod=readonly", "-timeout=20s", "-v", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "ALL_PROXY=http://127.0.0.1:1", "NO_PROXY=")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("local application transport: %v\n%s", err, output)
	}
	if redirects.Load() != 0 {
		t.Fatal("transport followed an application redirect")
	}
	t.Logf("%s", output)
}
