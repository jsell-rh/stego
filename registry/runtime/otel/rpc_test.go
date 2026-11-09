package otel

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	otelcodes "go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func TestRPCAttributes(t *testing.T) {
	names := map[grpccodes.Code]string{grpccodes.OK: "OK", grpccodes.Canceled: "CANCELLED", grpccodes.Unknown: "UNKNOWN", grpccodes.InvalidArgument: "INVALID_ARGUMENT", grpccodes.DeadlineExceeded: "DEADLINE_EXCEEDED", grpccodes.NotFound: "NOT_FOUND", grpccodes.AlreadyExists: "ALREADY_EXISTS", grpccodes.PermissionDenied: "PERMISSION_DENIED", grpccodes.ResourceExhausted: "RESOURCE_EXHAUSTED", grpccodes.FailedPrecondition: "FAILED_PRECONDITION", grpccodes.Aborted: "ABORTED", grpccodes.OutOfRange: "OUT_OF_RANGE", grpccodes.Unimplemented: "UNIMPLEMENTED", grpccodes.Internal: "INTERNAL", grpccodes.Unavailable: "UNAVAILABLE", grpccodes.DataLoss: "DATA_LOSS", grpccodes.Unauthenticated: "UNAUTHENTICATED"}
	failing := map[string]bool{"UNKNOWN": true, "DEADLINE_EXCEEDED": true, "UNIMPLEMENTED": true, "INTERNAL": true, "UNAVAILABLE": true, "DATA_LOSS": true}
	serve := func(t *testing.T) map[grpccodes.Code]sdktrace.ReadOnlySpan {
		h := newHarness(t, 1)
		spans := map[grpccodes.Code]sdktrace.ReadOnlySpan{}
		for code := range names {
			_, finish := h.runtime.TraceRPC(context.Background(), "/sample.v1.Records/Get")
			finish(status.Error(code, "detail"))
			spans[code] = h.lastSpan(t)
		}
		if len(spans) != 17 {
			t.Fatal("not all seventeen codes were served")
		}
		return spans
	}
	t.Run("ST-SUP-RP03-LC13-T01 The RPC span records exactly the three fixed attributes", func(t *testing.T) {
		for code, span := range serve(t) {
			keys := map[string]bool{}
			for _, attr := range span.Attributes() {
				keys[string(attr.Key)] = true
			}
			system, _ := spanAttribute(span, "rpc.system.name")
			method, _ := spanAttribute(span, "rpc.method")
			name, _ := spanAttribute(span, "rpc.response.status_code")
			delete(keys, "error.type")
			if len(keys) != 3 || system != "grpc" || method != "sample.v1.Records/Get" || name != names[code] {
				t.Fatalf("%s: attributes %v", names[code], span.Attributes())
			}
		}
	})
	t.Run("ST-SUP-RP03-LC13-T02 Only the six server statuses mark the RPC span failed", func(t *testing.T) {
		count := 0
		for code, span := range serve(t) {
			kind, marked := spanAttribute(span, "error.type")
			failed := span.Status().Code == otelcodes.Error
			if failed != failing[names[code]] || marked != failed || (marked && kind != names[code]) {
				t.Fatalf("%s: failed %v, error.type %q", names[code], failed, kind)
			}
			if failed {
				count++
			}
		}
		if count != 6 {
			t.Fatal("expected six failure codes, got", count)
		}
	})
	t.Run("ST-SUP-RP03-LC13-T03 No server span carries a resource ID, a user identity, a peer address or error text", func(t *testing.T) {
		h := newHarness(t, 1)
		markers := []string{"marker-resource", "marker-user", "198.51.100.77", "marker-error"}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "marker-error", http.StatusInternalServerError)
		})
		request := httptest.NewRequest("GET", "/records/marker-resource", nil)
		request.RemoteAddr = "198.51.100.77:4711"
		request.Header.Set("Authorization", "Bearer marker-user")
		request.Header.Set("X-User", "marker-user")
		h.runtime.Handler(h.runtime.Route(mux)).ServeHTTP(httptest.NewRecorder(), request)
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer marker-user", "x-user", "marker-user"))
		ctx = peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("198.51.100.77"), Port: 4711}})
		_, finish := h.runtime.TraceRPC(ctx, "/sample.v1.Records/Get")
		finish(status.Error(grpccodes.Internal, "marker-error marker-resource"))
		spans := h.spans.Ended()
		if len(spans) != 2 {
			t.Fatal("expected an HTTP and an RPC span")
		}
		for _, span := range spans {
			text := span.Name() + " " + span.Status().Description
			for _, attr := range span.Attributes() {
				text += " " + attr.Value.Emit()
			}
			for _, event := range span.Events() {
				text += " " + event.Name
				for _, attr := range event.Attributes {
					text += " " + attr.Value.Emit()
				}
			}
			for _, marker := range markers {
				if strings.Contains(text, marker) {
					t.Fatalf("the span %q holds %q", span.Name(), marker)
				}
			}
		}
	})
}
