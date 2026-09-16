package browser

import (
	"context"
	health "example.com/browser-test/out/health"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeclaredApplicationReadiness(t *testing.T) {
	f := applicationFixture(t)
	c := applicationLogin(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	monitor, err := health.NewApplicationMonitor(ctx, f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close()
	status := func() int {
		w := httptest.NewRecorder()
		monitor.Ready(w, httptest.NewRequest("GET", "/readyz", nil))
		return w.Code
	}
	require(t, status() == 503, "application was ready before the first dependency check")
	done := make(chan error, 1)
	go func() { done <- monitor.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("health monitor did not stop")
		}
	}()
	wait := func(code int) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if status() == code {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("application readiness did not follow dependency state")
	}
	wait(200)
	change := func(code string) {
		t.Helper()
		w := send(f.backend, "GET", apiPrefix+"/fixture-health-status?status="+code, "", []*http.Cookie{c}, nil)
		require(t, w.Code == 204, "could not set the test application state")
	}
	change("503")
	defer change("200")
	wait(503)
	w := httptest.NewRecorder()
	monitor.Live(w, httptest.NewRequest("GET", "/livez", nil))
	require(t, w.Code == 200, "application dependency failure changed process liveness")
	change("200")
	wait(200)
	cancel()
	wait(503)
}
