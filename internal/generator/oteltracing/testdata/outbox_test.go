package tracing

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestOutboxMetricsAggregation(t *testing.T) {
	sink := collectorFixture(t, false)
	runtime, err := newRuntime(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	outbox, err := runtime.OutboxTelemetry()
	if err != nil {
		t.Fatal(err)
	}
	detach, err := outbox.ObserveOutbox(func() OutboxStats {
		return OutboxStats{Delivered: 5, Retried: 2, DeliveryFailures: 1, DatabaseFailures: 3, LostLeases: 4, UnknownDestinations: 6}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	second, err := outbox.ObserveOutbox(func() OutboxStats { return OutboxStats{Delivered: 7, Retried: 1} })
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.signals.meter.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case batch := <-sink.metrics:
		for _, resource := range batch.ResourceMetrics {
			if value(resource.Resource.Attributes, "service.instance.id").GetStringValue() != runtime.instance {
				t.Fatal("outbox resource identity differs")
			}
			for _, scope := range resource.ScopeMetrics {
				for _, m := range scope.Metrics {
					if strings.HasPrefix(m.Name, "stego.outbox.") && scope.Scope.Name != "stego/outbox" {
						t.Fatal("outbox scope differs")
					}
				}
			}
		}
		metrics := metricMap(batch)
		for name, want := range map[string]int64{
			"stego.outbox.delivered":            12,
			"stego.outbox.retried":              3,
			"stego.outbox.delivery_failures":    1,
			"stego.outbox.database_failures":    3,
			"stego.outbox.lost_leases":          4,
			"stego.outbox.unknown_destinations": 6,
		} {
			metric := metrics[name]
			if metric == nil || metric.Unit != "{message}" {
				t.Fatal("outbox metric missing", name, metric)
			}
			points := metric.GetGauge().GetDataPoints()
			if len(points) != 1 || points[0].GetAsInt() != want || len(points[0].Attributes) != 0 {
				t.Fatal("outbox aggregate differs", name, metric)
			}
		}
	case <-ctx.Done():
		t.Fatal("outbox metrics did not arrive")
	}
}

func TestOutboxMetricsLimitAndRelease(t *testing.T) {
	traceEnvironment(t)
	runtime, err := newRuntime(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	outbox, err := runtime.OutboxTelemetry()
	if err != nil {
		t.Fatal(err)
	}
	var releases []func()
	for range 64 {
		release, err := outbox.ObserveOutbox(func() OutboxStats { return OutboxStats{} })
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := outbox.ObserveOutbox(func() OutboxStats { return OutboxStats{} }); err == nil {
		t.Fatal("unbounded outbox registration")
	}
	releases[0]()
	releases[0]()
	release, err := outbox.ObserveOutbox(func() OutboxStats { return OutboxStats{} })
	if err != nil {
		t.Fatal("outbox capacity was not released", err)
	}
	release()
	for _, release := range releases {
		release()
	}
	if _, err := outbox.ObserveOutbox(nil); err == nil {
		t.Fatal("nil outbox stats accepted")
	}
	if len(outbox.outboxes) != 0 {
		t.Fatal("detached outbox retained")
	}
}

func TestOutboxMetricsDisabled(t *testing.T) {
	collectorFixture(t, false)
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	runtime, err := newRuntime(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	outbox, err := runtime.OutboxTelemetry()
	if err != nil {
		t.Fatal(err)
	}
	if outbox == nil {
		t.Fatal("outbox telemetry missing")
	}
	detach, err := outbox.ObserveOutbox(func() OutboxStats { return OutboxStats{Delivered: 1} })
	if err != nil {
		t.Fatal(err)
	}
	detach()
}
