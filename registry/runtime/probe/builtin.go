package probe

import (
	"context"
	"errors"
	"slices"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// builtin makes the descriptor of one built-in observation. Its fields are enums whose values the Builtins of the
// runtime hold, and it has no emitter: only the runtime's own methods send it.
func builtin(name string, kind Kind, severity Severity, c class, fields ...string) *Observation {
	o := &Observation{name: name, signal: name, kind: kind, severity: severity, counts: CountsCommitted, class: c}
	for _, f := range fields {
		o.fields = append(o.fields, field{name: f, typ: FieldEnum})
	}
	return o
}

// The built-in observations of ST-SUP-RP12-LC21 and ST-SUP-RP12-LC23, with their fixed names and severities.
var (
	telemetryStarted = builtin("telemetry.runtime.started", Event, Info, serviceClass)
	telemetryStopped = builtin("telemetry.runtime.stopped", Event, Info, serviceClass)
	serviceReady     = builtin("service.ready", Event, Info, serviceClass)
	serviceStopping  = builtin("service.stopping", Event, Info, serviceClass)
	serviceFailed    = builtin("service.failed", Event, Error, serviceClass)
	httpDiagnostic   = builtin("http.server.diagnostic", Event, Warn, serviceClass)
	// schemaMismatch is the alert of a start whose database does not satisfy the schema manifest, one for each class
	// of the faults: the service starts, and the migration job refuses the database (decision on finding F10 of the
	// schema-change research, owner, 2026-11-13).
	schemaMismatch = builtin("schema.manifest.mismatch", Event, Error, serviceClass, "class")

	workerRunFailed       = builtin("worker.run.failed", Event, Error, workerClass, "worker", "outcome")
	workerTickSkipped     = builtin("worker.tick.skipped", Counter, Warn, workerClass, "worker")
	workerTenantUnreached = builtin("worker.tenant.unreached", Counter, Warn, workerClass, "worker")

	backfillRows       = builtin("schema.backfill.rows", Counter, Info, backfillClass, "change")
	backfillFailedRows = builtin("schema.backfill.failed_rows", Counter, Warn, backfillClass, "change")
	backfillDuplicates = builtin("schema.backfill.duplicate_groups", Counter, Warn, backfillClass, "change")
	backfillCompleted  = builtin("schema.backfill.completed", Event, Info, backfillClass, "change")
	backfillStalled    = builtin("schema.backfill.stalled", Event, Error, backfillClass, "change")
	backfillThrottled  = builtin("schema.backfill.throttled", Counter, Warn, backfillClass, "change", "reason")
)

// builtinObservations are the built-in observations, whose names no declared observation may take.
var builtinObservations = []*Observation{telemetryStarted, telemetryStopped, serviceReady, serviceStopping, serviceFailed, httpDiagnostic, schemaMismatch, workerRunFailed, workerTickSkipped, workerTenantUnreached, backfillRows, backfillFailedRows, backfillDuplicates, backfillCompleted, backfillStalled, backfillThrottled}

// builtinName tells whether name is the name of a built-in observation.
func builtinName(name string) bool {
	for _, o := range builtinObservations {
		if o.name == name {
			return true
		}
	}
	return false
}

// ServiceEvent is one fixed service event of the telemetry runtime (ST-SUP-RP12-LC21).
type ServiceEvent uint8

const (
	TelemetryStarted ServiceEvent = iota + 1
	TelemetryStopped
	ServiceReady
	ServiceStopping
	ServiceFailed
	HTTPDiagnostic
)

func (e ServiceEvent) observation() *Observation {
	switch e {
	case TelemetryStarted:
		return telemetryStarted
	case TelemetryStopped:
		return telemetryStopped
	case ServiceReady:
		return serviceReady
	case ServiceStopping:
		return serviceStopping
	case ServiceFailed:
		return serviceFailed
	case HTTPDiagnostic:
		return httpDiagnostic
	}
	return nil
}

// SchemaFaultClasses are the values of the field class of schema.manifest.mismatch: the classes of the faults of
// the schema manifest check (manifest.FaultClass), so that an operator can page on the classes that touch the
// tenant wall and the grants only.
var SchemaFaultClasses = []string{"rls", "policy", "grant", "owner", "trigger", "function", "rule", "column", "other"}

// SchemaMismatch sends schema.manifest.mismatch, an Error, with the class of the faults. A class that is not one of
// SchemaFaultClasses is dropped and counted.
func (r *Runtime) SchemaMismatch(ctx context.Context, class string) {
	r.core.builtin(ctx, schemaMismatch, 1, class)
}

// SchemaMismatch records schema.manifest.mismatch.
func (r *Recorder) SchemaMismatch(ctx context.Context, class string) {
	r.core.builtin(ctx, schemaMismatch, 1, class)
}

// WorkerOutcome is the fixed outcome of a worker run that did not commit (ST-SUP-RC35-OC09).
type WorkerOutcome string

const (
	WorkerRolledBack WorkerOutcome = "rolled_back"
	WorkerFenced     WorkerOutcome = "fenced"
	WorkerPanicked   WorkerOutcome = "panicked"
)

// SchemaChange is one schema change of the declaration. Open is set when its last step is expand.
type SchemaChange struct {
	ID   string
	Open bool
}

// Builtins is the part of the declaration that the built-in observations read: the declared workers, which are the
// values of the field worker, and the open schema changes, which are the values of the field change. Each is at
// most 127, so each built-in counter has at most 127 series (ST-DECL-INV64, ST-DECL-INV79).
type Builtins struct {
	workers map[string]bool
	changes []string
	open    map[string]bool
}

// NewBuiltins checks the declared worker names and the schema changes and keeps the open changes only.
func NewBuiltins(workers []string, changes []SchemaChange) (*Builtins, error) {
	b := &Builtins{workers: map[string]bool{}, open: map[string]bool{}}
	for _, name := range workers {
		if !emitterName(name) || b.workers[name] {
			return nil, errors.New("probe: the worker name " + quoted(name) + " is not well formed or is repeated")
		}
		b.workers[name] = true
	}
	if len(b.workers) > 127 {
		return nil, errors.New("probe: more than 127 workers")
	}
	seen := map[string]bool{}
	for _, change := range changes {
		if !emitterName(change.ID) || seen[change.ID] {
			return nil, errors.New("probe: the schema change " + quoted(change.ID) + " is not well formed or is repeated")
		}
		seen[change.ID] = true
		if change.Open {
			b.changes = append(b.changes, change.ID)
			b.open[change.ID] = true
		}
	}
	if len(b.changes) > 127 {
		return nil, errors.New("probe: more than 127 open schema changes")
	}
	return b, nil
}

// Changes are the values of the field change: the IDs of the open schema changes, in the order of the declaration.
func (b *Builtins) Changes() []string { return append([]string(nil), b.changes...) }

// builtin sends one built-in observation at once: it belongs to no call, so nothing holds it. The fields are the
// enum values; a value outside its set is dropped and counted.
func (c *core) builtin(ctx context.Context, o *Observation, count int64, values ...string) {
	if o == nil || len(values) != len(o.fields) || count < 0 {
		c.dropped.Add(1)
		return
	}
	r := &record{obs: o, count: count, time: time.Now()}
	r.spanCtx = trace.SpanContextFromContext(ctx)
	for i, v := range values {
		ok := false
		switch o.fields[i].name {
		case "worker":
			ok = c.builtins.workers[v]
		case "change":
			ok = c.builtins.open[v]
		case "class":
			ok = slices.Contains(SchemaFaultClasses, v)
		case "outcome":
			outcome := WorkerOutcome(v)
			ok = outcome == WorkerRolledBack || outcome == WorkerFenced || outcome == WorkerPanicked
		}
		if !ok {
			c.dropped.Add(1)
			return
		}
		r.values = append(r.values, Enum(v))
	}
	c.send(r)
}

// Service sends one fixed service event, with the trace ID and the span ID of ctx when they are valid and no
// field. It goes to the console in every setting and to OTLP when a collector endpoint is set (ST-SUP-RP12-LC22).
func (r *Runtime) Service(ctx context.Context, event ServiceEvent) {
	r.core.builtin(ctx, event.observation(), 1)
}

// Service records one fixed service event.
func (r *Recorder) Service(ctx context.Context, event ServiceEvent) {
	r.core.builtin(ctx, event.observation(), 1)
}

// Worker is the built-in observations of one declared worker. A name that the Builtins do not hold is dropped and
// counted.
type Worker struct {
	core *core
	name string
}

// Worker gives the built-in observations of the worker.
func (r *Runtime) Worker(name string) Worker { return Worker{core: r.core, name: name} }

// Worker gives the built-in observations of the worker, recorded.
func (r *Recorder) Worker(name string) Worker { return Worker{core: r.core, name: name} }

// RunFailed sends worker.run.failed for a run that did not commit, with the worker name and the outcome only.
func (w Worker) RunFailed(ctx context.Context, outcome WorkerOutcome) {
	w.core.builtin(ctx, workerRunFailed, 1, w.name, string(outcome))
}

// TickSkipped adds one to worker.tick.skipped.
func (w Worker) TickSkipped() { w.core.builtin(context.Background(), workerTickSkipped, 1, w.name) }

// TenantUnreached adds one to worker.tenant.unreached.
func (w Worker) TenantUnreached() {
	w.core.builtin(context.Background(), workerTenantUnreached, 1, w.name)
}

// Backfill sends the built-in schema backfill observations of ST-SUP-RP12-LC23, each with the one field change. It
// is the observer that a backfill of the lease runtime takes. A change that is not open is dropped and counted.
type Backfill struct{ core *core }

// Backfill gives the schema backfill observations.
func (r *Runtime) Backfill() Backfill { return Backfill{core: r.core} }

// Backfill gives the schema backfill observations, recorded.
func (r *Recorder) Backfill() Backfill { return Backfill{core: r.core} }

// BackfillRows adds the moved rows to schema.backfill.rows.
func (b Backfill) BackfillRows(change string, rows int64) {
	b.core.builtin(context.Background(), backfillRows, rows, change)
}

// BackfillFailedRows adds the rows that failed the conversion to schema.backfill.failed_rows.
func (b Backfill) BackfillFailedRows(change string, rows int64) {
	b.core.builtin(context.Background(), backfillFailedRows, rows, change)
}

// BackfillDuplicateGroups adds the duplicate groups to schema.backfill.duplicate_groups.
func (b Backfill) BackfillDuplicateGroups(change string, groups int64) {
	b.core.builtin(context.Background(), backfillDuplicates, groups, change)
}

// BackfillCompleted sends schema.backfill.completed.
func (b Backfill) BackfillCompleted(change string) {
	b.core.builtin(context.Background(), backfillCompleted, 1, change)
}

// BackfillStalled sends schema.backfill.stalled.
func (b Backfill) BackfillStalled(change string) {
	b.core.builtin(context.Background(), backfillStalled, 1, change)
}

// BackfillThrottled adds one to schema.backfill.throttled, with the reason: replica_lag or batch_time.
func (b Backfill) BackfillThrottled(change, reason string) {
	b.core.builtin(context.Background(), backfillThrottled, 1, change, reason)
}
