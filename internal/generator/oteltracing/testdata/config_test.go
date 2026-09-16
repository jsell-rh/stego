package tracing

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	logs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	traces "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestTelemetryDeploymentFilesAndRotation(t *testing.T) {
	traceEnvironment(t)
	t.Setenv(telemetryTokenSetting, "")
	certificate := httptest.NewTLSServer(http.NotFoundHandler())
	defer certificate.Close()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate().Raw})
	dir := t.TempDir()
	ca, token := filepath.Join(dir, "ca"), filepath.Join(dir, "token")
	if err := os.WriteFile(ca, cert, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("first-collector-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example.test:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", ca)
	t.Setenv(telemetryTokenSetting, token)
	t.Setenv("OTEL_SERVICE_NAME", "worker")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.25")
	env, files, err := ExportEnvironment("browser", "/var/run/stego")
	if err != nil {
		t.Fatal(err)
	}
	if env["OTEL_SERVICE_NAME"] != "browser" || env["OTEL_EXPORTER_OTLP_CERTIFICATE"] != "/var/run/stego/otel-ca.pem" || env[telemetryTokenSetting] != "/var/run/stego/otel-token" || string(files["otel-token"]) != "first-collector-token" || string(files["otel-ca.pem"]) != string(cert) {
		t.Fatal("deployment lost its configuration")
	}
	encoded, _ := json.Marshal(env)
	if strings.Contains(string(encoded), "first-collector-token") || strings.Contains(string(encoded), dir) {
		t.Fatal("private source or token entered the environment")
	}
	if err := os.WriteFile(token, []byte("second-collector-token"), 0600); err != nil {
		t.Fatal(err)
	}
	_, next, err := ExportEnvironment("browser", "/var/run/stego")
	if err != nil || string(next["otel-token"]) != "second-collector-token" || string(files["otel-token"]) != "first-collector-token" {
		t.Fatal("rotation changed a captured value", err)
	}
	for _, mode := range []string{"private CA", "world token", "invalid token", "relative token", "bad endpoint", "bad port", "unsupported", "bad mount", "token without endpoint"} {
		t.Run(mode, func(t *testing.T) {
			if err := os.WriteFile(ca, cert, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(token, []byte("second-collector-token"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(token, 0600); err != nil {
				t.Fatal(err)
			}
			mount := "/var/run/stego"
			switch mode {
			case "private CA":
				if err := os.WriteFile(ca, append(cert, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("private-data")})...), 0600); err != nil {
					t.Fatal(err)
				}
			case "world token":
				if err := os.Chmod(token, 0644); err != nil {
					t.Fatal(err)
				}
			case "invalid token":
				if err := os.WriteFile(token, []byte("private-token\ninjected-header"), 0600); err != nil {
					t.Fatal(err)
				}
			case "relative token":
				t.Setenv(telemetryTokenSetting, "relative")
			case "bad endpoint":
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.example.test")
			case "bad port":
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example.test:65536")
			case "unsupported":
				t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "private-token")
			case "bad mount":
				mount = "/var/../escape"
			case "token without endpoint":
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			}
			env, files, err := ExportEnvironment("browser", mount)
			if err == nil || env != nil || files != nil {
				t.Fatal("invalid deployment accepted")
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("private value entered the error")
			}
		})
	}
}

func TestTelemetryCollectorTokenProtectsAllSignalsAndRotates(t *testing.T) {
	traceEnvironment(t)
	certificate := httptest.NewTLSServer(http.NotFoundHandler())
	defer certificate.Close()
	dir := t.TempDir()
	ca, token := filepath.Join(dir, "ca"), filepath.Join(dir, "token")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("first-collector-token"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	expected := "Bearer first-collector-token"
	seen := map[string]int{}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: certificate.TLS.Certificates})), grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get("authorization")
		if len(values) != 1 || values[0] != expected {
			return nil, status.Error(codes.Unauthenticated, "denied")
		}
		seen[info.FullMethod]++
		return handler(ctx, request)
	}))
	sink := &traceCollector{received: make(chan *traces.ExportTraceServiceRequest, 32), logs: make(chan *logs.ExportLogsServiceRequest, 64), metrics: make(chan *metrics.ExportMetricsServiceRequest, 64)}
	traces.RegisterTraceServiceServer(server, sink)
	logs.RegisterLogsServiceServer(server, &testLogCollector{sink: sink})
	metrics.RegisterMetricsServiceServer(server, &testMetricCollector{sink: sink})
	go server.Serve(listener)
	defer server.Stop()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", ca)
	t.Setenv(telemetryTokenSetting, token)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	runtime, err := newRuntime(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	export := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, span := runtime.tracer.Start(ctx, "verified operation")
		span.End()
		if err := runtime.provider.ForceFlush(ctx); err != nil {
			t.Fatal(err)
		}
		runtime.LogServiceEvent(ctx, RuntimeStarted)
		if err := runtime.signals.logs.ForceFlush(ctx); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		runtime.signals.httpStarted(ctx)
		runtime.signals.httpFinished(ctx, start, start.Add(time.Millisecond), "GET", "/records", 200)
		if err := runtime.signals.meter.ForceFlush(ctx); err != nil {
			t.Fatal(err)
		}
	}

	export()
	replacement := filepath.Join(dir, "next")
	if err := os.WriteFile(replacement, []byte("second-collector-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, token); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	expected = "Bearer second-collector-token"
	mu.Unlock()
	export()
	if err := os.Chmod(token, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := traces.NewTraceServiceClient(runtime.connection).Export(context.Background(), &traces.ExportTraceServiceRequest{}); err == nil {
		t.Fatal("unsafe token file exported a request")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, method := range []string{"/opentelemetry.proto.collector.trace.v1.TraceService/Export", "/opentelemetry.proto.collector.logs.v1.LogsService/Export", "/opentelemetry.proto.collector.metrics.v1.MetricsService/Export"} {
		if seen[method] < 2 {
			t.Fatal("signal did not use both tokens", method, seen[method])
		}
	}
}
