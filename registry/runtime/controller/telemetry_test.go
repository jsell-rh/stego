package controller

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracecollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"stego.dev/runtime/modes"
	"stego.dev/runtime/otel"
	"stego.dev/runtime/probe"
)

// The telemetry runtime of the runtime library is a provider set of this package.
var _ Providers = (*otel.Runtime)(nil)

// The probe runtime and its recorder send the service events.
var (
	_ ServiceEvents = (*probe.Runtime)(nil)
	_ ServiceEvents = (*probe.Recorder)(nil)
)

// logStore is a log processor that keeps every record in memory.
type logStore struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (s *logStore) OnEmit(_ context.Context, record *sdklog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record.Clone())
	return nil
}
func (s *logStore) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (s *logStore) Shutdown(context.Context) error                         { return nil }
func (s *logStore) ForceFlush(context.Context) error                       { return nil }

func (s *logStore) all() []sdklog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.records)
}

// testProviders is a provider set of the OpenTelemetry SDK with exporters in memory. Its shutdown keeps what was
// exported, so a test reads it after the controller closes the set.
type testProviders struct {
	spans     *tracetest.SpanRecorder
	traces    *sdktrace.TracerProvider
	reader    *sdkmetric.ManualReader
	meters    *sdkmetric.MeterProvider
	logs      *logStore
	loggers   *sdklog.LoggerProvider
	events    *probe.Recorder
	local     bool // no collector set, so the controller writes its logs to standard error
	starts    atomic.Int32
	shutdowns atomic.Int32
	open      atomic.Bool
	// closing, when set, is called at the start of each shutdown, which then takes closeDelay.
	closing    func()
	closeDelay time.Duration
	mu         sync.Mutex
	order      []string // start, close and closed, in the order they ran
}

func (p *testProviders) note(step string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.order = append(p.order, step)
}

func newProviders(sampler sdktrace.Sampler, ids sdktrace.IDGenerator) *testProviders {
	p := &testProviders{spans: tracetest.NewSpanRecorder(), reader: sdkmetric.NewManualReader(), logs: &logStore{}}
	res := resource.NewSchemaless(attribute.String("service.name", "controller-test"), attribute.String("service.instance.id", "instance-1"))
	options := []sdktrace.TracerProviderOption{sdktrace.WithSampler(sampler), sdktrace.WithSpanProcessor(p.spans), sdktrace.WithResource(res)}
	if ids != nil {
		options = append(options, sdktrace.WithIDGenerator(ids))
	}
	p.traces = sdktrace.NewTracerProvider(options...)
	p.meters = sdkmetric.NewMeterProvider(sdkmetric.WithReader(p.reader), sdkmetric.WithResource(res))
	p.loggers = sdklog.NewLoggerProvider(sdklog.WithProcessor(p.logs), sdklog.WithResource(res))
	builtins, err := probe.NewBuiltins(nil, nil)
	if err != nil {
		panic(err)
	}
	p.events = probe.NewRecorder(builtins)
	return p
}

func (p *testProviders) TracerProvider() trace.TracerProvider { return p.traces }
func (p *testProviders) CollectorSet() bool                   { return !p.local }

// StartOperation starts the operation span on the SDK tracer, as the telemetry runtime does: a root has no parent.
func (p *testProviders) StartOperation(ctx context.Context, parent otel.Parent, name string) (context.Context, otel.Finish, error) {
	options := []trace.SpanStartOption{trace.WithAttributes(attribute.String("stego.operation", name))}
	if parent == otel.Root {
		options = append(options, trace.WithNewRoot())
	}
	ctx, span := p.traces.Tracer("stego/operation").Start(ctx, name, options...)
	return ctx, func(outcome otel.Outcome, retry, failed bool) {
		span.SetAttributes(attribute.String("stego.outcome", string(outcome)), attribute.Bool("stego.retry", retry))
		if failed {
			span.SetStatus(otelcodes.Error, "")
		}
		span.End()
	}, nil
}
func (p *testProviders) MeterProvider() metric.MeterProvider    { return p.meters }
func (p *testProviders) LoggerProvider() otellog.LoggerProvider { return p.loggers }
func (p *testProviders) Shutdown(context.Context) error {
	p.note("close")
	if p.closing != nil {
		p.closing()
	}
	time.Sleep(p.closeDelay)
	p.shutdowns.Add(1)
	p.open.Store(false)
	p.note("closed")
	return nil
}

func (p *testProviders) start() (Providers, error) {
	p.note("start")
	p.starts.Add(1)
	p.open.Store(true)
	return p, nil
}

func newTestTelemetryWith(t *testing.T, p *testProviders) *Telemetry {
	t.Helper()
	telemetry, err := NewTelemetry(p.start, p.events)
	if err != nil {
		t.Fatal(err)
	}
	return telemetry
}

func newTestTelemetry(t *testing.T) (*testProviders, *Telemetry) {
	t.Helper()
	p := newProviders(sdktrace.AlwaysSample(), nil)
	return p, newTestTelemetryWith(t, p)
}

// stoppedTelemetry starts the provider set once and gives it back, so the returned context carries an owner with no
// user: the next use starts the provider set again.
func stoppedTelemetry(t *testing.T, telemetry *Telemetry) context.Context {
	t.Helper()
	ctx, release, err := telemetry.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	return ctx
}

func (p *testProviders) collect(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := p.reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func (p *testProviders) metrics(t *testing.T) map[string]metricdata.Metrics {
	t.Helper()
	found := map[string]metricdata.Metrics{}
	for _, scope := range p.collect(t).ScopeMetrics {
		for _, m := range scope.Metrics {
			found[m.Name] = m
		}
	}
	return found
}

func (p *testProviders) gauge(t *testing.T, name string) int64 {
	t.Helper()
	m, ok := p.metrics(t)[name]
	if !ok {
		return -1
	}
	points := m.Data.(metricdata.Gauge[int64]).DataPoints
	if len(points) != 1 {
		t.Fatalf("gauge %s has %d points", name, len(points))
	}
	return points[0].Value
}

// completion is one controller.work.completed log record.
type completion struct {
	operation, outcome string
	retry              bool
	severity           otellog.Severity
	trace              trace.TraceID
	span               trace.SpanID
}

func logValue(record sdklog.Record, key string) (attribute.Value, bool) {
	var value attribute.Value
	found := false
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		if string(kv.Key) == key {
			value, found = kv.Value, true
			return false
		}
		return true
	})
	return value, found
}

func (p *testProviders) completions(operation string) []completion {
	var out []completion
	for _, record := range p.logs.all() {
		if record.EventName() != "controller.work.completed" {
			continue
		}
		op, _ := logValue(record, "operation")
		if operation != "" && op.AsString() != operation {
			continue
		}
		outcome, _ := logValue(record, "outcome")
		retry, _ := logValue(record, "retry")
		out = append(out, completion{operation: op.AsString(), outcome: outcome.AsString(), retry: retry.AsBool(), severity: record.Severity(), trace: record.TraceID(), span: record.SpanID()})
	}
	return out
}

func (p *testProviders) eventCount(name string) int {
	count := 0
	for _, record := range p.logs.all() {
		if record.EventName() == name {
			count++
		}
	}
	return count
}

func awaitMetrics(t *testing.T, m *Metrics, check func(MetricsSnapshot) bool) MetricsSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
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

func idleObserve(ctx context.Context, sink *KeySink[string]) error {
	sink.SetReady(true)
	<-ctx.Done()
	return ctx.Err()
}

func exposition(t *testing.T, m *Metrics) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	m.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatal("metrics status", recorder.Code)
	}
	return recorder.Body.String()
}

// testPinger is the database ping of the modes readiness.
type testPinger struct{ down atomic.Bool }

func (p *testPinger) PingContext(context.Context) error {
	if p.down.Load() {
		return errors.New("database unreachable")
	}
	return nil
}

// privateError panics when it is formatted, so any telemetry that formats it fails the test.
type privateError struct{}

func (*privateError) Error() string { panic("PRIVATE-process-error") }

func TestTelemetryOperations(t *testing.T) {
	t.Run("ST-SUP-RP02-LC07-T02 Timeout and terminal outcomes are classified", func(t *testing.T) {
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
				source := KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error { return emit("one") }}
				err := RunKeyed(ctx, source, func(ctx context.Context, _ string) error { return tc.action(ctx) }, o)
				s := m.Snapshot()
				if tc.terminal {
					if !errors.Is(err, denied) || s.Failed != 1 || s.TimedOut != 0 || s.Retries != 0 {
						t.Fatal(err, s)
					}
				} else if err != nil || s.TimedOut != 1 || s.Failed != 0 || s.Retries != 1 {
					t.Fatal(err, s)
				}
				if s.DurationBuckets[len(s.DurationBuckets)-1] != 1 {
					t.Fatal("duration histogram lost action", s)
				}
			})
		}
	})

	t.Run("ST-SUP-RP02-LC07-T03 The snapshot and the exposition carry counts and no private value", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		metrics := new(Metrics)
		holding, release := make(chan struct{}), make(chan struct{})
		var failed atomic.Bool
		source := KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error {
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
		body := exposition(t, metrics)
		if strings.Contains(body, "PRIVATE") || !strings.Contains(body, "stego_controller_admission_waiters 1\n") || !strings.Contains(body, "stego_controller_actions_total{outcome=\"failure\"} 1\n") || !strings.Contains(body, "stego_controller_capacity 2\n") {
			t.Fatal("invalid metrics", body)
		}
		if strings.Contains(fmt.Sprintf("%+v", s), "PRIVATE") {
			t.Fatal("the snapshot holds a private value")
		}
		close(release)
		awaitMetrics(t, metrics, func(s MetricsSnapshot) bool { return s.Succeeded == 3 && s.Queue.Active == 0 && s.Queue.Queued == 0 })
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestTelemetryOwnership(t *testing.T) {
	t.Run("ST-SUP-RP02-LC08-T01 The first controller starts the provider and the last closes it", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		first, closeFirst, err := telemetry.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		second, closeSecond, err := telemetry.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if providers.starts.Load() != 1 || signalsOf(first) != signalsOf(second) {
			t.Fatal("the controllers do not share one provider set", providers.starts.Load())
		}
		for _, ctx := range []context.Context{first, second} {
			detach, err := attachControllerQueue(ctx, func() QueueMetrics { return QueueMetrics{Capacity: 1} })
			if err != nil {
				t.Fatal(err)
			}
			defer detach()
		}
		if providers.gauge(t, "stego.controller.queue.running") != 2 {
			t.Fatal("the queues did not attach to the shared set")
		}
		closeFirst()
		closeFirst()
		if !providers.open.Load() || providers.shutdowns.Load() != 0 {
			t.Fatal("the first close closed the shared provider set")
		}
		closeSecond()
		if providers.open.Load() || providers.shutdowns.Load() != 1 {
			t.Fatal("the last close did not close the provider set")
		}
		if telemetry.users != 0 || telemetry.current != nil {
			t.Fatal("the owner retained its ownership", telemetry.users)
		}
	})

	t.Run("ST-SUP-RP02-LC08-T02 Monitor refuses unsafe addresses and a busy listener before the controller starts", func(t *testing.T) {
		for _, address := range []string{":9090", "0.0.0.0:9090", "[::]:9090", "localhost:9090", "192.0.2.1:9090", "127.0.0.1", "127.0.0.1:65536", "127.0.0.1:09090", "[::1%lo]:9090"} {
			called := false
			if _, err := Monitor(context.Background(), address, nil, func(context.Context, *Metrics) (int, error) { called = true; return 0, nil }); err == nil || called {
				t.Fatal("unsafe address accepted", address, err)
			}
		}
		_, err := Monitor(context.Background(), "", nil, func(_ context.Context, m *Metrics) (int, error) {
			if m != nil {
				t.Error("disabled metrics allocated a collector")
			}
			return 0, denied
		})
		if !errors.Is(err, denied) {
			t.Fatal(err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		called := false
		if _, err := Monitor(context.Background(), listener.Addr().String(), nil, func(context.Context, *Metrics) (int, error) { called = true; return 0, nil }); err == nil || called {
			t.Fatal("busy listener accepted", err)
		}
	})

	t.Run("ST-SUP-RP02-LC08-T03 A callback failure emits one service.failed event and keeps its error", func(t *testing.T) {
		failure := new(privateError)
		for _, mode := range []string{"success", "error", "canceled", "panic", "goexit"} {
			t.Run(mode, func(t *testing.T) {
				providers, telemetry := newTestTelemetry(t)
				_, err := Monitor(context.Background(), "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
					switch mode {
					case "error":
						return 0, failure
					case "canceled":
						return 0, context.Canceled
					case "panic":
						panic("PRIVATE-process-panic")
					case "goexit":
						runtime.Goexit()
					}
					return 0, nil
				})
				switch mode {
				case "success":
					if err != nil {
						t.Fatal(err)
					}
				case "error":
					if err != error(failure) {
						t.Fatal("the callback error identity was lost")
					}
				case "canceled":
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				default:
					if !errors.Is(err, ErrRunAborted) {
						t.Fatal(err)
					}
				}
				want := 0
				if mode == "error" || mode == "panic" || mode == "goexit" {
					want = 1
				}
				if got := len(providers.events.Emissions("service.failed")); got != want {
					t.Fatalf("%d service.failed events, want %d", got, want)
				}
				for _, record := range providers.logs.all() {
					if strings.Contains(fmt.Sprint(record.Body(), record.EventName()), "PRIVATE") {
						t.Fatal("private text entered the output")
					}
				}
				if strings.Contains(fmt.Sprint(providers.events.All()), "PRIVATE") {
					t.Fatal("private text entered the events")
				}
			})
		}
	})
}

func TestTelemetryCollector(t *testing.T) {
	t.Run("ST-SUP-RP02-LC12-T01 A collector that another running controller holds is refused", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := new(Metrics)
		o := keyOptions()
		o.Metrics = m
		done := make(chan error, 1)
		go func() {
			done <- RunKeyed(ctx, KeyedSource[string]{Observe: idleObserve, Scan: func(context.Context, func(string) error) error { return nil }}, func(context.Context, string) error { return nil }, o)
		}()
		awaitMetrics(t, m, func(s MetricsSnapshot) bool { return s.Running && s.ScansSucceeded == 1 })
		opened := false
		source := KeyedSource[string]{Observe: func(context.Context, *KeySink[string]) error { opened = true; return nil }, Scan: func(context.Context, func(string) error) error { opened = true; return nil }}
		if err := RunKeyed(context.Background(), source, func(context.Context, string) error { return nil }, o); !errors.Is(err, ErrMetricsInUse) || opened {
			t.Fatal("the second controller was not refused before its source opened", err, opened)
		}
		if s := m.Snapshot(); !s.Running || s.Queue.Capacity != o.Capacity {
			t.Fatal("the first controller was affected", s)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ST-SUP-RP02-LC12-T02 The collector exposes exactly the fixed instrument set", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		m := new(Metrics)
		o := keyOptions()
		o.Metrics = m
		o.Observe = func(e Event) {
			if e.Phase == "scan_completed" {
				cancel()
			}
		}
		if err := RunKeyed(ctx, KeyedSource[string]{Observe: idleObserve, Scan: func(context.Context, func(string) error) error { return nil }}, func(context.Context, string) error { return nil }, o); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, line := range strings.Split(exposition(t, m), "\n") {
			if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
				names = append(names, strings.Fields(name)[0])
				continue
			}
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			series := strings.Fields(line)[0]
			if open := strings.IndexByte(series, '{'); open >= 0 {
				labels := series[open+1 : len(series)-1]
				key, _, _ := strings.Cut(labels, "=")
				if key != "outcome" && !(key == "le" && strings.HasPrefix(series, "stego_controller_action_duration_seconds_bucket{")) {
					t.Fatal("a label other than outcome", series)
				}
			}
		}
		want := []string{"stego_controller_running", "stego_controller_ready", "stego_controller_capacity", "stego_controller_queued_keys", "stego_controller_active_keys", "stego_controller_retry_keys", "stego_controller_admission_waiters", "stego_controller_actions_total", "stego_controller_retries_total", "stego_controller_scans_total", "stego_controller_reconnects_total", "stego_controller_action_duration_seconds"}
		if !slices.Equal(names, want) {
			t.Fatalf("the instrument set is %v", names)
		}
	})

	t.Run("ST-SUP-RP02-LC12-T03 The collector size does not grow with the resource count", func(t *testing.T) {
		series := func(count int) int {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := new(Metrics)
			o := keyOptions()
			o.Metrics = m
			o.Capacity = 64
			o.Workers = 4
			var done atomic.Int32
			source := KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error {
				for i := range count {
					if err := emit(fmt.Sprint("resource-", i)); err != nil {
						return err
					}
				}
				return nil
			}}
			errs := make(chan error, 1)
			go func() {
				errs <- RunKeyed(ctx, source, func(context.Context, string) error {
					if done.Add(1) == int32(count) {
						cancel()
					}
					return nil
				}, o)
			}()
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, line := range strings.Split(exposition(t, m), "\n") {
				if line != "" && !strings.HasPrefix(line, "#") {
					n++
				}
			}
			return n
		}
		if small, large := series(10), series(10000); small != large {
			t.Fatal("the collector grew with the resource count", small, large)
		}
	})

	t.Run("ST-SUP-RP02-LC12-T04 Counters survive a reconnect and gauges reset when the queue stops", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		m := new(Metrics)
		var watches atomic.Int32
		source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
			session := watches.Add(1)
			keys := []string{"one", "two"}
			if session > 1 {
				keys = []string{"three"}
			}
			return func() (string, error) {
				if len(keys) > 0 {
					key := keys[0]
					keys = keys[1:]
					return key, nil
				}
				if session == 1 {
					awaitMetrics(t, m, func(s MetricsSnapshot) bool { return s.Succeeded == 2 })
					return "", io.EOF
				}
				<-ctx.Done()
				return "", ctx.Err()
			}, nil
		}, Scan: func(context.Context, func(string) error) error { return nil }}
		o := watchKeyOptions()
		o.Metrics = m
		done := make(chan error, 1)
		go func() { done <- RunKeyedWatch(ctx, source, func(context.Context, string) error { return nil }, o) }()
		s := awaitMetrics(t, m, func(s MetricsSnapshot) bool { return s.Reconnects == 1 && s.Succeeded == 3 })
		if s.Succeeded != 3 {
			t.Fatal("the counters did not survive the reconnect", s)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if s := m.Snapshot(); s.Running || s.Queue != (QueueMetrics{}) || s.Succeeded != 3 {
			t.Fatal("the gauges did not reset when the queue stopped", s)
		}
	})

	t.Run("ST-SUP-RP02-LC12-T05 A cancelled action schedules no retry in a stopped queue", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		m := new(Metrics)
		o := keyOptions()
		o.Metrics = m
		q := newKeyQueue[string](o.Capacity)
		source := KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error { return emit("private") }}
		if err := runKeyed(ctx, source, func(ctx context.Context, _ string) error { cancel(); <-ctx.Done(); return ctx.Err() }, o, q); err != nil {
			t.Fatal(err)
		}
		if s := m.Snapshot(); s.Canceled != 1 || s.Retries != 0 || s.Running {
			t.Fatal(s)
		}
		if metrics := q.metrics(); metrics.Retrying != 0 || metrics.Queued != 0 {
			t.Fatal("the stopped queue holds a retry", metrics)
		}
	})

	t.Run("ST-SUP-RP02-LC12-T06 An interrupted scan does not count as completed", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		m := new(Metrics)
		o := keyOptions()
		o.Metrics = m
		source := KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error {
			return Scan(ctx, func(ctx context.Context, after string, limit int) (CursorPage[string], error) {
				if after != "" {
					cancel()
					<-ctx.Done()
					return CursorPage[string]{}, ctx.Err()
				}
				return CursorPage[string]{Items: []CursorItem[string]{{Cursor: "1", Value: "one"}}, More: true}, nil
			}, emit, ScanOptions{PageSize: 1, MaxPages: 10, PageTimeout: time.Second})
		}}
		if err := RunKeyed(ctx, source, func(context.Context, string) error { return nil }, o); err != nil {
			t.Fatal(err)
		}
		if s := m.Snapshot(); s.ScansSucceeded != 0 || s.ScansFailed != 0 {
			t.Fatal("the interrupted scan was counted", s)
		}
	})
}

// freeAddress gives a loopback address with a port that was free a moment ago.
func freeAddress(t *testing.T, network, host string) string {
	t.Helper()
	listener, err := net.Listen(network, net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skip("no listener on", host, err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

// serveMonitor runs Monitor on address until the returned stop function runs.
func serveMonitor(t *testing.T, address string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Monitor(ctx, address, nil, func(ctx context.Context, m *Metrics) (int, error) {
			close(started)
			<-ctx.Done()
			return 0, nil
		})
		done <- err
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatal("monitor did not start", err)
	}
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func get(t *testing.T, method, url string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := http.Client{Timeout: time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response
}

func TestTelemetryMonitor(t *testing.T) {
	t.Run("ST-SUP-RP02-LC13-T01 The monitor serves only the metrics, with no-store and nosniff", func(t *testing.T) {
		address := freeAddress(t, "tcp4", "127.0.0.1")
		defer serveMonitor(t, address)()
		response := get(t, http.MethodGet, "http://"+address+"/metrics")
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("the metrics response", response.StatusCode, response.Header)
		}
		if response := get(t, http.MethodGet, "http://"+address+"/other"); response.StatusCode != http.StatusNotFound {
			t.Fatal("another path", response.StatusCode)
		}
		if response := get(t, http.MethodPost, "http://"+address+"/metrics"); response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatal("another method", response.StatusCode)
		}
	})

	t.Run("ST-SUP-RP02-LC13-T02 The monitor registers nothing on the default mux", func(t *testing.T) {
		defer serveMonitor(t, freeAddress(t, "tcp4", "127.0.0.1"))()
		recorder := httptest.NewRecorder()
		http.DefaultServeMux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatal("the default mux serves the metrics", recorder.Code)
		}
	})

	t.Run("ST-SUP-RP02-LC13-T03 The monitor binds loopback only", func(t *testing.T) {
		refused := []string{"0.0.0.0:0", "[::]:0"}
		var outside string
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ip, ok := a.(*net.IPNet); ok && !ip.IP.IsLoopback() && ip.IP.To4() != nil {
					outside = ip.IP.String()
					refused = append(refused, net.JoinHostPort(outside, "0"))
					break
				}
			}
		}
		for _, address := range refused {
			called := false
			if _, err := Monitor(context.Background(), address, nil, func(context.Context, *Metrics) (int, error) { called = true; return 0, nil }); err == nil || called {
				t.Fatal("a non-loopback address was accepted", address)
			}
		}
		for _, c := range []struct{ network, host string }{{"tcp4", "127.0.0.1"}, {"tcp6", "::1"}} {
			address := freeAddress(t, c.network, c.host)
			stop := serveMonitor(t, address)
			if response := get(t, http.MethodGet, "http://"+address+"/metrics"); response.StatusCode != http.StatusOK {
				t.Fatal("the loopback listener does not serve", address)
			}
			_, port, _ := net.SplitHostPort(address)
			if outside != "" && c.network == "tcp4" {
				if connection, err := net.DialTimeout("tcp", net.JoinHostPort(outside, port), 200*time.Millisecond); err == nil {
					connection.Close()
					t.Fatal("the listener is bound beyond loopback")
				}
			}
			stop()
		}
	})

	t.Run("ST-SUP-RP02-LC13-T04 The monitor server has its fixed timeouts and MaxHeaderBytes 4096", func(t *testing.T) {
		server := monitorServer(new(Metrics))
		if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 || server.MaxHeaderBytes != 4096 {
			t.Fatal("the server limits", server)
		}
		address := freeAddress(t, "tcp4", "127.0.0.1")
		defer serveMonitor(t, address)()
		request, _ := http.NewRequest(http.MethodGet, "http://"+address+"/metrics", nil)
		request.Header.Set("X-Large", strings.Repeat("a", 9<<10))
		response, err := (&http.Client{Timeout: time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatal("a 9 KiB header was not refused", response.StatusCode)
		}
		connection, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		_ = connection.SetReadDeadline(time.Now().Add(server.ReadHeaderTimeout + 2*time.Second))
		started := time.Now()
		if _, err := connection.Read(make([]byte, 1)); err == nil || errors.Is(err, context.DeadlineExceeded) || time.Since(started) > server.ReadHeaderTimeout+time.Second {
			t.Fatal("the silent connection was not closed after the header timeout", err, time.Since(started))
		}
	})

	t.Run("ST-SUP-RP02-LC13-T05 An empty address gives a nil collector and no server", func(t *testing.T) {
		var got *Metrics = new(Metrics)
		value, err := Monitor(context.Background(), "", nil, func(_ context.Context, m *Metrics) (int, error) { got = m; return 7, nil })
		if err != nil || value != 7 || got != nil {
			t.Fatal("the empty address", got, value, err)
		}
	})

	t.Run("ST-SUP-RP02-LC13-T06 A listener failure stops the controller from starting", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		called := false
		if _, err := Monitor(context.Background(), listener.Addr().String(), nil, func(context.Context, *Metrics) (int, error) { called = true; return 0, nil }); err == nil || called {
			t.Fatal("the busy address started the controller", err, called)
		}
	})

	t.Run("ST-SUP-RP02-LC13-T07 When one part exits the monitor closes the listener, cancels the context and joins both", func(t *testing.T) {
		for _, mode := range []string{"callback", "server"} {
			t.Run(mode, func(t *testing.T) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				address := listener.Addr().String()
				failing := &failingListener{Listener: listener, fail: make(chan struct{})}
				sentinel := errors.New("callback failure")
				var cancelled, returned atomic.Bool
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_, err = monitorListener(ctx, failing, nil, func(ctx context.Context, _ *Metrics) (int, error) {
					defer returned.Store(true)
					if mode == "callback" {
						return 0, sentinel
					}
					close(failing.fail)
					<-ctx.Done()
					cancelled.Store(true)
					time.Sleep(20 * time.Millisecond)
					return 0, ctx.Err()
				})
				if !returned.Load() {
					t.Fatal("the monitor returned before the callback")
				}
				if mode == "callback" && err != sentinel {
					t.Fatal("the callback error was not returned", err)
				}
				if mode == "server" && (!errors.Is(err, errListenerFailed) || !cancelled.Load()) {
					t.Fatal("the server failure was not first or did not cancel the callback", err)
				}
				if connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
					connection.Close()
					t.Fatal("the monitor kept its listener")
				}
			})
		}
	})
}

var errListenerFailed = errors.New("listener failed")

// failingListener fails its Accept once fail is closed.
type failingListener struct {
	net.Listener
	fail chan struct{}
}

func (l *failingListener) Accept() (net.Conn, error) {
	accepted := make(chan struct{})
	defer close(accepted)
	go func() {
		select {
		case <-l.fail:
			l.Listener.Close()
		case <-accepted:
		}
	}()
	connection, err := l.Listener.Accept()
	select {
	case <-l.fail:
		if err != nil {
			return nil, errListenerFailed
		}
	default:
	}
	return connection, err
}

func TestTelemetryStartup(t *testing.T) {
	t.Run("ST-SUP-RP02-LC14-T01 The telemetry runtime starts before the callback and lives for the callback's whole life", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		helper := func(ctx context.Context) error {
			_, err := ScanFrom(ctx, "", numbered(1), func(int) error { return nil }, scanOptions())
			return err
		}
		_, err := Monitor(context.Background(), "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			if err := helper(ctx); err != nil {
				return 0, err
			}
			defer func() {
				if err := helper(ctx); err != nil {
					t.Error(err)
				}
			}()
			o := keyOptions()
			return 0, RunKeyed(ctx, KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error { return emit("key") }}, func(context.Context, string) error { return denied }, o)
		})
		if !errors.Is(err, denied) {
			t.Fatal(err)
		}
		spans := providers.spans.Ended()
		names := map[string]int{}
		instances := map[string]bool{}
		for _, span := range spans {
			names[span.Name()]++
			for _, kv := range span.Resource().Attributes() {
				if kv.Key == "service.instance.id" {
					instances[kv.Value.AsString()] = true
				}
			}
		}
		if names["controller.scan"] != 3 || names["controller.reconcile"] != 1 || len(instances) != 1 || providers.starts.Load() != 1 || providers.shutdowns.Load() != 1 {
			t.Fatal("setup, the nested controller and the cleanup did not share one runtime", names, instances, providers.starts.Load())
		}
	})

	t.Run("ST-SUP-RP02-LC14-T02 The monitor waits for an ongoing bounded provider close", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		providers.closeDelay = 500 * time.Millisecond
		closing := make(chan struct{})
		var once sync.Once
		providers.closing = func() { once.Do(func() { close(closing) }) }
		first := make(chan error, 1)
		go func() {
			_, err := Monitor(context.Background(), "", telemetry, func(context.Context, *Metrics) (int, error) { return 0, nil })
			first <- err
		}()
		<-closing
		if _, err := Monitor(context.Background(), "", telemetry, func(context.Context, *Metrics) (int, error) { return 0, nil }); err != nil {
			t.Fatal(err)
		}
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		providers.mu.Lock()
		order := slices.Clone(providers.order)
		providers.mu.Unlock()
		if !slices.Equal(order, []string{"start", "close", "closed", "start", "close", "closed"}) {
			t.Fatal("the second runtime did not wait for the first close", order)
		}
	})

	t.Run("ST-SUP-RP02-LC14-T03 Each queue registers once and unregisters when its run ends", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		_, err := Monitor(context.Background(), "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			short, stopShort := context.WithCancel(ctx)
			long, stopLong := context.WithCancel(ctx)
			defer stopShort()
			defer stopLong()
			started := make(chan struct{}, 2)
			source := KeyedSource[string]{Observe: func(ctx context.Context, sink *KeySink[string]) error {
				started <- struct{}{}
				return idleObserve(ctx, sink)
			}, Scan: func(context.Context, func(string) error) error { return nil }}
			done := make(chan error, 2)
			go func() {
				done <- RunKeyed(short, source, func(context.Context, string) error { return nil }, keyOptions())
			}()
			go func() {
				done <- RunKeyed(long, source, func(context.Context, string) error { return nil }, keyOptions())
			}()
			<-started
			<-started
			if running := providers.gauge(t, "stego.controller.queue.running"); running != 2 {
				return 0, fmt.Errorf("%d queues registered while two run", running)
			}
			stopShort()
			if err := <-done; err != nil {
				return 0, err
			}
			if running := providers.gauge(t, "stego.controller.queue.running"); running != 1 {
				return 0, fmt.Errorf("%d queues registered after one ended", running)
			}
			stopLong()
			return 0, <-done
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ST-SUP-RP02-LC14-T04 A sixty-fifth queue is refused", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		ctx, release, err := telemetry.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		for i := range QueueLimit {
			detach, err := attachControllerQueue(ctx, func() QueueMetrics { return QueueMetrics{} })
			if err != nil {
				t.Fatalf("queue %d was refused: %v", i+1, err)
			}
			defer detach()
		}
		if _, err := attachControllerQueue(ctx, func() QueueMetrics { return QueueMetrics{} }); !errors.Is(err, ErrTelemetryInUse) {
			t.Fatal("the sixty-fifth queue was accepted", err)
		}
		if running := providers.gauge(t, "stego.controller.queue.running"); running != QueueLimit {
			t.Fatal("the first sixty-four queues are not registered", running)
		}
	})

	t.Run("ST-SUP-RP02-LC14-T05 Cancellation emits no failure event", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		ctx, cancel := context.WithCancel(context.Background())
		_, err := Monitor(ctx, "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			cancel()
			<-ctx.Done()
			return 0, ctx.Err()
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(providers.events.Emissions("service.failed")) != 0 {
			t.Fatal("a cancellation emitted a failure event")
		}
	})

	t.Run("ST-SUP-RP02-LC14-T06 The callback's value and error identity return unchanged", func(t *testing.T) {
		_, telemetry := newTestTelemetry(t)
		type answer struct{ n int }
		sentinel := &answer{n: 42}
		for _, address := range []string{"", freeAddress(t, "tcp4", "127.0.0.1")} {
			value, err := Monitor(context.Background(), address, telemetry, func(context.Context, *Metrics) (*answer, error) { return sentinel, nil })
			if err != nil || value != sentinel {
				t.Fatal("the callback value changed", value, err)
			}
			base := errors.New("sentinel")
			wrapped := fmt.Errorf("callback: %w", base)
			_, err = Monitor(context.Background(), address, telemetry, func(context.Context, *Metrics) (*answer, error) { return nil, wrapped })
			if !errors.Is(err, base) || err != wrapped {
				t.Fatal("the callback error identity changed", err)
			}
		}
	})

	t.Run("ST-SUP-RP02-LC14-T07 A probe command starts no telemetry", func(t *testing.T) {
		collector, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer collector.Close()
		var connections atomic.Int32
		go func() {
			for {
				connection, err := collector.Accept()
				if err != nil {
					return
				}
				connections.Add(1)
				connection.Close()
			}
		}()
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+collector.Addr().String())
		pinger := &testPinger{}
		health := httptest.NewServer(modes.NewReadiness(pinger).Handler(modes.Selection{modes.Controller}))
		defer health.Close()
		var starts atomic.Int32
		telemetry, err := NewTelemetry(func() (Providers, error) {
			starts.Add(1)
			runtime, err := otel.New("controller-test")
			if err != nil {
				return nil, err
			}
			return runtime, nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		called := false
		for _, arg := range []string{"--stego-probe=live", "--stego-probe=ready"} {
			code := Process(context.Background(), []string{arg}, ProcessOptions{MonitorAddress: "127.0.0.1:0", HealthAddress: strings.TrimPrefix(health.URL, "http://"), Telemetry: telemetry}, func(context.Context, *Metrics) error { called = true; return nil })
			if code != 0 {
				t.Fatal("the probe failed", arg, code)
			}
		}
		pinger.down.Store(true)
		if code := Process(context.Background(), []string{"--stego-probe=ready"}, ProcessOptions{HealthAddress: strings.TrimPrefix(health.URL, "http://"), Telemetry: telemetry}, func(context.Context, *Metrics) error { called = true; return nil }); code != 1 {
			t.Fatal("a not-ready probe passed", code)
		}
		time.Sleep(50 * time.Millisecond)
		if starts.Load() != 0 || connections.Load() != 0 || called {
			t.Fatal("a probe command started telemetry or the application", starts.Load(), connections.Load(), called)
		}
	})
}

func TestTelemetryLogs(t *testing.T) {
	t.Run("ST-SUP-RP02-LC15-T01 Each reconcile, scan and watch emits one completion log and one duration sample", func(t *testing.T) {
		for _, ratio := range []float64{1, 0} {
			t.Run(fmt.Sprint(ratio), func(t *testing.T) {
				providers := newProviders(sdktrace.TraceIDRatioBased(ratio), nil)
				telemetry := newTestTelemetryWith(t, providers)
				ctx, cancel := context.WithCancel(context.Background())
				_, err := Monitor(ctx, "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
					source := Source[string]{Watch: idle[string], Scan: func(ctx context.Context, emit func(string) error) error { return emit("key") }}
					o := watchKeyOptions()
					o.Workers = 1
					return 0, RunKeyedWatch(ctx, source, func(context.Context, string) error { cancel(); return nil }, o)
				})
				if err != nil {
					t.Fatal(err)
				}
				data := providers.metrics(t)["stego.controller.work.duration"].Data.(metricdata.Histogram[float64])
				for _, operation := range []string{"reconcile", "scan", "watch"} {
					logs := providers.completions(operation)
					if len(logs) != 1 {
						t.Fatalf("%d completion logs of %s", len(logs), operation)
					}
					if logs[0].outcome == "" || !logs[0].trace.IsValid() || !logs[0].span.IsValid() {
						t.Fatal("the completion log lacks its fields", logs[0])
					}
					samples := uint64(0)
					for _, point := range data.DataPoints {
						if value, ok := point.Attributes.Value("operation"); ok && value.AsString() == operation {
							samples += point.Count
							if _, ok := point.Attributes.Value("retry"); !ok {
								t.Fatal("the duration sample has no retry flag")
							}
						}
					}
					if samples != 1 {
						t.Fatalf("%d duration samples of %s", samples, operation)
					}
				}
			})
		}
	})

	t.Run("ST-SUP-RP02-LC15-T02 A watch span covers one session and ends before the reconnect", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var watches atomic.Int32
		source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
			if watches.Add(1) == 1 {
				return func() (string, error) { return "", io.EOF }, nil
			}
			cancel()
			return idle[string](ctx)
		}, Scan: func(context.Context, func(string) error) error { return nil }}
		_, err := Monitor(ctx, "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			return 0, RunKeyedWatch(ctx, source, func(context.Context, string) error { return nil }, watchKeyOptions())
		})
		if err != nil {
			t.Fatal(err)
		}
		var sessions []sdktrace.ReadOnlySpan
		for _, span := range providers.spans.Ended() {
			if span.Name() == "controller.watch" {
				sessions = append(sessions, span)
			}
		}
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].StartTime().Before(sessions[j].StartTime()) })
		if len(sessions) != 2 || sessions[0].EndTime().After(sessions[1].StartTime()) {
			t.Fatal("the watch spans do not each cover one session", len(sessions))
		}
	})

	t.Run("ST-SUP-RP02-LC15-T03 The completion log severity follows the outcome", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		o := keyOptions()
		o.Workers = 1
		_, err := Monitor(context.Background(), "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			source := KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error {
				for _, key := range []string{"success", "cancelled", "retry", "terminal"} {
					if err := emit(key); err != nil {
						return err
					}
				}
				return nil
			}}
			return 0, RunKeyed(ctx, source, func(_ context.Context, key string) error {
				switch key {
				case "cancelled":
					return context.Canceled
				case "retry":
					return errors.New("transient")
				case "terminal":
					return denied
				}
				return nil
			}, o)
		})
		if !errors.Is(err, denied) {
			t.Fatal(err)
		}
		var got []otellog.Severity
		for _, log := range providers.completions("reconcile") {
			got = append(got, log.severity)
		}
		want := []otellog.Severity{otellog.SeverityInfo, otellog.SeverityInfo, otellog.SeverityWarn, otellog.SeverityError}
		if !slices.Equal(got, want) {
			t.Fatal("the severities", got)
		}
	})

	t.Run("ST-SUP-RP02-LC15-T04 Watch start, reconnect and cleanup-summary failure are fixed events", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		m := new(Metrics)
		var watches, reads atomic.Int32
		failedRead := make(chan struct{})
		source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
			if watches.Add(1) == 1 {
				return func() (string, error) {
					select {
					case <-failedRead:
						return "", io.EOF
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}, nil
			}
			return idle[string](ctx)
		}, Scan: func(context.Context, func(string) error) error { return nil }}
		o := watchKeyOptions()
		o.Metrics = m
		o.ResyncInterval = 5 * time.Millisecond
		o.Observe = func(e Event) {
			if e.Phase == "metrics_failed" {
				close(failedRead)
			}
		}
		o.Cleanup = func(context.Context) (CleanupSample, error) {
			if reads.Add(1) == 1 {
				return CleanupSample{}, errors.New("PRIVATE cleanup failure")
			}
			if watches.Load() > 1 {
				cancel()
			}
			return CleanupSample{}, nil
		}
		_, err := Monitor(ctx, "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			return 0, RunKeyedWatch(ctx, source, func(context.Context, string) error { return nil }, o)
		})
		if err != nil {
			t.Fatal(err)
		}
		fixed := map[string]bool{"controller.work.completed": true, "controller.watch.started": true, "controller.watch.reconnect": true, "controller.cleanup.sample_failed": true, "controller.scan.completed": true, "controller.scan.failed": true, "controller.reconcile.failed": true, "controller.reconcile.pending": true}
		for _, record := range providers.logs.all() {
			if !fixed[record.EventName()] || strings.Contains(record.Body().AsString(), "PRIVATE") {
				t.Fatal("a free-text event", record.EventName(), record.Body().AsString())
			}
		}
		if providers.eventCount("controller.watch.started") < 1 || providers.eventCount("controller.watch.reconnect") != 1 || providers.eventCount("controller.cleanup.sample_failed") != 1 {
			t.Fatal("the fixed events", providers.eventCount("controller.watch.started"), providers.eventCount("controller.watch.reconnect"), providers.eventCount("controller.cleanup.sample_failed"))
		}
	})

	t.Run("ST-SUP-RP02-LC15-T05 The controller instruments are the fixed stego.controller.* set with no configurable label", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		var found map[string]metricdata.Metrics
		_, err := Monitor(context.Background(), "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			attempts := 0
			source := KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error { return emit("key") }}
			return 0, RunKeyed(ctx, source, func(context.Context, string) error {
				attempts++
				if attempts == 1 {
					return errors.New("transient")
				}
				found = providers.metrics(t)
				return denied
			}, keyOptions())
		})
		if !errors.Is(err, denied) {
			t.Fatal(err)
		}
		want := []string{"stego.controller.events", "stego.controller.queue.active", "stego.controller.queue.capacity", "stego.controller.queue.queued", "stego.controller.queue.ready", "stego.controller.queue.retrying", "stego.controller.queue.running", "stego.controller.queue.waiting", "stego.controller.retries", "stego.controller.work.duration"}
		var names []string
		keys := map[string]bool{}
		for name, m := range found {
			names = append(names, name)
			var sets []attribute.Set
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range data.DataPoints {
					sets = append(sets, p.Attributes)
				}
			case metricdata.Gauge[int64]:
				for _, p := range data.DataPoints {
					sets = append(sets, p.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, p := range data.DataPoints {
					sets = append(sets, p.Attributes)
				}
			}
			for _, set := range sets {
				for _, kv := range set.ToSlice() {
					keys[string(kv.Key)] = true
				}
			}
		}
		sort.Strings(names)
		if !slices.Equal(names, want) {
			t.Fatal("the instrument set", names)
		}
		for key := range keys {
			if key != "operation" && key != "outcome" && key != "retry" && key != "event" {
				t.Fatal("a configurable label", key)
			}
		}
	})
}

func TestTelemetryCleanup(t *testing.T) {
	cleanupSource := func(scan func(context.Context) error) KeyedSource[string] {
		return KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, _ func(string) error) error { return scan(ctx) }}
	}
	blocked := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

	t.Run("ST-SUP-RP02-LC18-T01 Cleanup sampling runs on its own worker under the action timeout and at the resync interval", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			o := keyOptions()
			o.Metrics = new(Metrics)
			o.ResyncInterval = time.Second
			o.Timeout = 100 * time.Millisecond
			start := time.Now()
			var budgets []time.Duration
			var times []time.Duration
			o.Cleanup = func(ctx context.Context) (CleanupSample, error) {
				end, _ := ctx.Deadline()
				budgets = append(budgets, time.Until(end))
				times = append(times, time.Since(start))
				if len(times) == 2 {
					cancel()
				}
				return CleanupSample{}, nil
			}
			scanning := false
			err := RunKeyed(ctx, cleanupSource(func(ctx context.Context) error { scanning = true; return blocked(ctx) }), func(context.Context, string) error { return nil }, o)
			if err != nil || !scanning {
				t.Fatal(err, scanning)
			}
			if len(budgets) != 2 || budgets[0] != o.Timeout || budgets[1] != o.Timeout || times[1] != o.ResyncInterval {
				t.Fatal("the cleanup reads", budgets, times)
			}
		})
	})

	t.Run("ST-SUP-RP02-LC18-T02 The cleanup worker is joined on shutdown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		o := keyOptions()
		o.Metrics = new(Metrics)
		var returned atomic.Bool
		o.Cleanup = func(context.Context) (CleanupSample, error) {
			cancel()
			time.Sleep(50 * time.Millisecond)
			returned.Store(true)
			return CleanupSample{}, nil
		}
		if err := RunKeyed(ctx, cleanupSource(blocked), func(context.Context, string) error { return nil }, o); err != nil {
			t.Fatal(err)
		}
		if !returned.Load() {
			t.Fatal("the run returned before the cleanup read")
		}
	})

	t.Run("ST-SUP-RP02-LC18-T03 With metrics disabled the cleanup summary is never read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			o := keyOptions()
			o.ResyncInterval = time.Second
			calls := 0
			o.Cleanup = func(context.Context) (CleanupSample, error) { calls++; return CleanupSample{}, nil }
			time.AfterFunc(3*o.ResyncInterval+time.Millisecond, cancel)
			if err := RunKeyed(ctx, cleanupSource(func(context.Context) error { return nil }), func(context.Context, string) error { return nil }, o); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatal("the disabled collector read the cleanup summary", calls)
			}
		})
	})

	t.Run("ST-SUP-RP02-LC18-T04 A failed read keeps the last values but marks them unavailable, and so does shutdown", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		m := new(Metrics)
		o := keyOptions()
		o.Metrics = m
		o.ResyncInterval = 5 * time.Millisecond
		var calls atomic.Int32
		oldest := time.Now().Add(-time.Hour)
		o.Cleanup = func(ctx context.Context) (CleanupSample, error) {
			if calls.Add(1) == 1 {
				return CleanupSample{Pending: 2, OldestPending: &oldest}, nil
			}
			return CleanupSample{}, errors.New("PRIVATE cleanup read failure")
		}
		done := make(chan error, 1)
		go func() {
			done <- RunKeyed(ctx, cleanupSource(blocked), func(context.Context, string) error { return nil }, o)
		}()
		failed := awaitMetrics(t, m, func(s MetricsSnapshot) bool { return s.CleanupReadsFailed >= 1 })
		if !failed.CleanupEnabled || failed.CleanupAvailable || failed.CleanupPending != 2 || !failed.CleanupOldest.Equal(oldest) {
			t.Fatal(failed)
		}
		body := exposition(t, m)
		if !strings.Contains(body, "stego_controller_cleanup_available 0\n") || !strings.Contains(body, "stego_controller_cleanup_pending_resources 2\n") || strings.Contains(body, "PRIVATE") {
			t.Fatal("the exposition after a failed read", body)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if m.Snapshot().CleanupAvailable || !strings.Contains(exposition(t, m), "stego_controller_cleanup_available 0\n") {
			t.Fatal("shutdown left the values available")
		}
	})

	t.Run("ST-SUP-RP02-LC18-T05 A malformed sample or a terminal access error stops the controller", func(t *testing.T) {
		now := time.Now()
		for _, c := range []struct {
			name   string
			sample CleanupSample
			err    error
		}{{"malformed", CleanupSample{Pending: 0, OldestPending: &now}, nil}, {"permission", CleanupSample{}, denied}} {
			t.Run(c.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				o := keyOptions()
				o.Metrics = new(Metrics)
				o.Cleanup = func(context.Context) (CleanupSample, error) { return c.sample, c.err }
				err := RunKeyed(ctx, cleanupSource(blocked), func(context.Context, string) error { return nil }, o)
				if err == nil || ctx.Err() != nil {
					t.Fatal("the controller did not stop with an error", err)
				}
				if c.err == nil && !errors.Is(err, ErrMetricsContract) || c.err != nil && !errors.Is(err, c.err) {
					t.Fatal(err)
				}
			})
		}
	})

	t.Run("ST-SUP-RP02-LC18-T06 Any other failed sample is reported and sampled again", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		m := new(Metrics)
		o := keyOptions()
		o.Metrics = m
		o.ResyncInterval = 5 * time.Millisecond
		var failures atomic.Int32
		o.Observe = func(e Event) {
			if e.Phase == "metrics_failed" {
				failures.Add(1)
			}
		}
		var calls atomic.Int32
		o.Cleanup = func(context.Context) (CleanupSample, error) {
			if calls.Add(1) == 1 {
				return CleanupSample{}, errors.New("transient")
			}
			oldest := time.Now()
			return CleanupSample{Pending: 1, OldestPending: &oldest}, nil
		}
		done := make(chan error, 1)
		go func() {
			done <- RunKeyed(ctx, cleanupSource(blocked), func(context.Context, string) error { return nil }, o)
		}()
		s := awaitMetrics(t, m, func(s MetricsSnapshot) bool { return s.CleanupAvailable && s.CleanupPending == 1 })
		if !s.Running || failures.Load() != 1 || s.CleanupReadsFailed != 1 {
			t.Fatal("the transient failure was not reported once or stopped the controller", s, failures.Load())
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ST-SUP-RP02-LC18-T07 A failed sample keeps its own counter and is never a work retry", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		m := new(Metrics)
		o := keyOptions()
		o.Metrics = m
		o.ResyncInterval = 5 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var calls atomic.Int32
		o.Cleanup = func(context.Context) (CleanupSample, error) {
			if calls.Add(1) > 2 {
				cancel()
				return CleanupSample{}, nil
			}
			return CleanupSample{}, new(privateError)
		}
		_, err := Monitor(ctx, "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			return 0, RunKeyed(ctx, cleanupSource(blocked), func(context.Context, string) error { return nil }, o)
		})
		if err != nil {
			t.Fatal(err)
		}
		if s := m.Snapshot(); s.CleanupReadsFailed != 2 || s.Retries != 0 {
			t.Fatal("the failed samples changed the work counters", s)
		}
		if _, ok := providers.metrics(t)["stego.controller.retries"]; ok {
			t.Fatal("a failed sample counted as a work retry")
		}
		for _, log := range providers.completions("cleanup") {
			if log.retry {
				t.Fatal("a cleanup completion is marked as a retry")
			}
		}
	})
}

// sequenceIDs gives trace and span IDs from a counter, so the sampling decisions are the same on every run.
type sequenceIDs struct {
	mu   sync.Mutex
	next uint64
}

func (s *sequenceIDs) id() (out [16]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	// Spread the counter over the low half, which the ratio sampler reads.
	value := s.next * 0x9E3779B97F4A7C15
	for i := range 8 {
		out[8+i] = byte(value >> (8 * i))
		out[i] = byte(s.next >> (8 * i))
	}
	return out
}

func (s *sequenceIDs) NewIDs(context.Context) (trace.TraceID, trace.SpanID) {
	id := s.id()
	var span trace.SpanID
	copy(span[:], id[8:])
	return trace.TraceID(id), span
}

func (s *sequenceIDs) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	id := s.id()
	var span trace.SpanID
	copy(span[:], id[8:])
	return span
}

func TestTelemetrySpans(t *testing.T) {
	t.Run("ST-SUP-RP02-LC19-T01 An operation root keeps the supplied context's cancellation, deadline and ownership", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		owned, release, err := telemetry.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		type key struct{}
		parentCtx, parentSpan := providers.traces.Tracer("test").Start(owned, "parent")
		deadline := time.Now().Add(time.Minute)
		parentCtx, cancel := context.WithDeadline(context.WithValue(parentCtx, key{}, "value"), deadline)
		var root trace.SpanContext
		err, finish := controllerWork(parentCtx, "reconcile", func(ctx context.Context) error {
			root = trace.SpanContextFromContext(ctx)
			if end, ok := ctx.Deadline(); !ok || !end.Equal(deadline) || ctx.Value(key{}) != "value" || ownershipOf(ctx) == nil {
				return errors.New("the root lost the deadline, the value or the ownership")
			}
			cancel()
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
				return errors.New("the root did not end with its parent")
			}
		})
		finish(false)
		parentSpan.End()
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if root.TraceID() == parentSpan.SpanContext().TraceID() {
			t.Fatal("the root continued the parent trace")
		}
		for _, span := range providers.spans.Ended() {
			if span.Name() == "controller.reconcile" && span.Parent().IsValid() {
				t.Fatal("the root has a parent span")
			}
		}
	})

	t.Run("ST-SUP-RP02-LC19-T02 A database span is a child of the current operation", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		_, err := Monitor(context.Background(), "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			source := KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error { return emit("key") }}
			return 0, RunKeyed(ctx, source, func(ctx context.Context, _ string) error {
				// The generated store starts its span from the operation's context.
				_, span := trace.SpanFromContext(ctx).TracerProvider().Tracer("stego/database").Start(ctx, "SELECT")
				span.End()
				return denied
			}, keyOptions())
		})
		if !errors.Is(err, denied) {
			t.Fatal(err)
		}
		var database, reconcile sdktrace.ReadOnlySpan
		for _, span := range providers.spans.Ended() {
			switch span.Name() {
			case "SELECT":
				database = span
			case "controller.reconcile":
				reconcile = span
			}
		}
		if database == nil || reconcile == nil || database.Parent().SpanID() != reconcile.SpanContext().SpanID() {
			t.Fatal("the database span is not a child of the reconcile span")
		}
	})

	t.Run("ST-SUP-RP02-LC19-T03 Sampling applies to each operation root", func(t *testing.T) {
		providers := newProviders(sdktrace.TraceIDRatioBased(0.5), &sequenceIDs{})
		telemetry := newTestTelemetryWith(t, providers)
		owned, release, err := telemetry.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		parent := trace.ContextWithRemoteSpanContext(owned, trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled}))
		decide := sdktrace.TraceIDRatioBased(0.5)
		sampled := 0
		traces := map[trace.TraceID]bool{}
		for range 100 {
			err, finish := controllerWork(parent, "reconcile", func(ctx context.Context) error {
				root := trace.SpanContextFromContext(ctx)
				traces[root.TraceID()] = true
				want := decide.ShouldSample(sdktrace.SamplingParameters{TraceID: root.TraceID()}).Decision == sdktrace.RecordAndSample
				if root.IsSampled() != want {
					return errors.New("the root was not sampled by its own trace ID")
				}
				if root.IsSampled() {
					sampled++
				}
				_, child := trace.SpanFromContext(ctx).TracerProvider().Tracer("child").Start(ctx, "child")
				defer child.End()
				if child.SpanContext().IsSampled() != root.IsSampled() {
					return errors.New("a child did not share its root's decision")
				}
				return nil
			})
			finish(false)
			if err != nil {
				t.Fatal(err)
			}
		}
		if len(traces) != 100 || sampled == 0 || sampled == 100 {
			t.Fatal("the roots did not each take a decision", len(traces), sampled)
		}
	})

	t.Run("ST-SUP-RP02-LC19-T04 A controller span or log carries no resource key, provider response, credential or private error text", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		m := new(Metrics)
		_, err := Monitor(context.Background(), "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			attempts := 0
			o := keyOptions()
			o.Metrics = m
			source := KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error { return emit("KEY-MARKER-7f3a") }}
			return 0, RunKeyed(ctx, source, func(context.Context, string) error {
				attempts++
				if attempts == 1 {
					return errors.New("provider said ERROR-MARKER-91c2 with token=sk-SECRET-5d1e")
				}
				return fmt.Errorf("%w: ERROR-MARKER-91c2 sk-SECRET-5d1e", denied)
			}, o)
		})
		if !errors.Is(err, denied) {
			t.Fatal(err)
		}
		var export strings.Builder
		for _, span := range providers.spans.Ended() {
			fmt.Fprintf(&export, "%s %v %v %v %v\n", span.Name(), span.Attributes(), span.Events(), span.Status(), span.Links())
		}
		for _, record := range providers.logs.all() {
			fmt.Fprintf(&export, "%s %s %v\n", record.EventName(), record.Body().AsString(), record.SeverityText())
			record.WalkAttributes(func(kv attribute.KeyValue) bool {
				fmt.Fprintf(&export, "%s=%s\n", kv.Key, kv.Value.Emit())
				return true
			})
		}
		fmt.Fprintf(&export, "%+v\n%s\n", providers.collect(t), exposition(t, m))
		for _, marker := range []string{"KEY-MARKER", "ERROR-MARKER", "SECRET"} {
			if strings.Contains(export.String(), marker) {
				t.Fatal("the export holds", marker)
			}
		}
		if len(providers.spans.Ended()) == 0 || len(providers.logs.all()) == 0 {
			t.Fatal("nothing was exported")
		}
	})

	t.Run("ST-SUP-RP02-LC19-T05 Attribute and link counts per span stay within the default limits", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		_, err := Monitor(context.Background(), "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			source := KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error { return emit("key") }}
			return 0, RunKeyed(ctx, source, func(ctx context.Context, _ string) error {
				span := trace.SpanFromContext(ctx)
				for i := range 300 {
					span.SetAttributes(attribute.Int(fmt.Sprint("extra.", i), i))
					span.AddLink(trace.Link{SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{byte(i + 1)}, SpanID: trace.SpanID{byte(i + 1)}})})
				}
				return denied
			}, keyOptions())
		})
		if !errors.Is(err, denied) {
			t.Fatal(err)
		}
		limits := sdktrace.NewSpanLimits()
		checked := 0
		for _, span := range providers.spans.Ended() {
			if len(span.Attributes()) > limits.AttributeCountLimit || len(span.Links()) > limits.LinkCountLimit {
				t.Fatal("a span is over its limits", span.Name(), len(span.Attributes()), len(span.Links()))
			}
			if span.Name() == "controller.reconcile" {
				checked++
			}
		}
		if checked != 1 || limits.AttributeCountLimit != 128 || limits.LinkCountLimit != 128 {
			t.Fatal("the reconcile span was not checked", checked)
		}
	})
}

// traceCollector is a fake OTLP gRPC trace collector on loopback with TLS, the receiver of the real telemetry
// runtime in the tests that need its export.
type traceCollector struct {
	tracecollector.UnimplementedTraceServiceServer
	mu    sync.Mutex
	spans []*tracepb.Span
}

func (c *traceCollector) Export(_ context.Context, request *tracecollector.ExportTraceServiceRequest) (*tracecollector.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, resource := range request.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			c.spans = append(c.spans, scope.Spans...)
		}
	}
	return &tracecollector.ExportTraceServiceResponse{}, nil
}

func (c *traceCollector) received() []*tracepb.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.spans)
}

// startTraceCollector starts the fake collector and points the telemetry settings at it, with sampling ratio 1 and
// no metric or log export.
func startTraceCollector(t *testing.T) *traceCollector {
	t.Helper()
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "OTEL_") || strings.HasPrefix(name, "STEGO_OTEL_") {
			t.Setenv(name, "")
		}
	}
	server := httptest.NewTLSServer(http.NotFoundHandler())
	server.Close()
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &traceCollector{}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: server.TLS.Certificates})))
	tracecollector.RegisterTraceServiceServer(grpcServer, c)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", file)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	return c
}

func TestTelemetryExport(t *testing.T) {
	t.Run("ST-SUP-RP02-LC07-T01 Each operation is an independent root with its provider children", func(t *testing.T) {
		collector := startTraceCollector(t)
		var runtime *otel.Runtime
		telemetry, err := NewTelemetry(func() (Providers, error) {
			var err error
			runtime, err = otel.New("controller-test", append(SpanNames(), "provider.call")...)
			if err != nil {
				return nil, err
			}
			return runtime, nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		call := func(ctx context.Context) error {
			_, finish, err := runtime.StartOperation(ctx, otel.Child, "provider.call")
			if err != nil {
				return err
			}
			finish(otel.OutcomeSuccess, false, false)
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var actions atomic.Int32
		_, err = Monitor(ctx, "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
				if err := call(ctx); err != nil {
					return nil, err
				}
				return idle[string](ctx)
			}, Scan: func(ctx context.Context, emit func(string) error) error {
				if err := call(ctx); err != nil {
					return err
				}
				for i := range 6 {
					if err := emit(fmt.Sprint("key-", i)); err != nil {
						return err
					}
				}
				return nil
			}}
			o := watchKeyOptions()
			o.Workers = 1
			return 0, RunKeyedWatch(ctx, source, func(ctx context.Context, _ string) error {
				if err := call(ctx); err != nil {
					return err
				}
				if actions.Add(1) == 6 {
					cancel()
				}
				return nil
			}, o)
		})
		if err != nil {
			t.Fatal(err)
		}
		var spans []*tracepb.Span
		deadline := time.Now().Add(5 * time.Second)
		for spans = collector.received(); len(spans) < 16 && time.Now().Before(deadline); spans = collector.received() {
			time.Sleep(10 * time.Millisecond)
		}
		roots := map[string]*tracepb.Span{}
		traces := map[string]bool{}
		var children []*tracepb.Span
		for _, span := range spans {
			if strings.HasPrefix(span.Name, "controller.") {
				if len(span.ParentSpanId) != 0 || len(span.Links) != 0 {
					t.Fatal("an operation span has a parent or a link", span.Name)
				}
				roots[string(span.SpanId)] = span
				traces[string(span.TraceId)] = true
			} else if span.Name == "provider.call" {
				children = append(children, span)
			}
		}
		if len(roots) != 8 || len(traces) != 8 || len(children) != 8 {
			t.Fatalf("%d roots in %d traces and %d provider spans, want 8 of each", len(roots), len(traces), len(children))
		}
		for _, child := range children {
			root, ok := roots[string(child.ParentSpanId)]
			if !ok || string(root.TraceId) != string(child.TraceId) {
				t.Fatal("a provider span is not under its operation")
			}
		}
	})

	t.Run("ST-SUP-RP02-LC20-T01 Without a collector, completion logs go to standard error", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		var stderr lockedBuffer
		telemetry, err := NewTelemetry(func() (Providers, error) {
			runtime, err := otel.New("controller-test", SpanNames()...)
			if err != nil {
				return nil, err
			}
			return runtime, nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		telemetry.stderr = &stderr
		_, err = Monitor(context.Background(), "", telemetry, func(ctx context.Context, _ *Metrics) (int, error) {
			o := keyOptions()
			o.Workers = 1
			source := KeyedSource[string]{Observe: idleObserve, Scan: func(ctx context.Context, emit func(string) error) error {
				for _, key := range []string{"KEY-MARKER-ok", "KEY-MARKER-bad"} {
					if err := emit(key); err != nil {
						return err
					}
				}
				return nil
			}}
			return 0, RunKeyed(ctx, source, func(_ context.Context, key string) error {
				if key == "KEY-MARKER-bad" {
					return fmt.Errorf("%w: ERROR-MARKER", denied)
				}
				return nil
			}, o)
		})
		if !errors.Is(err, denied) {
			t.Fatal(err)
		}
		output := stderr.String()
		if strings.Contains(output, "MARKER") {
			t.Fatal("a key or error text reached standard error", output)
		}
		var reconciles []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("a line is not JSON: %q", line)
			}
			if record["event.name"] == "controller.work.completed" && record["operation"] == "reconcile" {
				reconciles = append(reconciles, record)
			}
		}
		if len(reconciles) != 2 || reconciles[0]["outcome"] != "success" || reconciles[1]["outcome"] != "failure" || reconciles[1]["retry"] != false || reconciles[1]["severity"] != "ERROR" {
			t.Fatal("the completion logs on standard error", reconciles)
		}
	})
}

// lockedBuffer is a writer that the test reads after the writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
