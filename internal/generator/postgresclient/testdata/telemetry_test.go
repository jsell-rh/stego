package postgres

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	telemetry "example.com/sql-client/tracing"
	logs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	traces "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

// Invalid identities stop before a SQL connection. The real client must still
// report its operation through the caller's private runtime and TLS exporter.
func TestClientPrivateTelemetry(t *testing.T) {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") {
			t.Setenv(name, "")
		}
	}
	cert := httptest.NewTLSServer(http.NotFoundHandler())
	defer cert.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var received []proto.Message
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: cert.TLS.Certificates})), grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		mu.Lock()
		received = append(received, proto.Clone(request.(proto.Message)))
		mu.Unlock()
		switch request.(type) {
		case *traces.ExportTraceServiceRequest:
			return &traces.ExportTraceServiceResponse{}, nil
		case *logs.ExportLogsServiceRequest:
			return &logs.ExportLogsServiceResponse{}, nil
		case *metrics.ExportMetricsServiceRequest:
			return &metrics.ExportMetricsServiceResponse{}, nil
		}
		return handler(ctx, request)
	}))
	traces.RegisterTraceServiceServer(server, &traces.UnimplementedTraceServiceServer{})
	logs.RegisterLogsServiceServer(server, &logs.UnimplementedLogsServiceServer{})
	metrics.RegisterMetricsServiceServer(server, &metrics.UnimplementedMetricsServiceServer{})
	go server.Serve(listener)
	defer server.Stop()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", ca)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	runtime, err := telemetry.NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, parentDone := telemetry.TraceDatabase(runtime.Context(context.Background()), "query")
	options := Options{Password: "private-administrator"}
	spec := DatabaseSpec{Key: DatabaseKey{"private-scope", "private-resource"}, Password: strings.Repeat("a", 64)}
	if _, err := PrepareDatabaseCredentials(ctx, options, spec.Key); err == nil {
		t.Fatal("invalid credential preparation accepted")
	}
	if _, err := EnsureDatabase(ctx, options, spec); err == nil {
		t.Fatal("invalid identity accepted")
	}
	if err := DeleteDatabase(ctx, options, spec.Key); err == nil {
		t.Fatal("invalid delete accepted")
	}
	var result string
	if err := ReadRow(ctx, options, "SELECT private_value", []any{"private-argument"}, &result); err == nil {
		t.Fatal("invalid read accepted")
	}
	if _, err := DatabaseServerIdentity(ctx, options); err == nil {
		t.Fatal("invalid server accepted")
	}
	session := &provisionSession{ctx: ctx}
	if err := session.quarantine(databaseRecord{}, 0); err == nil {
		t.Fatal("invalid quarantine bound accepted")
	}
	for _, failure := range []error{ErrDatabaseBusy, context.Canceled, context.DeadlineExceeded} {
		_, done := databaseSignal(ctx, "ensure")
		done(failure)
		done(nil)
	}
	parentDone("success")
	runtime.Close()
	attr := func(attrs []*common.KeyValue, key string) string {
		for _, a := range attrs {
			if a.Key == key {
				return a.Value.GetStringValue()
			}
		}
		return ""
	}
	mu.Lock()
	defer mu.Unlock()
	spanIDs := map[string]string{}
	logIDs := map[string]string{}
	parents := map[string][]byte{}
	var root []byte
	var total uint64
	active := int64(-1)
	for _, message := range received {
		data, _ := proto.Marshal(message)
		if bytes.Contains(data, []byte("private-")) || bytes.Contains(data, []byte(spec.Password)) {
			t.Fatal("private input reached telemetry")
		}
		switch batch := message.(type) {
		case *traces.ExportTraceServiceRequest:
			for _, resource := range batch.ResourceSpans {
				for _, scope := range resource.ScopeSpans {
					for _, span := range scope.Spans {
						if scope.Scope.Name == "stego/database" {
							root = span.SpanId
							continue
						}
						if scope.Scope.Name != "stego/postgres-client" {
							continue
						}
						spanIDs[string(span.SpanId)] = attr(span.Attributes, "outcome")
						parents[string(span.SpanId)] = span.ParentSpanId
					}
				}
			}
		case *logs.ExportLogsServiceRequest:
			for _, resource := range batch.ResourceLogs {
				for _, scope := range resource.ScopeLogs {
					if scope.Scope.Name != "stego/postgres-client" {
						continue
					}
					for _, record := range scope.LogRecords {
						if record.EventName != "postgres.database.completed" {
							t.Fatal("wrong client event")
						}
						logIDs[string(record.SpanId)] = attr(record.Attributes, "outcome")
					}
				}
			}
		case *metrics.ExportMetricsServiceRequest:
			for _, resource := range batch.ResourceMetrics {
				for _, scope := range resource.ScopeMetrics {
					if scope.Scope.Name != "stego/postgres-client" {
						continue
					}
					for _, metric := range scope.Metrics {
						if metric.Name == "stego.postgres.database.duration" {
							total = 0
							for _, point := range metric.GetHistogram().DataPoints {
								total += point.Count
							}
						}
						if metric.Name == "stego.postgres.database.active" {
							for _, point := range metric.GetSum().DataPoints {
								active = point.GetAsInt()
							}
						}
					}
				}
			}
		}
	}
	if len(spanIDs) != 9 || len(logIDs) != 9 || total != 9 || active != 0 || len(root) != 8 {
		t.Fatal("missing or repeated client signals", len(spanIDs), len(logIDs), total, active)
	}
	outcomes := map[string]int{}
	for id, outcome := range spanIDs {
		if logIDs[id] != outcome || !bytes.Equal(parents[id], root) {
			t.Fatal("client signals lost correlation")
		}
		outcomes[outcome]++
	}
	if outcomes["failure"] != 6 || outcomes["busy"] != 1 || outcomes["canceled"] != 1 || outcomes["deadline"] != 1 {
		t.Fatal("wrong client outcomes", outcomes)
	}
}
