package otel

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	logcollector "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metriccollector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func value(attrs []*commonpb.KeyValue, key string) *commonpb.AnyValue {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value
		}
	}
	return nil
}

func metricMap(request *metriccollector.ExportMetricsServiceRequest) map[string]*metricpb.Metric {
	result := make(map[string]*metricpb.Metric)
	for _, resource := range request.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			for _, metric := range scope.Metrics {
				result[metric.Name] = metric
			}
		}
	}
	return result
}

func TestExportFailures(t *testing.T) {
	t.Run("ST-SUP-RP03-LC03-T02 The runtime counts export failures per signal", func(t *testing.T) {
		startCollector(t, true)
		t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "1000")
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		started := time.Now()
		for range QueueSize * 3 {
			_, finish := r.TraceRPC(context.Background(), "/sample.Records/Get")
			finish(nil)
		}
		if time.Since(started) > time.Second {
			t.Fatal("a blocked export delayed the calls")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := r.meter.ForceFlush(ctx); err == nil {
			t.Fatal("a blocked metric collector reported success")
		}
		waitFor(t, 3*time.Second, func() bool {
			return r.MetricExportFailures() > 0 && r.LogExportFailures() > 0 && r.ExportFailures() > 0
		})
	})
}

// signalRun is what one HTTP request and one RPC call, both denied and both with private data, gave the collector.
type signalRun struct {
	httpSpan, rpcSpan trace.SpanContext
	logs              *logcollector.ExportLogsServiceRequest
	metrics           *metriccollector.ExportMetricsServiceRequest
	sink              *collector
	instance          string
}

func serveDenied(t *testing.T, ratio string) signalRun {
	t.Helper()
	sink := startCollector(t, false)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", ratio)
	r, err := New("records")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	run := signalRun{sink: sink, instance: r.instance}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, req *http.Request) {
		run.httpSpan = trace.SpanContextFromContext(req.Context())
		http.Error(w, "private-error", http.StatusForbidden)
	})
	handler := r.Handler(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.Route(mux).ServeHTTP(w, req.WithContext(req.Context()))
	}))
	request := httptest.NewRequest("GET", "/records/private-record?secret=private-query", nil)
	request.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	request.Header.Set("authorization", "Bearer private-token")
	request.Header.Set("baggage", "value=private-baggage")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("traceparent", "00-33333333333333333333333333333333-4444444444444444-01", "authorization", "Bearer private-token", "baggage", "value=private-baggage"))
	ctx, finish := r.TraceRPC(ctx, "/sample.v1.Records/Get")
	run.rpcSpan = trace.SpanContextFromContext(ctx)
	finish(status.Error(codes.PermissionDenied, "private-error"))
	r.Close()
	run.logs = &logcollector.ExportLogsServiceRequest{}
	for count := 0; count < 2; {
		select {
		case batch := <-sink.logs:
			run.logs.ResourceLogs = append(run.logs.ResourceLogs, batch.ResourceLogs...)
			for _, resource := range batch.ResourceLogs {
				for _, scope := range resource.ScopeLogs {
					count += len(scope.LogRecords)
				}
			}
		case <-time.After(time.Second):
			t.Fatal("the logs were not exported")
		}
	}
	select {
	case run.metrics = <-sink.metrics:
	case <-time.After(time.Second):
		t.Fatal("the metrics were not exported")
	}
	return run
}

func TestRequestSignals(t *testing.T) {
	t.Run("ST-SUP-RP03-LC06-T01 The metrics count unsampled calls and link sampled ones", func(t *testing.T) {
		for _, ratio := range []string{"1", "0"} {
			run := serveDenied(t, ratio)
			metrics := metricMap(run.metrics)
			for _, name := range []string{"http.server.request.duration", "rpc.server.call.duration"} {
				histogram := metrics[name].GetHistogram()
				if histogram == nil || len(histogram.DataPoints) != 1 || histogram.DataPoints[0].Count != 1 {
					t.Fatalf("ratio %s: %s does not count the call once", ratio, name)
				}
				if ratio == "1" {
					expected := run.httpSpan
					if name == "rpc.server.call.duration" {
						expected = run.rpcSpan
					}
					found := false
					for _, exemplar := range histogram.DataPoints[0].Exemplars {
						if hex.EncodeToString(exemplar.TraceId) == expected.TraceID().String() && hex.EncodeToString(exemplar.SpanId) == expected.SpanID().String() {
							found = true
						}
					}
					if !found {
						t.Fatalf("%s has no exemplar of the call's span", name)
					}
				}
			}
			if ratio == "0" {
				select {
				case <-run.sink.traces:
					t.Fatal("a span was exported at ratio 0")
				default:
				}
			}
		}
	})
	t.Run("ST-SUP-RP03-LC06-T02 The runtime logs one correlated completion per call", func(t *testing.T) {
		run := serveDenied(t, "1")
		count := 0
		for _, resource := range run.logs.ResourceLogs {
			if len(resource.Resource.Attributes) != 2 || value(resource.Resource.Attributes, "service.instance.id").GetStringValue() != run.instance || value(resource.Resource.Attributes, "service.name").GetStringValue() != "records" {
				t.Fatal("the log lost the service identity")
			}
			for _, scope := range resource.ScopeLogs {
				for _, record := range scope.LogRecords {
					count++
					expected := run.httpSpan
					if record.EventName == "rpc.server.call.completed" {
						expected = run.rpcSpan
					} else if record.EventName != "http.server.request.completed" {
						t.Fatal("unexpected event name", record.EventName)
					}
					if hex.EncodeToString(record.TraceId) != expected.TraceID().String() || hex.EncodeToString(record.SpanId) != expected.SpanID().String() || record.SeverityText != "WARN" || len(record.Attributes) != 4 || record.TimeUnixNano == 0 || record.ObservedTimeUnixNano == 0 {
						t.Fatal("invalid correlated completion log", record)
					}
				}
			}
		}
		if count != 2 {
			t.Fatal("expected two completion logs, got", count)
		}
	})
	t.Run("ST-SUP-RP03-LC06-T03 The metrics and logs hold no private data", func(t *testing.T) {
		for _, ratio := range []string{"1", "0"} {
			run := serveDenied(t, ratio)
			logs, _ := proto.Marshal(run.logs)
			metrics, _ := proto.Marshal(run.metrics)
			for _, secret := range []string{"private-token", "private-query", "private-record", "private-baggage", "private-error"} {
				if strings.Contains(string(logs), secret) || strings.Contains(string(metrics), secret) {
					t.Fatalf("ratio %s: %s is in the exported logs or metrics", ratio, secret)
				}
			}
		}
	})
}
