package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tracing "example.com/records/telemetry"
	"go.opentelemetry.io/otel/trace"
	logs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	traces "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

type runTraceSink struct {
	traces.UnimplementedTraceServiceServer
	batches chan *traces.ExportTraceServiceRequest
}

func (s *runTraceSink) Export(ctx context.Context, r *traces.ExportTraceServiceRequest) (*traces.ExportTraceServiceResponse, error) {
	select {
	case s.batches <- r:
		return &traces.ExportTraceServiceResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type runLogSink struct {
	logs.UnimplementedLogsServiceServer
	batches chan *logs.ExportLogsServiceRequest
}

func (s *runLogSink) Export(ctx context.Context, r *logs.ExportLogsServiceRequest) (*logs.ExportLogsServiceResponse, error) {
	select {
	case s.batches <- r:
		return &logs.ExportLogsServiceResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type runMetricSink struct {
	metrics.UnimplementedMetricsServiceServer
	batches chan *metrics.ExportMetricsServiceRequest
}

func (s *runMetricSink) Export(ctx context.Context, r *metrics.ExportMetricsServiceRequest) (*metrics.ExportMetricsServiceResponse, error) {
	select {
	case s.batches <- r:
		return &metrics.ExportMetricsServiceResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func runTelemetryCollector(t *testing.T) (*runTraceSink, *runLogSink, *runMetricSink) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") {
			t.Setenv(name, "")
		}
	}
	certificate := httptest.NewTLSServer(http.NotFoundHandler())
	defer certificate.Close()
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: certificate.TLS.Certificates})))
	ts := &runTraceSink{batches: make(chan *traces.ExportTraceServiceRequest, 32)}
	ls := &runLogSink{batches: make(chan *logs.ExportLogsServiceRequest, 32)}
	ms := &runMetricSink{batches: make(chan *metrics.ExportMetricsServiceRequest, 32)}
	traces.RegisterTraceServiceServer(server, ts)
	logs.RegisterLogsServiceServer(server, ls)
	metrics.RegisterMetricsServiceServer(server, ms)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", file)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "1000")
	return ts, ls, ms
}

func TestRunOwnsLogsMetricsAndIndependentTraces(t *testing.T) {
	for _, sample := range []string{"1", "0"} {
		t.Run(sample, func(t *testing.T) {
			ts, ls, ms := runTelemetryCollector(t)
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", sample)
			output, err := os.CreateTemp(t.TempDir(), "run-logs")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			saved := os.Stderr
			os.Stderr = output
			defer func() { os.Stderr = saved }()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var mu sync.Mutex
			roots := map[string]string{}
			children := map[string]string{}
			traceIDs := map[string]bool{}
			record := func(ctx context.Context, operation string) {
				sc := trace.SpanContextFromContext(ctx)
				mu.Lock()
				defer mu.Unlock()
				if !sc.IsValid() || sc.IsSampled() != (sample == "1") || traceIDs[sc.TraceID().String()] {
					t.Error("operation has no independent trace")
				}
				roots[sc.SpanID().String()] = "controller." + operation
				traceIDs[sc.TraceID().String()] = true
				child, _, finish := tracing.TraceClientHTTP(ctx, "GET")
				finish(200, "")
				cs := trace.SpanContextFromContext(child)
				if !cs.IsValid() || cs.TraceID() != sc.TraceID() || cs.SpanID() == sc.SpanID() {
					t.Error("provider call lost its parent")
				}
				children[cs.SpanID().String()] = sc.SpanID().String()
			}
			opened, scanned, actions := 0, 0, 0
			o := options()
			o.QueueCapacity = 2
			o.ReconcileTimeout = 4 * time.Second
			o.ResyncInterval = time.Hour
			source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
				opened++
				record(ctx, "watch")
				if opened == 1 {
					return nil, new(privateProcessError)
				}
				return idle[string](ctx)
			}, Scan: func(ctx context.Context, emit func(string) error) error {
				scanned++
				record(ctx, "scan")
				if scanned == 1 {
					return new(privateProcessError)
				}
				for range 5 {
					if err := emit("private-resource"); err != nil {
						return err
					}
				}
				<-ctx.Done()
				return ctx.Err()
			}}
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, source, func(ctx context.Context, _ string) error {
					actions++
					record(ctx, "reconcile")
					if actions == 1 {
						return new(privateProcessError)
					}
					if actions == 2 {
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return denied
				}, o)
			}()
			// Always join the controller before restoring shared test output or stopping
			// the collector, including a failed metric assertion.
			joined := false
			defer func() {
				if !joined {
					unblock()
					cancel()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("controller did not stop")
					}
				}
			}()
			allMetrics := map[string]*metricpb.Metric{}
			private := func(message proto.Message) {
				t.Helper()
				body, err := proto.Marshal(message)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(body, []byte("private-")) {
					t.Fatal("private callback data entered telemetry")
				}
			}
			takeMetrics := func(batch *metrics.ExportMetricsServiceRequest) {
				private(batch)
				for _, r := range batch.ResourceMetrics {
					for _, s := range r.ScopeMetrics {
						for _, m := range s.Metrics {
							allMetrics[m.Name] = m
						}
					}
				}
			}
			ready := false
			for !ready {
				select {
				case batch := <-ms.batches:
					takeMetrics(batch)
					want := map[string]int64{"running": 1, "capacity": 3, "queued": 2, "active": 1, "waiting": 1, "ready": 1, "retrying": 0}
					ready = true
					for name, value := range want {
						points := allMetrics["stego.controller.queue."+name].GetGauge().GetDataPoints()
						if len(points) != 1 || points[0].GetAsInt() != value {
							ready = false
						}
					}
				case <-ctx.Done():
					t.Fatal("Run did not export its bounded queue")
				case <-done:
					joined = true
					t.Fatal("Run stopped before queue observation")
				}
			}
			unblock()
			select {
			case err := <-done:
				joined = true
				if !errors.Is(err, denied) {
					t.Fatal("terminal action error changed")
				}
			case <-ctx.Done():
				t.Fatal("Run did not finish")
			}
			if opened != 3 || scanned != 2 || actions != 3 || controllerTelemetryOwner.runtime != nil || controllerTelemetryOwner.users != 0 {
				t.Fatal("Run changed recovery or retained the telemetry owner", opened, scanned, actions)
			}
			spans := map[string]*tracepb.Span{}
			for len(ts.batches) > 0 {
				batch := <-ts.batches
				private(batch)
				for _, r := range batch.ResourceSpans {
					for _, s := range r.ScopeSpans {
						for _, v := range s.Spans {
							id := hex.EncodeToString(v.SpanId)
							if spans[id] != nil {
								t.Fatal("duplicate span")
							}
							spans[id] = v
						}
					}
				}
			}
			if len(roots) != 8 || len(children) != 8 {
				t.Fatal("missing operation or provider context", len(roots), len(children))
			}
			if sample == "0" && len(spans) != 0 {
				t.Fatal("unsampled spans were exported")
			}
			if sample == "1" {
				if len(spans) != 16 {
					t.Fatal("Run did not flush every span", len(spans))
				}
				for id, name := range roots {
					v := spans[id]
					if v == nil || v.Name != name || len(v.ParentSpanId) != 0 || len(v.Links) != 0 {
						t.Fatal("operation is not an independent root")
					}
				}
				for id, parent := range children {
					v := spans[id]
					if v == nil || hex.EncodeToString(v.ParentSpanId) != parent {
						t.Fatal("provider span has the wrong parent")
					}
				}
			}
			pairs := map[string]bool{}
			for len(ls.batches) > 0 {
				batch := <-ls.batches
				private(batch)
				for _, r := range batch.ResourceLogs {
					for _, s := range r.ScopeLogs {
						for _, v := range s.LogRecords {
							if v.EventName == "controller.work.completed" {
								id := hex.EncodeToString(v.SpanId)
								if roots[id] == "" || pairs[id] {
									t.Fatal("work log has no unique operation span")
								}
								pairs[id] = true
							}
						}
					}
				}
			}
			if len(pairs) != 8 {
				t.Fatal("Run did not flush paired work logs", len(pairs))
			}
			for len(ms.batches) > 0 {
				takeMetrics(<-ms.batches)
			}
			var measured uint64
			for _, point := range allMetrics["stego.controller.work.duration"].GetHistogram().GetDataPoints() {
				measured += point.Count
			}
			if measured != 8 {
				t.Fatal("Run work metrics are incomplete", measured)
			}
			body, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(body, []byte("private-")) {
				t.Fatal("local log exposed a callback error or resource")
			}
			local := 0
			for _, line := range bytes.Split(body, []byte{'\n'}) {
				var row map[string]any
				if json.Unmarshal(line, &row) == nil && row["event.name"] == "controller.work.completed" {
					local++
				}
			}
			if local != 8 {
				t.Fatal("local work logs are incomplete", local)
			}
		})
	}
}

func TestRunTelemetryFailureStopsBeforeSource(t *testing.T) {
	for _, mode := range []string{"invalid exporter", "queue limit"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			if mode == "invalid exporter" {
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "ftp://invalid.example.test")
			}
			ctx := context.Background()
			if mode == "queue limit" {
				owner, closeOwner, err := startControllerTelemetry(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer closeOwner()
				for range 64 {
					detach, err := attachControllerQueue(owner, func() QueueMetrics { return QueueMetrics{} })
					if err != nil {
						t.Fatal(err)
					}
					defer detach()
				}
			}
			opened := 0
			source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) { opened++; return idle[string](ctx) }, Scan: func(context.Context, func(string) error) error { return nil }}
			run, stop := context.WithTimeout(ctx, time.Second)
			defer stop()
			err := Run(run, source, func(context.Context, string) error { return nil }, options())
			if err == nil || opened != 0 {
				t.Fatal("invalid telemetry opened a source", opened)
			}
			if mode == "queue limit" && !errors.Is(err, ErrTelemetryInUse) {
				t.Fatal("queue registration failure was lost")
			}
		})
	}
}
