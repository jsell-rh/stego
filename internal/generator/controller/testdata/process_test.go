package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestProcessHealthTracksQueueAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	metrics := new(Metrics)
	var ready atomic.Bool
	handler := controllerHealth(ctx, metrics)
	check := func(path string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	check("/livez", 200)
	check("/readyz", 503)
	if err := metrics.attach(func() QueueMetrics { return QueueMetrics{Ready: ready.Load()} }); err != nil {
		t.Fatal(err)
	}
	check("/readyz", 503)
	ready.Store(true)
	check("/readyz", 200)
	ready.Store(false)
	check("/readyz", 503)
	ready.Store(true)
	metrics.detach()
	check("/readyz", 503)
	if err := metrics.attach(func() QueueMetrics { return QueueMetrics{Ready: true} }); err != nil {
		t.Fatal(err)
	}
	cancel()
	check("/livez", 503)
	check("/readyz", 503)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/livez", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "GET" {
		t.Fatal("probe method accepted")
	}
}

func TestProcessProbesDoNotStartApplication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/livez" && r.URL.Path != "/readyz" {
			t.Error("wrong probe path")
		}
		w.Write([]byte("ok\n"))
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	run := func(context.Context, *Metrics) error { t.Error("probe started application"); return nil }
	for _, arg := range []string{"--stego-probe=live", "--stego-probe=ready"} {
		if code := controllerProcess(context.Background(), []string{arg}, address, run); code != 0 {
			t.Fatal("probe failed", code)
		}
	}
	for _, args := range [][]string{{"secret"}, {"--stego-probe=metrics"}, {"--stego-probe=live", "extra"}} {
		if code := controllerProcess(context.Background(), args, address, run); code != 2 {
			t.Fatal("unknown argument accepted")
		}
	}
	for _, address := range []string{"localhost:9081", "0.0.0.0:9081", "127.0.0.1:0", "127.0.0.1:09081", "127.0.0.1:9081/elsewhere"} {
		if code := controllerProcess(context.Background(), nil, address, run); code != 2 {
			t.Fatal("invalid probe address accepted")
		}
	}
}

func TestProcessProbeRejectsRedirectsAndInvalidBodies(t *testing.T) {
	for _, body := range []string{"", "ok\nx", strings.Repeat("x", 8192)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		if err := controllerProbe(context.Background(), strings.TrimPrefix(server.URL, "http://"), "/livez"); err == nil {
			t.Error("invalid probe body accepted")
		}
		server.Close()
	}
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Store(true); w.Write([]byte("ok\n")) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	if err := controllerProbe(context.Background(), strings.TrimPrefix(redirect.URL, "http://"), "/livez"); err == nil || followed.Load() {
		t.Fatal("probe followed redirect")
	}
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer blocked.Close()
	started := time.Now()
	if err := controllerProbe(context.Background(), strings.TrimPrefix(blocked.URL, "http://"), "/livez"); err == nil || time.Since(started) > 2*time.Second {
		t.Fatal("probe has no time bound")
	}
}

func TestProcessChild(t *testing.T) {
	mode := os.Getenv("STEGO_TEST_PROCESS_CHILD")
	if mode == "" {
		return
	}
	os.Args = []string{os.Args[0]}
	Main(func(ctx context.Context, m *Metrics) error {
		defer os.WriteFile(os.Getenv("STEGO_TEST_PROCESS_MARKER"), []byte("closed"), 0600)
		switch mode {
		case "panic":
			panic("private-domain-credential")
		case "nil-panic", "legacy-nil-panic":
			panic(nil)
		case "goexit":
			runtime.Goexit()
		case "defer-panic":
			defer func() { panic("private-domain-credential") }()
			return nil
		}
		if mode == "failure" {
			return errors.New("private-domain-credential")
		}
		if err := m.attach(func() QueueMetrics { return QueueMetrics{Ready: true} }); err != nil {
			return err
		}
		defer m.detach()
		<-ctx.Done()
		return os.WriteFile(os.Getenv("STEGO_TEST_PROCESS_MARKER"), []byte("closed"), 0600)
	})
}

func TestProcessSignalAndErrorPrivacy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM process check")
	}
	for _, mode := range []string{"signal", "failure", "panic", "nil-panic", "legacy-nil-panic", "goexit", "defer-panic"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			listener.Close()
			marker := t.TempDir() + "/closed"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessChild$")
			command.Env = append(os.Environ(), "STEGO_TEST_PROCESS_CHILD="+mode, "STEGO_CONTROLLER_MONITOR_ADDR="+address, "STEGO_TEST_PROCESS_MARKER="+marker, "OTEL_EXPORTER_OTLP_ENDPOINT=")
			if mode == "legacy-nil-panic" {
				command.Env = append(command.Env, "GODEBUG=panicnil=1")
			}
			if mode != "signal" {
				output, err := command.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 || strings.Count(string(output), "controller.process.failed") != 1 || strings.Contains(string(output), "private-domain-credential") || strings.Contains(string(output), "goroutine ") {
					t.Fatal("process error contract failed", err)
				}
				if data, err := os.ReadFile(marker); err != nil || string(data) != "closed" {
					t.Fatal("callback cleanup did not finish", err)
				}
				return
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			joined := false
			defer func() {
				if !joined {
					command.Process.Kill()
					command.Wait()
				}
			}()
			for controllerProbe(ctx, address, "/readyz") != nil {
				if ctx.Err() != nil {
					t.Fatal("process did not become ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := command.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			err = command.Wait()
			joined = true
			if err != nil {
				t.Fatal("signal shutdown failed", err)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "closed" {
				t.Fatal("callback cleanup did not finish", err)
			}
		})
	}
}

func TestMonitorAbortClosesListenerAndPreservesErrors(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, mode := range []string{"panic", "goexit", "error"} {
			t.Run(fmt.Sprintf("%t/%s", enabled, mode), func(t *testing.T) {
				sentinel := errors.New("ordinary error")
				var cleaned atomic.Bool
				var address string
				var listener net.Listener
				if enabled {
					var err error
					listener, err = net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					address = listener.Addr().String()
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				done := make(chan error, 1)
				run := func(context.Context, *Metrics) error {
					defer cleaned.Store(true)
					switch mode {
					case "panic":
						panic("private value")
					case "goexit":
						runtime.Goexit()
					}
					return sentinel
				}
				go func() {
					if enabled {
						done <- monitorListener(ctx, listener, run)
					} else {
						done <- Monitor(ctx, "", run)
					}
				}()
				var err error
				select {
				case err = <-done:
				case <-ctx.Done():
					t.Fatal("monitor lost the callback result")
				}
				want := ErrRunAborted
				if mode == "error" {
					want = sentinel
				}
				if !errors.Is(err, want) || !cleaned.Load() {
					t.Fatal("monitor lost failure or cleanup", err)
				}
				if enabled {
					connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
					if err == nil {
						connection.Close()
						t.Fatal("monitor retained its listener")
					}
				}
			})
		}
	}
}
