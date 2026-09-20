package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestControllerTraceBoundaries(t *testing.T) {
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
			controller, err := runtime.ControllerTelemetry()
			if err != nil {
				t.Fatal(err)
			}
			type contextKey struct{}
			parent, cancel := context.WithTimeout(context.WithValue(context.Background(), contextKey{}, "private-context"), 10*time.Second)
			defer cancel()
			deadline, _ := parent.Deadline()
			var mu sync.Mutex
			roots := map[string]string{}
			children := map[string]string{}
			traceIDs := map[string]bool{}
			check := func(ctx context.Context) {
				t.Helper()
				got, ok := ctx.Deadline()
				if !ok || got != deadline || ctx.Done() != parent.Done() || ctx.Value(contextKey{}) != "private-context" || ctx.Value(runtimeContextKey{}) != runtime {
					t.Error("work lost its context or runtime owner")
				}
			}
			root := func(ctx context.Context, name string) {
				check(ctx)
				sc := trace.SpanContextFromContext(ctx)
				mu.Lock()
				defer mu.Unlock()
				if !sc.IsValid() || sc.IsSampled() != (sample == "1") || traceIDs[sc.TraceID().String()] {
					t.Error("work did not get an independent sampled or unsampled trace")
				}
				traceIDs[sc.TraceID().String()] = true
				roots[sc.SpanID().String()] = "controller." + name
			}
			providers := func(ctx context.Context) {
				parentSpan := trace.SpanContextFromContext(ctx)
				db, endDB := TracePostgresOperation(ctx, "delete")
				http, _, endHTTP := TraceClientHTTP(ctx, "GET")
				rpc, endRPC := TraceClientRPC(ctx, "records.Service/Get")
				for _, child := range []context.Context{db, http, rpc} {
					check(child)
					sc := trace.SpanContextFromContext(child)
					if !sc.IsValid() || sc.TraceID() != parentSpan.TraceID() || sc.SpanID() == parentSpan.SpanID() {
						t.Error("provider call lost its work trace")
					}
					mu.Lock()
					children[sc.SpanID().String()] = parentSpan.SpanID().String()
					mu.Unlock()
				}
				endDB("success")
				endHTTP(200, "")
				endRPC(nil)
			}
			// A reconnect can inherit the old watch context. Each session and each
			// serial or concurrent operation must still have an independent trace.
			watchParent := parent
			for range 2 {
				watch, endWatch := controller.Begin(watchParent, "watch")
				root(watch, "watch")
				for _, name := range []string{"scan", "cleanup"} {
					work, end := controller.Begin(watch, name)
					root(work, name)
					providers(work)
					end(nil, false)
				}
				for range 2 {
					work, end := controller.BeginResult(watch, "reconcile")
					root(work, "reconcile")
					providers(work)
					end(ControllerWorkResult{Pending: true})
				}
				var workers sync.WaitGroup
				for range 2 {
					workers.Go(func() {
						work, end := controller.BeginResult(watch, "reconcile")
						root(work, "reconcile")
						providers(work)
						end(ControllerWorkResult{})
					})
				}
				workers.Wait()
				endWatch(nil, false)
				watchParent = watch
			}
			cancel()
			if watchParent.Err() != context.Canceled {
				t.Fatal("trace boundary detached cancellation")
			}
			runtime.Close()
			if runtime.ShutdownFailures() != 0 || runtime.ExportFailures() != 0 {
				t.Fatal("telemetry did not flush")
			}
			spans := map[string]*tracepb.Span{}
		Drain:
			for {
				select {
				case batch := <-sink.received:
					for _, resource := range batch.ResourceSpans {
						for _, scope := range resource.ScopeSpans {
							for _, span := range scope.Spans {
								id := hex.EncodeToString(span.SpanId)
								if spans[id] != nil {
									t.Fatal("duplicate span")
								}
								spans[id] = span
							}
						}
					}
				default:
					break Drain
				}
			}
			if len(roots) != 14 || len(children) != 36 {
				t.Fatal("work fixture did not cover every operation")
			}
			if sample == "0" && len(spans) != 0 {
				t.Fatal("unsampled work was exported")
			}
			if sample == "1" {
				if len(spans) != len(roots)+len(children) {
					t.Fatal("work spans were lost", len(spans))
				}
				for id, name := range roots {
					span := spans[id]
					if span == nil || span.Name != name || len(span.ParentSpanId) != 0 || len(span.Links) != 0 {
						t.Fatal("work retained trace ancestry")
					}
				}
				for id, parent := range children {
					span := spans[id]
					if span == nil || hex.EncodeToString(span.ParentSpanId) != parent {
						t.Fatal("provider has the wrong parent")
					}
				}
			}
			if bytes.Contains(output.Bytes(), []byte("private-context")) {
				t.Fatal("context value entered logs")
			}
			logged := map[string]bool{}
			for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
				var record map[string]any
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				if record["event.name"] != "controller.work.completed" {
					continue
				}
				id, _ := record["span_id"].(string)
				traceID, _ := record["trace_id"].(string)
				if roots[id] == "" || !traceIDs[traceID] || logged[id] {
					t.Fatal("work log lost correlation")
				}
				logged[id] = true
			}
			if len(logged) != len(roots) {
				t.Fatal("sampled or unsampled work lost completion logs")
			}
		})
	}
}
