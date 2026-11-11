package otel

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// statusNames are the seventeen gRPC status names, by code.
var statusNames = [...]string{"OK", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED", "NOT_FOUND", "ALREADY_EXISTS", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION", "ABORTED", "OUT_OF_RANGE", "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS", "UNAUTHENTICATED"}

// TraceRPC starts the server span of one gRPC call, before authentication and capacity admission. The transport
// calls finish once, with the final error of the call, when the call or the stream ends for any reason; the span
// covers the whole call (ST-SUP-RP03-LC04, ST-SUP-RP03-LC07). The span records exactly rpc.system.name,
// rpc.method and rpc.response.status_code, and error.type for a server failure (ST-SUP-RP03-LC13). It reads only
// the traceparent metadata. On a runtime with no provider or a closed one, TraceRPC returns ctx unchanged and a
// finish that does nothing.
func (r *Runtime) TraceRPC(ctx context.Context, fullMethod string) (context.Context, func(error)) {
	if r.provider == nil || r.closed.Load() {
		return ctx, func(error) {}
	}
	md, _ := metadata.FromIncomingContext(ctx)
	ctx = continueTrace(ctx, md.Get("traceparent"))
	method := rpcMethod(fullMethod)
	tracer := r.rpcTracer
	if tracer == nil {
		tracer = r.provider.Tracer("stego/grpc")
	}
	start := time.Now()
	ctx, span, record := r.startSpan(ctx, tracer, method, trace.SpanKindServer, start, attribute.String("rpc.system.name", "grpc"), attribute.String("rpc.method", method))
	ctx = withErrorCode(ctx)
	r.signals.started(ctx, r.signals.rpcActive)
	return ctx, func(err error) {
		end := time.Now()
		code := status.Code(err)
		if int(code) >= len(statusNames) {
			code = grpccodes.Unknown
		}
		name := statusNames[code]
		record.set(attribute.String("rpc.response.status_code", name))
		if rpcFailed(code) {
			record.fail()
			record.set(attribute.String("error.type", name))
		}
		r.signals.rpcFinished(ctx, start, end, method, name, code)
		span.End(trace.WithTimestamp(end))
	}
}

// rpcMethod is service/method of a registered full method when it is bounded and well formed, and _OTHER
// otherwise.
func rpcMethod(fullMethod string) string {
	method := strings.TrimPrefix(fullMethod, "/")
	service, name, found := strings.Cut(method, "/")
	if !strings.HasPrefix(fullMethod, "/") || !found || service == "" || name == "" || len(method) > 256 || strings.Contains(name, "/") || strings.Trim(service, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._") != "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" {
		return "_OTHER"
	}
	return method
}

// rpcFailed tells the six server failure codes, which alone mark an RPC span failed (ST-SUP-RP03-LC13).
func rpcFailed(code grpccodes.Code) bool {
	switch code {
	case grpccodes.Unknown, grpccodes.DeadlineExceeded, grpccodes.Unimplemented, grpccodes.Internal, grpccodes.Unavailable, grpccodes.DataLoss:
		return true
	}
	return false
}

// rpcFinished records the duration and the completion log of one call from the method and the status, never from
// span attributes.
func (s *requestSignals) rpcFinished(ctx context.Context, start, end time.Time, method, name string, code grpccodes.Code) {
	if s.rpcDuration == nil && s.logger == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("rpc.system.name", "grpc"), attribute.String("rpc.method", method), attribute.String("rpc.response.status_code", name)}
	if rpcFailed(code) {
		attrs = append(attrs, attribute.String("error.type", name))
	}
	duration := end.Sub(start).Seconds()
	if s.rpcDuration != nil {
		s.rpcDuration.Record(ctx, duration, metric.WithAttributes(attrs...))
		s.rpcActive.Add(ctx, -1)
	}
	s.completed(ctx, end, rpcCompleted, duration, rpcSeverity(code), attrs)
}
