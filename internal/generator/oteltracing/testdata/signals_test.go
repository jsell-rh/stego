package tracing

import (
	"context"
	"encoding/hex"
	"fmt"
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

type testLogCollector struct {
	logcollector.UnimplementedLogsServiceServer
	sink *traceCollector
}

func (c *testLogCollector) Export(ctx context.Context, request *logcollector.ExportLogsServiceRequest) (*logcollector.ExportLogsServiceResponse, error) {
	if c.sink.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case c.sink.logs <- request:
		return &logcollector.ExportLogsServiceResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type testMetricCollector struct {
	metriccollector.UnimplementedMetricsServiceServer
	sink *traceCollector
}

func (c *testMetricCollector) Export(ctx context.Context, request *metriccollector.ExportMetricsServiceRequest) (*metriccollector.ExportMetricsServiceResponse, error) {
	if c.sink.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case c.sink.metrics <- request:
		return &metriccollector.ExportMetricsServiceResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
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
func TestRequestSignalsWithAndWithoutSampledSpans(t *testing.T) {
	for _, sample := range []string{"1", "0"} {
		t.Run(sample, func(t *testing.T) {
			sink := collectorFixture(t, false)
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", sample)
			runtime, err := NewRuntime()
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			var httpSpan, rpcSpan trace.SpanContext
			mux := http.NewServeMux()
			mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, r *http.Request) {
				httpSpan = trace.SpanContextFromContext(r.Context())
				w.WriteHeader(403)
			})
			handler := runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				runtime.Route(mux).ServeHTTP(w, r.WithContext(r.Context()))
			}))
			request := httptest.NewRequest("GET", "/records/private-record?secret=private-query", nil)
			request.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
			request.Header.Set("authorization", "Bearer private-token")
			handler.ServeHTTP(httptest.NewRecorder(), request)
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("traceparent", "00-33333333333333333333333333333333-4444444444444444-01", "authorization", "Bearer private-token", "baggage", "value=private-baggage"))
			ctx, finish := runtime.TraceRPC(ctx, "/sample.v1.Records/Get")
			rpcSpan = trace.SpanContextFromContext(ctx)
			finish(status.Error(codes.PermissionDenied, "private-error"))
			runtime.Close()
			logRequest := &logcollector.ExportLogsServiceRequest{}
			logCount := 0
			for logCount < 2 {
				select {
				case batch := <-sink.logs:
					logRequest.ResourceLogs = append(logRequest.ResourceLogs, batch.ResourceLogs...)
					for _, resource := range batch.ResourceLogs {
						for _, scope := range resource.ScopeLogs {
							logCount += len(scope.LogRecords)
						}
					}
				case <-time.After(time.Second):
					t.Fatal("logs not exported")
				}
			}
			{
				count := 0
				for _, resource := range logRequest.ResourceLogs {
					if len(resource.Resource.Attributes) != 2 || value(resource.Resource.Attributes, "service.instance.id").GetStringValue() != runtime.instance || value(resource.Resource.Attributes, "service.name").GetStringValue() != "records" {
						t.Fatal("log service identity lost")
					}
					for _, scope := range resource.ScopeLogs {
						for _, record := range scope.LogRecords {
							count++
							expected := httpSpan
							if record.EventName == "rpc.server.call.completed" {
								expected = rpcSpan
							} else if record.EventName != "http.server.request.completed" {
								t.Fatal("unexpected event name")
							}
							if hex.EncodeToString(record.TraceId) != expected.TraceID().String() || hex.EncodeToString(record.SpanId) != expected.SpanID().String() || record.SeverityText != "WARN" || len(record.Attributes) != 4 || record.TimeUnixNano == 0 || record.ObservedTimeUnixNano == 0 {
								t.Fatal("invalid correlated request log", record)
							}
						}
					}
				}
				if count != 2 {
					t.Fatal("request logs lost", count)
				}
				raw, _ := proto.Marshal(logRequest)
				for _, secret := range []string{"private-token", "private-query", "private-record", "private-baggage", "private-error"} {
					if strings.Contains(string(raw), secret) {
						t.Fatal("private log data")
					}
				}
			}
			select {
			case request := <-sink.metrics:
				metrics := metricMap(request)
				for _, name := range []string{"http.server.request.duration", "rpc.server.call.duration"} {
					metric := metrics[name]
					hist := metric.GetHistogram()
					if metric.Unit != "s" || hist.AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE || len(hist.DataPoints) != 1 || hist.DataPoints[0].Count != 1 || len(hist.DataPoints[0].Attributes) != 3 {
						t.Fatal("invalid duration metric", metric)
					}
					if sample == "1" {
						expected := httpSpan
						if name == "rpc.server.call.duration" {
							expected = rpcSpan
						}
						found := false
						for _, exemplar := range hist.DataPoints[0].Exemplars {
							if hex.EncodeToString(exemplar.TraceId) == expected.TraceID().String() && hex.EncodeToString(exemplar.SpanId) == expected.SpanID().String() {
								found = true
							}
						}
						if !found {
							t.Fatal("metric lost trace exemplar")
						}
					}
				}
				for _, name := range []string{"http.server.active_requests", "rpc.server.active_requests"} {
					points := metrics[name].GetSum().GetDataPoints()
					if len(points) != 1 || points[0].GetAsInt() != 0 || len(points[0].Attributes) != 0 {
						t.Fatal("active request count did not return to zero")
					}
				}
				raw, _ := proto.Marshal(request)
				if strings.Contains(string(raw), "private-") {
					t.Fatal("private metric data")
				}
			case <-time.After(time.Second):
				t.Fatal("metrics not exported")
			}
			if sample == "0" {
				select {
				case <-sink.received:
					t.Fatal("local sampling was bypassed")
				default:
				}
			}
		})
	}
}
func TestMetricCardinalityAndActiveCalls(t *testing.T) {
	sink := collectorFixture(t, false)
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	_, finish := runtime.TraceRPC(context.Background(), "/sample.Records/Watch")
	flush := func() *metriccollector.ExportMetricsServiceRequest {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := runtime.signals.meter.ForceFlush(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case request := <-sink.metrics:
			return request
		case <-ctx.Done():
			t.Fatal("metrics not received")
			return nil
		}
	}
	points := metricMap(flush())["rpc.server.active_requests"].GetSum().GetDataPoints()
	if len(points) != 1 || points[0].GetAsInt() != 1 {
		t.Fatal("active stream was not counted")
	}
	finish(nil)
	for cycle := 0; cycle < 3; cycle++ {
		for i := 0; i < MetricSeriesLimit*2; i++ {
			_, end := runtime.TraceRPC(context.Background(), fmt.Sprintf("/sample.Records/Method%d", cycle*MetricSeriesLimit*2+i))
			end(nil)
		}
		metrics := metricMap(flush())
		hist := metrics["rpc.server.call.duration"].GetHistogram()
		if len(hist.DataPoints) > MetricSeriesLimit {
			t.Fatal("metric series exceeded bound")
		}
		var total uint64
		overflow := false
		for _, point := range hist.DataPoints {
			total += point.Count
			if value(point.Attributes, "otel.metric.overflow").GetBoolValue() {
				overflow = true
			}
		}
		if total != uint64(1+(cycle+1)*MetricSeriesLimit*2) || !overflow {
			t.Fatal("overflow lost request counts", total, overflow)
		}
	}
	if metricMap(flush())["rpc.server.active_requests"].GetSum().DataPoints[0].GetAsInt() != 0 {
		t.Fatal("active count lost completion")
	}
}
func TestSignalSettingsAndBlockedCollectors(t *testing.T) {
	collectorFixture(t, true)
	for name, values := range map[string][]string{"OTEL_METRICS_EXPORTER": {"prometheus", "invalid"}, "OTEL_LOGS_EXPORTER": {"console", "invalid"}, "OTEL_METRIC_EXPORT_INTERVAL": {"0", "999", "60001", "+1000", "invalid"}} {
		for _, value := range values {
			t.Setenv(name, value)
			if _, err := NewRuntime(); err == nil {
				t.Fatalf("accepted %s=%s", name, value)
			}
		}
		t.Setenv(name, "")
	}
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "1000")
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	started := time.Now()
	for range QueueSize * 3 {
		_, finish := runtime.TraceRPC(context.Background(), "/sample.Records/Get")
		finish(nil)
	}
	if time.Since(started) > time.Second {
		t.Fatal("blocked export delayed requests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := runtime.signals.meter.ForceFlush(ctx); err == nil {
		t.Fatal("blocked metric collector reported success")
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.MetricExportFailures() == 0 || runtime.LogExportFailures() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("signal export deadline failed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started = time.Now()
	runtime.Close()
	if time.Since(started) > ShutdownTimeout+time.Second {
		t.Fatal("signal shutdown budgets accumulated")
	}
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	disabled, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer disabled.Close()
	if disabled.signals.meter != nil || disabled.signals.logs != nil {
		t.Fatal("disabled signal started provider")
	}
}

func TestHTTPPanicBalancesMetricsWithoutInventingAResponse(t *testing.T) {
	sink := collectorFixture(t, false)
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	handler := runtime.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("private-panic") }))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("instrumentation swallowed panic")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/private-resource", nil))
	}()
	runtime.Close()
	select {
	case request := <-sink.metrics:
		metrics := metricMap(request)
		point := metrics["http.server.request.duration"].GetHistogram().DataPoints[0]
		if value(point.Attributes, "http.response.status_code") != nil || value(point.Attributes, "error.type").GetStringValue() != "panic" || point.Count != 1 {
			t.Fatal("panic recorded a false HTTP response")
		}
		if metrics["http.server.active_requests"].GetSum().DataPoints[0].GetAsInt() != 0 {
			t.Fatal("panic left an active request")
		}
	case <-time.After(time.Second):
		t.Fatal("panic metrics missing")
	}
	select {
	case request := <-sink.logs:
		record := request.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
		if record.SeverityText != "ERROR" || value(record.Attributes, "error.type").GetStringValue() != "panic" {
			t.Fatal("panic log status lost")
		}
		raw, _ := proto.Marshal(request)
		if strings.Contains(string(raw), "private-") {
			t.Fatal("panic log contains private data")
		}
	case <-time.After(time.Second):
		t.Fatal("panic log missing")
	}
}
func BenchmarkRequestSignals(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprint(enabled), func(b *testing.B) {
			sink := collectorFixture(b, false)
			if !enabled {
				b.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			}
			runtime, err := NewRuntime()
			if err != nil {
				b.Fatal(err)
			}
			defer runtime.Close()
			done := make(chan struct{})
			defer close(done)
			go func() {
				for {
					select {
					case <-sink.received:
					case <-sink.metrics:
					case <-sink.logs:
					case <-done:
						return
					}
				}
			}()
			handler := runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.Pattern = "GET /records/{id}"; w.WriteHeader(204) }))
			request := httptest.NewRequest("GET", "/records/id", nil)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				handler.ServeHTTP(httptest.NewRecorder(), request)
			}
			b.StopTimer()
		})
	}
}
