package controller

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func awaitMetrics(t *testing.T, m *Metrics, check func(MetricsSnapshot) bool) MetricsSnapshot {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		s := m.Snapshot()
		if check(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatal("metrics did not reach expected state", s)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestMetricsExposeBackpressureRetryAndCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	metrics := new(Metrics)
	holding, release := make(chan struct{}), make(chan struct{})
	var failed atomic.Bool
	source := KeyedSource[string]{Observe: func(ctx context.Context, sink *KeySink[string]) error {
		sink.SetReady(true)
		<-ctx.Done()
		return ctx.Err()
	}, Scan: func(ctx context.Context, emit func(string) error) error {
		for _, key := range []string{"PRIVATE-resource-token", "holding", "after"} {
			if err := emit(key); err != nil {
				return err
			}
		}
		return nil
	}}
	options := keyOptions()
	options.Capacity = 2
	options.Metrics = metrics
	options.RetryMin = 20 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		done <- RunKeyed(ctx, source, func(ctx context.Context, key string) error {
			if key == "PRIVATE-resource-token" && !failed.Swap(true) {
				return errors.New("PRIVATE remote error")
			}
			if key == "holding" {
				close(holding)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}, options)
	}()
	select {
	case <-holding:
	case <-ctx.Done():
		t.Fatal("action did not start")
	}
	s := awaitMetrics(t, metrics, func(s MetricsSnapshot) bool { return s.Failed == 1 && s.Queue.Waiting == 1 })
	if !s.Running || !s.Queue.Ready || s.Queue.Capacity != 2 || s.Queue.Active != 1 || s.Queue.Queued != 1 || s.Queue.Retrying != 1 || s.Retries != 1 {
		t.Fatal(s)
	}
	recorder := httptest.NewRecorder()
	metrics.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	if recorder.Code != 200 || strings.Contains(body, "PRIVATE") || !strings.Contains(body, "stego_controller_admission_waiters 1\n") || !strings.Contains(body, "stego_controller_actions_total{outcome=\"failure\"} 1\n") {
		t.Fatal("invalid metrics", recorder.Code, body)
	}
	close(release)
	s = awaitMetrics(t, metrics, func(s MetricsSnapshot) bool { return s.Succeeded == 3 && s.Queue.Active == 0 && s.Queue.Queued == 0 })
	if s.Retries != 1 || s.ScansSucceeded != 1 || s.DurationSeconds <= 0 {
		t.Fatal(s)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s = metrics.Snapshot()
	if s.Running || s.Queue != (QueueMetrics{}) || s.Succeeded != 3 {
		t.Fatal("shutdown did not detach metrics", s)
	}
}

func TestMetricsOutcomesAndBoundedRoutes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		action   func(context.Context) error
		terminal bool
	}{
		{"timeout", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, false},
		{"terminal", func(context.Context) error { return denied }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			m := new(Metrics)
			o := keyOptions()
			o.Metrics = m
			o.Timeout = 5 * time.Millisecond
			o.RetryMin = time.Second
			o.RetryMax = time.Second
			o.Observe = func(e Event) {
				if e.Phase == "reconcile_failed" {
					cancel()
				}
			}
			source := KeyedSource[string]{Observe: func(ctx context.Context, sink *KeySink[string]) error {
				sink.SetReady(true)
				<-ctx.Done()
				return ctx.Err()
			}, Scan: func(ctx context.Context, emit func(string) error) error { return emit("one") }}
			err := RunKeyed(ctx, source, func(ctx context.Context, _ string) error { return tc.action(ctx) }, o)
			s := m.Snapshot()
			if tc.terminal {
				if !errors.Is(err, denied) || s.Failed != 1 || s.Retries != 0 {
					t.Fatal(err, s)
				}
			} else if err != nil || s.TimedOut != 1 || s.Retries != 1 {
				t.Fatal(err, s)
			}
			if s.DurationBuckets[9] != 1 {
				t.Fatal("duration histogram lost action", s)
			}
		})
	}
	m := new(Metrics)
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"POST", "/metrics", 405}, {"GET", "/debug/vars", 404}, {"GET", "/metrics/", 404}} {
		w := httptest.NewRecorder()
		m.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatal(w.Code)
		}
	}
}
func TestMetricsCancelAndSingleOwner(t *testing.T) {
	m := new(Metrics)
	if err := m.attach(func() QueueMetrics { return QueueMetrics{Capacity: 3} }); err != nil {
		t.Fatal(err)
	}
	if err := m.attach(func() QueueMetrics { return QueueMetrics{Capacity: 4} }); !errors.Is(err, ErrMetricsInUse) {
		t.Fatal(err)
	}
	if m.Snapshot().Queue.Capacity != 3 {
		t.Fatal("replaced active queue")
	}
	m.detach()
	ctx, cancel := context.WithCancel(context.Background())
	source := KeyedSource[string]{Observe: func(ctx context.Context, sink *KeySink[string]) error {
		sink.SetReady(true)
		<-ctx.Done()
		return ctx.Err()
	}, Scan: func(ctx context.Context, emit func(string) error) error { return emit("private") }}
	o := keyOptions()
	o.Metrics = m
	if err := RunKeyed(ctx, source, func(ctx context.Context, _ string) error { cancel(); <-ctx.Done(); return ctx.Err() }, o); err != nil {
		t.Fatal(err)
	}
	if s := m.Snapshot(); s.Canceled != 1 || s.Retries != 0 || s.Running {
		t.Fatal(s)
	}
}
func TestMonitorLifecycleAndAddressBoundary(t *testing.T) {
	for _, address := range []string{":9090", "0.0.0.0:9090", "[::]:9090", "localhost:9090", "192.0.2.1:9090", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:09090", "[::1%lo]:9090"} {
		called := false
		if err := Monitor(context.Background(), address, func(context.Context, *Metrics) error { called = true; return nil }); err == nil || called {
			t.Fatal("unsafe address accepted", address, err)
		}
	}
	if err := Monitor(context.Background(), "", func(_ context.Context, m *Metrics) error {
		if m != nil {
			t.Fatal("disabled metrics allocated a collector")
		}
		return denied
	}); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := Monitor(context.Background(), address, func(context.Context, *Metrics) error { t.Error("busy listener started controller"); return nil }); err == nil {
		t.Fatal("busy listener accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- monitorListener(ctx, listener, func(ctx context.Context, m *Metrics) error {
			if m == nil {
				return errors.New("no metrics")
			}
			<-ctx.Done()
			return nil
		})
	}()
	client := http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + address + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16384))
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), "stego_controller_running 0") {
		t.Fatal("metrics endpoint", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not join")
	}
	if response, err := client.Get("http://" + address + "/metrics"); err == nil {
		response.Body.Close()
		t.Fatal("listener remained open")
	}
}
func BenchmarkControllerMetrics(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		var m *Metrics
		if enabled {
			name = "enabled"
			m = new(Metrics)
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					m.action(0, time.Millisecond, false)
				}
			})
		})
	}
}

func TestMetricsSurviveWatchReconnectAndCountFailedScan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m := new(Metrics)
	var watches atomic.Int32
	source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
		attempt := watches.Add(1)
		if attempt == 1 {
			return nil, errors.New("PRIVATE watch failure")
		}
		return func() (string, error) { <-ctx.Done(); return "", ctx.Err() }, nil
	}, Scan: func(context.Context, func(string) error) error { return denied }}
	o := watchKeyOptions()
	o.Metrics = m
	err := RunKeyedWatch(ctx, source, func(context.Context, string) error { return nil }, o)
	s := m.Snapshot()
	if !errors.Is(err, denied) || s.Running || s.Reconnects != 1 || s.ScansFailed != 1 || s.ScansSucceeded != 0 {
		t.Fatal(err, s)
	}
	// A later run can reuse the collector and keeps its process counters.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	source.Scan = func(context.Context, func(string) error) error { return nil }
	o.Observe = func(e Event) {
		if e.Phase == "scan_completed" {
			cancel2()
		}
	}
	if err := RunKeyedWatch(ctx2, source, func(context.Context, string) error { return nil }, o); err != nil {
		t.Fatal(err)
	}
	s = m.Snapshot()
	if s.Reconnects != 1 || s.ScansFailed != 1 || s.ScansSucceeded != 1 || s.Running {
		t.Fatal(s)
	}
}

func TestCleanupMetricsRefreshWhileScanIsBlocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m := new(Metrics)
	o := keyOptions()
	o.Metrics = m
	o.ResyncInterval = 5 * time.Millisecond
	release := make(chan struct{})
	var calls atomic.Int32
	oldest := time.Now().Add(-time.Hour)
	o.Cleanup = func(ctx context.Context) (CleanupSample, error) {
		switch calls.Add(1) {
		case 1:
			return CleanupSample{Pending: 2, OldestPending: &oldest}, nil
		case 2:
			return CleanupSample{}, errors.New("PRIVATE cleanup read failure")
		default:
			select {
			case <-release:
				return CleanupSample{Pending: 1, OldestPending: &oldest}, nil
			case <-ctx.Done():
				return CleanupSample{}, ctx.Err()
			}
		}
	}
	source := KeyedSource[string]{Observe: func(ctx context.Context, sink *KeySink[string]) error {
		sink.SetReady(true)
		<-ctx.Done()
		return ctx.Err()
	}, Scan: func(ctx context.Context, emit func(string) error) error { <-ctx.Done(); return ctx.Err() }}
	done := make(chan error, 1)
	go func() { done <- RunKeyed(ctx, source, func(context.Context, string) error { return nil }, o) }()
	failed := awaitMetrics(t, m, func(s MetricsSnapshot) bool { return s.CleanupReadsFailed == 1 })
	if !failed.CleanupEnabled || failed.CleanupAvailable || failed.CleanupPending != 2 || !failed.CleanupOldest.Equal(oldest) || failed.CleanupSampleAt.IsZero() {
		t.Fatal(failed)
	}
	close(release)
	current := awaitMetrics(t, m, func(s MetricsSnapshot) bool { return s.CleanupAvailable && s.CleanupPending == 1 })
	if current.CleanupReadsSucceeded < 2 || current.ScansSucceeded != 0 || !current.CleanupSampleAt.After(failed.CleanupSampleAt) {
		t.Fatal(current)
	}
	recorder := httptest.NewRecorder()
	m.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(recorder.Body.String(), "PRIVATE") || !strings.Contains(recorder.Body.String(), "stego_controller_cleanup_pending_resources 1\n") {
		t.Fatal("invalid cleanup metrics")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().CleanupAvailable {
		t.Fatal("stopped sampler reports fresh data")
	}
}
func TestCleanupMetricsRejectContradictorySamples(t *testing.T) {
	now := time.Now()
	for _, sample := range []CleanupSample{{Pending: -1}, {Pending: 1}, {Pending: 0, OldestPending: &now}, {Pending: 1, OldestPending: new(time.Time)}} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		o := keyOptions()
		o.Metrics = new(Metrics)
		o.Cleanup = func(context.Context) (CleanupSample, error) { return sample, nil }
		source := KeyedSource[string]{Observe: func(ctx context.Context, sink *KeySink[string]) error {
			sink.SetReady(true)
			<-ctx.Done()
			return ctx.Err()
		}, Scan: func(context.Context, func(string) error) error { return nil }}
		err := RunKeyed(ctx, source, func(context.Context, string) error { return nil }, o)
		cancel()
		if !errors.Is(err, ErrMetricsContract) || o.Metrics.Snapshot().CleanupReadsFailed != 1 {
			t.Fatal(err, o.Metrics.Snapshot())
		}
	}
}
func TestDisabledMetricsDoNotReadCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := keyOptions()
	o.Cleanup = func(context.Context) (CleanupSample, error) {
		t.Error("disabled collector read cleanup")
		return CleanupSample{}, nil
	}
	o.Observe = func(e Event) {
		if e.Phase == "scan_completed" {
			cancel()
		}
	}
	source := KeyedSource[string]{Observe: func(ctx context.Context, sink *KeySink[string]) error {
		sink.SetReady(true)
		<-ctx.Done()
		return ctx.Err()
	}, Scan: func(context.Context, func(string) error) error { return nil }}
	if err := RunKeyed(ctx, source, func(context.Context, string) error { return nil }, o); err != nil {
		t.Fatal(err)
	}
}
