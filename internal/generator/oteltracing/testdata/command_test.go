package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestCommandSignals(t *testing.T) {
	for _, sample := range []string{"1", "0"} {
		t.Run(sample, func(t *testing.T) {
			sink := collectorFixture(t, false)
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", sample)
			var local bytes.Buffer
			r, err := newRuntime(&local)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			expected := map[string]string{}
			var childID, parentID string
			for _, outcome := range []string{"success", "failure", "canceled", "deadline", "aborted", "private-outcome"} {
				ctx, finish := r.TraceCommand(context.Background())
				sc := trace.SpanContextFromContext(ctx)
				if !sc.IsValid() {
					t.Fatal("command has no span context")
				}
				if outcome == "success" {
					child, _, done := TraceClientHTTP(ctx, "GET")
					childID = trace.SpanContextFromContext(child).SpanID().String()
					parentID = sc.SpanID().String()
					done(200, "")
				}
				finish(outcome)
				finish("failure")
				if outcome == "private-outcome" {
					outcome = "failure"
				}
				expected[sc.SpanID().String()] = outcome
			}
			r.Close()
			check := func(m proto.Message) {
				data, _ := proto.Marshal(m)
				if bytes.Contains(data, []byte("private-")) {
					t.Fatal("command telemetry exposed private data")
				}
			}
			spans := map[string]*tracepb.Span{}
			for len(sink.received) > 0 {
				batch := <-sink.received
				check(batch)
				for _, res := range batch.ResourceSpans {
					for _, scope := range res.ScopeSpans {
						for _, span := range scope.Spans {
							spans[hex.EncodeToString(span.SpanId)] = span
						}
					}
				}
			}
			logs := 0
			for len(sink.logs) > 0 {
				batch := <-sink.logs
				check(batch)
				for _, res := range batch.ResourceLogs {
					for _, scope := range res.ScopeLogs {
						if scope.Scope.Name != "stego/cli" {
							continue
						}
						for _, record := range scope.LogRecords {
							id := hex.EncodeToString(record.SpanId)
							outcome, ok := expected[id]
							if !ok || record.EventName != "cli.command.completed" || value(record.Attributes, "outcome").GetStringValue() != outcome {
								t.Fatal("wrong command completion")
							}
							logs++
							if sample == "1" {
								span := spans[id]
								if span == nil || span.Kind != tracepb.Span_SPAN_KIND_INTERNAL || len(span.ParentSpanId) != 0 || !bytes.Equal(span.TraceId, record.TraceId) {
									t.Fatal("command root lost correlation")
								}
								if (span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR) != (outcome != "success" && outcome != "canceled") {
									t.Fatal("wrong command error status")
								}
							}
						}
					}
				}
			}
			var total uint64
			active := int64(-1)
			for len(sink.metrics) > 0 {
				batch := <-sink.metrics
				check(batch)
				for _, res := range batch.ResourceMetrics {
					for _, scope := range res.ScopeMetrics {
						if scope.Scope.Name != "stego/cli" {
							continue
						}
						for _, m := range scope.Metrics {
							switch m.Name {
							case "stego.cli.command.duration":
								total = 0
								for _, p := range m.GetHistogram().DataPoints {
									total += p.Count
								}
							case "stego.cli.active_commands":
								for _, p := range m.GetSum().DataPoints {
									active = p.GetAsInt()
								}
							}
						}
					}
				}
			}
			if logs != 6 || total != 6 || active != 0 {
				t.Fatal("command completion missing or repeated", logs, total, active)
			}
			if sample == "1" {
				child := spans[childID]
				if child == nil || hex.EncodeToString(child.ParentSpanId) != parentID {
					t.Fatal("HTTP call lost command parent")
				}
			} else if len(spans) != 0 {
				t.Fatal("unsampled command exported spans")
			}
			if strings.Contains(local.String(), "private-") || strings.Count(local.String(), `"event.name":"cli.command.completed"`) != 6 {
				t.Fatal("local command completion failed")
			}
		})
	}
}

func BenchmarkCommandSignals(b *testing.B) {
	for _, mode := range []string{"local", "otlp"} {
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
				_, finish := runtime.TraceCommand(ctx)
				finish("success")
			}
			b.StopTimer()
			b.ReportMetric(float64(runtime.LocalLogDrops())/float64(b.N), "local_drops/op")
		})
	}
}
