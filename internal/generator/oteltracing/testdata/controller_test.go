package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestControllerSignalsAndQueueAggregation(t *testing.T) {
	for _, sample := range []string{"1", "0"} {
		t.Run(sample, func(t *testing.T) { testControllerSignals(t, sample) })
	}
}
func testControllerSignals(t *testing.T, sample string) {
	sink := collectorFixture(t, false)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", sample)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "1000")
	var output bytes.Buffer
	runtime, err := newRuntime(&output)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	controller, err := runtime.ControllerTelemetry()
	if err != nil {
		t.Fatal(err)
	}
	detach, err := controller.Attach(func() ControllerQueue {
		return ControllerQueue{Capacity: 8, Queued: 3, Active: 2, Retrying: 1, Waiting: 4, Ready: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := controller.Attach(func() ControllerQueue { return ControllerQueue{Capacity: 2, Queued: 1} })
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	defer second()
	ctx, finish := controller.Begin(context.Background(), "reconcile")
	id := trace.SpanContextFromContext(ctx).TraceID().String()
	finish(errors.New("private-provider-credential"), true)
	finish(nil, false)
	controller.Event(ctx, "watch_started")
	controller.Event(ctx, "private-event")
	_, unknown := controller.Begin(ctx, "private-operation")
	unknown(nil, false)
	var span *tracepb.Span
	var record *logpb.LogRecord
	var metrics map[string]*metricpb.Metric
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	check := func(message proto.Message) {
		t.Helper()
		data, _ := proto.Marshal(message)
		if bytes.Contains(data, []byte("private-")) {
			t.Fatal("controller telemetry exposed private data")
		}
	}
	for (sample == "1" && span == nil) || record == nil || metrics == nil {
		select {
		case batch := <-sink.received:
			check(batch)
			for _, resource := range batch.ResourceSpans {
				for _, scope := range resource.ScopeSpans {
					for _, item := range scope.Spans {
						if hex.EncodeToString(item.TraceId) == id {
							if span != nil {
								t.Fatal("duplicate work span")
							}
							span = item
						}
					}
				}
			}
		case batch := <-sink.logs:
			check(batch)
			for _, resource := range batch.ResourceLogs {
				for _, scope := range resource.ScopeLogs {
					for _, item := range scope.LogRecords {
						if item.EventName == "controller.work.completed" {
							if record != nil {
								t.Fatal("duplicate work log")
							}
							record = item
						}
					}
				}
			}
		case batch := <-sink.metrics:
			check(batch)
			metrics = metricMap(batch)
		case <-timer.C:
			t.Fatal("controller signals did not arrive")
		}
	}
	if (sample == "1" && !bytes.Equal(record.SpanId, span.SpanId)) || value(record.Attributes, "outcome").GetStringValue() != "failure" || !value(record.Attributes, "retry").GetBoolValue() || value(record.Attributes, "duration_seconds").GetDoubleValue() < 0 {
		t.Fatal("controller log correlation failed")
	}
	for name, want := range map[string]int64{"running": 2, "capacity": 10, "queued": 4, "active": 2, "retrying": 1, "waiting": 4, "ready": 1} {
		metric := metrics["stego.controller.queue."+name]
		points := metric.GetGauge().GetDataPoints()
		if len(points) != 1 || points[0].GetAsInt() != want || len(points[0].Attributes) != 0 {
			t.Fatal("queue aggregate differs", name, metric)
		}
	}
	histogram := metrics["stego.controller.work.duration"].GetHistogram().GetDataPoints()
	if len(histogram) != 1 || histogram[0].Count != 1 || value(histogram[0].Attributes, "operation").GetStringValue() != "reconcile" {
		t.Fatal("controller duration differs")
	}
	retries := metrics["stego.controller.retries"].GetSum().GetDataPoints()
	if len(retries) != 1 || retries[0].GetAsInt() != 1 {
		t.Fatal("retry count differs")
	}
	detach()
	second()
	runtime.Close()
	if strings.Contains(output.String(), "private-") || !strings.Contains(output.String(), `"outcome":"failure"`) {
		t.Fatal("local controller logs are unsafe or absent")
	}
}

func TestControllerQueueLimitAndRelease(t *testing.T) {
	traceEnvironment(t)
	runtime, err := newRuntime(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	controller, err := runtime.ControllerTelemetry()
	if err != nil {
		t.Fatal(err)
	}
	var releases []func()
	for i := 0; i < 64; i++ {
		release, err := controller.Attach(func() ControllerQueue { return ControllerQueue{} })
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := controller.Attach(func() ControllerQueue { return ControllerQueue{} }); err == nil {
		t.Fatal("unbounded queue registration")
	}
	releases[0]()
	releases[0]()
	release, err := controller.Attach(func() ControllerQueue { return ControllerQueue{} })
	if err != nil {
		t.Fatal("queue capacity was not released", err)
	}
	release()
	for _, release := range releases {
		release()
	}
	if len(controller.queues) != 0 {
		t.Fatal("detached queue retained")
	}
}

func TestControllerWorkOutcomes(t *testing.T) {
	traceEnvironment(t)
	var output bytes.Buffer
	runtime, err := newRuntime(&output)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	controller, err := runtime.ControllerTelemetry()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		err               error
		retry             bool
		outcome, severity string
	}{
		{nil, true, "success", "INFO"},
		{errors.New("private-error"), true, "failure", "WARN"},
		{errors.New("private-error"), false, "failure", "ERROR"},
		{context.DeadlineExceeded, true, "timeout", "WARN"},
		{context.Canceled, false, "canceled", "INFO"},
	}
	for _, item := range cases {
		_, finish := controller.Begin(context.Background(), "reconcile")
		finish(item.err, item.retry)
	}
	runtime.Close()
	decoder := json.NewDecoder(&output)
	for _, item := range cases {
		var record localRecord
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record.Outcome != item.outcome || record.Severity != item.severity || record.Retry != (item.retry && item.err != nil) {
			t.Fatal("incorrect fixed work outcome", record)
		}
	}
	if _, err := runtime.ControllerTelemetry(); err == nil {
		t.Fatal("closed runtime created controller telemetry")
	}
}

func BenchmarkControllerSignals(b *testing.B) {
	for _, mode := range []string{"local", "otlp"} {
		b.Run(mode, func(b *testing.B) {
			var stop chan struct{}
			if mode == "local" {
				traceEnvironment(b)
			} else {
				sink := collectorFixture(b, false)
				stop = make(chan struct{})
				go func() {
					for {
						select {
						case <-sink.received:
						case <-sink.logs:
						case <-sink.metrics:
						case <-stop:
							return
						}
					}
				}()
				defer close(stop)
			}
			runtime, err := newRuntime(io.Discard)
			if err != nil {
				b.Fatal(err)
			}
			defer runtime.Close()
			controller, err := runtime.ControllerTelemetry()
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, finish := controller.Begin(context.Background(), "reconcile")
				finish(nil, false)
			}
			b.StopTimer()
			b.ReportMetric(float64(runtime.LocalLogDrops())/float64(b.N), "local_drops/op")
		})
	}
}
