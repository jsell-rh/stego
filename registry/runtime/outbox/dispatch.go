package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"stego.dev/runtime/tenant"
)

// State is a dispatch state of a message (ST-SUP-RP01-LC08).
type State string

// The seven dispatch states.
const (
	Pending        State = "pending"
	Dispatching    State = "dispatching"
	OutcomeUnknown State = "outcome_unknown"
	Reconciling    State = "reconciling"
	Done           State = "done"
	Parked         State = "parked"
	Discarded      State = "discarded"
)

// States returns the seven dispatch states.
func States() []State {
	return []State{Pending, Dispatching, OutcomeUnknown, Reconciling, Done, Parked, Discarded}
}

// transitions are the 15 allowed transitions of ST-SUP-RP01-LC08. Parked to done, to pending and to discarded are
// taken only by a person, through Resolve.
var transitions = map[State][]State{
	Pending:        {Dispatching},
	Dispatching:    {Done, Pending, OutcomeUnknown, Parked},
	OutcomeUnknown: {Dispatching, Reconciling, Parked},
	Reconciling:    {Done, Pending, Parked, Reconciling},
	Parked:         {Done, Pending, Discarded},
}

// Transition returns nil when a message may move from the state from to the state to, and otherwise an error that
// names both states (ST-SUP-RP01-LC08).
func Transition(from, to State) error {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("outbox: a message in %s cannot move to %s", from, to)
}

// mark is how a handler marks an error.
type mark int

const (
	markNotSent mark = iota + 1
	markTransientRefusal
	markPermanentRefusal
	markConflict
)

type markedError struct {
	mark mark
	err  error
}

func (e *markedError) Error() string {
	if e.err == nil {
		return "outbox: marked error"
	}
	return e.err.Error()
}

func (e *markedError) Unwrap() error { return e.err }

func markOf(err error) mark {
	var marked *markedError
	if errors.As(err, &marked) {
		return marked.mark
	}
	return 0
}

// NotSent marks err as a failure before the request left the process, so the destination cannot have acted. The
// message goes back to pending (ST-SUP-RP01-LC09).
func NotSent(err error) error { return &markedError{mark: markNotSent, err: err} }

// TransientRefusal marks err as a refusal of the destination that may pass, such as a 503. The message goes back
// to pending (ST-SUP-RP01-LC09).
func TransientRefusal(err error) error { return &markedError{mark: markTransientRefusal, err: err} }

// PermanentRefusal marks err as a refusal of the destination that a retry does not change, such as a 400. The
// message is parked (ST-SUP-RP01-LC09, ST-SUP-RP01-LC12).
func PermanentRefusal(err error) error { return &markedError{mark: markPermanentRefusal, err: err} }

// Conflict marks err as a conflict of a consumer, such as the error that RunConsumer returns (ST-SUP-RP10-LC26). The
// message goes back to pending with a backoff, and the first 20 conflicts in a row count no attempt
// (ST-SUP-RP01-LC18).
func Conflict(err error) error { return &markedError{mark: markConflict, err: err} }

// IsConflict reports whether err is marked as a conflict.
func IsConflict(err error) bool { return markOf(err) == markConflict }

// The conflict backoff of ST-SUP-RP01-LC18.
const (
	ConflictWaitMin  = 100 * time.Millisecond
	ConflictWaitMax  = 30 * time.Second
	FreeConflicts    = 20
	DefaultLookups   = 10
	conflictJitterOf = 10 // the jitter is at most one tenth of the wait
)

// conflictWait is the wait after the conflict number n in a row (n from 1): 100 ms that doubles at each conflict up
// to 30 s, with a random jitter of at most a tenth of the wait that never takes it past 30 s.
func conflictWait(n int64) time.Duration {
	wait := ConflictWaitMin
	for i := int64(1); i < n && wait < ConflictWaitMax; i++ {
		wait *= 2
	}
	if wait >= ConflictWaitMax {
		return ConflictWaitMax - time.Duration(rand.Int64N(int64(ConflictWaitMax/conflictJitterOf)+1))
	}
	return wait + time.Duration(rand.Int64N(int64(wait/conflictJitterOf)+1))
}

// outcome is the end of one attempt.
type outcome struct {
	to          State
	failureCode string
	conflict    bool
	notSent     bool // no request reached the destination: the attempt is not counted
}

// classify gives the state that an attempt ends in (ST-SUP-RP01-LC09). A conflict ends in pending, also when the
// deadline has passed. Otherwise a passed deadline or an error with no mark ends in outcome unknown, because the
// request may have reached the destination; only a success ends in done.
func classify(err error, expired bool) outcome {
	switch m := markOf(err); {
	case errors.Is(err, errHandlerPanic):
		return outcome{to: OutcomeUnknown, failureCode: "handler-panic"}
	case err != nil && m == markConflict:
		return outcome{to: Pending, failureCode: "delivery-conflict", conflict: true}
	case expired:
		return outcome{to: OutcomeUnknown, failureCode: "delivery-timeout"}
	case err == nil:
		return outcome{to: Done}
	case m == markNotSent:
		return outcome{to: Pending, failureCode: "delivery-not-sent"}
	case m == markTransientRefusal:
		return outcome{to: Pending, failureCode: "delivery-refused"}
	case m == markPermanentRefusal:
		return outcome{to: Parked, failureCode: "delivery-refused-permanent"}
	default:
		return outcome{to: OutcomeUnknown, failureCode: "delivery-failed"}
	}
}

// errHandlerPanic stands for a handler or lookup that panicked. It keeps no part of the panic value, which can hold
// the payload or a credential.
var errHandlerPanic = errors.New("outbox: the handler panicked")

// callHandler calls handler and turns a panic into errHandlerPanic, so the request may have reached the destination
// and the message ends in outcome unknown, not in a crashed worker.
func callHandler(ctx context.Context, handler Handler, d Delivery) (err error) {
	defer func() {
		if recover() != nil {
			err = errHandlerPanic
		}
	}()
	return handler(ctx, d)
}

// callLookup calls lookup and turns a panic into an error, which keeps the message in reconciling.
func callLookup(ctx context.Context, lookup Lookup, request LookupRequest) (status LookupStatus, err error) {
	defer func() {
		if recover() != nil {
			status, err = StatusUnknown, errHandlerPanic
		}
	}()
	return lookup(ctx, request)
}

// LookupStatus is the answer of a status lookup.
type LookupStatus int

// The three answers of a status lookup (ST-SUP-RP01-LC11).
const (
	StatusUnknown LookupStatus = iota
	StatusActed
	StatusNotActed
)

// LookupRequest is what a status lookup gets: the message identity and its outgoing key.
type LookupRequest struct {
	ID          uuid.UUID
	OutgoingKey string
	Destination string
	Tenant      tenant.Tenant
}

// Lookup asks a destination whether it acted on a message (ST-SUP-RP01-LC11).
type Lookup func(ctx context.Context, request LookupRequest) (LookupStatus, error)

// step takes the next step for one claimed message.
func (w *Worker) step(ctx context.Context, d Delivery) {
	reg, ok := w.destinations[d.Destination]
	switch d.Receipt.State {
	case Dispatching:
		if !ok {
			w.unknownDestinations.Add(1)
			w.finish(ctx, d, Pending, change{attempts: -1, delay: w.unknownDelay(), failureCode: "destination-unknown"})
			return
		}
		w.dispatch(ctx, d, reg)
	case OutcomeUnknown:
		if !ok {
			w.unknownDestinations.Add(1)
			w.releaseLease(ctx, d, w.unknownDelay(), "destination-unknown")
			return
		}
		w.afterUnknown(ctx, d, reg)
	case Reconciling:
		if !ok {
			w.unknownDestinations.Add(1)
			w.releaseLease(ctx, d, w.unknownDelay(), "destination-unknown")
			return
		}
		w.reconcile(ctx, d, reg)
	}
}

// dispatch sends a dispatching message to its handler and moves it to the state the attempt ends in.
func (w *Worker) dispatch(ctx context.Context, d Delivery, reg Destination) {
	if ctx.Err() != nil {
		w.finish(ctx, d, Pending, change{attempts: -1, failureCode: "worker-stopping"})
		return
	}
	attempt, cancel := context.WithTimeout(ctx, w.config.AttemptTimeout)
	err := callHandler(attempt, reg.Handler, d)
	expired := errors.Is(attempt.Err(), context.DeadlineExceeded)
	cancel()
	result := classify(err, expired)
	switch result.to {
	case Done:
		w.finish(ctx, d, Done, doneChange(d.Receipt.State))
	case Pending:
		if result.conflict {
			w.conflicts.Add(1)
			n := d.Conflicts + 1
			c := change{conflict: true, delay: conflictWait(n), failureCode: result.failureCode}
			if n <= FreeConflicts {
				c.attempts = -1
			} else if d.Attempts >= w.config.MaxAttempts {
				w.park(ctx, d, change{conflict: true, failureCode: "attempts-exhausted"})
				return
			}
			w.finish(ctx, d, Pending, c)
			return
		}
		w.deliveryFailures.Add(1)
		if d.Attempts >= w.config.MaxAttempts {
			w.park(ctx, d, change{failureCode: "attempts-exhausted"})
			return
		}
		w.finish(ctx, d, Pending, change{delay: w.retryDelay(d.Attempts), failureCode: result.failureCode})
	case OutcomeUnknown:
		w.deliveryFailures.Add(1)
		w.unknownOutcomes.Add(1)
		w.finish(ctx, d, OutcomeUnknown, change{failureCode: result.failureCode})
	case Parked:
		w.deliveryFailures.Add(1)
		w.park(ctx, d, change{failureCode: result.failureCode})
	}
}

// afterUnknown takes the next step of a message in outcome unknown (ST-SUP-RP01-LC10, ST-SUP-RP01-LC11,
// ST-SUP-RP01-LC12): it sends the message again with the same outgoing key only to an idempotent destination, moves
// it to reconciling for a destination with a status lookup, due after the attempt deadline plus the settle time,
// and parks it otherwise.
func (w *Worker) afterUnknown(ctx context.Context, d Delivery, reg Destination) {
	switch {
	case reg.Idempotent:
		if ctx.Err() != nil {
			// The worker stops: the message keeps its outcome unknown for the next claim.
			w.releaseLease(ctx, d, 0, "worker-stopping")
			return
		}
		if d.Attempts >= w.config.MaxAttempts {
			w.park(ctx, d, change{failureCode: "attempts-exhausted"})
			return
		}
		ok, m, err := w.queue.move(ctx, &d.Receipt, Dispatching, change{attempts: 1, failureCode: "resend",
			lease: w.config.LeaseDuration})
		if !w.counted(ok, err) {
			return
		}
		d.Attempts = m.attempts
		d.Conflicts = m.conflicts
		w.dispatch(ctx, d, reg)
	case reg.Lookup != nil:
		due := d.DispatchedAt.Add(w.config.AttemptTimeout + reg.SettleTime)
		w.finish(ctx, d, Reconciling, change{dueAt: &due, failureCode: "outcome-unknown"})
	default:
		w.park(ctx, d, change{failureCode: "outcome-unknown"})
	}
}

// reconcile asks the status lookup of a reconciling message and moves it as the answer says (ST-SUP-RP01-LC11).
func (w *Worker) reconcile(ctx context.Context, d Delivery, reg Destination) {
	if reg.Lookup == nil {
		w.park(ctx, d, change{failureCode: "outcome-unknown"})
		return
	}
	if ctx.Err() != nil {
		w.releaseLease(ctx, d, 0, "worker-stopping")
		return
	}
	attempt, cancel := context.WithTimeout(ctx, w.config.AttemptTimeout)
	status, err := callLookup(attempt, reg.Lookup, LookupRequest{ID: d.ID, OutgoingKey: d.OutgoingKey,
		Destination: d.Destination, Tenant: d.Tenant})
	cancel()
	w.lookups.Add(1)
	switch {
	case err == nil && status == StatusActed:
		w.finish(ctx, d, Done, change{lookup: true})
	case err == nil && status == StatusNotActed:
		w.finish(ctx, d, Pending, change{lookup: true, failureCode: "lookup-not-acted"})
	default:
		if d.Lookups+1 >= int64(w.config.LookupBound) {
			w.park(ctx, d, change{lookup: true, failureCode: "lookups-exhausted"})
			return
		}
		code := "lookup-unknown"
		if err != nil {
			code = "lookup-failed"
		}
		w.finish(ctx, d, Reconciling, change{lookup: true, delay: w.retryDelay(d.Lookups + 1), failureCode: code})
	}
}

// finish moves a claimed message under its lease. On done it records the delivery delay (ST-SUP-RP01-LC07).
func (w *Worker) finish(ctx context.Context, d Delivery, to State, c change) {
	// A completed delivery is still recorded during shutdown; a database result that is not known keeps the lease.
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	ok, m, err := w.queue.move(finish, &d.Receipt, to, c)
	if !w.counted(ok, err) {
		return
	}
	switch to {
	case Done:
		w.delivered.Add(1)
		w.metrics.delivered(finish, d.Destination, m.delay)
	case Pending:
		w.retried.Add(1)
	}
}

// park moves a claimed message to parked, records the state it left, and raises one alert event and metric that
// name the destination and that state (ST-SUP-RP01-LC12).
func (w *Worker) park(ctx context.Context, d Delivery, c change) {
	from := d.Receipt.State
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	ok, _, err := w.queue.move(finish, &d.Receipt, Parked, c)
	if !w.counted(ok, err) {
		return
	}
	w.parked.Add(1)
	w.metrics.parked(finish, d.Destination, from)
}

func (w *Worker) releaseLease(ctx context.Context, d Delivery, delay time.Duration, code string) {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	ok, err := w.queue.release(finish, d.Receipt, delay, code)
	w.counted(ok, err)
}

// counted counts a database failure or a lost lease, and reports whether the move took place.
func (w *Worker) counted(ok bool, err error) bool {
	if err != nil {
		w.databaseFailures.Add(1)
		return false
	}
	if !ok {
		w.lostLeases.Add(1)
		return false
	}
	return true
}

// Resolution is the choice of a person for one parked message (ST-SUP-RP01-LC13).
type Resolution string

// The three resolutions.
const (
	Acted    Resolution = "acted"
	NotActed Resolution = "not_acted"
	Discard  Resolution = "discarded"
)

// resource is the prefix of the resource that Resolve asks the decision port about (ST-SUP-RP01-LC17).
const resource = "outbox_destination:"

// DecisionRequest is one question to the decision port: may Subject do Permission on Resource. The port decides at
// the consistency floor of ST-SUP-RP06-LC09: an outbox message carries no row token, so the floor is the service's
// revocation floor (ST-SUP-RP06-LC13), which the port holds.
type DecisionRequest struct {
	Subject    string
	Permission string
	Resource   string
	Tenant     tenant.Tenant
}

// Decider is the decision port that the worker is built with (ST-SUP-RP01-LC17). A deny is allowed false with no
// error; an error is not a decision.
type Decider interface {
	Decide(ctx context.Context, request DecisionRequest) (allowed bool, err error)
}

// AuditEntry is the audit row of one resolution.
type AuditEntry struct {
	MessageID   uuid.UUID
	Tenant      tenant.Tenant
	Destination string
	Subject     string
	Resolution  Resolution
	At          time.Time
}

// Auditor writes the audit row of a resolution through the caller's transaction tx, so it commits with the state
// change or not at all (ST-SUP-RP01-LC13). It must not commit.
type Auditor interface {
	Record(ctx context.Context, tx *sql.Tx, entry AuditEntry) error
}

// ResolveRequest is one resolution of a parked message by a person.
type ResolveRequest struct {
	ID         uuid.UUID
	Subject    string
	Resolution Resolution
}

// ErrNotParked is the error of Resolve for a message that is not parked.
var ErrNotParked = errors.New("outbox: only a parked message can be resolved")

// ErrNotAllowed is the error of Resolve when the decision port does not allow the resolution.
var ErrNotAllowed = errors.New("outbox: the subject may not resolve messages of this destination")

// Resolve applies a person's resolution of one parked message in the caller's transaction tx (ST-SUP-RP01-LC13).
// It asks the decision port itself whether the subject has the permission resolve on
// outbox_destination:<destination>, and refuses a deny, a port error or a worker with no port (ST-SUP-RP01-LC17);
// it takes no decision from its caller. acted moves the message to done, not acted to pending with the same
// identity and outgoing key, and discarded to discarded, which releases its key. For an idempotent destination it
// accepts only not acted, or discarded when the destination must land; discarded is refused for a destination that
// need not land. The audit row is written in tx with the state change. A refused resolution changes nothing. The
// caller commits tx, with tenant.Commit when tenant.Begin opened it.
func (w *Worker) Resolve(ctx context.Context, tx *sql.Tx, request ResolveRequest) error {
	if w.decider == nil {
		return errors.New("outbox: the worker has no decision port, so it refuses every resolution")
	}
	if w.auditor == nil {
		return errors.New("outbox: the worker has no auditor, so it refuses every resolution")
	}
	if tx == nil || request.ID == uuid.Nil || request.Subject == "" || !validText(request.Subject, MaxKeyBytes) {
		return errors.New("outbox: a resolution needs a transaction, a message ID and a subject")
	}
	if request.Resolution != Acted && request.Resolution != NotActed && request.Resolution != Discard {
		return fmt.Errorf("outbox: %q is not a resolution", request.Resolution)
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	rows, err := tx.QueryContext(ctx, `SELECT tenant_id, destination, state FROM stego_outbox.messages WHERE id = $1`,
		request.ID)
	if err != nil {
		return canceled(ctx, err)
	}
	var destination, state string
	var owner tenant.Tenant
	found := rows.Next()
	if found {
		owner, err = tenant.ScanRows(rows, &destination, &state)
	}
	closeErr := rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return canceled(ctx, err)
	}
	if !found {
		return errors.New("outbox: no message has this ID")
	}
	if State(state) != Parked {
		return ErrNotParked
	}
	allowed, err := w.decider.Decide(ctx, DecisionRequest{Subject: request.Subject, Permission: "resolve",
		Resource: resource + destination, Tenant: owner})
	if err != nil {
		return fmt.Errorf("outbox: the decision port failed, so the resolution is refused: %w", err)
	}
	if !allowed {
		return ErrNotAllowed
	}
	reg, ok := w.destinations[destination]
	if !ok {
		return fmt.Errorf("outbox: the destination %s is not registered on this worker", destination)
	}
	switch {
	case request.Resolution == Discard && !reg.MustLand:
		return fmt.Errorf("outbox: a message of %s, which need not land, cannot be discarded", destination)
	case request.Resolution == Acted && reg.Idempotent:
		return fmt.Errorf("outbox: a message of the idempotent destination %s is resolved as not acted or discarded", destination)
	}
	to := map[Resolution]State{Acted: Done, NotActed: Pending, Discard: Discarded}[request.Resolution]
	if err := Transition(Parked, to); err != nil {
		return err
	}
	var at time.Time
	var seconds float64
	err = tx.QueryRowContext(ctx, `WITH `+nowCTE+`
		UPDATE stego_outbox.messages AS m SET state = $3, resolution = $4, resolved_by = $5, resolved_at = now.t,
			parked_from = NULL, parked_at = NULL, available_at = now.t, failure_code = '',
			attempts = CASE WHEN $3 = 'pending' THEN 0 ELSE m.attempts END, conflicts = 0, lookups = 0,
			done_at = CASE WHEN $3 = 'done' THEN now.t ELSE m.done_at END
		FROM now WHERE m.id = $2 AND m.state = 'parked'
		RETURNING now.t, pg_catalog.date_part('epoch', now.t - m.created_at)::float8`,
		w.queue.now(), request.ID, string(to), string(request.Resolution), request.Subject).Scan(&at, &seconds)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotParked
	}
	if err != nil {
		return canceled(ctx, err)
	}
	if err := w.auditor.Record(ctx, tx, AuditEntry{MessageID: request.ID, Tenant: owner, Destination: destination,
		Subject: request.Subject, Resolution: request.Resolution, At: at}); err != nil {
		return fmt.Errorf("outbox: write the audit row of the resolution: %w", err)
	}
	if to == Done {
		// A message resolved as acted reaches done; its delay is recorded here, before the caller commits
		// (ST-SUP-RP01-LC07). A caller that rolls back leaves one observation of a message that is not done.
		w.metrics.delivered(ctx, destination, time.Duration(seconds*float64(time.Second)))
	}
	return nil
}
