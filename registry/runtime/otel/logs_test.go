package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fields lists the key=value text of attributes, sorted.
func fields(attrs []attribute.KeyValue) []string {
	var out []string
	for _, kv := range attrs {
		out = append(out, string(kv.Key)+"="+kv.Value.Emit())
	}
	slices.Sort(out)
	return out
}

func recordFields(record *sdklog.Record) (out []attribute.KeyValue, duration bool) {
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv.Key == "duration_seconds" {
			duration = kv.Value.Type() == attribute.FLOAT64 && kv.Value.AsFloat64() >= 0
			return true
		}
		out = append(out, kv)
		return true
	})
	return out, duration
}

func TestCompletionLogs(t *testing.T) {
	t.Run("ST-SUP-RP03-LC12-T01 The completion log has the fixed name and the span's fields", func(t *testing.T) {
		h := newHarness(t, 1)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		h.runtime.Handler(h.runtime.Route(mux)).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records/1", nil))
		_, finish := h.runtime.TraceRPC(context.Background(), "/sample.v1.Records/Get")
		finish(nil)
		spans, records := h.spans.Ended(), h.logs.all()
		if len(spans) != 2 || len(records) != 2 {
			t.Fatalf("got %d spans and %d logs, want 2 of each", len(spans), len(records))
		}
		for i, name := range []string{"http.server.request.completed", "rpc.server.call.completed"} {
			record, span := &records[i], spans[i]
			got, duration := recordFields(record)
			if record.EventName() != name || !duration || !slices.Equal(fields(got), fields(span.Attributes())) {
				t.Fatalf("%s: event %q, duration %v, fields %v, span fields %v", name, record.EventName(), duration, fields(got), fields(span.Attributes()))
			}
			if record.TraceID() != span.SpanContext().TraceID() || record.SpanID() != span.SpanContext().SpanID() {
				t.Fatalf("%s is not correlated with its span", name)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC12-T02 The request completion log severity follows the request outcome", func(t *testing.T) {
		h := newHarness(t, 1)
		for _, code := range []int{200, 403, 400, 500} {
			h.runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
		}
		for _, code := range []grpccodes.Code{grpccodes.OK, grpccodes.PermissionDenied, grpccodes.InvalidArgument, grpccodes.Internal} {
			_, finish := h.runtime.TraceRPC(context.Background(), "/sample.v1.Records/Get")
			finish(status.Error(code, "outcome"))
		}
		var got []string
		for _, record := range h.logs.all() {
			got = append(got, record.SeverityText()+"/"+record.Severity().String())
		}
		want := []string{"INFO/INFO", "WARN/WARN", "WARN/WARN", "ERROR/ERROR", "INFO/INFO", "WARN/WARN", "WARN/WARN", "ERROR/ERROR"}
		if !slices.Equal(got, want) {
			t.Fatalf("severities %v, want %v", got, want)
		}
	})
}
