package probe

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"stego.dev/runtime/otel"
)

// The fixed bucket boundaries of a domain histogram, by unit (ST-SUP-RP12-LC15). Those of s are the boundaries of
// the request duration metrics of ST-SUP-RP03-LC11.
var boundaries = map[Unit][]float64{
	Seconds:      {0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 1800},
	Milliseconds: {1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000},
	Bytes:        {64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864},
	One:          {1, 2, 5, 10, 20, 50, 100, 200, 500, 1000},
}

// scope is the instrumentation scope of every probe signal.
const scope = "stego/probe"

// otlpSink sends records through the providers of the telemetry runtime, and opens no provider, exporter or
// connection of its own (ST-SUP-RP12-LC14). Its goroutine is the only user of the instruments, so they need no
// lock. The temporality is the telemetry runtime's, which is cumulative.
type otlpSink struct {
	queue       *queue
	logger      otellog.Logger
	meter       metric.Meter
	counters    map[*Observation]metric.Int64Counter
	histograms  map[*Observation]metric.Float64Histogram
	instruments map[string]bool
}

// newOTLPSink takes the logger and the meter of the telemetry runtime, either of which may be off, and registers
// the counters domain.probe.dropped and domain.probe.failed, labelled by sink only (ST-SUP-RP12-LC19).
func newOTLPSink(telemetry *otel.Runtime, probe *core, console *consoleSink) (*otlpSink, error) {
	s := &otlpSink{counters: map[*Observation]metric.Int64Counter{}, histograms: map[*Observation]metric.Float64Histogram{}, instruments: map[string]bool{}}
	s.queue = newQueue(s.write)
	if provider := telemetry.LoggerProvider(); provider != nil {
		s.logger = provider.Logger(scope)
	}
	if provider := telemetry.MeterProvider(); provider != nil {
		s.meter = provider.Meter(scope)
		failed := errors.New("probe: cannot create the delivery counters")
		dropped, err := s.meter.Int64ObservableCounter("domain.probe.dropped", metric.WithUnit("{record}"))
		if err != nil {
			s.queue.drain(context.Background())
			return nil, failed
		}
		failures, err := s.meter.Int64ObservableCounter("domain.probe.failed", metric.WithUnit("{record}"))
		if err != nil {
			s.queue.drain(context.Background())
			return nil, failed
		}
		probeSet := metric.WithAttributes(attribute.String("sink", sinkProbe))
		otlpSet := metric.WithAttributes(attribute.String("sink", sinkOTLP))
		consoleSet := metric.WithAttributes(attribute.String("sink", sinkConsole))
		if _, err := s.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
			o.ObserveInt64(dropped, int64(probe.dropped.Load()), probeSet)
			o.ObserveInt64(dropped, int64(s.queue.dropped.Load()), otlpSet)
			o.ObserveInt64(dropped, int64(console.queue.dropped.Load()), consoleSet)
			o.ObserveInt64(failures, int64(s.queue.failed.Load()), otlpSet)
			o.ObserveInt64(failures, int64(console.queue.failed.Load()), consoleSet)
			return nil
		}, dropped, failures); err != nil {
			s.queue.drain(context.Background())
			return nil, failed
		}
	}
	return s, nil
}

// send adds the span event of an event to its bound span when the span is recording, at once, because the span ends
// with its call; the log record and the metric go through the queue (ST-SUP-RP12-LC04, ST-SUP-RP12-LC08).
func (s *otlpSink) send(r *record) {
	if r.obs.kind == Event && r.obs.class == declared && r.span != nil && r.span.IsRecording() {
		r.span.AddEvent(r.obs.signal, trace.WithTimestamp(r.time), trace.WithAttributes(fieldAttributes(r, true)...))
	}
	s.queue.put(r)
}

// write sends one record: an event as one log record with no body, a counter or a histogram as one metric value
// (ST-SUP-RP12-LC04, ST-SUP-RP12-LC05).
func (s *otlpSink) write(r *record) error {
	switch r.obs.kind {
	case Event:
		if s.logger == nil {
			return nil
		}
		var entry otellog.Record
		entry.SetTimestamp(r.time)
		entry.SetObservedTimestamp(r.time)
		entry.SetEventName(r.obs.signal)
		entry.SetSeverity(logSeverity(r.obs.severity))
		entry.SetSeverityText(r.obs.severity.String())
		entry.AddAttributes(logAttributes(r)...)
		ctx := context.Background()
		if r.spanCtx.IsValid() {
			ctx = trace.ContextWithSpanContext(ctx, r.spanCtx)
		}
		s.logger.Emit(ctx, entry)
	case Counter:
		if s.meter == nil {
			return nil
		}
		counter, ok := s.counters[r.obs]
		if !ok {
			var err error
			if s.instruments[r.obs.signal] {
				return errors.New("probe: two observations have one signal name")
			}
			if counter, err = s.meter.Int64Counter(r.obs.signal); err != nil {
				return err
			}
			s.counters[r.obs] = counter
			s.instruments[r.obs.signal] = true
		}
		counter.Add(context.Background(), r.count, metric.WithAttributes(metricAttributes(r)...))
	case Histogram:
		if s.meter == nil {
			return nil
		}
		histogram, ok := s.histograms[r.obs]
		if !ok {
			var err error
			if s.instruments[r.obs.signal] {
				return errors.New("probe: two observations have one signal name")
			}
			if histogram, err = s.meter.Float64Histogram(r.obs.signal, metric.WithUnit(string(r.obs.unit)), metric.WithExplicitBucketBoundaries(boundaries[r.obs.unit]...)); err != nil {
				return err
			}
			s.histograms[r.obs] = histogram
			s.instruments[r.obs.signal] = true
		}
		histogram.Record(context.Background(), r.number, metric.WithAttributes(metricAttributes(r)...))
	}
	return nil
}

// fieldAttributes are the declared fields of the record and, when its observation counts each attempt, its
// stego.outcome: all a span event carries (ST-SUP-RP12-LC13).
func fieldAttributes(r *record, outcome bool) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(r.values)+4)
	if outcome && r.outcome != "" {
		attrs = append(attrs, attribute.String("stego.outcome", r.outcome))
	}
	for i, f := range r.obs.fields {
		attrs = append(attrs, valueAttribute(f.name, r.values[i]))
	}
	return attrs
}

// logAttributes are the correlation of ST-SUP-RP12-LC03, first, then stego.outcome and the declared fields. The
// trace ID and the span ID are the record's own, from its context.
func logAttributes(r *record) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(r.values)+4)
	if r.obs.class == declared {
		attrs = append(attrs, attribute.String("tenant", r.tenant), attribute.String("principal", r.principal), attribute.String("operation", r.operation))
	}
	return append(attrs, fieldAttributes(r, true)...)
}

// metricAttributes are the declared fields, stego.outcome and the operation of a declared observation, and never a
// tenant, a principal, a trace ID or a span ID (ST-SUP-RP12-LC13). A built-in metric carries its fields only.
func metricAttributes(r *record) []attribute.KeyValue {
	attrs := fieldAttributes(r, true)
	if r.obs.class == declared {
		attrs = append(attrs, attribute.String("operation", r.operation))
	}
	return attrs
}

func valueAttribute(name string, v Value) attribute.KeyValue {
	switch v.kind {
	case integerValue, measureValue:
		return attribute.Int64(name, v.number)
	case boolValue:
		return attribute.Bool(name, v.flag)
	}
	return attribute.String(name, v.text)
}

func logSeverity(s Severity) otellog.Severity {
	switch s {
	case Debug:
		return otellog.SeverityDebug
	case Warn:
		return otellog.SeverityWarn
	case Error:
		return otellog.SeverityError
	}
	return otellog.SeverityInfo
}
