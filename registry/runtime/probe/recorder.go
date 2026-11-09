package probe

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FieldValue is one field of a recorded emission, as text.
type FieldValue struct {
	Name  string
	Value string
}

// Emission is one emission that a Recorder keeps, with its correlation.
type Emission struct {
	// Name is the declared name of the observation, or the fixed name of a built-in observation.
	Name     string
	Kind     Kind
	Severity Severity
	Fields   []FieldValue
	// Value is the histogram value, or the count of a built-in counter, when HasValue is set.
	Value    float64
	HasValue bool
	// Outcome is stego.outcome: committed or rolled_back for an observation that counts each attempt, else empty.
	Outcome   string
	Time      time.Time
	TraceID   string
	SpanID    string
	Tenant    string
	Principal string
	Operation string
}

// TB is the part of testing.TB that the checks of a Recorder use.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// Recorder is a sink in memory for fill tests. It keeps every emission in the order of the calls, never drops one
// and is safe for concurrent use. A probe bound to a recorder sends to no other sink (ST-SUP-RP12-LC10).
type Recorder struct {
	core *core
	mu   sync.Mutex
	kept []Emission
}

// NewRecorder makes a recorder. builtins may be nil when the test emits no built-in worker or backfill observation.
func NewRecorder(builtins *Builtins) *Recorder {
	if builtins == nil {
		builtins = &Builtins{}
	}
	r := &Recorder{core: &core{builtins: builtins}}
	r.core.sinks = []sink{r}
	return r
}

// Bind binds a new call to the recorder, as Runtime.Bind does. The emissions of the call wait until the test ends
// the call with Commit or Rollback, as ST-SUP-RP12-LC20 states.
func (r *Recorder) Bind(ctx context.Context, tenant, principal, operation string) *Call {
	return r.core.bind(ctx, tenant, principal, operation)
}

// Dropped counts the emissions that the probe dropped before the recorder: values that do not match, emissions
// after the fill returned and emissions beyond HoldLimit. The recorder itself drops none.
func (r *Recorder) Dropped() uint64 { return r.core.dropped.Load() }

func (r *Recorder) send(rec *record) {
	e := Emission{Name: rec.obs.name, Kind: rec.obs.kind, Severity: rec.obs.severity, Outcome: rec.outcome, Time: rec.time, Tenant: rec.tenant, Principal: rec.principal, Operation: rec.operation}
	if rec.obs.class != declared {
		e.Name = rec.obs.signal
	}
	for i, f := range rec.obs.fields {
		e.Fields = append(e.Fields, FieldValue{Name: f.name, Value: rec.values[i].String()})
	}
	if text, ok := consoleValue(rec); ok {
		e.HasValue = true
		e.Value, _ = strconv.ParseFloat(string(text), 64)
	}
	if rec.spanCtx.IsValid() {
		e.TraceID, e.SpanID = rec.spanCtx.TraceID().String(), rec.spanCtx.SpanID().String()
	}
	r.mu.Lock()
	r.kept = append(r.kept, e)
	r.mu.Unlock()
}

// All is every emission in the order of the calls.
func (r *Recorder) All() []Emission {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Emission(nil), r.kept...)
}

// Emissions is every emission of one observation, by its name, in the order of the calls.
func (r *Recorder) Emissions(name string) []Emission {
	var out []Emission
	for _, e := range r.All() {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// ExpectEmitted fails the test when no emission of the observation has each of the given field values, and names
// the observation and lists what was recorded (ST-SUP-RP12-LC11).
func (r *Recorder) ExpectEmitted(t TB, name string, fields ...FieldValue) {
	t.Helper()
	for _, e := range r.Emissions(name) {
		if e.has(fields) {
			return
		}
	}
	var want []string
	for _, f := range fields {
		want = append(want, f.Name+"="+f.Value)
	}
	t.Errorf("probe: no emission of %s has %s; recorded: %s", name, strings.Join(want, " "), r.listing())
}

// ExpectNotEmitted fails the test when the observation was emitted at all, and names the observation and lists
// what was recorded.
func (r *Recorder) ExpectNotEmitted(t TB, name string) {
	t.Helper()
	if len(r.Emissions(name)) > 0 {
		t.Errorf("probe: %s was emitted; recorded: %s", name, r.listing())
	}
}

func (e Emission) has(fields []FieldValue) bool {
	for _, want := range fields {
		found := false
		for _, got := range e.Fields {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// String is the emission as one short line: its name, its fields, its value and its outcome.
func (e Emission) String() string {
	var b strings.Builder
	b.WriteString(e.Name)
	b.WriteByte('{')
	for i, f := range e.Fields {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(f.Name + "=" + f.Value)
	}
	if e.HasValue {
		if len(e.Fields) > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("value=" + strconv.FormatFloat(e.Value, 'g', -1, 64))
	}
	if e.Outcome != "" {
		if len(e.Fields) > 0 || e.HasValue {
			b.WriteByte(' ')
		}
		b.WriteString("stego.outcome=" + e.Outcome)
	}
	b.WriteByte('}')
	return b.String()
}

// listing is what the recorder holds, at most 50 emissions, for a failure message.
func (r *Recorder) listing() string {
	all := r.All()
	if len(all) == 0 {
		return "nothing"
	}
	var lines []string
	for i, e := range all {
		if i == 50 {
			lines = append(lines, "and "+strconv.Itoa(len(all)-50)+" more")
			break
		}
		lines = append(lines, e.String())
	}
	return strings.Join(lines, ", ")
}
