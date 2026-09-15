package keycloak

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	tracing "example.com/provider/out/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestProviderTraceContextAndPrivacy(t *testing.T) {
	runtime, recorder := tracing.ProviderTestRuntime()
	defer runtime.Close()
	parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1, 2, 3}, SpanID: trace.SpanID{4, 5, 6}, TraceFlags: trace.FlagsSampled})
	ctx := runtime.Context(trace.ContextWithSpanContext(context.Background(), parent))
	headers := make(chan string, 2)
	c, options := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		incoming := (propagation.TraceContext{}).Extract(context.Background(), propagation.HeaderCarrier(r.Header))
		sc := trace.SpanContextFromContext(incoming)
		if sc.TraceID() != parent.TraceID() || sc.SpanID() == parent.SpanID() {
			t.Error("provider lost the caller trace")
		}
		if r.Header.Get("Baggage") != "" || r.Header.Get("Tracestate") != "" {
			t.Error("private propagation reached the provider")
		}
		headers <- sc.SpanID().String()
		if authRequest(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"id":"private-resource-id","clientId":"private-client-name"}`))
	})
	if _, err := c.GetClient(ctx, "private-resource-id"); err != nil {
		t.Fatal(err)
	}
	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatal("authentication and client request spans missing", len(spans))
	}
	methods := map[string]bool{}
	for _, span := range spans {
		if span.Parent().SpanID() != parent.SpanID() {
			t.Fatal("provider span parent differs")
		}
		methods[span.Name()] = true
		if span.SpanKind() != trace.SpanKindClient {
			t.Fatal("provider request is not a client span")
		}
		status := false
		for _, a := range span.Attributes() {
			if a == attribute.Int("http.response.status_code", 200) {
				status = true
			}
		}
		if !status {
			t.Fatal("provider status was not recorded")
		}
		encoded, err := json.Marshal(span.Attributes())
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private-resource-id", "private-client-name", "private-admin-secret", "admin-token", options.ServerURL} {
			if strings.Contains(string(encoded), secret) {
				t.Fatal("provider span exposed request data")
			}
		}
	}
	if !methods["GET"] || !methods["POST"] {
		t.Fatal("provider method spans differ")
	}
	if first, second := <-headers, <-headers; first == second {
		t.Fatal("provider reused a request span")
	}
}
