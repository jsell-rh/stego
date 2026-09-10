package tracing

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

type traceCollector struct {
	collector.UnimplementedTraceServiceServer
	received chan *collector.ExportTraceServiceRequest
	block    bool
}

func (c *traceCollector) Export(ctx context.Context, req *collector.ExportTraceServiceRequest) (*collector.ExportTraceServiceResponse, error) {
	if c.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case c.received <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &collector.ExportTraceServiceResponse{}, nil
}
func traceEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") {
			t.Setenv(name, "")
		}
	}
}
func collectorFixture(t *testing.T, block bool) *traceCollector {
	t.Helper()
	traceEnvironment(t)
	certificate := httptest.NewTLSServer(http.NotFoundHandler())
	defer certificate.Close()
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: certificate.TLS.Certificates})))
	c := &traceCollector{received: make(chan *collector.ExportTraceServiceRequest, 32), block: block}
	collector.RegisterTraceServiceServer(server, c)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", file)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	return c
}
func TestTLSExportContextAndPrivacy(t *testing.T) {
	c := collectorFixture(t, false)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	observed := false
	handler := runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = trace.SpanContextFromContext(r.Context()).IsValid()
		w.WriteHeader(201)
		w.Write([]byte("private-response"))
	}))
	req := httptest.NewRequest("POST", "/private-resource-id?secret=private-query", strings.NewReader("private-request"))
	req.Header.Set("Authorization", "Bearer private-token")
	req.Header.Set("Baggage", "user=private-user")
	req.Header.Set("Tracestate", "vendor=private-state")
	req.Header.Set("Traceparent", "00-11111111111111111111111111111111-2222222222222222-00")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !observed || w.Code != 201 || w.Body.String() != "private-response" {
		t.Fatal("instrumentation changed response or lost context")
	}
	runtime.Close()
	var result *collector.ExportTraceServiceRequest
	select {
	case result = <-c.received:
	case <-time.After(time.Second):
		t.Fatal("trace was not exported")
	}
	spans := result.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 1 {
		t.Fatal("unexpected span count", len(spans))
	}
	span := spans[0]
	if span.Name != "HTTP POST" || hex.EncodeToString(span.TraceId) != "11111111111111111111111111111111" || hex.EncodeToString(span.ParentSpanId) != "2222222222222222" || len(span.Attributes) != 2 || span.TraceState != "" || len(span.Events) != 0 || len(span.Links) != 0 {
		t.Fatal("invalid trace shape", span)
	}
	data, _ := proto.Marshal(result)
	for _, private := range []string{"private-resource-id", "private-query", "private-request", "private-response", "private-token", "private-user", "private-state"} {
		if strings.Contains(string(data), private) {
			t.Fatal("trace exposed private request data")
		}
	}
}
func TestDisabledAndInvalidSettings(t *testing.T) {
	traceEnvironment(t)
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if runtime.provider != nil {
		t.Fatal("disabled tracing started a provider")
	}
	runtime.Close()
	_ = runtime.Handler(next)
	for _, endpoint := range []string{"http://localhost:4317", "https://user:secret@localhost:4317", "https://localhost:4317/path", "https://localhost:4317?secret=value"} {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
		if _, err := NewRuntime(); err == nil {
			t.Fatal("invalid endpoint accepted")
		}
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://localhost:4317")
	for _, value := range []string{"NaN", "Inf", "-1", "1.1", "invalid"} {
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", value)
		if _, err := NewRuntime(); err == nil {
			t.Fatal("invalid sampler accepted")
		}
	}
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "private=value")
	if _, err := NewRuntime(); err == nil {
		t.Fatal("undeclared resource settings accepted")
	}
}
func TestBlockedCollectorDoesNotBlockRequestsOrShutdown(t *testing.T) {
	collectorFixture(t, true)
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	handler := runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	started := time.Now()
	for i := 0; i < QueueSize*3; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/records", nil))
		if w.Code != 204 {
			t.Fatal("collector failure reached request")
		}
	}
	if time.Since(started) > time.Second {
		t.Fatal("export backpressure delayed requests")
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.ExportFailures() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("blocked export did not reach deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started = time.Now()
	runtime.Close()
	if time.Since(started) > ShutdownTimeout+time.Second {
		t.Fatal("trace shutdown exceeded its limit")
	}
}

func TestRegisteredRouteAndStreamingResponse(t *testing.T) {
	c := collectorFixture(t, false)
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("first"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error("tracing lost flush support", err)
		}
		w.Write([]byte("second"))
	})
	result := httptest.NewRecorder()
	runtime.Handler(mux).ServeHTTP(result, httptest.NewRequest("GET", "/records/private-record-id", nil))
	if !result.Flushed || result.Body.String() != "firstsecond" {
		t.Fatal("tracing changed streaming response")
	}
	runtime.Close()
	select {
	case request := <-c.received:
		span := request.ResourceSpans[0].ScopeSpans[0].Spans[0]
		if span.Name != "GET /records/{id}" || len(span.Attributes) != 3 {
			t.Fatal("registered route was not retained")
		}
		raw, _ := proto.Marshal(request)
		if strings.Contains(string(raw), "private-record-id") {
			t.Fatal("trace used request path instead of route pattern")
		}
	case <-time.After(time.Second):
		t.Fatal("stream span was not exported")
	}
}

func TestCollectorCertificateMustBeTrusted(t *testing.T) {
	collector := collectorFixture(t, false)
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", "")
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
	deadline := time.Now().Add(3 * time.Second)
	for runtime.ExportFailures() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("untrusted collector did not fail export")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-collector.received:
		t.Fatal("untrusted collector received a trace")
	default:
	}
}

func TestRouteSurvivesAuthenticationRequestCopy(t *testing.T) {
	c := collectorFixture(t, false)
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /widgets/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	inner := runtime.Route(mux)
	auth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		inner.ServeHTTP(w, r.WithContext(ctx))
	})
	runtime.Handler(auth).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/widgets/private-widget", nil))
	runtime.Close()
	select {
	case request := <-c.received:
		if request.ResourceSpans[0].ScopeSpans[0].Spans[0].Name != "GET /widgets/{id}" {
			t.Fatal("authentication request copy lost route pattern")
		}
	case <-time.After(time.Second):
		t.Fatal("trace not exported")
	}
}

func BenchmarkHTTPTracing(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "recording"
		}
		b.Run(name, func(b *testing.B) {
			runtime := &Runtime{}
			if enabled {
				runtime.provider = sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
				runtime.tracer = runtime.provider.Tracer("stego/http")
				defer runtime.provider.Shutdown(context.Background())
			}
			handler := runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			request := httptest.NewRequest("GET", "/records", nil)
			writer := &benchmarkWriter{header: make(http.Header)}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				handler.ServeHTTP(writer, request)
			}
		})
	}
}

type benchmarkWriter struct{ header http.Header }

func (w *benchmarkWriter) Header() http.Header         { return w.header }
func (w *benchmarkWriter) WriteHeader(int)             {}
func (w *benchmarkWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestRemoteSamplingCannotOverrideLocalZero(t *testing.T) {
	c := collectorFixture(t, false)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0")
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	handler := runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if trace.SpanFromContext(r.Context()).IsRecording() {
			t.Error("remote sampled flag bypassed local ratio")
		}
		w.WriteHeader(204)
	}))
	request := httptest.NewRequest("GET", "/records", nil)
	request.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	runtime.Close()
	select {
	case <-c.received:
		t.Fatal("zero-ratio trace was exported")
	default:
	}
}
func TestInvalidParentsStartRootSpans(t *testing.T) {
	c := collectorFixture(t, false)
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	handler := runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	valid := "00-11111111111111111111111111111111-2222222222222222-01"
	values := [][]string{nil, {"bad"}, {"00-00000000000000000000000000000000-2222222222222222-01"}, {valid, valid}}
	for _, parent := range values {
		request := httptest.NewRequest("GET", "/records", nil)
		request.Header["Traceparent"] = parent
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	runtime.Close()
	count := 0
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for count < len(values) {
		select {
		case request := <-c.received:
			for _, resource := range request.ResourceSpans {
				for _, scope := range resource.ScopeSpans {
					for _, span := range scope.Spans {
						count++
						if len(span.ParentSpanId) != 0 {
							t.Fatal("invalid parent was accepted")
						}
					}
				}
			}
		case <-deadline.C:
			t.Fatal("missing root spans")
		}
	}
}
