package otel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	logcollector "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metriccollector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// collector is a fake OTLP gRPC collector on loopback with TLS. With hold set, each export waits until release, a
// hold limit or the caller's deadline. With large set, it answers a trace export with a response of about that many
// bytes.
type collector struct {
	tracecollector.UnimplementedTraceServiceServer
	traces    chan *tracecollector.ExportTraceServiceRequest
	logs      chan *logcollector.ExportLogsServiceRequest
	metrics   chan *metriccollector.ExportMetricsServiceRequest
	hold      chan struct{}
	holding   atomic.Bool
	holdLimit atomic.Int64
	large     atomic.Int64
	release   func()
	mu        sync.Mutex
	batches   []int
	held      []time.Duration
}

// wait holds one export when the collector holds, and tells how long the hold lasted and whether the caller gave
// up first.
func (c *collector) wait(ctx context.Context) error {
	if !c.holding.Load() {
		return nil
	}
	start := time.Now()
	limit := time.Duration(c.holdLimit.Load())
	if limit == 0 {
		limit = time.Hour
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-c.hold:
		return nil
	case <-timer.C:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		c.held = append(c.held, time.Since(start))
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *collector) Export(ctx context.Context, request *tracecollector.ExportTraceServiceRequest) (*tracecollector.ExportTraceServiceResponse, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	count := 0
	for _, resource := range request.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			count += len(scope.Spans)
		}
	}
	c.mu.Lock()
	c.batches = append(c.batches, count)
	c.mu.Unlock()
	select {
	case c.traces <- request:
	default:
	}
	if size := c.large.Load(); size > 0 {
		// An unknown field pads the response to the size, and a client that reads it ignores it.
		response := &tracecollector.ExportTraceServiceResponse{}
		padding := protowire.AppendTag(nil, 1000, protowire.BytesType)
		padding = protowire.AppendBytes(padding, make([]byte, int(size)-len(padding)-3))
		response.ProtoReflect().SetUnknown(padding)
		return response, nil
	}
	return &tracecollector.ExportTraceServiceResponse{}, nil
}

func (c *collector) exported() (total, largest int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, count := range c.batches {
		total += count
		largest = max(largest, count)
	}
	return total, largest
}

type logSink struct {
	logcollector.UnimplementedLogsServiceServer
	c *collector
}

func (s *logSink) Export(ctx context.Context, request *logcollector.ExportLogsServiceRequest) (*logcollector.ExportLogsServiceResponse, error) {
	if err := s.c.wait(ctx); err != nil {
		return nil, err
	}
	select {
	case s.c.logs <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &logcollector.ExportLogsServiceResponse{}, nil
}

type metricSink struct {
	metriccollector.UnimplementedMetricsServiceServer
	c *collector
}

func (s *metricSink) Export(ctx context.Context, request *metriccollector.ExportMetricsServiceRequest) (*metriccollector.ExportMetricsServiceResponse, error) {
	if err := s.c.wait(ctx); err != nil {
		return nil, err
	}
	select {
	case s.c.metrics <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &metriccollector.ExportMetricsServiceResponse{}, nil
}

// clearEnvironment removes every telemetry setting of the test process for the test.
func clearEnvironment(t testing.TB) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") || name == tokenSetting {
			t.Setenv(name, "")
		}
	}
}

// trustFile writes the certificate of a new TLS test server to a PEM file, and returns the file and the server's
// certificate pair.
func trustFile(t testing.TB) (string, tls.Certificate) {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	server.Close()
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return file, server.TLS.Certificates[0]
}

// startCollector starts the fake collector and points the telemetry settings at it, with sampling ratio 1.
func startCollector(t testing.TB, hold bool) *collector {
	t.Helper()
	clearEnvironment(t)
	file, certificate := trustFile(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}})))
	c := &collector{traces: make(chan *tracecollector.ExportTraceServiceRequest, 1024), logs: make(chan *logcollector.ExportLogsServiceRequest, 64), metrics: make(chan *metriccollector.ExportMetricsServiceRequest, 64), release: func() {}}
	if hold {
		c.hold = make(chan struct{})
		c.holding.Store(true)
		var once sync.Once
		c.release = func() { once.Do(func() { close(c.hold) }) }
	}
	tracecollector.RegisterTraceServiceServer(server, c)
	logcollector.RegisterLogsServiceServer(server, &logSink{c: c})
	metriccollector.RegisterMetricsServiceServer(server, &metricSink{c: c})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	t.Cleanup(c.release)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", file)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	return c
}

// logRecorder is a log processor that keeps every record it is given.
type logRecorder struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (l *logRecorder) OnEmit(_ context.Context, record *sdklog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, record.Clone())
	return nil
}
func (l *logRecorder) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (l *logRecorder) Shutdown(context.Context) error                         { return nil }
func (l *logRecorder) ForceFlush(context.Context) error                       { return nil }
func (l *logRecorder) all() []sdklog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]sdklog.Record(nil), l.records...)
}

// harness is a runtime with the runtime's own sampler, span limits, instruments and completion logs, whose spans,
// metrics and logs go to in-memory recorders instead of a collector.
type harness struct {
	runtime *Runtime
	spans   *tracetest.SpanRecorder
	reader  *sdkmetric.ManualReader
	logs    *logRecorder
}

func newHarness(t testing.TB, ratio float64) *harness {
	t.Helper()
	h := &harness{spans: tracetest.NewSpanRecorder(), reader: sdkmetric.NewManualReader(), logs: &logRecorder{}}
	h.runtime = &Runtime{service: "records", instance: "00000000-0000-4000-8000-000000000000"}
	h.runtime.resource = serviceResource(h.runtime.service, h.runtime.instance)
	h.runtime.startTraces(ratio, h.spans)
	if err := h.runtime.startSignals(h.reader, h.logs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.runtime.Close)
	return h
}

func (h *harness) collect(t testing.TB) metricdata.ResourceMetrics {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func (h *harness) lastSpan(t testing.TB) sdktrace.ReadOnlySpan {
	t.Helper()
	ended := h.spans.Ended()
	if len(ended) == 0 {
		t.Fatal("no span ended")
	}
	return ended[len(ended)-1]
}

func spanAttribute(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value.Emit(), true
		}
	}
	return "", false
}

type countingHandler struct{ calls atomic.Int32 }

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.calls.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

func TestWithoutCollector(t *testing.T) {
	t.Run("ST-SUP-RP03-LC01-T01 The runtime starts no provider without a collector", func(t *testing.T) {
		clearEnvironment(t)
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		for _, phase := range []string{"open", "closed"} {
			if r.provider != nil || r.connection != nil || r.meter != nil || r.logs != nil || r.spans != nil {
				t.Fatalf("%s: a provider or a connection exists without a collector", phase)
			}
			next := &countingHandler{}
			if r.Handler(next) != http.Handler(next) || r.Route(next) != http.Handler(next) {
				t.Fatalf("%s: the handler is not served directly", phase)
			}
			ctx := context.Background()
			if got, finish := r.TraceRPC(ctx, "/sample.v1.Records/Get"); got != ctx {
				t.Fatalf("%s: the RPC context changed", phase)
			} else {
				finish(nil)
			}
			r.Close()
		}
	})
}

// silentCollector accepts connections and never answers.
func silentCollector(t testing.TB) {
	t.Helper()
	clearEnvironment(t)
	file, _ := trustFile(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			conn.Close()
		}
	})
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", file)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
}

func TestExportConnection(t *testing.T) {
	t.Run("ST-SUP-RP03-LC02-T01 The runtime starts while the collector never answers", func(t *testing.T) {
		silentCollector(t)
		global := otel.GetTracerProvider()
		started := time.Now()
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if time.Since(started) > 500*time.Millisecond {
			t.Fatal("startup waited for the collector")
		}
		r.Handler(&countingHandler{}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
		if r.ExportFailures() != 0 || r.MetricExportFailures() != 0 || r.LogExportFailures() != 0 {
			t.Fatal("an export completed before creation returned")
		}
		if otel.GetTracerProvider() != global || r.provider == nil || r.connection == nil {
			t.Fatal("the runtime set a global provider or has no private one")
		}
	})
	t.Run("ST-SUP-RP03-LC02-T02 A disabled runtime returns the caller context unchanged", func(t *testing.T) {
		startCollector(t, false)
		closed, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		closed.Close()
		for name, r := range map[string]*Runtime{"closed": closed, "no provider": {}} {
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("traceparent", "00-11111111111111111111111111111111-2222222222222222-01"))
			got, finish := r.TraceRPC(ctx, "/sample.v1.Records/Echo")
			finish(nil)
			if got != ctx {
				t.Fatalf("%s: the runtime changed the caller context", name)
			}
		}
	})
}

func TestQueuesDoNotBlock(t *testing.T) {
	t.Run("ST-SUP-RP03-LC03-T01 A full queue does not block the request", func(t *testing.T) {
		startCollector(t, true)
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		handler := r.Handler(&countingHandler{})
		started := time.Now()
		for i := 0; i < QueueSize*3; i++ {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", "/records", nil))
			if w.Code != http.StatusNoContent {
				t.Fatal("the collector failure reached the request")
			}
		}
		if time.Since(started) > time.Second {
			t.Fatal("export back pressure delayed the requests")
		}
	})
}

func TestHTTPSpan(t *testing.T) {
	t.Run("ST-SUP-RP03-LC04-T01 The span carries the status the client received", func(t *testing.T) {
		h := newHarness(t, 1)
		for _, tc := range []struct {
			name    string
			want    int
			handler http.HandlerFunc
		}{
			{"upgrade", 101, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(101) }},
			{"hints then upgrade", 101, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(103); w.WriteHeader(101) }},
			{"readfrom then upgrade", 200, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.(io.ReaderFrom).ReadFrom(strings.NewReader("data"))
				w.WriteHeader(101)
			}},
			{"empty readfrom then created", 201, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.(io.ReaderFrom).ReadFrom(strings.NewReader(""))
				w.WriteHeader(201)
			}},
			{"hints then created", 201, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(103); w.WriteHeader(201) }},
			{"final then upgrade", 204, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204); w.WriteHeader(101) }},
			{"implicit then upgrade", 200, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("data")); w.WriteHeader(101) }},
			{"string then upgrade", 200, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "data"); w.WriteHeader(101) }},
			{"flush then upgrade", 200, func(w http.ResponseWriter, _ *http.Request) {
				_ = http.NewResponseController(w).Flush()
				w.WriteHeader(101)
			}},
			{"empty", 200, func(http.ResponseWriter, *http.Request) {}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				done := make(chan struct{})
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.want == 101 {
						w.Header().Set("Upgrade", "websocket")
						w.Header().Set("Connection", "Upgrade")
					}
					h.runtime.Handler(tc.handler).ServeHTTP(w, r)
					close(done)
				}))
				server.Config.ErrorLog = log.New(io.Discard, "", 0)
				server.Start()
				defer server.Close()
				client := &http.Client{Timeout: 2 * time.Second}
				defer client.CloseIdleConnections()
				response, err := client.Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != tc.want {
					t.Fatalf("wire status: got %d, want %d", response.StatusCode, tc.want)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("the handler did not finish")
				}
				if got, _ := spanAttribute(h.lastSpan(t), "http.response.status_code"); got != strconv.Itoa(tc.want) {
					t.Fatalf("span status: got %s, want %d", got, tc.want)
				}
			})
		}
	})
	t.Run("ST-SUP-RP03-LC04-T02 The span names the registered route and not the path", func(t *testing.T) {
		c := startCollector(t, false)
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("first"))
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Error("the wrapper lost flush support", err)
			}
			w.Write([]byte("second"))
		})
		result := httptest.NewRecorder()
		r.Handler(mux).ServeHTTP(result, httptest.NewRequest("GET", "/records/private-record-id", nil))
		if !result.Flushed || result.Body.String() != "firstsecond" {
			t.Fatal("the wrapper changed the streaming response")
		}
		r.Close()
		select {
		case request := <-c.traces:
			span := request.ResourceSpans[0].ScopeSpans[0].Spans[0]
			if span.Name != "GET /records/{id}" || len(span.Attributes) != 3 {
				t.Fatal("the registered route was not kept", span)
			}
			raw, _ := proto.Marshal(request)
			if strings.Contains(string(raw), "private-record-id") {
				t.Fatal("the span used the request path instead of the route pattern")
			}
		case <-time.After(time.Second):
			t.Fatal("the span was not exported")
		}
	})
}

const parent = "00-11111111111111111111111111111111-2222222222222222-01"

func TestRPCSpan(t *testing.T) {
	failing := map[grpccodes.Code]bool{grpccodes.Unknown: true, grpccodes.DeadlineExceeded: true, grpccodes.Unimplemented: true, grpccodes.Internal: true, grpccodes.Unavailable: true, grpccodes.DataLoss: true}
	private := func() context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs("traceparent", parent, "authorization", "Bearer private-token", "baggage", "user=private-user", "tracestate", "vendor=private-state"))
	}
	t.Run("ST-SUP-RP03-LC04-T03 The RPC span lasts until the final status", func(t *testing.T) {
		h := newHarness(t, 1)
		for code := grpccodes.OK; code <= grpccodes.Unauthenticated; code++ {
			ctx, finish := h.runtime.TraceRPC(private(), "/sample.v1.Records/Watch")
			if !trace.SpanContextFromContext(ctx).IsValid() || len(h.spans.Ended()) != int(code) {
				t.Fatal("the span is missing or ended before the call finished")
			}
			finish(status.Error(code, "private-error"))
			ended := h.spans.Ended()
			if len(ended) != int(code)+1 || ended[len(ended)-1].SpanKind() != trace.SpanKindServer {
				t.Fatal("no server span ended after finish")
			}
		}
	})
	t.Run("ST-SUP-RP03-LC05-T01 The RPC span carries three fields and a fixed failure class", func(t *testing.T) {
		h := newHarness(t, 1)
		for code := grpccodes.OK; code <= grpccodes.Unauthenticated; code++ {
			_, finish := h.runtime.TraceRPC(private(), "/sample.v1.Records/Watch")
			finish(status.Error(code, "private-error"))
			span := h.lastSpan(t)
			want := 3
			if failing[code] {
				want = 4
				if kind, _ := spanAttribute(span, "error.type"); kind != statusNames[code] {
					t.Fatal("the failure class is not the status name", code)
				}
			}
			if len(span.Attributes()) != want || (span.Status().Code == otelcodes.Error) != failing[code] || span.Status().Description != "" {
				t.Fatal("incorrect RPC failure status", code)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC05-T02 The RPC span holds no private call data", func(t *testing.T) {
		h := newHarness(t, 1)
		for code := grpccodes.OK; code <= grpccodes.Unauthenticated; code++ {
			_, finish := h.runtime.TraceRPC(private(), "/sample.v1.Records/Watch")
			finish(status.Error(code, "private-error"))
			span := h.lastSpan(t)
			if span.SpanContext().TraceState().Len() != 0 || len(span.Events()) != 0 || len(span.Links()) != 0 {
				t.Fatal("the span kept a trace state, an event or a link")
			}
			for _, attr := range span.Attributes() {
				if strings.Contains(attr.Value.Emit(), "private") {
					t.Fatal("private data in the RPC span")
				}
			}
		}
	})
}

func TestParentAndSampling(t *testing.T) {
	t.Run("ST-SUP-RP03-LC08-T01 A single valid traceparent continues its trace and parent", func(t *testing.T) {
		h := newHarness(t, 1)
		request := httptest.NewRequest("GET", "/records", nil)
		request.Header.Set("traceparent", parent)
		h.runtime.Handler(&countingHandler{}).ServeHTTP(httptest.NewRecorder(), request)
		span := h.lastSpan(t)
		if span.SpanContext().TraceID().String() != "11111111111111111111111111111111" || span.Parent().SpanID().String() != "2222222222222222" || !span.Parent().IsRemote() {
			t.Fatal("the span did not continue the remote trace")
		}
		for _, values := range [][]string{nil, {"bad"}, {"00-00000000000000000000000000000000-2222222222222222-01"}, {parent, parent}} {
			request := httptest.NewRequest("GET", "/records", nil)
			request.Header["Traceparent"] = values
			h.runtime.Handler(&countingHandler{}).ServeHTTP(httptest.NewRecorder(), request)
			if h.lastSpan(t).Parent().IsValid() {
				t.Fatal("an invalid set of parents was continued", values)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC08-T02 The remote sampled flag does not change the local ratio decision", func(t *testing.T) {
		traceIDs := []string{"0123456789abcdef0000000000000001", "0123456789abcdefffffffffffffffff", "0123456789abcdef4000000000000000"}
		for _, ratio := range []float64{0, 0.5, 1} {
			h := newHarness(t, ratio)
			for _, id := range traceIDs {
				for _, flag := range []string{"00", "01"} {
					var sampled bool
					handler := h.runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						sampled = trace.SpanContextFromContext(r.Context()).IsSampled()
					}))
					request := httptest.NewRequest("GET", "/records", nil)
					request.Header.Set("traceparent", "00-"+id+"-2222222222222222-"+flag)
					handler.ServeHTTP(httptest.NewRecorder(), request)
					if want := ratioDecision(t, ratio, id); sampled != want {
						t.Fatalf("ratio %v, trace %s, flag %s: sampled %v, the ratio decides %v", ratio, id, flag, sampled, want)
					}
				}
			}
		}
	})
}

// ratioDecision is the trace ID ratio rule of OpenTelemetry, worked out here and not by the SDK: a trace is sampled
// when its last 63 bits are below the ratio of 2^63.
func ratioDecision(t testing.TB, ratio float64, id string) bool {
	t.Helper()
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 16 {
		t.Fatal("bad trace ID", id)
	}
	if ratio >= 1 {
		return true
	}
	bound := uint64(ratio * math.Exp2(63))
	return binary.BigEndian.Uint64(raw[8:16])>>1 < bound
}

func TestSettings(t *testing.T) {
	endpoint := "OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example:4317"
	t.Run("ST-SUP-RP03-LC09-T01 With a collector endpoint and no exporter setting all three signals are enabled", func(t *testing.T) {
		values, err := readSettings("records", []string{endpoint})
		if err != nil || !values.metrics || !values.logs || values.endpoint == "" {
			t.Fatal("a signal is not enabled", err)
		}
		startCollector(t, false)
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if r.provider == nil || r.meter == nil || r.logs == nil || r.signals.httpDuration == nil || r.signals.logger == nil {
			t.Fatal("traces, metrics or request logs are not enabled")
		}
	})
	t.Run("ST-SUP-RP03-LC09-T02 The settings defaults are ratio 0.1, a ten-second interval and the declared service name", func(t *testing.T) {
		values, err := readSettings("records", []string{endpoint})
		if err != nil || values.ratio != 0.1 || values.interval != 10*time.Second || values.service != "records" {
			t.Fatal("unexpected defaults", values, err)
		}
	})
	t.Run("ST-SUP-RP03-LC09-T03 A protocol setting other than grpc is refused", func(t *testing.T) {
		for protocol, accepted := range map[string]bool{"": true, "grpc": true, "http/protobuf": false, "http/json": false} {
			_, err := readSettings("records", []string{endpoint, "OTEL_EXPORTER_OTLP_PROTOCOL=" + protocol})
			if (err == nil) != accepted {
				t.Fatalf("protocol %q: accepted %v", protocol, err == nil)
			}
			if err != nil && !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_PROTOCOL") {
				t.Fatal("the error does not name the setting", err)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC09-T04 The certificate setting is read only as a bounded regular PEM file", func(t *testing.T) {
		valid, certificate := trustFile(t)
		dir := t.TempDir()
		device := filepath.Join(dir, "device.pem")
		if err := os.Symlink("/dev/zero", device); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(valid)
		large := filepath.Join(dir, "large.pem")
		if err := os.WriteFile(large, append(append([]byte{}, data...), make([]byte, certificateLimit)...), 0o600); err != nil {
			t.Fatal(err)
		}
		text := filepath.Join(dir, "text.pem")
		if err := os.WriteFile(text, []byte("not a certificate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		roots, err := trustRoots(valid)
		parsed, _ := x509.ParseCertificate(certificate.Certificate[0])
		want := x509.NewCertPool()
		want.AddCert(parsed)
		if err != nil || !roots.Equal(want) {
			t.Fatal("the PEM file was not used", err)
		}
		for name, file := range map[string]string{"directory": dir, "device link": device, "larger than the bound": large, "not PEM": text} {
			if _, err := trustRoots(file); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		system, err := x509.SystemCertPool()
		if err != nil {
			t.Skip("no system roots on this host")
		}
		if roots, err := trustRoots(""); err != nil || !roots.Equal(system) {
			t.Fatal("the system roots are not used when the setting is absent")
		}
	})
}

func TestExportLimits(t *testing.T) {
	t.Run("ST-SUP-RP03-LC10-T01 The trace queue holds at most 256 spans in batches of at most 32", func(t *testing.T) {
		c := startCollector(t, true)
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for range 300 {
			_, span := r.tracer.Start(context.Background(), "span")
			span.End()
		}
		time.Sleep(300 * time.Millisecond)
		c.release()
		r.Close()
		exported, largest := c.exported()
		dropped := int(r.DroppedSpans())
		if exported+dropped != 300 || exported > QueueSize+BatchSize || dropped < 300-QueueSize-BatchSize || largest > BatchSize {
			t.Fatalf("exported %d, dropped %d, largest batch %d", exported, dropped, largest)
		}
	})
	t.Run("ST-SUP-RP03-LC10-T02 Each export ends within two seconds with a 64 KiB response limit", func(t *testing.T) {
		c := startCollector(t, true)
		c.holdLimit.Store(int64(5 * time.Second))
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		_, span := r.tracer.Start(context.Background(), "held")
		span.End()
		waitFor(t, 4*time.Second, func() bool { return r.ExportFailures() == 1 })
		c.mu.Lock()
		held := append([]time.Duration(nil), c.held...)
		c.mu.Unlock()
		if len(held) != 1 || held[0] < ExportTimeout-200*time.Millisecond || held[0] > ExportTimeout+500*time.Millisecond {
			t.Fatal("the held export was not cancelled at two seconds", held)
		}
		c.holding.Store(false)
		for _, size := range []int64{ResponseLimit - 1024, 65 << 10} {
			c.large.Store(size)
			before := r.ExportFailures()
			c.mu.Lock()
			batches := len(c.batches)
			c.mu.Unlock()
			_, span := r.tracer.Start(context.Background(), "large")
			span.End()
			waitFor(t, 3*time.Second, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.batches) > batches })
			time.Sleep(100 * time.Millisecond)
			refused := r.ExportFailures() > before
			if refused != (size > ResponseLimit) {
				t.Fatalf("a response of %d bytes: refused %v", size, refused)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC10-T03 Shutdown gives the provider at most three seconds", func(t *testing.T) {
		startCollector(t, true)
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		for range QueueSize {
			_, span := r.tracer.Start(context.Background(), "span")
			span.End()
		}
		started := time.Now()
		r.Close()
		if elapsed := time.Since(started); elapsed > ShutdownTimeout+500*time.Millisecond {
			t.Fatal("shutdown took", elapsed)
		}
	})
}

func waitFor(t testing.TB, limit time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("the condition did not hold in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
