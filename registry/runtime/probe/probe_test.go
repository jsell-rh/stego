package probe

import (
	"bytes"
	"context"
	"go/types"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// claimsKey holds the claims of the principal in a context, as an authentication layer keeps them.
type claimsKey struct{}

func TestProbeBinding(t *testing.T) {
	t.Run("ST-SUP-RP12-LC01-T02 An emission with a value of the wrong kind or count, or a measure out of range, is dropped", func(t *testing.T) {
		refused := declare(t, Spec{Name: "reservation_refused", Kind: Event, Severity: Warn, EmittedBy: []string{command}, Fields: []Field{
			{Name: "sku", Type: FieldText},
			{Name: "reason", Type: FieldEnum, Values: []string{"short", "expired"}},
			{Name: "qty", Type: FieldMeasure, Min: 1, Max: 1000, Unit: One},
		}})
		recorder := NewRecorder(nil)
		call := recorder.Bind(context.Background(), "t1", "user:u-7", command)
		p := call.Probe()
		p.Emit(refused, Text("A-1"))
		p.Emit(refused, Text("A-1"), Enum("short"), Measure(0))
		p.Emit(refused, Text("A-1"), Enum("short"), Measure(1001))
		p.Emit(refused, Text("A-1"), Enum("short"), Measure(5))
		call.Returned()
		call.Commit()
		if got := recorder.All(); len(got) != 1 || got[0].String() != "reservation_refused{sku=A-1 reason=short qty=5 stego.outcome=committed}" {
			t.Fatalf("the recorder holds %v, want the one emission with every value right", got)
		}
		if got := recorder.Dropped(); got != 3 {
			t.Fatalf("the drop count is %d, want 3", got)
		}
		// A value of the wrong kind, an enum value outside its set and an observation of another emitter are
		// dropped and counted as well.
		other := recorder.Bind(context.Background(), "t1", "user:u-7", "open-reservations")
		call = recorder.Bind(context.Background(), "t1", "user:u-7", command)
		call.Probe().Emit(refused, Enum("A-1"), Enum("short"), Measure(5))
		call.Probe().Emit(refused, Text("A-1"), Enum("lost"), Measure(5))
		other.Probe().Emit(refused, Text("A-1"), Enum("short"), Measure(5))
		call.Commit()
		other.Commit()
		if len(recorder.All()) != 1 || recorder.Dropped() != 6 {
			t.Fatalf("the recorder holds %d emissions with %d drops, want 1 and 6", len(recorder.All()), recorder.Dropped())
		}
	})

	t.Run("ST-SUP-RP12-LC02-T01 A probe reports the call it was bound to and offers no way to change it", func(t *testing.T) {
		clearEnvironment(t)
		telemetry := newTelemetry(t)
		r := newProbe(t, telemetry, "", nil)
		ctx := unsampled()
		call := r.Bind(ctx, "t1", "user:u-7", command)
		p := call.Probe()
		if p.Tenant() != "t1" || p.Principal() != "user:u-7" || p.Operation() != command || !p.SpanContext().Equal(trace.SpanContextFromContext(ctx)) {
			t.Fatalf("the probe reports %q %q %q %v", p.Tenant(), p.Principal(), p.Operation(), p.SpanContext())
		}
		pkg, _, _ := checkPackage(t)
		funcs, fields := exported(pkg)
		for name := range fields {
			if strings.HasPrefix(name, "Probe.") || strings.HasPrefix(name, "Call.") {
				t.Fatalf("%s is an exported field", name)
			}
		}
		var methods []string
		for name, sig := range funcs {
			if !strings.HasPrefix(name, "Probe.") {
				continue
			}
			methods = append(methods, name)
			if strings.HasPrefix(name, "Probe.Set") {
				t.Fatalf("%s is a setter", name)
			}
			for i := 0; i < sig.Params().Len(); i++ {
				typ := sig.Params().At(i).Type()
				if basic, ok := typ.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
					t.Fatalf("%s takes a string, which could set a tenant, a principal or an operation", name)
				}
				if named, ok := types.Unalias(typ).(*types.Named); ok && (named.Obj().Name() == "SpanContext" || named.Obj().Name() == "Context") {
					t.Fatalf("%s takes a %s", name, named.Obj().Name())
				}
			}
		}
		slices.Sort(methods)
		if want := []string{"Probe.Emit", "Probe.Muted", "Probe.Operation", "Probe.Principal", "Probe.SpanContext", "Probe.Tenant"}; !slices.Equal(methods, want) {
			t.Fatalf("the probe has the methods %v, want %v", methods, want)
		}
	})

	t.Run("ST-SUP-RP12-LC02-T02 A probe goes quiet once the fill returns", func(t *testing.T) {
		refused := reservationRefused(t, CountsAttempt)
		recorder := NewRecorder(nil)
		call := recorder.Bind(context.Background(), "t1", "user:u-7", command)
		emit := make(chan struct{})
		emitted := make(chan struct{})
		fill := func(p *Probe) {
			go func() {
				for range emit {
					p.Emit(refused, Text("A-1"), Enum("short"))
					emitted <- struct{}{}
				}
			}()
		}
		fill(call.Probe())
		call.Returned()
		emit <- struct{}{}
		<-emitted
		call.Rollback()
		emit <- struct{}{}
		<-emitted
		close(emit)
		if got := recorder.All(); len(got) != 0 {
			t.Fatalf("the recorder holds %v from the goroutine of a returned fill", got)
		}
		if got := recorder.Dropped(); got != 2 {
			t.Fatalf("the drop count is %d, want each of the two attempts", got)
		}
		// The muted probe of another emitter emits nothing and counts each attempt too.
		call = recorder.Bind(context.Background(), "t1", "user:u-7", command)
		call.Probe().Muted().Emit(refused, Text("A-1"), Enum("short"))
		call.Commit()
		if len(recorder.All()) != 0 || recorder.Dropped() != 3 {
			t.Fatalf("the muted probe sent %d emissions with %d drops", len(recorder.All()), recorder.Dropped())
		}
	})

	t.Run("ST-SUP-RP12-LC03-T01 A log record carries the opaque tenant and principal, and the span event and the metric carry neither", func(t *testing.T) {
		c := startCollector(t)
		telemetry := newTelemetry(t)
		stdout := &lines{}
		r := newProbe(t, telemetry, "json", stdout)
		// Both observations count committed work, so the span event and the metric carry no stego.outcome here;
		// ST-SUP-RP12-LC13-T01 proves the sets of an observation that counts each attempt.
		event := reservationRefused(t, CountsCommitted)
		counter := reservationsRefused(t, CountsCommitted)
		ctx, span := recordingSpan(t, telemetry)
		ctx = context.WithValue(ctx, claimsKey{}, map[string]string{"name": "Ada Lovelace", "email": "ada@example.com"})
		call := r.Bind(ctx, "t1", "user:u-7", command)
		bound := call.Probe().SpanContext()
		call.Probe().Emit(event, Text("A-1"), Enum("short"))
		call.Probe().Emit(counter, Enum("short"))
		call.Returned()
		call.Commit()
		span.End()
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		records := c.logRecords()
		if len(records) != 1 {
			t.Fatalf("the collector received %d log records, want 1", len(records))
		}
		record := records[0]
		got := attributes(record.Attributes)
		if record.EventName != "domain.reservation_refused" || !slices.Equal(got, []string{"operation=reserve-and-charge", "principal=user:u-7", "reason=short", "sku=A-1", "tenant=t1"}) {
			t.Fatalf("the log record %s has the attributes %v", record.EventName, got)
		}
		// The bound span is the call's own child span of the server span (ST-SUP-RP03-LC15).
		traceID, spanID := bound.TraceID(), bound.SpanID()
		if bound.TraceID() != span.SpanContext().TraceID() || !bytes.Equal(record.TraceId, traceID[:]) || !bytes.Equal(record.SpanId, spanID[:]) {
			t.Fatalf("the log record has the trace %x and the span %x, want those of the bound span", record.TraceId, record.SpanId)
		}
		var events int
		for _, s := range c.spans() {
			for _, e := range s.Events {
				events++
				if got := attributeKeys(e.Attributes); !slices.Equal(got, []string{"reason", "sku"}) {
					t.Fatalf("the span event has the attributes %v, want sku and reason only", got)
				}
			}
		}
		if events != 1 {
			t.Fatalf("the spans hold %d events, want 1", events)
		}
		m := c.lastMetrics()["domain.reservations_refused"]
		if m == nil || len(m.GetSum().DataPoints) != 1 {
			t.Fatal("no metric domain.reservations_refused")
		}
		if got := attributeKeys(m.GetSum().DataPoints[0].Attributes); !slices.Equal(got, []string{"operation", "reason"}) {
			t.Fatalf("the metric has the attributes %v, want reason and the operation only", got)
		}
		text := strings.Join(stdout.all(), "")
		for _, private := range []string{"Ada", "ada@example.com", "email"} {
			if strings.Contains(text, private) || strings.Contains(record.String(), private) {
				t.Fatalf("a record holds the claim %q", private)
			}
		}
		if !strings.Contains(text, `"tenant":"t1","principal":"user:u-7","operation":"reserve-and-charge"`) || !strings.Contains(text, `"trace_id":"`+bound.TraceID().String()+`"`) {
			t.Fatalf("the console lines are %q, want the tenant, the principal, the operation and the trace ID", text)
		}
	})
}
