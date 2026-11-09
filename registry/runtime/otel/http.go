package otel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/trace"
)

// durationBoundaries are the fixed histogram boundaries, in seconds, of both request duration metrics
// (ST-SUP-RP03-LC11).
var durationBoundaries = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 1800}

// requestSignals are the request metric instruments and the completion logger. A nil instrument or logger is a
// signal the settings turned off.
type requestSignals struct {
	httpDuration metric.Float64Histogram
	rpcDuration  metric.Float64Histogram
	httpActive   metric.Int64UpDownCounter
	rpcActive    metric.Int64UpDownCounter
	logger       otellog.Logger
}

// startSignals starts the private meter provider on the reader and the private logger provider on the processor,
// and creates the fixed request instruments of ST-SUP-RP03-LC11. A nil reader or processor leaves that signal off.
func (r *Runtime) startSignals(reader sdkmetric.Reader, processor sdklog.Processor) error {
	if reader != nil {
		r.meter = sdkmetric.NewMeterProvider(sdkmetric.WithResource(r.resource), sdkmetric.WithCardinalityLimit(MetricSeriesLimit), sdkmetric.WithReader(reader))
		meter := r.meter.Meter("stego/requests")
		failed := errors.New("cannot create a request metric")
		var err error
		if r.signals.httpDuration, err = meter.Float64Histogram("http.server.request.duration", metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBoundaries...)); err != nil {
			return failed
		}
		if r.signals.rpcDuration, err = meter.Float64Histogram("rpc.server.call.duration", metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(durationBoundaries...)); err != nil {
			return failed
		}
		if r.signals.httpActive, err = meter.Int64UpDownCounter("http.server.active_requests", metric.WithUnit("{request}")); err != nil {
			return failed
		}
		if r.signals.rpcActive, err = meter.Int64UpDownCounter("rpc.server.active_requests", metric.WithUnit("{request}")); err != nil {
			return failed
		}
	}
	if processor != nil {
		// Twelve attributes: a probe record carries the tenant, the principal, the operation, stego.outcome and up to
		// eight declared fields (ST-SUP-RP12-LC03, ST-SUP-RP12-LC04).
		r.logs = sdklog.NewLoggerProvider(sdklog.WithResource(r.resource), sdklog.WithAttributeCountLimit(12), sdklog.WithAttributeValueLengthLimit(256), sdklog.WithProcessor(processor))
		r.signals.logger = r.logs.Logger("stego/requests")
	}
	return nil
}

// Handler wraps the whole HTTP handler of a service, outside authentication and capacity admission, so the server
// span starts before both and ends with the status the client receives (ST-SUP-RP03-LC07). It records the method,
// the registered route pattern and the status, never the URL, a header or a body (ST-SUP-RP03-LC04). The health
// and discovery routes stay outside it: the service mounts them on its top router beside Handler, not under it.
// Without a provider Handler is next itself, and after Close it passes each request to next.
func (r *Runtime) Handler(next http.Handler) http.Handler {
	if r.provider == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if r.closed.Load() {
			next.ServeHTTP(w, request)
			return
		}
		ctx := continueTrace(request.Context(), request.Header.Values("traceparent"))
		method := httpMethod(request.Method)
		start := time.Now()
		ctx, span, record := r.startSpan(ctx, r.tracer, "HTTP "+method, trace.SpanKindServer, start, attribute.String("http.request.method", method))
		observed := &routeObservation{}
		ctx = context.WithValue(ctx, routeKey{}, observed)
		r.signals.started(ctx, r.signals.httpActive)
		status := 0
		defer func() {
			end := time.Now()
			route := observed.load()
			markHTTP(record, method, route, status)
			r.signals.httpFinished(ctx, start, end, method, route, status)
			span.End(trace.WithTimestamp(end))
		}()
		traced := request.WithContext(ctx)
		status = ObserveHTTP(next, w, traced)
		observed.store(traced.Pattern)
	})
}

// Route wraps the router, inside authentication, so the registered pattern reaches the span, the metric and the
// log also when authentication passes on a copy of the request (ST-SUP-RP03-LC14).
func (r *Runtime) Route(next http.Handler) http.Handler {
	if r.provider == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer func() {
			if observed, ok := request.Context().Value(routeKey{}).(*routeObservation); ok {
				observed.store(request.Pattern)
			}
		}()
		next.ServeHTTP(w, request)
	})
}

type routeKey struct{}

// routeObservation keeps the first bounded route pattern seen for one request.
type routeObservation struct{ route atomic.Pointer[string] }

func (o *routeObservation) store(pattern string) {
	if route := safeRoute(pattern); route != "" {
		o.route.CompareAndSwap(nil, &route)
	}
}

func (o *routeObservation) load() string {
	if route := o.route.Load(); route != nil {
		return *route
	}
	return ""
}

// markHTTP names the span by the method and the route, and marks a status of 500 or more, or a panic (status 0),
// with a fixed error type and no message (ST-SUP-RP03-LC05).
func markHTTP(span *spanRecord, method, route string, status int) {
	if route != "" {
		span.setName(method + " " + route)
		span.set(attribute.String("http.route", route))
	}
	switch {
	case status == 0:
		span.fail()
		span.set(attribute.String("error.type", "panic"))
	case status >= 500:
		span.set(attribute.Int("http.response.status_code", status))
		span.fail()
		span.set(attribute.String("error.type", strconv.Itoa(status)))
	default:
		span.set(attribute.Int("http.response.status_code", status))
	}
}

// httpMethod is the method when it is one of the nine standard ones, and _OTHER otherwise.
func httpMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH":
		return method
	}
	return "_OTHER"
}

// safeRoute is the path of a registered pattern when it is bounded and holds only route characters; otherwise it
// is empty and the span keeps the method name only.
func safeRoute(pattern string) string {
	if pattern == "" || pattern == "/" || len(pattern) > 256 {
		return ""
	}
	_, route, hasMethod := strings.Cut(pattern, " ")
	if !hasMethod {
		route = pattern
	}
	if strings.HasPrefix(route, "/") && strings.Trim(route, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-{}$") == "" {
		return route
	}
	return ""
}

// ObserveHTTP serves the request through a writer that keeps every optional interface of w, and returns the first
// final status: 101, or 200 or more. An informational status before it does not count, and a body with no status
// counts as 200.
func ObserveHTTP(next http.Handler, w http.ResponseWriter, request *http.Request) int {
	code := 0
	final := func(status int) {
		if code == 0 && (status == http.StatusSwitchingProtocols || status >= 200) {
			code = status
		}
	}
	wrapped := httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) { next(status); final(status) }
		},
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(data []byte) (int, error) { final(200); return next(data) }
		},
		WriteString: func(next httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
			return func(data string) (int, error) { final(200); return next(data) }
		},
		ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(reader io.Reader) (int64, error) {
				count, err := next(reader)
				if count > 0 {
					final(200)
				}
				return count, err
			}
		},
		Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() { final(200); next() }
		},
		FlushError: func(next httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
			return func() error { final(200); return next() }
		},
	})
	next.ServeHTTP(wrapped, request)
	if code == 0 {
		return http.StatusOK
	}
	return code
}

func (s *requestSignals) started(ctx context.Context, active metric.Int64UpDownCounter) {
	if active != nil {
		active.Add(ctx, 1)
	}
}

// httpFinished records the duration and the completion log of one request from the values the wrapper observed,
// never from span attributes (ST-SUP-RP03-LC14). Status 0 is a panic.
func (s *requestSignals) httpFinished(ctx context.Context, start, end time.Time, method, route string, status int) {
	if s.httpDuration == nil && s.logger == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("http.request.method", method)}
	if status == 0 {
		attrs = append(attrs, attribute.String("error.type", "panic"))
	} else {
		attrs = append(attrs, attribute.Int("http.response.status_code", status))
	}
	if status >= 500 {
		attrs = append(attrs, attribute.String("error.type", strconv.Itoa(status)))
	}
	if route != "" {
		attrs = append(attrs, attribute.String("http.route", route))
	}
	duration := end.Sub(start).Seconds()
	if s.httpDuration != nil {
		s.httpDuration.Record(ctx, duration, metric.WithAttributes(attrs...))
		s.httpActive.Add(ctx, -1)
	}
	s.completed(ctx, end, httpCompleted, duration, httpSeverity(status), attrs)
}
