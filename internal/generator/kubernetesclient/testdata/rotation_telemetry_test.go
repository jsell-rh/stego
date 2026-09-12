package kubernetes

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"example.com/widget/out/tracing"
	logs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	traces "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

const rotationTelemetryEnabled = true

type rotationSignals struct {
	traces, logs, metrics atomic.Int64
	private               atomic.Bool
	forbidden             [][]byte
}

func (s *rotationSignals) check(message proto.Message) {
	data, err := proto.Marshal(message)
	if err != nil {
		s.private.Store(true)
		return
	}
	for _, value := range s.forbidden {
		if len(value) > 0 && bytes.Contains(data, value) {
			s.private.Store(true)
		}
	}
}

type rotationTraces struct {
	traces.UnimplementedTraceServiceServer
	signals *rotationSignals
}

func (c *rotationTraces) Export(_ context.Context, r *traces.ExportTraceServiceRequest) (*traces.ExportTraceServiceResponse, error) {
	c.signals.check(r)
	for _, resource := range r.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			c.signals.traces.Add(int64(len(scope.Spans)))
		}
	}
	return &traces.ExportTraceServiceResponse{}, nil
}

type rotationLogs struct {
	logs.UnimplementedLogsServiceServer
	signals *rotationSignals
}

func (c *rotationLogs) Export(_ context.Context, r *logs.ExportLogsServiceRequest) (*logs.ExportLogsServiceResponse, error) {
	c.signals.check(r)
	for _, resource := range r.ResourceLogs {
		for _, scope := range resource.ScopeLogs {
			c.signals.logs.Add(int64(len(scope.LogRecords)))
		}
	}
	return &logs.ExportLogsServiceResponse{}, nil
}

type rotationMetrics struct {
	metrics.UnimplementedMetricsServiceServer
	signals *rotationSignals
}

func (c *rotationMetrics) Export(_ context.Context, r *metrics.ExportMetricsServiceRequest) (*metrics.ExportMetricsServiceResponse, error) {
	c.signals.check(r)
	for _, resource := range r.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			c.signals.metrics.Add(int64(len(scope.Metrics)))
		}
	}
	return &metrics.ExportMetricsServiceResponse{}, nil
}

// The collector retains counters only. It cannot fill a queue while the live
// rotation check waits. All three signal paths use the generated TLS exporter.
func rotationTelemetry(t *testing.T, ctx context.Context) (context.Context, func()) {
	t.Helper()
	certificate := httptest.NewTLSServer(nil)
	defer certificate.Close()
	ca := filepath.Join(t.TempDir(), "collector-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate().Raw}), 0600); err != nil {
		t.Fatal("cannot prepare collector trust")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot start collector listener")
	}
	projectedToken, err := readToken("/var/run/stego-kubernetes/token")
	if err != nil || len(projectedToken) < 32 {
		t.Fatal("cannot read the private comparison value")
	}
	signals := &rotationSignals{forbidden: [][]byte{[]byte("Bearer "), []byte(projectedToken[:32]), []byte(os.Getenv("STEGO_TEST_NAMESPACE")), []byte(os.Getenv("STEGO_TEST_POD_UID"))}}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(1<<20), grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: certificate.TLS.Certificates})))
	traces.RegisterTraceServiceServer(server, &rotationTraces{signals: signals})
	logs.RegisterLogsServiceServer(server, &rotationLogs{signals: signals})
	metrics.RegisterMetricsServiceServer(server, &rotationMetrics{signals: signals})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", ca)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "10000")
	runtime, err := tracing.NewRuntime()
	if err != nil {
		t.Fatal("cannot start generated telemetry")
	}
	t.Cleanup(runtime.Close)
	return runtime.Context(ctx), func() {
		runtime.Close()
		if signals.traces.Load() < 2 || signals.logs.Load() < 2 || signals.metrics.Load() < 1 || signals.private.Load() {
			t.Fatal("rotation did not prove private, complete telemetry export")
		}
		t.Log("Rotation requests exported logs, metrics, and traces without the selected private values")
	}
}
