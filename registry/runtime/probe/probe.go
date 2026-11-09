package probe

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"stego.dev/runtime/otel"
)

// HoldLimit is the most emissions that one call holds until its outcome (ST-SUP-RP12-LC20).
const HoldLimit = 64

// Kind is the kind of an observation.
type Kind uint8

// The kinds of ST-DECL-INV49.
const (
	Event Kind = iota + 1
	Counter
	Histogram
)

func (k Kind) String() string {
	switch k {
	case Event:
		return "event"
	case Counter:
		return "counter"
	case Histogram:
		return "histogram"
	}
	return "unknown"
}

// Severity is the severity of an observation.
type Severity uint8

// The severities of ST-DECL-INV49.
const (
	Debug Severity = iota + 1
	Info
	Warn
	Error
)

// String is the severity text of log records and console lines.
func (s Severity) String() string {
	switch s {
	case Debug:
		return "DEBUG"
	case Info:
		return "INFO"
	case Warn:
		return "WARN"
	case Error:
		return "ERROR"
	}
	return "UNKNOWN"
}

// Counts tells when an observation counts (ST-DECL-INV55). The zero value is CountsAttempt, as a declaration with
// no Counts states.
type Counts uint8

const (
	// CountsAttempt sends each emission at the end of its call, with stego.outcome committed or rolled_back.
	CountsAttempt Counts = iota
	// CountsCommitted sends each emission only when its call commits, with no stego.outcome.
	CountsCommitted
)

// Unit is the unit of a histogram or a measure field.
type Unit string

// The units of ST-DECL-INV50 and ST-DECL-INV51.
const (
	Seconds      Unit = "s"
	Milliseconds Unit = "ms"
	Bytes        Unit = "By"
	One          Unit = "1"
)

func (u Unit) known() bool { return u == Seconds || u == Milliseconds || u == Bytes || u == One }

// FieldType is the type of an observation field (ST-DECL-INV51).
type FieldType uint8

const (
	// FieldText is an entity field of type text, read from a loaded value.
	FieldText FieldType = iota + 1
	// FieldInteger is an entity field of an integer type, read from a loaded value.
	FieldInteger
	// FieldBool is a bool field, or an entity field of type bool.
	FieldBool
	// FieldEnum is an enum field with its declared values.
	FieldEnum
	// FieldMeasure is a measure field with its unit and its range.
	FieldMeasure
)

// Field is one declared field of an observation.
type Field struct {
	Name string
	Type FieldType
	// Values are the declared values of an enum field.
	Values []string
	// Min, Max and Unit are the range and the unit of a measure field.
	Min, Max int64
	Unit     Unit
}

// Spec is the declaration of one observation as the generated code gives it to Declare.
type Spec struct {
	Name     string
	Kind     Kind
	Severity Severity
	Counts   Counts
	Fields   []Field
	// Value and Unit name the one value of a histogram and its unit.
	Value string
	Unit  Unit
	// EmittedBy names the commands, queries, workers, process methods and slot bindings whose fills emit it.
	EmittedBy []string
}

// field is a checked field.
type field struct {
	name     string
	typ      FieldType
	values   map[string]bool
	min, max int64
}

// class tells a declared observation from the built-in ones of ST-SUP-RP12-LC21 and ST-SUP-RP12-LC23.
type class uint8

const (
	declared class = iota
	serviceClass
	workerClass
	backfillClass
)

// Observation is the descriptor of one observation: the generated code makes it once with Declare and passes it to
// Emit. It has no exported field, so only Declare makes one.
type Observation struct {
	name     string
	signal   string
	kind     Kind
	severity Severity
	counts   Counts
	fields   []field
	unit     Unit
	emitters map[string]bool
	class    class
}

// Name is the declared name of the observation.
func (o *Observation) Name() string { return o.name }

// Signal is the name of its log record, span event or metric: domain.<name> for a declared observation.
func (o *Observation) Signal() string { return o.signal }

// Declare checks the declaration of one observation and makes its descriptor. It refuses, naming the observation,
// a name that is not well formed or that a built-in observation has, an unknown kind, severity or unit, more than
// eight fields, a field that is not well formed, and a counter or a histogram with a field that is not an enum or a
// bool or with more than 127 series (ST-DECL-INV48 to ST-DECL-INV54, ST-SUP-RP12-LC21, ST-SUP-RP12-LC23).
func Declare(spec Spec) (*Observation, error) {
	if builtinName(spec.Name) {
		return nil, errors.New("probe: the observation name " + spec.Name + " is reserved for a built-in observation")
	}
	if !snake(spec.Name, 63) {
		return nil, errors.New("probe: the observation name " + quoted(spec.Name) + " is not well formed")
	}
	refuse := func(text string) (*Observation, error) {
		return nil, errors.New("probe: observation " + spec.Name + ": " + text)
	}
	if spec.Kind < Event || spec.Kind > Histogram {
		return refuse("the kind is not event, counter or histogram")
	}
	if spec.Severity < Debug || spec.Severity > Error {
		return refuse("the severity is not debug, info, warn or error")
	}
	if spec.Counts > CountsCommitted {
		return refuse("counts is not attempt or committed")
	}
	if spec.Kind == Histogram {
		if !snake(spec.Value, 63) || !spec.Unit.known() {
			return refuse("a histogram names one value in lower snake case with the unit s, ms, By or 1")
		}
	} else if spec.Value != "" || spec.Unit != "" {
		return refuse("only a histogram names a value and a unit")
	}
	if len(spec.Fields) > 8 {
		return refuse("it has more than eight fields")
	}
	o := &Observation{name: spec.Name, signal: "domain." + spec.Name, kind: spec.Kind, severity: spec.Severity, counts: spec.Counts, unit: spec.Unit, emitters: map[string]bool{}}
	series := 1
	names := map[string]bool{}
	for _, f := range spec.Fields {
		if !snake(f.Name, 63) || reservedFields[f.Name] || names[f.Name] {
			return refuse("the field " + quoted(f.Name) + " is not well formed, is reserved or is repeated")
		}
		names[f.Name] = true
		checked := field{name: f.Name, typ: f.Type}
		switch f.Type {
		case FieldText, FieldInteger, FieldBool:
			if len(f.Values) != 0 || f.Min != 0 || f.Max != 0 || f.Unit != "" {
				return refuse("the field " + f.Name + " has a set, a range or a unit that its type does not take")
			}
		case FieldEnum:
			if len(f.Values) < 1 || len(f.Values) > 32 || f.Min != 0 || f.Max != 0 || f.Unit != "" {
				return refuse("the enum field " + f.Name + " has not one to 32 values")
			}
			checked.values = map[string]bool{}
			for _, v := range f.Values {
				if !snake(v, 63) || checked.values[v] {
					return refuse("the enum field " + f.Name + " has a value that is not well formed or is repeated")
				}
				checked.values[v] = true
			}
		case FieldMeasure:
			if len(f.Values) != 0 || !f.Unit.known() || f.Min > f.Max {
				return refuse("the measure field " + f.Name + " has no unit s, ms, By or 1 or no range min..max")
			}
			checked.min, checked.max = f.Min, f.Max
		default:
			return refuse("the field " + f.Name + " has an unknown type")
		}
		if spec.Kind != Event {
			switch f.Type {
			case FieldEnum:
				series *= len(f.Values)
			case FieldBool:
				series *= 2
			default:
				return refuse("the field " + f.Name + " of a counter or a histogram is not an enum or a bool")
			}
			if series > 127 {
				return refuse("more than 127 series")
			}
		}
		o.fields = append(o.fields, checked)
	}
	if len(spec.EmittedBy) == 0 || len(spec.EmittedBy) > 127 {
		return refuse("it names no emitter or more than 127")
	}
	for _, name := range spec.EmittedBy {
		if !emitterName(name) || o.emitters[name] {
			return refuse("the emitter " + quoted(name) + " is not well formed or is repeated")
		}
		o.emitters[name] = true
	}
	if spec.Kind != Event {
		if spec.Counts == CountsAttempt {
			series *= 2
		}
		series *= len(spec.EmittedBy)
		if series > 127 {
			return refuse(strconv.Itoa(series) + " series is more than 127")
		}
	}
	return o, nil
}

// reservedFields are the names of ST-DECL-INV51 that the runtime adds itself.
var reservedFields = map[string]bool{"value": true, "trace_id": true, "span_id": true, "tenant": true, "principal": true, "operation": true}

// snake tells whether name matches ^[a-z][a-z0-9_]{0,limit-1}$.
func snake(name string, limit int) bool {
	if name == "" || len(name) > limit || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// emitterName tells whether name can be the name of a command, query, worker, process method or slot binding.
func emitterName(name string) bool {
	if name == "" || len(name) > 128 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// quoted is a name for an error text: cut and quoted in ASCII, so an error never carries raw bytes.
func quoted(name string) string {
	if len(name) > 64 {
		name = name[:64]
	}
	return strconv.QuoteToASCII(name)
}

// record is one emission as the sinks take it. A sink must not change it.
type record struct {
	obs       *Observation
	values    []Value
	number    float64
	hasNumber bool
	count     int64
	outcome   string
	time      time.Time
	spanCtx   trace.SpanContext
	tenant    string
	principal string
	operation string
}

// The values of the stego.outcome attribute.
const (
	outcomeCommitted  = "committed"
	outcomeRolledBack = "rolled_back"
)

// callState is the state of one call: open while the fill runs, returned once the fill returned and ended once the
// generated code ended the call's transaction.
type callState uint8

const (
	open callState = iota
	returned
	ended
)

// Call is one call of a fill as the generated code sees it: the command, query, worker, process method or slot
// binding call that a probe is bound to. Only the generated code holds it; the fill gets only its Probe.
type Call struct {
	core      *core
	span      trace.Span
	tenant    string
	principal string
	operation string
	probe     Probe
	// finish ends the call's own child span, when the call has one (ST-SUP-RP03-LC15).
	finish otel.Finish

	mu    sync.Mutex
	state callState
	held  []*record
}

// bind makes the call of one fill. It reads nothing from ctx but the span (ST-SUP-RP12-LC09).
func (c *core) bind(ctx context.Context, tenant, principal, operation string) *Call {
	call := &Call{core: c, span: trace.SpanFromContext(ctx), tenant: cleanText(tenant), principal: cleanText(principal), operation: cleanText(operation)}
	call.probe = Probe{call: call, core: c}
	return call
}

// Probe is the probe that the generated typed probe of the fill wraps.
func (c *Call) Probe() *Probe { return &c.probe }

// Returned tells the call that the fill returned or panicked: from now on the probe emits nothing and counts each
// attempt, also from a goroutine that the fill started (ST-SUP-RP12-LC02). The emissions it holds wait for Commit
// or Rollback.
func (c *Call) Returned() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == open {
		c.state = returned
	}
}

// Commit ends the call with a commit: each emission it holds is sent, with stego.outcome committed when its
// observation counts each attempt (ST-SUP-RP12-LC20). A query that ends without an error commits.
func (c *Call) Commit() { c.end(true) }

// Rollback ends the call with a rollback: each emission of an observation that counts each attempt is sent with
// stego.outcome rolled_back, and each emission of an observation that counts committed work is dropped.
func (c *Call) Rollback() { c.end(false) }

func (c *Call) end(committed bool) {
	c.mu.Lock()
	if c.state == ended {
		c.mu.Unlock()
		return
	}
	c.state = ended
	held := c.held
	c.held = nil
	finish := c.finish
	c.mu.Unlock()
	outcome := outcomeRolledBack
	if committed {
		outcome = outcomeCommitted
	}
	for _, r := range held {
		if r.obs.counts == CountsAttempt {
			r.outcome = outcome
		} else if !committed {
			continue
		}
		c.core.safeSend(r)
	}
	// The span events went on the call's span at once; the span ends after them.
	if finish != nil {
		if committed {
			finish(otel.OutcomeCommitted, false, false)
		} else {
			// A rollback is an outcome of the call, not an error of the span.
			finish(otel.OutcomeRolledBack, false, false)
		}
	}
}

// Probe is the emitter that one call's generated typed probe calls. It is bound to the span context, the tenant, the
// principal and the operation of its call, which it reports and does not change (ST-SUP-RP12-LC02).
type Probe struct {
	call *Call
	core *core
}

// Tenant is the tenant ID that the probe is bound to.
func (p *Probe) Tenant() string {
	if p == nil || p.call == nil {
		return ""
	}
	return p.call.tenant
}

// Principal is the opaque ID of the principal that the probe is bound to.
func (p *Probe) Principal() string {
	if p == nil || p.call == nil {
		return ""
	}
	return p.call.principal
}

// Operation is the name of the command, query, worker, process method or slot binding that the probe is bound to.
func (p *Probe) Operation() string {
	if p == nil || p.call == nil {
		return ""
	}
	return p.call.operation
}

// SpanContext is the span context that the probe is bound to.
func (p *Probe) SpanContext() trace.SpanContext {
	if p == nil || p.call == nil {
		return trace.SpanContext{}
	}
	return p.call.span.SpanContext()
}

// Muted is a probe that emits nothing and counts each attempt, for a fill that asks for its probe in the context of
// another emitter (ST-SUP-RC34-OC04).
func (p *Probe) Muted() *Probe {
	if p == nil {
		return nil
	}
	return &Probe{core: p.core}
}

// Emit takes one emission of the observation o with its typed values: the declared fields in order and, for a
// histogram, the value last. It returns nothing and never blocks or panics. It drops and counts an emission whose
// values do not match the declaration in kind, count, set or range, an emission of an observation that does not
// name the probe's operation as an emitter, an emission after the fill returned or after Close, and an emission
// beyond HoldLimit in one call (ST-SUP-RP12-LC01, ST-SUP-RP12-LC02, ST-SUP-RP12-LC20).
func (p *Probe) Emit(o *Observation, values ...Value) {
	if p == nil {
		return
	}
	defer func() {
		if recover() != nil {
			p.drop()
		}
	}()
	c := p.call
	if c == nil || o == nil || o.class != declared || !o.emitters[c.operation] {
		p.drop()
		return
	}
	r, ok := o.record(values)
	if !ok {
		p.drop()
		return
	}
	r.time = time.Now()
	r.spanCtx = c.span.SpanContext()
	r.tenant, r.principal, r.operation = c.tenant, c.principal, c.operation
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != open || c.core.closed.Load() || len(c.held) >= HoldLimit {
		p.drop()
		return
	}
	c.held = append(c.held, r)
}

func (p *Probe) drop() {
	if p.core != nil {
		p.core.dropped.Add(1)
	}
}

// record checks the values against the declaration and makes the record, with each text value cut as
// ST-SUP-RP12-LC09 states.
func (o *Observation) record(values []Value) (*record, bool) {
	want := len(o.fields)
	if o.kind == Histogram {
		want++
	}
	if len(values) != want {
		return nil, false
	}
	r := &record{obs: o, values: make([]Value, len(o.fields)), count: 1}
	for i := range o.fields {
		if !o.fields[i].matches(values[i]) {
			return nil, false
		}
		r.values[i] = values[i].sent()
	}
	if o.kind == Histogram {
		last := values[len(values)-1]
		if !last.finite() {
			return nil, false
		}
		r.number, r.hasNumber = last.float, true
	}
	return r, true
}
