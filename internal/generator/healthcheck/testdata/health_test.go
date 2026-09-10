package health

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func response(m *Monitor, path, method string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, nil)
	if path == "/livez" {
		m.Live(w, r)
	} else {
		m.Ready(w, r)
	}
	return w
}
func awaitStatus(t *testing.T, m *Monitor, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for response(m, "/readyz", "GET").Code != want {
		if time.Now().After(deadline) {
			t.Fatal("readiness did not change", want)
		}
		time.Sleep(time.Millisecond)
	}
}
func runMonitor(t *testing.T, m *Monitor) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("monitor did not stop")
		}
	}
}
func TestFailureRecoveryAndBoundedProbeWork(t *testing.T) {
	var failed atomic.Bool
	failed.Store(true)
	var calls atomic.Int32
	m, err := NewMonitor(context.Background(), func(context.Context) error {
		calls.Add(1)
		if failed.Load() {
			return errors.New("password=private")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if response(m, "/readyz", "GET").Code != 503 {
		t.Fatal("unstarted monitor is ready")
	}
	stop := runMonitor(t, m)
	defer stop()
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("check did not run")
		}
		time.Sleep(time.Millisecond)
	}
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				w := response(m, "/readyz", "GET")
				if w.Code != 503 || w.Body.String() != "unavailable\n" || w.Header().Get("Cache-Control") != "no-store" {
					t.Error("invalid failure response")
				}
			}
		}()
	}
	workers.Wait()
	if calls.Load() != 1 {
		t.Fatal("probe traffic triggered checks", calls.Load())
	}
	if response(m, "/livez", "GET").Code != 200 {
		t.Fatal("dependency failure changed liveness")
	}
	failed.Store(false)
	awaitStatus(t, m, 200)
	failed.Store(true)
	awaitStatus(t, m, 503)
}
func TestCheckDeadlineAndCancellation(t *testing.T) {
	checked := make(chan time.Duration, 1)
	m, _ := NewMonitor(context.Background(), func(ctx context.Context) error {
		started := time.Now()
		<-ctx.Done()
		checked <- time.Since(started)
		return nil
	})
	stop := runMonitor(t, m)
	select {
	case elapsed := <-checked:
		if elapsed > time.Second {
			t.Fatal("check deadline exceeded", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing check deadline")
	}
	if response(m, "/readyz", "GET").Code != 503 {
		t.Fatal("late success was accepted")
	}
	stop()
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("monitor started twice")
	}
	parent, cancel := context.WithCancel(context.Background())
	m, _ = NewMonitor(parent)
	stop = runMonitor(t, m)
	awaitStatus(t, m, 200)
	cancel()
	if response(m, "/readyz", "GET").Code != 503 {
		t.Fatal("parent cancellation did not clear readiness")
	}
	stop()
}
func TestStalledCheckCannotKeepOldReadinessOrSpawnMoreWork(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	m, _ := NewMonitor(context.Background(), func(context.Context) error {
		if calls.Add(1) == 2 {
			close(entered)
			<-release
		}
		return nil
	})
	stop := runMonitor(t, m)
	defer stop()
	defer close(release)
	awaitStatus(t, m, 200)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second check did not start")
	}
	awaitStatus(t, m, 503)
	if calls.Load() != 2 {
		t.Fatal("stalled check caused overlapping calls", calls.Load())
	}
	if response(m, "/livez", "GET").Code != 200 {
		t.Fatal("stalled dependency changed liveness")
	}
}
func TestMethodsAndInvalidConstruction(t *testing.T) {
	m, _ := NewMonitor(context.Background())
	for _, path := range []string{"/livez", "/readyz"} {
		if w := response(m, path, "HEAD"); w.Body.Len() != 0 {
			t.Fatal("HEAD returned content")
		}
		if w := response(m, path, "POST"); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatal("unexpected method accepted")
		}
	}
	if _, err := NewMonitor(nil); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err := NewMonitor(context.Background(), nil); err == nil {
		t.Fatal("nil check accepted")
	}
	if _, err := NewMonitor(context.Background(), make([]Check, MaxChecks+1)...); err == nil {
		t.Fatal("too many checks accepted")
	}
	if _, err := NewDatabaseMonitor(context.Background(), nil); err == nil {
		t.Fatal("nil database accepted")
	}
}
