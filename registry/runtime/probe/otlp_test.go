package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	logcollector "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metriccollector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/jsell-rh/stego/registry/runtime/otel"
)

// collector is a fake OTLP gRPC collector on loopback with TLS, the receiver of the telemetry runtime in these
// tests. It keeps every request and counts the connections it accepts. With hold set, each export waits until the
// caller gives up.
type collector struct {
	tracecollector.UnimplementedTraceServiceServer
	hold     atomic.Bool
	accepted atomic.Int32
	mu       sync.Mutex
	traces   []*tracecollector.ExportTraceServiceRequest
	logs     []*logcollector.ExportLogsServiceRequest
	metrics  []*metriccollector.ExportMetricsServiceRequest
}

func (c *collector) wait(ctx context.Context) error {
	if c.hold.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (c *collector) Export(ctx context.Context, request *tracecollector.ExportTraceServiceRequest) (*tracecollector.ExportTraceServiceResponse, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.traces = append(c.traces, request)
	c.mu.Unlock()
	return &tracecollector.ExportTraceServiceResponse{}, nil
}

type logService struct {
	logcollector.UnimplementedLogsServiceServer
	c *collector
}

func (s *logService) Export(ctx context.Context, request *logcollector.ExportLogsServiceRequest) (*logcollector.ExportLogsServiceResponse, error) {
	if err := s.c.wait(ctx); err != nil {
		return nil, err
	}
	s.c.mu.Lock()
	s.c.logs = append(s.c.logs, request)
	s.c.mu.Unlock()
	return &logcollector.ExportLogsServiceResponse{}, nil
}

type metricService struct {
	metriccollector.UnimplementedMetricsServiceServer
	c *collector
}

func (s *metricService) Export(ctx context.Context, request *metriccollector.ExportMetricsServiceRequest) (*metriccollector.ExportMetricsServiceResponse, error) {
	if err := s.c.wait(ctx); err != nil {
		return nil, err
	}
	s.c.mu.Lock()
	s.c.metrics = append(s.c.metrics, request)
	s.c.mu.Unlock()
	return &metriccollector.ExportMetricsServiceResponse{}, nil
}

// countingListener counts the connections that the collector accepts.
type countingListener struct {
	net.Listener
	c *collector
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.c.accepted.Add(1)
	}
	return conn, err
}

// clearEnvironment removes every telemetry and probe setting of the test process for the test.
func clearEnvironment(t testing.TB) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") || strings.HasPrefix(name, "STEGO_OTEL_") || name == consoleSetting {
			t.Setenv(name, "")
		}
	}
}

// startCollector starts the fake collector and points the telemetry settings at it, with sampling ratio 1.
func startCollector(t testing.TB) *collector {
	t.Helper()
	clearEnvironment(t)
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
	c := &collector{}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: server.TLS.Certificates})))
	tracecollector.RegisterTraceServiceServer(grpcServer, c)
	logcollector.RegisterLogsServiceServer(grpcServer, &logService{c: c})
	metriccollector.RegisterMetricsServiceServer(grpcServer, &metricService{c: c})
	go grpcServer.Serve(countingListener{Listener: listener, c: c})
	t.Cleanup(grpcServer.Stop)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", file)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	return c
}

// logRecords are the log records of the probe that the collector received, in order, but telemetry.runtime.stopped,
// which every Close sends; the completion logs of the telemetry runtime's own requests are left out too.
func (c *collector) logRecords() []*logpb.LogRecord {
	var out []*logpb.LogRecord
	for _, record := range c.allLogRecords() {
		if record.EventName != stoppedEvent && record.EventName != "http.server.request.completed" {
			out = append(out, record)
		}
	}
	return out
}

const stoppedEvent = "telemetry.runtime.stopped"

// allLogRecords are all the log records that the collector received, in order.
func (c *collector) allLogRecords() []*logpb.LogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*logpb.LogRecord
	for _, request := range c.logs {
		for _, resource := range request.ResourceLogs {
			for _, scope := range resource.ScopeLogs {
				out = append(out, scope.LogRecords...)
			}
		}
	}
	return out
}

// lastMetrics are the metrics of the last export, by name. The temporality is cumulative, so the last export holds
// every value.
func (c *collector) lastMetrics() map[string]*metricpb.Metric {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]*metricpb.Metric{}
	if len(c.metrics) == 0 {
		return out
	}
	for _, resource := range c.metrics[len(c.metrics)-1].ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			for _, m := range scope.Metrics {
				out[m.Name] = m
			}
		}
	}
	return out
}

// spans are the spans that the collector received.
func (c *collector) spans() []*tracepb.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*tracepb.Span
	for _, request := range c.traces {
		for _, resource := range request.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				out = append(out, scope.Spans...)
			}
		}
	}
	return out
}

// attributes are key=value texts of OTLP attributes, sorted.
func attributes(attrs []*commonpb.KeyValue) []string {
	var out []string
	for _, kv := range attrs {
		var value string
		switch v := kv.Value.Value.(type) {
		case *commonpb.AnyValue_StringValue:
			value = v.StringValue
		case *commonpb.AnyValue_IntValue:
			value = strconv.FormatInt(v.IntValue, 10)
		case *commonpb.AnyValue_BoolValue:
			value = strconv.FormatBool(v.BoolValue)
		default:
			value = kv.Value.String()
		}
		out = append(out, kv.Key+"="+value)
	}
	slices.Sort(out)
	return out
}

func attributeKeys(attrs []*commonpb.KeyValue) []string {
	var out []string
	for _, kv := range attrs {
		out = append(out, kv.Key)
	}
	slices.Sort(out)
	return out
}

// newTelemetry starts the real telemetry runtime of the service orders from the process environment.
func newTelemetry(t testing.TB) *otel.Runtime {
	t.Helper()
	telemetry, err := otel.New("orders", SpanNames()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telemetry.Close)
	return telemetry
}

// newProbe starts the probe runtime on telemetry with the console setting and the writer of standard output.
func newProbe(t testing.TB, telemetry *otel.Runtime, setting string, stdout *lines) *Runtime {
	t.Helper()
	if stdout == nil {
		stdout = &lines{}
	}
	r, err := start(telemetry, nil, setting, stdout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })
	return r
}

// lines is a writer of standard output that keeps each line. It can hold a write until release and fail writes.
type lines struct {
	mu      sync.Mutex
	kept    []string
	held    chan struct{}
	entered chan struct{}
	enter   sync.Once
	fails   atomic.Int32
}

// hold makes the next write wait until release.
func (l *lines) hold() (release func()) {
	l.mu.Lock()
	l.held = make(chan struct{})
	l.entered = make(chan struct{})
	l.enter = sync.Once{}
	held := l.held
	l.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(held) }) }
}

func (l *lines) Write(b []byte) (int, error) {
	l.mu.Lock()
	held, entered := l.held, l.entered
	l.mu.Unlock()
	if held != nil {
		l.enter.Do(func() { close(entered) })
		<-held
	}
	if l.fails.Load() > 0 {
		l.fails.Add(-1)
		return 0, os.ErrClosed
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.kept = append(l.kept, string(bytes.Clone(b)))
	return len(b), nil
}

// all are the lines but that of telemetry.runtime.stopped, which every Close writes.
func (l *lines) all() []string {
	var out []string
	for _, line := range l.everything() {
		if !strings.Contains(line, `"event.name":"`+stoppedEvent+`"`) {
			out = append(out, line)
		}
	}
	return out
}

func (l *lines) everything() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.kept...)
}

func (l *lines) waitEntered(t testing.TB) {
	t.Helper()
	l.mu.Lock()
	entered := l.entered
	l.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the console sink did not start a write")
	}
}

// declare makes the descriptor of a test observation.
func declare(t testing.TB, spec Spec) *Observation {
	t.Helper()
	o, err := Declare(spec)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

const command = "reserve-and-charge"

// The observations of the reserve-and-charge example of the test records.
func reservationRefused(t testing.TB, counts Counts) *Observation {
	return declare(t, Spec{Name: "reservation_refused", Kind: Event, Severity: Warn, Counts: counts, EmittedBy: []string{command}, Fields: []Field{
		{Name: "sku", Type: FieldText}, {Name: "reason", Type: FieldEnum, Values: []string{"short", "expired"}},
	}})
}

func reservationsRefused(t testing.TB, counts Counts) *Observation {
	return declare(t, Spec{Name: "reservations_refused", Kind: Counter, Severity: Warn, Counts: counts, EmittedBy: []string{command}, Fields: []Field{
		{Name: "reason", Type: FieldEnum, Values: []string{"short", "expired"}},
	}})
}

func reservationsMade(t testing.TB) *Observation {
	return declare(t, Spec{Name: "reservations_made", Kind: Counter, Severity: Info, Counts: CountsCommitted, EmittedBy: []string{command}})
}

func histogram(t testing.TB, name string, unit Unit) *Observation {
	return declare(t, Spec{Name: name, Kind: Histogram, Severity: Info, Value: "wait", Unit: unit, EmittedBy: []string{command}})
}

// servedSpan is the server span of one request that the telemetry runtime's Handler serves. The request waits in
// its handler until End.
type servedSpan struct {
	sc      trace.SpanContext
	release chan struct{}
	done    chan struct{}
}

// End lets the handler return, so the Handler ends the span.
func (s *servedSpan) End() {
	close(s.release)
	<-s.done
}

func (s *servedSpan) SpanContext() trace.SpanContext { return s.sc }

// recordingSpan serves one request through the telemetry runtime's Handler and gives the context of its sampled
// server span, the span that the generated handler binds a probe to.
func recordingSpan(t testing.TB, telemetry *otel.Runtime) (context.Context, *servedSpan) {
	t.Helper()
	s := &servedSpan{release: make(chan struct{}), done: make(chan struct{})}
	contexts := make(chan context.Context)
	handler := telemetry.Handler(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		contexts <- request.Context()
		<-s.release
		w.WriteHeader(http.StatusNoContent)
	}))
	go func() {
		defer close(s.done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/reservations", nil))
	}()
	ctx := <-contexts
	s.sc = trace.SpanContextFromContext(ctx)
	if !s.sc.IsSampled() {
		t.Fatal("the server span is not sampled")
	}
	return ctx, s
}

// unsampled is a context with a valid span context that is not sampled, so its span does not record.
func unsampled() context.Context {
	return trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:  trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
	}))
}

func TestProbeOTLP(t *testing.T) {
	t.Run("ST-SUP-RP12-LC04-T01 An event becomes one log record and one event on the recording span", func(t *testing.T) {
		c := startCollector(t)
		telemetry := newTelemetry(t)
		r := newProbe(t, telemetry, "", nil)
		refused := reservationRefused(t, CountsAttempt)
		ctx, span := recordingSpan(t, telemetry)
		for _, ctx := range []context.Context{ctx, unsampled()} {
			call := r.Bind(ctx, "t1", "user:u-7", command)
			call.Probe().Emit(refused, Text("A-1"), Enum("short"))
			call.Returned()
			call.Commit()
		}
		span.End()
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		records := c.logRecords()
		if len(records) != 2 {
			t.Fatalf("the collector received %d log records, want 2", len(records))
		}
		for _, record := range records {
			if record.EventName != "domain.reservation_refused" || record.SeverityText != "WARN" || record.SeverityNumber != logpb.SeverityNumber_SEVERITY_NUMBER_WARN || (record.Body != nil && record.Body.Value != nil) {
				t.Fatalf("the log record is %q %s %v with body %v, want domain.reservation_refused WARN and no body", record.EventName, record.SeverityText, record.SeverityNumber, record.Body)
			}
			got := attributes(record.Attributes)
			if !slices.Contains(got, "sku=A-1") || !slices.Contains(got, "reason=short") {
				t.Fatalf("the log record has the attributes %v, want sku=A-1 and reason=short", got)
			}
		}
		var events int
		for _, s := range c.spans() {
			for _, e := range s.Events {
				if e.Name != "domain.reservation_refused" {
					continue
				}
				events++
				if got := attributes(e.Attributes); !slices.Equal(got, []string{"reason=short", "sku=A-1", "stego.outcome=committed"}) {
					t.Fatalf("the span event has the attributes %v", got)
				}
			}
		}
		if events != 1 {
			t.Fatalf("the spans hold %d events domain.reservation_refused, want 1, on the recording span only", events)
		}
	})

	t.Run("ST-SUP-RP12-LC05-T01 A counter adds one and a histogram records its value in its unit", func(t *testing.T) {
		c := startCollector(t)
		telemetry := newTelemetry(t)
		r := newProbe(t, telemetry, "", nil)
		refused := reservationsRefused(t, CountsAttempt)
		wait := histogram(t, "reservation_wait", Milliseconds)
		call := r.Bind(context.Background(), "t1", "user:u-7", command)
		for range 3 {
			call.Probe().Emit(refused, Enum("short"))
		}
		call.Probe().Emit(wait, Number(12.5))
		call.Commit()
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		metrics := c.lastMetrics()
		counter := metrics["domain.reservations_refused"]
		if counter == nil || counter.GetSum() == nil || !counter.GetSum().IsMonotonic || len(counter.GetSum().DataPoints) != 1 {
			t.Fatalf("domain.reservations_refused is %v, want one monotonic sum", counter)
		}
		point := counter.GetSum().DataPoints[0]
		got := attributes(point.Attributes)
		if point.GetAsInt() != 3 || !slices.Contains(got, "reason=short") || !slices.Contains(got, "operation=reserve-and-charge") {
			t.Fatalf("domain.reservations_refused is %d with %v, want 3 with reason=short and operation=reserve-and-charge", point.GetAsInt(), got)
		}
		h := metrics["domain.reservation_wait"]
		if h == nil || h.Unit != "ms" || h.GetHistogram() == nil || len(h.GetHistogram().DataPoints) != 1 {
			t.Fatalf("domain.reservation_wait is %v, want one histogram in ms", h)
		}
		if p := h.GetHistogram().DataPoints[0]; p.Count != 1 || p.GetSum() != 12.5 {
			t.Fatalf("domain.reservation_wait holds %d values with sum %v, want one value 12.5", p.Count, p.GetSum())
		}
	})

	t.Run("ST-SUP-RP12-LC13-T01 A span event and a metric hold no tenant, principal, trace ID or span ID", func(t *testing.T) {
		c := startCollector(t)
		telemetry := newTelemetry(t)
		r := newProbe(t, telemetry, "", nil)
		event := reservationRefused(t, CountsAttempt)
		counter := reservationsRefused(t, CountsAttempt)
		ctx, span := recordingSpan(t, telemetry)
		call := r.Bind(ctx, "t1", "user:u-7", command)
		call.Probe().Emit(event, Text("A-1"), Enum("short"))
		call.Probe().Emit(counter, Enum("short"))
		call.Returned()
		call.Commit()
		span.End()
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, s := range c.spans() {
			for _, e := range s.Events {
				found = true
				if got := attributeKeys(e.Attributes); !slices.Equal(got, []string{"reason", "sku", "stego.outcome"}) {
					t.Fatalf("the span event has the attributes %v, want sku, reason and stego.outcome only", got)
				}
			}
		}
		if !found {
			t.Fatalf("no span event was exported in %d spans", len(c.spans()))
		}
		m := c.lastMetrics()["domain.reservations_refused"]
		if m == nil || len(m.GetSum().DataPoints) != 1 {
			t.Fatal("no metric domain.reservations_refused with one data point")
		}
		for _, p := range m.GetSum().DataPoints {
			if got := attributeKeys(p.Attributes); !slices.Equal(got, []string{"operation", "reason", "stego.outcome"}) {
				t.Fatalf("the metric has the attributes %v, want reason, stego.outcome and operation only", got)
			}
		}
	})

	t.Run("ST-SUP-RP12-LC14-T01 The probe opens no provider or connection of its own", func(t *testing.T) {
		c := startCollector(t)
		telemetry := newTelemetry(t)
		ctx, span := recordingSpan(t, telemetry)
		span.End()
		// The telemetry runtime connects on its first export; wait for that one connection before the probe starts.
		telemetry.TracerProvider().(interface{ ForceFlush(context.Context) error }).ForceFlush(ctx)
		waitFor(t, func() bool { return c.accepted.Load() == 1 })
		r := newProbe(t, telemetry, "", nil)
		if r.otlp == nil || r.otlp.logger == nil || r.otlp.meter == nil {
			t.Fatal("the probe has no OTLP sink on the telemetry runtime's providers")
		}
		event := reservationRefused(t, CountsAttempt)
		counter := reservationsRefused(t, CountsAttempt)
		for range 10 {
			call := r.Bind(context.Background(), "t1", "user:u-7", command)
			call.Probe().Emit(event, Text("A-1"), Enum("short"))
			call.Probe().Emit(counter, Enum("expired"))
			call.Commit()
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(c.logRecords()) != 10 || c.lastMetrics()["domain.reservations_refused"] == nil {
			t.Fatalf("the collector received %d log records and the metrics %v, want 10 records and the counter", len(c.logRecords()), c.lastMetrics())
		}
		if got := c.accepted.Load(); got != 1 {
			t.Fatalf("the collector accepted %d connections, want only the telemetry runtime's one", got)
		}
		// The package source makes no provider, exporter or connection: it imports no SDK, exporter or network
		// package, and no global provider.
		fset := token.NewFileSet()
		names, _ := filepath.Glob("*.go")
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, spec := range file.Imports {
				path, _ := strconv.Unquote(spec.Path.Value)
				if path == "go.opentelemetry.io/otel" || strings.HasPrefix(path, "go.opentelemetry.io/otel/sdk") || strings.Contains(path, "/exporters/") || strings.HasPrefix(path, "google.golang.org/grpc") || path == "net" || strings.HasPrefix(path, "net/") {
					t.Fatalf("%s imports %s", name, path)
				}
			}
		}
	})

	t.Run("ST-SUP-RP12-LC15-T01 A domain histogram uses the fixed boundaries of its unit and cumulative temporality", func(t *testing.T) {
		c := startCollector(t)
		telemetry := newTelemetry(t)
		r := newProbe(t, telemetry, "", nil)
		units := map[Unit]string{Seconds: "wait_s", Milliseconds: "wait_ms", Bytes: "wait_by", One: "wait_one"}
		call := r.Bind(context.Background(), "t1", "user:u-7", command)
		for unit, name := range units {
			call.Probe().Emit(histogram(t, name, unit), Number(3))
		}
		call.Commit()
		// One request through the telemetry runtime gives the request duration histogram of ST-SUP-RP03-LC11.
		handler := telemetry.Handler(telemetry.Route(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		metrics := c.lastMetrics()
		request := metrics["http.server.request.duration"]
		if request == nil {
			t.Fatal("no request duration histogram")
		}
		want := map[Unit][]float64{
			Seconds:      request.GetHistogram().DataPoints[0].ExplicitBounds,
			Milliseconds: {1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000},
			Bytes:        {64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864},
			One:          {1, 2, 5, 10, 20, 50, 100, 200, 500, 1000},
		}
		for unit, name := range units {
			m := metrics["domain."+name]
			if m == nil || m.GetHistogram() == nil {
				t.Fatalf("no histogram domain.%s", name)
			}
			if m.Unit != string(unit) || m.GetHistogram().AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
				t.Fatalf("domain.%s has the unit %q and the temporality %v, want %s and cumulative", name, m.Unit, m.GetHistogram().AggregationTemporality, unit)
			}
			if got := m.GetHistogram().DataPoints[0].ExplicitBounds; !slices.Equal(got, want[unit]) {
				t.Fatalf("domain.%s has the boundaries %v, want %v", name, got, want[unit])
			}
		}
	})
}

// waitFor waits until done, at most five seconds.
func waitFor(t testing.TB, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("the condition did not come true in five seconds")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProbeCallSpan(t *testing.T) {
	t.Run("ST-SUP-RP03-LC15-T03 The probe runtime starts a child span per call", func(t *testing.T) {
		c := startCollector(t)
		telemetry, err := otel.New("orders", SpanNames()...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(telemetry.Close)
		r := newProbe(t, telemetry, "", nil)
		refused := reservationRefused(t, CountsAttempt)
		ctx, span := recordingSpan(t, telemetry)
		server := span.SpanContext()
		for _, commit := range []bool{true, false} {
			call := r.Bind(ctx, "t1", "user:u-7", command)
			if call.Probe().SpanContext().SpanID() == server.SpanID() || call.Probe().SpanContext().TraceID() != server.TraceID() {
				t.Fatal("the call is not bound to a child span of the server span")
			}
			call.Probe().Emit(refused, Text("A-1"), Enum("short"))
			call.Returned()
			if commit {
				call.Commit()
			} else {
				call.Rollback()
			}
		}
		span.End()
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		var outcomes []string
		for _, s := range c.spans() {
			if s.Name != CallSpanName {
				if s.SpanId != nil && trace.SpanID(s.SpanId) == server.SpanID() && len(s.Events) != 0 {
					t.Fatal("an event of a call went on the server span")
				}
				continue
			}
			if trace.SpanID(s.ParentSpanId) != server.SpanID() {
				t.Fatal("the call span is not a child of the server span")
			}
			if len(s.Events) != 1 || s.Events[0].Name != "domain.reservation_refused" {
				t.Fatal("the call span does not hold the call's event", s.Events)
			}
			for _, kv := range s.Attributes {
				if kv.Key == "stego.outcome" {
					outcomes = append(outcomes, kv.Value.GetStringValue())
				}
			}
			if s.Status.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
				t.Fatal("a call span is marked as an error; a rollback is an outcome, not an error")
			}
		}
		slices.Sort(outcomes)
		if !slices.Equal(outcomes, []string{"committed", "rolled_back"}) {
			t.Fatal("the call spans have the outcomes", outcomes)
		}
	})

	t.Run("ST-SUP-RP03-LC15-T03 A telemetry runtime without the call span name fails the start", func(t *testing.T) {
		clearEnvironment(t)
		telemetry, err := otel.New("orders")
		if err != nil {
			t.Fatal(err)
		}
		defer telemetry.Close()
		if _, err := Start(telemetry, nil); err == nil || !strings.Contains(err.Error(), CallSpanName) {
			t.Fatal("the probe started on a runtime without its span name", err)
		}
	})
}
