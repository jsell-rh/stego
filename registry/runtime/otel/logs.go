package otel

import (
	"context"
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
// duration and, from ctx, the trace and span IDs of the call's span. Its fields are the bounded values the
// wrapper observed, so it holds no raw path, resource ID, body, credential or user detail (ST-SUP-RP03-LC06).
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
	s.logger.Emit(ctx, record)
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
