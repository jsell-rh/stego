package tracing

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	logcollector "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metriccollector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func browserTrace() *tracecollector.ExportTraceServiceRequest {
	now := uint64(time.Now().UnixNano())
	return &tracecollector.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "private.resource", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "private"}}}}}, ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "private.scope"}, Spans: []*tracepb.Span{{Name: "workflow.create", TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8), StartTimeUnixNano: now, EndTimeUnixNano: now, TraceState: "private=value"}}}}}}}
}
func TestBrowserRelaySignalIdentity(t *testing.T) {
	collector := collectorFixture(t, false)
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx := runtime.Context(context.Background())
	send := func(signal string, message proto.Message) {
		t.Helper()
		data, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := RelayBrowserTelemetry(ctx, signal, data, "example-browser"); err != nil {
			t.Fatal(signal, err)
		}
	}
	send("traces", browserTrace())
	select {
	case batch := <-collector.received:
		r := batch.ResourceSpans[0]
		if len(r.Resource.Attributes) != 1 || r.Resource.Attributes[0].Key != "service.name" || r.Resource.Attributes[0].Value.GetStringValue() != "example-browser" || r.ScopeSpans[0].Scope.Name != "stego.browser" || r.ScopeSpans[0].Spans[0].TraceState != "" {
			t.Fatal("browser resource or vendor state escaped")
		}
	case <-time.After(time.Second):
		t.Fatal("trace was not delivered")
	}
	now := uint64(time.Now().UnixNano())
	send("logs", &logcollector.ExportLogsServiceRequest{ResourceLogs: []*logpb.ResourceLogs{{ScopeLogs: []*logpb.ScopeLogs{{LogRecords: []*logpb.LogRecord{{TimeUnixNano: now, Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "workflow.created"}}}}}}}}})
	select {
	case batch := <-collector.logs:
		if batch.ResourceLogs[0].Resource.Attributes[0].Value.GetStringValue() != "example-browser" {
			t.Fatal("log identity differs")
		}
	case <-time.After(time.Second):
		t.Fatal("log was not delivered")
	}
	send("metrics", &metriccollector.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: []*metricpb.Metric{{Name: "workflow.created", Data: &metricpb.Metric_Sum{Sum: &metricpb.Sum{DataPoints: []*metricpb.NumberDataPoint{{TimeUnixNano: now, Value: &metricpb.NumberDataPoint_AsInt{AsInt: 1}}}}}}}}}}}})
	select {
	case batch := <-collector.metrics:
		if batch.ResourceMetrics[0].Resource.Attributes[0].Value.GetStringValue() != "example-browser" {
			t.Fatal("metric identity differs")
		}
	case <-time.After(time.Second):
		t.Fatal("metric was not delivered")
	}
	for _, data := range [][]byte{nil, bytes.Repeat([]byte{1}, BrowserTelemetryMaxBytes+1), {255}, {0xF8, 0x07, 0x01}} {
		if err := RelayBrowserTelemetry(ctx, "traces", data, "example-browser"); !errors.Is(err, ErrBrowserTelemetryInput) {
			t.Fatal("invalid payload accepted", err)
		}
	}
	unknownKind := browserTrace()
	unknownKind.ResourceSpans[0].ScopeSpans[0].Spans[0].Kind = tracepb.Span_SpanKind(99)
	unknownData, _ := proto.Marshal(unknownKind)
	if !errors.Is(RelayBrowserTelemetry(ctx, "traces", unknownData, "example-browser"), ErrBrowserTelemetryInput) {
		t.Fatal("unknown span kind accepted")
	}
	invalid := browserTrace()
	invalid.ResourceSpans[0].ScopeSpans[0].Spans[0].TraceId = make([]byte, 16)
	data, _ := proto.Marshal(invalid)
	if !errors.Is(RelayBrowserTelemetry(ctx, "traces", data, "example-browser"), ErrBrowserTelemetryInput) {
		t.Fatal("zero trace ID accepted")
	}
}
func TestBrowserRelayCollectorDeadline(t *testing.T) {
	collectorFixture(t, true)
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	data, _ := proto.Marshal(browserTrace())
	start := time.Now()
	if !errors.Is(RelayBrowserTelemetry(runtime.Context(context.Background()), "traces", data, "example-browser"), ErrBrowserTelemetryUnavailable) || time.Since(start) > 3*time.Second {
		t.Fatal("collector deadline was not enforced")
	}
	if !errors.Is(RelayBrowserTelemetry(context.Background(), "traces", data, "example-browser"), ErrBrowserTelemetryUnavailable) {
		t.Fatal("relay ran without a runtime")
	}
}
