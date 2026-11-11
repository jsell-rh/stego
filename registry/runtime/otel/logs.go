package otel

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	grpccodes "google.golang.org/grpc/codes"
)

// completion is the fixed event name and body of one kind of completion log (ST-SUP-RP03-LC12).
type completion struct{ event, body string }

var (
	httpCompleted = completion{event: "http.server.request.completed", body: "HTTP request completed"}
	rpcCompleted  = completion{event: "rpc.server.call.completed", body: "RPC call completed"}
)

// completed emits the one completion log of a call, sampled or not. The record carries the call's fields, the
// duration, from ctx the trace and span IDs of the call's span and, for a denied or failed call, the fixed error
// code that the handler gave (SetErrorCode). Its fields are the bounded values the wrapper observed, so it holds no
// raw path, resource ID, body, credential, user detail or error text (ST-SUP-RP03-LC06, ST-SUP-RP03-LC12).
func (s *requestSignals) completed(ctx context.Context, end time.Time, kind completion, duration float64, severity otellog.Severity, attrs []attribute.KeyValue) {
	if s.logger == nil {
		return
	}
	var record otellog.Record
	record.SetTimestamp(end)
	record.SetEventName(kind.event)
	record.SetBody(attribute.StringValue(kind.body))
	record.SetSeverity(severity)
	record.SetSeverityText(severityText(severity))
	record.AddAttributes(attrs...)
	record.AddAttributes(attribute.Float64("duration_seconds", duration))
	if severity != otellog.SeverityInfo {
		record.AddAttributes(errorCodeOf(ctx)...)
	}
	s.logger.Emit(ctx, record)
}

// MaxErrorCode is the longest fixed error code that a completion log carries.
const MaxErrorCode = 64

type errorCodeKey struct{}

// errorCode keeps the first fixed error code that the handler of one call gives.
type errorCode struct{ code atomic.Pointer[string] }

// SetErrorCode gives the fixed error code of a denied or failed call, such as denied or internal, to the completion
// log of the call of ctx (ST-SUP-RP03-LC12). The first valid code counts: lower-case letters, digits, '.', '_' and
// '-', at most MaxErrorCode bytes, so no error text and no value of a request reaches the log. A code of another
// form, and a ctx of no call that Handler or TraceRPC started, are ignored. The log of a call that passes holds no
// code.
func SetErrorCode(ctx context.Context, code string) {
	holder, ok := ctx.Value(errorCodeKey{}).(*errorCode)
	if !ok || code == "" || len(code) > MaxErrorCode || strings.Trim(code, "abcdefghijklmnopqrstuvwxyz0123456789._-") != "" {
		return
	}
	holder.code.CompareAndSwap(nil, &code)
}

// withErrorCode gives ctx with a place for the fixed error code of its call.
func withErrorCode(ctx context.Context) context.Context {
	return context.WithValue(ctx, errorCodeKey{}, &errorCode{})
}

// errorCodeOf gives the completion log attribute of the fixed error code of the call of ctx, when the handler gave
// one.
func errorCodeOf(ctx context.Context) []attribute.KeyValue {
	if holder, ok := ctx.Value(errorCodeKey{}).(*errorCode); ok {
		if code := holder.code.Load(); code != nil {
			return []attribute.KeyValue{attribute.String("error.code", *code)}
		}
	}
	return nil
}

// httpSeverity is INFO for a normal completion, WARN for a client error status and ERROR for a server error status
// or a panic (status 0).
func httpSeverity(status int) otellog.Severity {
	switch {
	case status == 0 || status >= 500:
		return otellog.SeverityError
	case status >= 400:
		return otellog.SeverityWarn
	}
	return otellog.SeverityInfo
}

// rpcSeverity is INFO for OK, ERROR for the six server failure codes and WARN for every other code.
func rpcSeverity(code grpccodes.Code) otellog.Severity {
	switch {
	case code == grpccodes.OK:
		return otellog.SeverityInfo
	case rpcFailed(code):
		return otellog.SeverityError
	}
	return otellog.SeverityWarn
}

func severityText(severity otellog.Severity) string {
	switch severity {
	case otellog.SeverityWarn:
		return "WARN"
	case otellog.SeverityError:
		return "ERROR"
	}
	return "INFO"
}
