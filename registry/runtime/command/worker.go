package command

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/internal/staging"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// ErrDeadline matches the error of a worker run that reached its deadline under a lease and rolled back before the
// lease can expire (ST-SUP-RP10-LC22); it is also a conflict: the tenant runs again in the next tick.
var ErrDeadline = errDeadline

// WorkerSpec is what the generated code declares of one worker.
type WorkerSpec[P any] struct {
	// Name is the worker's name: its service account is service_account:<Name> (ST-SUP-RP06-LC07), and it is the
	// action of its audit rows and the operation of its probe.
	Name string
	// Permission is the worker's declared permission, which its handle decides on each resource it touches
	// (ST-SUP-RP10-LC17).
	Permission string
	// Interval is the interval of a worker with an every trigger; its ticks are due at its multiples since the Unix
	// epoch (ST-SUP-RP10-LC21). It is zero for a worker with a consumes trigger.
	Interval time.Duration
	// Writes are the entities that the worker declares it writes.
	Writes []Declared
	// Destinations are the outbox destinations of its effects.
	Destinations []Destination
	// Relations are the relations whose tuples its fill may write.
	Relations []authz.Relation
	// Stagers maps the name of each command or worker that stages a message for a consumed worker to its own
	// declared permission, which the consumer decides as the staging principal (ST-SUP-RP10-LC17).
	Stagers map[string]string
	// Probe wraps the probe of one call in the worker's typed probe.
	Probe func(*probe.Probe) P
}

// Worker is one checked worker declaration.
type Worker[P any] struct {
	spec         WorkerSpec[P]
	principal    authz.Principal
	writes       map[Declared]bool
	destinations map[string]Destination
	bound        []authz.Relation
	own          map[authz.Relation]bool
}

// NewWorker checks the declaration of one worker.
func NewWorker[P any](spec WorkerSpec[P]) (*Worker[P], error) {
	if spec.Name == "" || spec.Permission == "" || spec.Probe == nil {
		return nil, errors.New("command: a worker needs a name, its permission and its probe")
	}
	if spec.Interval < 0 || spec.Interval%time.Second != 0 {
		return nil, fmt.Errorf("command: the interval of %s is not a whole number of seconds", spec.Name)
	}
	principal, err := authz.System(spec.Name)
	if err != nil {
		return nil, err
	}
	writes, err := checkWrites(spec.Name, spec.Writes)
	if err != nil {
		return nil, err
	}
	destinations, err := checkDestinations(spec.Name, spec.Destinations)
	if err != nil {
		return nil, err
	}
	bound, own := boundRelations(spec.Relations, spec.Writes)
	return &Worker[P]{spec: spec, principal: principal, writes: writes, destinations: destinations, bound: bound, own: own}, nil
}

// TickFill is the fill of a worker with an every trigger: it gets the scheduled tick time and the handle.
type TickFill[P any] func(ctx context.Context, tick time.Time, h *Handle[P]) error

// RunWorker runs one tick of the worker w for the tenant t (ST-SUP-RP10-LC18): it calls fill at most once, in one
// transaction that it opens with tenant.Begin, with no idempotency wrapper, and commits with tenant.Commit only when
// the fill returns no error and met no conflict; it rolls back every write, audit row and staged effect otherwise.
// The handle is bound to the worker's service account (ST-SUP-RP10-LC16), and each effect takes the key of the tick
// due, a multiple of the worker's interval since the Unix epoch (ST-SUP-RP10-LC21).
//
// When held is not nil, RunWorker locks the lease row with FOR SHARE right after the tenant setting and the lock
// timeout and before the fill, and keeps it until the transaction ends; a row with another token fences the run,
// which returns lease.ErrFenced without calling the fill (ST-SUP-RP10-LC20). The transaction then has a deadline no
// later than the lease TTL less the renew interval after it starts, and no later than the lease's expiry as last
// renewed; a run that reaches it rolls back and returns ErrDeadline, and never commits after it (ST-SUP-RP10-LC22).
// A conflict rolls back and returns ErrConflict: the fill does not run again in this tick (ST-SUP-RP10-LC26).
func RunWorker[P any](ctx context.Context, rt *Runtime, w *Worker[P], due time.Time, t tenant.Tenant, held *lease.Held,
	fill TickFill[P]) (err error) {
	if rt == nil || w == nil || fill == nil || t.IsZero() {
		return errors.New("command: RunWorker needs the runtime, the worker, the fill and the tenant")
	}
	interval := w.spec.Interval
	if interval <= 0 || due.UnixNano()%interval.Nanoseconds() != 0 {
		return fmt.Errorf("command: the tick of %s is not due at a multiple of its interval", w.spec.Name)
	}
	ctx, finish := rt.span(ctx, WorkerSpanName)
	outcome := otel.OutcomeRolledBack
	defer func() { finish(outcome, false, err != nil) }()

	var notAfter time.Time
	if held != nil {
		if held.Err() != nil {
			return fmt.Errorf("command: %s: %w", w.spec.Name, lease.ErrStopped)
		}
		notAfter = time.Now().Add(held.TTL() - held.RenewEvery())
		if expires := held.Expires(); expires.Before(notAfter) {
			notAfter = expires
		}
		var cancel context.CancelFunc
		// The transaction's context ends at the deadline, and database/sql then rolls the transaction back.
		ctx, cancel = context.WithDeadline(ctx, notAfter)
		defer cancel()
	}
	tx, err := tenant.Begin(ctx, rt.cfg.DB, t, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return workerBegin(ctx, notAfter, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	s := newState(ctx, rt, tx, t, w.principal, modeWorker, w.spec.Name, w.writes, w.destinations)
	s.request = Request{RequestID: uuid.NewString()}
	s.permission = w.spec.Permission
	s.lockTimeout = DefaultLockTimeout
	tick := strconv.FormatInt(due.Unix(), 10)
	s.inbound = func(id string) string { return lengthPrefixed(w.spec.Name, tick, id) }
	if err := s.setLockTimeout(ctx); err != nil {
		return s.failRun(ctx, notAfter, "lock timeout", err)
	}
	if err := s.checkIsolation(ctx); err != nil {
		return deadlineOr(ctx, notAfter, err)
	}
	if held != nil {
		if _, err := held.Fence(ctx, tx); err != nil {
			if errors.Is(err, lease.ErrFenced) {
				return fmt.Errorf("command: %s: %w", w.spec.Name, lease.ErrFenced)
			}
			return s.failRun(ctx, notAfter, "lease fence", err)
		}
	}
	s.ownRelations = w.own
	if s.az, err = rt.cfg.Authz.Bind(tx, t, w.principal, w.bound); err != nil {
		return internalf("the authorization handle was not bound")
	}
	call := rt.cfg.Probes.Bind(ctx, t.ID(), w.principal.Subject().String(), w.spec.Name)
	defer call.Rollback()
	h := &Handle[P]{st: s, probe: w.spec.Probe(call.Probe())}
	if _, err := callFill(ctx, s, call, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fill(ctx, due, h)
	}); err != nil {
		var failure *fillFailure
		if errors.As(err, &failure) {
			err = failure.err
		}
		return deadlineOr(ctx, notAfter, err)
	}
	// The transaction's context ends at the deadline, so database/sql refuses a COMMIT after it
	// (ST-SUP-RP10-LC22). An outcome of COMMIT that is not known stays one: it is never a deadline to run again.
	if err := commit(ctx, s); err != nil {
		if errors.Is(err, ErrCommitUnknown) {
			return err
		}
		return deadlineOr(ctx, notAfter, err)
	}
	committed = true
	outcome = otel.OutcomeCommitted
	call.Commit()
	return nil
}

// workerBegin maps an error of tenant.Begin in a worker run.
func workerBegin(ctx context.Context, notAfter time.Time, err error) error {
	if isConflict(ctx, err) {
		return deadlineOr(ctx, notAfter, &conflictError{text: "the transaction did not begin before the deadline"})
	}
	return internalf("the transaction did not begin")
}

// failRun maps a statement error of a worker run.
func (s *state) failRun(ctx context.Context, notAfter time.Time, step string, err error) error {
	return deadlineOr(ctx, notAfter, s.fail(ctx, step, err))
}

// deadlineOr gives ErrDeadline for an error of a run under a lease that reached its deadline, and err otherwise.
func deadlineOr(ctx context.Context, notAfter time.Time, err error) error {
	if !notAfter.IsZero() && errors.Is(ctx.Err(), context.DeadlineExceeded) && !time.Now().Before(notAfter) {
		return errDeadline
	}
	return err
}

// MessageFill is the fill of a worker with a consumes trigger: it gets the decoded message, its effect name and the
// handle.
type MessageFill[M, P any] func(ctx context.Context, message M, effect string, h *Handle[P]) error

// RunConsumer calls the fill of the consuming worker w once for the claimed outbox message d, in tx, the outbox
// worker's transaction of the message's tenant, which the outbox opened with tenant.Begin and commits with the
// message's state (ST-SUP-RP01-LC19). It is the body of the destination handler of the consumed destination.
//
// The handle is bound to tx, to the tenant of the claimed row and to the worker's service account, and decides each
// resource as the service account with the worker's permission and as the staging principal, which it builds from
// the row's principal column through the sealed constructor, with the permission of the row's stager
// (ST-SUP-RP10-LC16, ST-SUP-RP10-LC17); neither comes from the fill or the payload. Before the fill it inserts the
// message identity as an idempotency key of the tenant and the service account, and returns success without calling
// the fill when that key is completed (ST-SUP-RP10-LC19); each staged effect's key derives from that identity
// (ST-SUP-RP10-LC08).
//
// It returns nil when the outbox may commit tx with the message done; an error marked with outbox.Conflict for a
// conflict, which the outbox dispatches again and does not count as an attempt (ST-SUP-RP10-LC26); an error marked
// with outbox.PermanentRefusal for a row whose stager or principal it cannot read; and an error marked with
// outbox.TransientRefusal for a fill that fails or panics, whose writes and effects roll back with tx
// (ST-SUP-RC35-OC06).
func RunConsumer[M, P any](ctx context.Context, rt *Runtime, w *Worker[P], tx *sql.Tx, d outbox.Delivery,
	decode func(payload []byte) (M, error), fill MessageFill[M, P]) (err error) {
	if rt == nil || w == nil || tx == nil || decode == nil || fill == nil || d.Tenant.IsZero() {
		return errors.New("command: RunConsumer needs the runtime, the worker, the transaction, the decoder, the fill and a claimed message")
	}
	ctx, finish := rt.span(ctx, ConsumerSpanName)
	outcome := otel.OutcomeRolledBack
	defer func() { finish(outcome, false, err != nil && !outbox.IsConflict(err)) }()

	stagingPrincipal, err := authz.StagingPrincipal(staging.NewStaging(), d.Principal)
	if err != nil {
		return outbox.PermanentRefusal(fmt.Errorf("command: %s: the message has no staging principal", w.spec.Name))
	}
	stagerPermission, ok := w.spec.Stagers[d.Origin]
	if !ok || stagerPermission == "" {
		return outbox.PermanentRefusal(fmt.Errorf("command: %s consumes no message that %q stages", w.spec.Name, d.Origin))
	}
	s := newState(ctx, rt, tx, d.Tenant, w.principal, modeConsumer, w.spec.Name, w.writes, w.destinations)
	s.request = Request{RequestID: uuid.NewString(), Key: d.ID.String()}
	s.permission = w.spec.Permission
	s.stagingPermission = stagerPermission
	s.onBehalfOf = stagingPrincipal.Subject().String()
	s.lockTimeout = DefaultLockTimeout
	message := d.ID.String()
	s.inbound = func(id string) string { return lengthPrefixed(message, id) }
	if err := s.setLockTimeout(ctx); err != nil {
		return consumerError(s.fail(ctx, "lock timeout", err))
	}
	if err := s.checkIsolation(ctx); err != nil {
		return consumerError(err)
	}
	s.ownRelations = w.own
	if s.az, err = rt.cfg.Authz.Bind(tx, d.Tenant, w.principal, w.bound); err != nil {
		return outbox.TransientRefusal(internalf("the authorization handle was not bound"))
	}
	if s.staging, err = rt.cfg.Authz.Bind(tx, d.Tenant, stagingPrincipal, nil); err != nil {
		return outbox.TransientRefusal(internalf("the authorization handle of the staging principal was not bound"))
	}
	fingerprint, err := idempotency.Fingerprint("CONSUME", d.Destination+"/"+d.Kind, d.Payload)
	if err != nil {
		return outbox.PermanentRefusal(fmt.Errorf("command: %s: the payload is not one JSON value", w.spec.Name))
	}
	call := rt.cfg.Probes.Bind(ctx, d.Tenant.ID(), w.principal.Subject().String(), w.spec.Name)
	defer call.Rollback()
	h := &Handle[P]{st: s, probe: w.spec.Probe(call.Probe())}
	wrapped, err := idempotency.Wrap(ctx, wrapTx{s: s}, idempotency.Request{Tenant: d.Tenant.ID(),
		Principal: w.principal.Subject().String(), Key: message, Fingerprint: fingerprint},
		func(ctx context.Context) (idempotency.Response, error) {
			decoded, err := decode(bytes.Clone(d.Payload))
			if err != nil {
				s.close()
				call.Returned()
				return idempotency.Response{}, &fillFailure{err: err}
			}
			_, err = callFill(ctx, s, call, func(ctx context.Context) (struct{}, error) {
				return struct{}{}, fill(ctx, decoded, d.Kind, h)
			})
			if err != nil {
				return idempotency.Response{}, err
			}
			return idempotency.Response{Status: statusOK}, nil
		})
	var failure *fillFailure
	switch {
	case errors.As(err, &failure):
		if failure.conflict {
			return outbox.Conflict(failure.err)
		}
		// A deny of the staging principal and a declared refusal give the same answer on every dispatch: the
		// message is parked, so it does not hold back its key without end (ST-SUP-RP10-LC26, ST-SUP-RC35-OC06).
		var denied *deniedError
		if (errors.As(failure.err, &denied) && denied.staging) || errors.Is(failure.err, errDeclaredRefusal) {
			return outbox.PermanentRefusal(failure.err)
		}
		return outbox.TransientRefusal(failure.err)
	case err != nil:
		return outbox.TransientRefusal(internalf("the idempotency wrapper failed"))
	case wrapped.Replayed:
		// The message was consumed already: the outbox completes it, and nothing changes a second time.
		outcome = otel.OutcomeSuccess
		return nil
	case wrapped.Problem == idempotency.ProblemInProgress:
		return outbox.Conflict(&conflictError{text: "another delivery of the message is in progress"})
	case wrapped.Problem != "" || !wrapped.Commit:
		return outbox.PermanentRefusal(fmt.Errorf("command: %s: the key of the message is used for another payload", w.spec.Name))
	}
	if rt.beforeCommit != nil {
		if err := rt.beforeCommit(ctx, tx); err != nil {
			return outbox.TransientRefusal(internalf("the step before COMMIT failed"))
		}
	}
	if err := s.setLockTimeout(ctx); err != nil {
		return consumerError(s.fail(ctx, "lock timeout", err))
	}
	outcome = otel.OutcomeCommitted
	call.Commit()
	return nil
}

// errDeclaredRefusal matches an error that Refusal marks.
var errDeclaredRefusal = errors.New("command: a declared refusal")

// declaredRefusal is a declared refusal of a fill: it matches errDeclaredRefusal and unwraps to the refusal.
type declaredRefusal struct{ err error }

func (r *declaredRefusal) Error() string        { return r.err.Error() }
func (r *declaredRefusal) Unwrap() error        { return r.err }
func (r *declaredRefusal) Is(target error) bool { return target == errDeclaredRefusal }

// Refusal marks err as a declared refusal of a fill (ST-DECL-EN18, ST-SUP-RC32-OC09): the generated refusal value
// of a command or a worker wraps itself with it. RunConsumer parks a message whose fill returns one, because a
// dispatch again gives the same refusal (ST-SUP-RP10-LC26).
func Refusal(err error) error {
	if err == nil {
		return nil
	}
	return &declaredRefusal{err: err}
}

// consumerError marks the error of a consumer's statement for the outbox.
func consumerError(err error) error {
	if errors.Is(err, ErrConflict) {
		return outbox.Conflict(err)
	}
	return outbox.TransientRefusal(err)
}
