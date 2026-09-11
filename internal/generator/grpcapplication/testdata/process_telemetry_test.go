package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"net"
	"path/filepath"
	"sync"
	"testing"

	logs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	traces "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type processCollector struct {
	sync.Mutex
	batches []proto.Message
}

func (c *processCollector) store(p proto.Message) error {
	c.Lock()
	defer c.Unlock()
	if len(c.batches) >= 256 {
		return status.Error(codes.ResourceExhausted, "fixture full")
	}
	c.batches = append(c.batches, p)
	return nil
}

type processTraces struct {
	traces.UnimplementedTraceServiceServer
	c *processCollector
}

func (s *processTraces) Export(_ context.Context, p *traces.ExportTraceServiceRequest) (*traces.ExportTraceServiceResponse, error) {
	return &traces.ExportTraceServiceResponse{}, s.c.store(p)
}

type processLogs struct {
	logs.UnimplementedLogsServiceServer
	c *processCollector
}

func (s *processLogs) Export(_ context.Context, p *logs.ExportLogsServiceRequest) (*logs.ExportLogsServiceResponse, error) {
	return &logs.ExportLogsServiceResponse{}, s.c.store(p)
}

type processMetrics struct {
	metrics.UnimplementedMetricsServiceServer
	c *processCollector
}

func (s *processMetrics) Export(_ context.Context, p *metrics.ExportMetricsServiceRequest) (*metrics.ExportMetricsServiceResponse, error) {
	return &metrics.ExportMetricsServiceResponse{}, s.c.store(p)
}
func newProcessCollector(t *testing.T, dir string) (*processCollector, []string) {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.pem"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}})))
	collector := new(processCollector)
	traces.RegisterTraceServiceServer(server, &processTraces{c: collector})
	logs.RegisterLogsServiceServer(server, &processLogs{c: collector})
	metrics.RegisterMetricsServiceServer(server, &processMetrics{c: collector})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	return collector, []string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://" + listener.Addr().String(), "OTEL_EXPORTER_OTLP_CERTIFICATE=" + filepath.Join(dir, "tls.pem"), "OTEL_TRACES_SAMPLER_ARG=1", "OTEL_METRIC_EXPORT_INTERVAL=1000", "OTEL_LOGS_EXPORTER=otlp", "OTEL_METRICS_EXPORTER=otlp"}
}
func (c *processCollector) check(t *testing.T, tokens []string) {
	t.Helper()
	c.Lock()
	defer c.Unlock()
	spans, records := map[string]bool{}, map[string]bool{}
	var count uint64
	for _, batch := range c.batches {
		encoded, err := proto.Marshal(batch)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte("private-process")) {
			t.Fatal("private data entered process telemetry")
		}
		for _, token := range tokens {
			if bytes.Contains(encoded, []byte(token)) {
				t.Fatal("token entered process telemetry")
			}
		}
		switch value := batch.(type) {
		case *traces.ExportTraceServiceRequest:
			for _, resource := range value.ResourceSpans {
				for _, scope := range resource.ScopeSpans {
					for _, span := range scope.Spans {
						if span.Name == "sample.v1.Records/Echo" {
							spans[hex.EncodeToString(span.TraceId)+hex.EncodeToString(span.SpanId)] = true
						}
					}
				}
			}
		case *logs.ExportLogsServiceRequest:
			for _, resource := range value.ResourceLogs {
				for _, scope := range resource.ScopeLogs {
					for _, record := range scope.LogRecords {
						if record.EventName == "rpc.server.call.completed" && len(record.TraceId) == 16 && len(record.SpanId) == 8 {
							records[hex.EncodeToString(record.TraceId)+hex.EncodeToString(record.SpanId)] = true
						}
					}
				}
			}
		case *metrics.ExportMetricsServiceRequest:
			for _, resource := range value.ResourceMetrics {
				for _, scope := range resource.ScopeMetrics {
					for _, metric := range scope.Metrics {
						if metric.Name == "rpc.server.call.duration" {
							for _, point := range metric.GetHistogram().GetDataPoints() {
								count += point.Count
							}
						}
					}
				}
			}
		}
	}
	matched := false
	for key := range records {
		if spans[key] {
			matched = true
		}
	}
	if !matched || count == 0 {
		t.Fatal("process did not export correlated RPC logs, traces, and metrics")
	}
}
