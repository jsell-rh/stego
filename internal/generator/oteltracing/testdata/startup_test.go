package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestStartupSignals(t *testing.T) {
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
				ctx, finish := TraceStartup(r.Context(context.Background()), StartupBrowserSessionSchema)
				sc := trace.SpanContextFromContext(ctx)
				if !sc.IsValid() {
					t.Fatal("startup has no span context")
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
					t.Fatal("startup telemetry exposed private data")
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
						if scope.Scope.Name != "stego/startup" {
							continue
						}
						for _, record := range scope.LogRecords {
							id := hex.EncodeToString(record.SpanId)
							outcome, ok := expected[id]
							if value(record.Attributes, "startup.stage").GetStringValue() != "browser.session_schema" {
								t.Fatal("startup stage missing")
							}
							if !ok || record.EventName != "startup.step.completed" || value(record.Attributes, "outcome").GetStringValue() != outcome {
								t.Fatal("wrong startup completion")
							}
							logs++
							if sample == "1" {
								span := spans[id]
								if span == nil || span.Kind != tracepb.Span_SPAN_KIND_INTERNAL || value(span.Attributes, "startup.stage").GetStringValue() != "browser.session_schema" || len(span.ParentSpanId) != 0 || !bytes.Equal(span.TraceId, record.TraceId) {
									t.Fatal("startup root lost correlation")
								}
								if (span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR) != (outcome != "success" && outcome != "canceled") {
									t.Fatal("wrong startup error status")
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
						if scope.Scope.Name != "stego/startup" {
							continue
						}
						for _, m := range scope.Metrics {
							switch m.Name {
							case "stego.startup.duration":
								total = 0
								for _, p := range m.GetHistogram().DataPoints {
									total += p.Count
								}
							case "stego.startup.active_steps":
								for _, p := range m.GetSum().DataPoints {
									active = p.GetAsInt()
								}
							}
						}
					}
				}
			}
			if logs != 6 || total != 6 || active != 0 {
				t.Fatal("startup completion missing or repeated", logs, total, active)
			}
			if sample == "1" {
				child := spans[childID]
				if child == nil || hex.EncodeToString(child.ParentSpanId) != parentID {
					t.Fatal("HTTP call lost startup parent")
				}
			} else if len(spans) != 0 {
				t.Fatal("unsampled startup exported spans")
			}
			if strings.Contains(local.String(), "private-") || strings.Count(local.String(), `"event.name":"startup.step.completed"`) != 6 {
				t.Fatal("local startup completion failed")
			}
		})
	}
}

func TestStartupLocalStagesAndClosedRuntime(t *testing.T) {
	traceEnvironment(t)
	var output bytes.Buffer
	r, err := newRuntime(&output)
	if err != nil {
		t.Fatal(err)
	}
	ctx := r.Context(context.Background())
	for _, invalid := range []StartupStage{0, 255} {
		_, finish := TraceStartup(ctx, invalid)
		finish("private-outcome")
	}
	for _, missing := range []context.Context{nil, context.Background()} {
		_, finish := TraceStartup(missing, StartupBrowserDiscovery)
		finish("failure")
	}
	for stage := StartupBrowserTransport; stage <= StartupBrowserRoutes; stage++ {
		_, finish := TraceStartup(ctx, stage)
		finish("deadline")
		finish("failure")
	}
	r.Close()
	_, finish := TraceStartup(ctx, StartupBrowserDiscovery)
	finish("failure")
	if strings.Count(output.String(), `"event.name":"startup.step.completed"`) != 8 || strings.Contains(output.String(), "private-") {
		t.Fatal("invalid, missing, closed, or repeated startup event")
	}
	for stage := StartupBrowserTransport; stage <= StartupBrowserRoutes; stage++ {
		if strings.Count(output.String(), `"startup.stage":"`+stage.name()+`"`) != 1 {
			t.Fatal("local stage missing or repeated")
		}
	}
}
