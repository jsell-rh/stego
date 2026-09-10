package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// HTTPClientTestRuntime supplies an isolated runtime to the external wire test.
func HTTPClientTestRuntime(t *testing.T) (*Runtime, *tracetest.SpanRecorder, *bytes.Buffer) {
	t.Helper()
	traceEnvironment(t)
	output := new(bytes.Buffer)
	r, err := newRuntime(output)
	if err != nil {
		t.Fatal(err)
	}
	recorder := tracetest.NewSpanRecorder()
	r.provider = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	if err := r.initHTTPClientSignals(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, recorder, output
}

func TestHTTPClientSignals(t *testing.T) {
	for _, sample := range []string{"1", "0"} {
		t.Run(sample, func(t *testing.T) {
			sink := collectorFixture(t, false)
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", sample)
			var output bytes.Buffer
			r, err := newRuntime(&output)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			parentCtx, parentSpan := r.provider.Tracer("test").Start(context.Background(), "parent")
			parent := parentSpan.SpanContext()
			ctx := r.Context(parentCtx)
			original := http.Header{"authorization": {"private-token"}, "traceparent": {"private-parent"}, "Traceparent": {"private-duplicate"}, "tracestate": {"private-state"}, "baggage": {"private-baggage"}}
			expected := map[string]string{}
			for _, item := range []struct {
				method string
				status int
				kind   string
			}{{"POST", 201, ""}, {"GET", 403, ""}, {"GET", 0, "deadline"}, {"GET", 200, "canceled"}, {"private-method", 999, "private-error"}} {
				child, inject, finish := TraceClientHTTP(ctx, item.method)
				sc := trace.SpanContextFromContext(child)
				headers := inject(original)
				if !sc.IsValid() || sc.TraceID() != parent.TraceID() || sc.SpanID() == parent.SpanID() {
					t.Fatal("missing child context")
				}
				if len(headers.Values("Traceparent")) != 1 || !strings.Contains(headers.Get("Traceparent"), sc.SpanID().String()) || headers["authorization"][0] != "private-token" {
					t.Fatal("invalid propagation")
				}
				for key := range headers {
					switch strings.ToLower(key) {
					case "baggage", "tracestate":
						t.Fatal("private propagation reached headers")
					}
				}
				finish(item.status, item.kind)
				finish(500, "transport")
				outcome := "success"
				if item.kind == "canceled" {
					outcome = "canceled"
				} else if item.kind != "" || item.status >= 400 {
					outcome = "failure"
				}
				expected[sc.SpanID().String()] = outcome
			}
			if original["traceparent"][0] != "private-parent" || original["baggage"][0] != "private-baggage" {
				t.Fatal("caller headers changed")
			}
			parentSpan.End()
			r.Close()
			check := func(m proto.Message) {
				data, _ := proto.Marshal(m)
				if bytes.Contains(data, []byte("private-")) {
					t.Fatal("private data reached telemetry")
				}
			}
			spans := map[string]*tracepb.Span{}
			for len(sink.received) > 0 {
				batch := <-sink.received
				check(batch)
				for _, resource := range batch.ResourceSpans {
					for _, scope := range resource.ScopeSpans {
						if scope.Scope.Name == "stego/http-client" {
							for _, span := range scope.Spans {
								spans[hex.EncodeToString(span.SpanId)] = span
							}
						}
					}
				}
			}
			count := 0
			for len(sink.logs) > 0 {
				batch := <-sink.logs
				check(batch)
				for _, resource := range batch.ResourceLogs {
					for _, scope := range resource.ScopeLogs {
						if scope.Scope.Name != "stego/http-client" {
							continue
						}
						for _, record := range scope.LogRecords {
							id := hex.EncodeToString(record.SpanId)
							outcome, ok := expected[id]
							if !ok || value(record.Attributes, "outcome").GetStringValue() != outcome {
								t.Fatal("invalid HTTP client log")
							}
							count++
							if sample == "1" {
								span := spans[id]
								if span == nil || span.Kind != tracepb.Span_SPAN_KIND_CLIENT || hex.EncodeToString(span.ParentSpanId) != parent.SpanID().String() || !bytes.Equal(span.TraceId, record.TraceId) {
									t.Fatal("HTTP signals lost correlation")
								}
								if (span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR) != (outcome == "failure") {
									t.Fatal("wrong HTTP client span status")
								}
							}
							if outcome == "canceled" && value(record.Attributes, "error.type") != nil {
								t.Fatal("caller cancellation became an error")
							}
						}
					}
				}
			}
			var measured uint64
			active := int64(-1)
			for len(sink.metrics) > 0 {
				batch := <-sink.metrics
				check(batch)
				for _, resource := range batch.ResourceMetrics {
					for _, scope := range resource.ScopeMetrics {
						if scope.Scope.Name != "stego/http-client" {
							continue
						}
						for _, m := range scope.Metrics {
							switch m.Name {
							case "http.client.request.duration":
								measured = 0
								for _, point := range m.GetHistogram().DataPoints {
									measured += point.Count
								}
							case "http.client.active_requests":
								for _, point := range m.GetSum().DataPoints {
									active = point.GetAsInt()
								}
							}
						}
					}
				}
			}
			if count != 5 || measured != 5 || active != 0 {
				t.Fatal("HTTP signals lost completion", count, measured, active)
			}
			if sample == "0" && len(spans) != 0 {
				t.Fatal("unsampled HTTP spans were exported")
			}
			if strings.Contains(output.String(), "private-") {
				t.Fatal("private data reached local logs")
			}
			local := 0
			for _, line := range bytes.Split(output.Bytes(), []byte("\n")) {
				var record map[string]any
				if json.Unmarshal(line, &record) == nil && record["event.name"] == "http.client.request.completed" {
					local++
				}
			}
			if local != 5 {
				t.Fatal("local HTTP completion count", local)
			}
		})
	}
}

func BenchmarkHTTPClientSignals(b *testing.B) {
	for _, mode := range []string{"unbound", "local", "otlp"} {
		b.Run(mode, func(b *testing.B) {
			var stop chan struct{}
			if mode != "otlp" {
				traceEnvironment(b)
			} else {
				sink := collectorFixture(b, false)
				stop = make(chan struct{})
				defer close(stop)
				go func() {
					for {
						select {
						case <-sink.received:
						case <-sink.logs:
						case <-sink.metrics:
						case <-stop:
							return
						}
					}
				}()
			}
			runtime, err := newRuntime(io.Discard)
			if err != nil {
				b.Fatal(err)
			}
			defer runtime.Close()
			ctx := context.Background()
			if mode != "unbound" {
				ctx = runtime.Context(ctx)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _, finish := TraceClientHTTP(ctx, "GET")
				finish(200, "")
			}
			b.StopTimer()
			b.ReportMetric(float64(runtime.LocalLogDrops())/float64(b.N), "local_drops/op")
		})
	}
}

func TestHTTPClientLocalAndUnavailableRuntime(t *testing.T) {
	traceEnvironment(t)
	var output bytes.Buffer
	r, err := newRuntime(&output)
	if err != nil {
		t.Fatal(err)
	}
	original := http.Header{"Traceparent": {"private-parent"}}
	ctx := context.Background()
	same, inject, finish := TraceClientHTTP(ctx, "GET")
	finish(200, "")
	if same != ctx || inject(original).Get("Traceparent") != "private-parent" {
		t.Fatal("unbound call changed propagation")
	}
	bound := r.Context(ctx)
	_, inject, finish = TraceClientHTTP(bound, "private-method")
	finish(0, "private-error")
	finish(200, "")
	if inject(original).Get("Traceparent") != "private-parent" {
		t.Fatal("local call changed propagation")
	}
	r.Close()
	before := output.Len()
	_, _, finish = TraceClientHTTP(bound, "GET")
	finish(200, "")
	if output.Len() != before || strings.Contains(output.String(), "private-") {
		t.Fatal("closed runtime or local privacy failed")
	}
	var record map[string]any
	if json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record) != nil || record["http.request.method"] != "_OTHER" || record["error.type"] != "_OTHER" {
		t.Fatal("local completion is missing or repeated")
	}
	_, _, finish = TraceClientHTTP(nil, "GET")
	finish(200, "")
}
