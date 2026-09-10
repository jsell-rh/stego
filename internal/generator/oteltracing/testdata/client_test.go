package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type clientPrivateError struct{ cause error }

func (clientPrivateError) Error() string   { panic("private error was formatted") }
func (e clientPrivateError) Unwrap() error { return e.cause }

func TestClientSignalsPreserveContextAndExcludePrivateData(t *testing.T) {
	for _, sample := range []string{"1", "0"} {
		t.Run(sample, func(t *testing.T) {
			sink := collectorFixture(t, false)
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", sample)
			var output bytes.Buffer
			runtime, err := newRuntime(&output)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			rootCtx, rootSpan := runtime.provider.Tracer("test").Start(context.Background(), "parent")
			parent := rootSpan.SpanContext()
			original := metadata.Pairs("authorization", "private-token", "traceparent", "private-parent", "tracestate", "private-state", "baggage", "private-baggage")
			ctx := runtime.Context(metadata.NewOutgoingContext(rootCtx, original))
			expected := map[string]string{}
			for _, cause := range []error{clientPrivateError{status.Error(codes.PermissionDenied, "private-provider-message")}, context.DeadlineExceeded, nil} {
				child, finish := TraceClientRPC(ctx, "sample.v1.Records/Echo")
				sc := trace.SpanContextFromContext(child)
				if !sc.IsValid() || sc.TraceID() != parent.TraceID() || sc.SpanID() == parent.SpanID() {
					t.Fatal("client span lost its parent")
				}
				md, _ := metadata.FromOutgoingContext(child)
				if len(md.Get("traceparent")) != 1 || len(md.Get("traceparent")[0]) != 55 || !strings.Contains(md.Get("traceparent")[0], sc.SpanID().String()) || len(md.Get("tracestate")) != 0 || len(md.Get("baggage")) != 0 || md.Get("authorization")[0] != "private-token" {
					t.Fatal("client metadata was not isolated")
				}
				finish(cause)
				finish(nil)
				expected[sc.SpanID().String()] = clientCode(cause).String()
			}
			if original.Get("traceparent")[0] != "private-parent" || original.Get("baggage")[0] != "private-baggage" {
				t.Fatal("client changed caller metadata")
			}
			rootSpan.End()
			runtime.Close()
			check := func(message proto.Message) {
				data, _ := proto.Marshal(message)
				if bytes.Contains(data, []byte("private-")) {
					t.Fatal("client export exposed private data")
				}
			}
			spans := map[string]*tracepb.Span{}
			logs := map[string]*logpb.LogRecord{}
			for len(sink.received) > 0 {
				batch := <-sink.received
				check(batch)
				for _, resource := range batch.ResourceSpans {
					for _, scope := range resource.ScopeSpans {
						if scope.Scope.Name == "stego/grpc-client" {
							for _, span := range scope.Spans {
								spans[hex.EncodeToString(span.SpanId)] = span
							}
						}
					}
				}
			}
			for len(sink.logs) > 0 {
				batch := <-sink.logs
				check(batch)
				for _, resource := range batch.ResourceLogs {
					for _, scope := range resource.ScopeLogs {
						if scope.Scope.Name == "stego/grpc-client" {
							for _, record := range scope.LogRecords {
								logs[hex.EncodeToString(record.SpanId)] = record
							}
						}
					}
				}
			}
			var measured uint64
			var active int64
			for len(sink.metrics) > 0 {
				batch := <-sink.metrics
				check(batch)
				for _, resource := range batch.ResourceMetrics {
					for _, scope := range resource.ScopeMetrics {
						for _, metric := range scope.Metrics {
							switch metric.Name {
							case "rpc.client.call.duration":
								measured = 0
								for _, point := range metric.GetHistogram().DataPoints {
									measured += point.Count
								}
							case "rpc.client.active_requests":
								for _, point := range metric.GetSum().DataPoints {
									active = point.GetAsInt()
								}
							}
						}
					}
				}
			}
			if len(logs) != 3 || measured != 3 || active != 0 {
				t.Fatal("client completion was lost or repeated", len(logs), measured, active)
			}
			for id, code := range expected {
				record := logs[id]
				if record == nil || record.EventName != "rpc.client.call.completed" {
					t.Fatal("missing client log", id)
				}
				if sample == "1" {
					span := spans[id]
					if span == nil || span.Kind != tracepb.Span_SPAN_KIND_CLIENT || hex.EncodeToString(span.ParentSpanId) != parent.SpanID().String() || !bytes.Equal(span.TraceId, record.TraceId) {
						t.Fatal("client signals lost correlation")
					}
				}
				if code == "OK" && record.SeverityNumber != logpb.SeverityNumber_SEVERITY_NUMBER_INFO {
					t.Fatal("success has wrong severity")
				}
			}
			if sample == "0" && len(spans) != 0 {
				t.Fatal("unsampled client exported spans")
			}
			if strings.Contains(output.String(), "private-") {
				t.Fatal("local client log exposed private data")
			}
			count := 0
			for _, line := range bytes.Split(output.Bytes(), []byte("\n")) {
				var record map[string]any
				if json.Unmarshal(line, &record) == nil && record["event.name"] == "rpc.client.call.completed" {
					count++
				}
			}
			if count != 3 {
				t.Fatal("local client completion was lost or repeated", count)
			}
		})
	}
}

func TestClientLocalLogsAndUnavailableRuntime(t *testing.T) {
	traceEnvironment(t)
	var output bytes.Buffer
	runtime, err := newRuntime(&output)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	unchanged, finish := TraceClientRPC(ctx, "private-method")
	finish(nil)
	if unchanged != ctx {
		t.Fatal("client without runtime changed context")
	}
	bound := runtime.Context(ctx)
	_, finish = TraceClientRPC(bound, "private-method")
	finish(nil)
	runtime.Close()
	_, finish = TraceClientRPC(bound, "private-after-close")
	finish(nil)
	if strings.Contains(output.String(), "private-") || !strings.Contains(output.String(), `"rpc.method":"_OTHER"`) {
		t.Fatal("local client method policy failed")
	}
}

func BenchmarkClientSignals(b *testing.B) {
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
				_, finish := TraceClientRPC(ctx, "sample.v1.Records/Echo")
				finish(nil)
			}
			b.StopTimer()
			b.ReportMetric(float64(runtime.LocalLogDrops())/float64(b.N), "local_drops/op")
		})
	}
}
