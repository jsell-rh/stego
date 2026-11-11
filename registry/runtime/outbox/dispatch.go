package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
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

// IsPermanent reports whether err is marked as a permanent refusal, which parks the message.
func IsPermanent(err error) bool { return markOf(err) == markPermanentRefusal }

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

// callHandler calls handler in the message's tenant transaction tx and turns a panic into errHandlerPanic, so the
// request may have reached the destination, the message ends in outcome unknown, and the worker keeps running
// (ST-SUP-RP01-LC27).
func callHandler(ctx context.Context, handler Handler, tx *sql.Tx, d Delivery) (err error) {
	defer func() {
		if recover() != nil {
			err = errHandlerPanic
		}
	}()
	return handler(ctx, tx, d)
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

// StateChange is one change of a message's state that a destination's state hook gets (ST-SUP-RP01-LC25).
type StateChange struct {
	ID          uuid.UUID
	Group       string
	Destination string
	Tenant      tenant.Tenant
	State       State
}

// StateHook follows the state of a destination's messages. The worker calls it in the transaction of each change of
// a message to done, parked, pending from parked, or discarded, in the message's tenant; an error rolls the change
// back (ST-SUP-RP01-LC25). It must not commit.
type StateHook func(ctx context.Context, tx *sql.Tx, change StateChange) error

// hooked reports whether a change from the state from to the state to calls the state hook.
func hooked(from, to State) bool {
	switch to {
	case Done, Parked, Discarded:
		return true
	case Pending:
		return from == Parked
	}
	return false
}

// step takes the next step for one claimed message.
func (w *Worker) step(ctx context.Context, d Delivery) {
	reg, ok := w.destinations[d.Destination]
	switch d.Receipt.State {
	case Dispatching:
		if !ok {
			// No request is sent: the attempt is taken back (ST-SUP-RP01-LC22).
			w.unknownDestinations.Add(1)
			w.finish(ctx, d, reg, Pending, change{attempts: -1, delay: w.unknownDelay(), failureCode: "destination-unknown"})
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

// exhausted reports whether the attempts of d reached the limit for a destination that may park on it: a
// destination registered as must land is never parked for its attempt count (ST-SUP-RP01-LC12, ST-SUP-RP01-LC23).
func (w *Worker) exhausted(d Delivery, reg Destination) bool {
	return !reg.MustLand && d.Attempts >= w.config.MaxAttempts
}

// dispatch sends a dispatching message to its handler in a transaction of the message's tenant, and moves it to the
// state the attempt ends in. A success moves it to done in the handler's transaction and commits both together; any
// other end rolls the handler's transaction back and moves the message in a new one (ST-SUP-RP01-LC19).
func (w *Worker) dispatch(ctx context.Context, d Delivery, reg Destination) {
	if ctx.Err() != nil {
		// The worker stops before it sends: the attempt is taken back (ST-SUP-RP01-LC22).
		w.finish(ctx, d, reg, Pending, change{attempts: -1, failureCode: "worker-stopping"})
		return
	}
	// The handler runs only while the lease, measured on the local monotonic clock from the claim, holds the attempt
	// and the two statements after it; else the message goes back to pending unsent (ST-SUP-RP01-LC02).
	left := time.Until(d.leaseEnd)
	if left < w.config.AttemptTimeout+2*operationTimeout {
		w.lostLeases.Add(1)
		w.finish(ctx, d, reg, Pending, change{attempts: -1, failureCode: "lease-short"})
		return
	}
	// The transaction, its begin included, ends with the lease, so nothing of the attempt commits after the lease
	// is gone.
	txCtx, txCancel := context.WithTimeout(context.WithoutCancel(ctx), left)
	defer txCancel()
	tx, err := tenant.Begin(txCtx, w.queue.db, d.Tenant, nil)
	if err == nil {
		if left = time.Until(d.leaseEnd); left < w.config.AttemptTimeout+2*operationTimeout {
			_ = tx.Rollback()
			w.lostLeases.Add(1)
			w.finish(ctx, d, reg, Pending, change{attempts: -1, failureCode: "lease-short"})
			return
		}
	}
	if err != nil {
		// No request is sent, but a failed begin is not one of the takebacks of ST-SUP-RP01-LC22.
		w.databaseFailures.Add(1)
		w.finish(ctx, d, reg, Pending, change{delay: w.retryDelay(d.Attempts), failureCode: "database-failed"})
		return
	}
	deadline := min(w.config.AttemptTimeout, left-2*operationTimeout)
	attempt, cancel := context.WithTimeout(ctx, deadline)
	err = callHandler(attempt, reg.Handler, tx, d)
	expired := errors.Is(attempt.Err(), context.DeadlineExceeded)
	cancel()
	result := classify(err, expired)
	if result.to == Done {
		w.apply(ctx, tx, d, reg, Done, change{})
		return
	}
	_ = tx.Rollback()
	switch result.to {
	case Pending:
		if result.conflict {
			w.conflicts.Add(1)
			n := d.Conflicts + 1
			c := change{conflict: true, delay: conflictWait(n), failureCode: result.failureCode}
			if n <= FreeConflicts {
				c.attempts = -1
			} else if w.exhausted(d, reg) {
				w.park(ctx, d, reg, change{conflict: true, failureCode: "attempts-exhausted"})
				return
			}
			w.finish(ctx, d, reg, Pending, c)
			return
		}
		w.deliveryFailures.Add(1)
		if w.exhausted(d, reg) {
			w.park(ctx, d, reg, change{failureCode: "attempts-exhausted"})
			return
		}
		w.finish(ctx, d, reg, Pending, change{delay: w.retryDelay(d.Attempts), failureCode: result.failureCode})
	case OutcomeUnknown:
		w.deliveryFailures.Add(1)
		w.unknownOutcomes.Add(1)
		w.finish(ctx, d, reg, OutcomeUnknown, change{failureCode: result.failureCode})
	case Parked:
		w.deliveryFailures.Add(1)
		w.park(ctx, d, reg, change{failureCode: result.failureCode})
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
		if w.exhausted(d, reg) {
			w.park(ctx, d, reg, change{failureCode: "attempts-exhausted"})
			return
		}
		sent := time.Now()
		ok, m := w.resend(ctx, &d)
		if !ok {
			return
		}
		d.leaseEnd = sent.Add(w.config.LeaseDuration)
		d.Attempts = m.attempts
		d.Conflicts = m.conflicts
		w.dispatch(ctx, d, reg)
	case reg.Lookup != nil:
		due := d.DispatchedAt.Add(w.config.AttemptTimeout + reg.SettleTime)
		w.finish(ctx, d, reg, Reconciling, change{dueAt: &due, failureCode: "outcome-unknown"})
	default:
		w.park(ctx, d, reg, change{failureCode: "outcome-unknown"})
	}
}

// resend moves a message in outcome unknown back to dispatching with a new lease and one more attempt, in a
// transaction of its tenant.
func (w *Worker) resend(ctx context.Context, d *Delivery) (bool, moved) {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	tx, err := tenant.Begin(finish, w.queue.db, d.Tenant, nil)
	if err != nil {
		w.databaseFailures.Add(1)
		return false, moved{}
	}
	defer func() { _ = tx.Rollback() }()
	ok, m, err := w.queue.move(finish, tx, &d.Receipt, Dispatching, change{attempts: 1, failureCode: "resend",
		lease: w.config.LeaseDuration})
	if !w.counted(ok, err) {
		return false, moved{}
	}
	if err := tenant.Commit(finish, tx, d.Tenant); err != nil {
		w.databaseFailures.Add(1)
		return false, moved{}
	}
	return true, m
}

// reconcile asks the status lookup of a reconciling message and moves it as the answer says (ST-SUP-RP01-LC11).
func (w *Worker) reconcile(ctx context.Context, d Delivery, reg Destination) {
	if reg.Lookup == nil {
		w.park(ctx, d, reg, change{failureCode: "outcome-unknown"})
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
		w.finish(ctx, d, reg, Done, change{lookup: true})
	case err == nil && status == StatusNotActed:
		w.finish(ctx, d, reg, Pending, change{lookup: true, failureCode: "lookup-not-acted"})
	default:
		if d.Lookups+1 >= int64(w.config.LookupBound) {
			w.park(ctx, d, reg, change{lookup: true, failureCode: "lookups-exhausted"})
			return
		}
		code := "lookup-unknown"
		if err != nil {
			code = "lookup-failed"
		}
		w.finish(ctx, d, reg, Reconciling, change{lookup: true, delay: w.retryDelay(d.Lookups + 1), failureCode: code})
	}
}

// finish moves a claimed message under its lease in a new transaction of its tenant.
func (w *Worker) finish(ctx context.Context, d Delivery, reg Destination, to State, c change) {
	w.apply(ctx, nil, d, reg, to, c)
}

// apply moves a claimed message under its lease in tx, or in a new transaction of the message's tenant when tx is
// nil, calls the destination's state hook in the same transaction, and commits with tenant.Commit. A failed move,
// hook or commit rolls everything back and changes nothing. On done it records the delivery delay
// (ST-SUP-RP01-LC07); on parked it raises the alert (ST-SUP-RP01-LC12). It reports whether the move committed.
func (w *Worker) apply(ctx context.Context, tx *sql.Tx, d Delivery, reg Destination, to State, c change) bool {
	// A completed delivery is still recorded during shutdown; a database result that is not known keeps the lease.
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	from := d.Receipt.State
	if tx == nil {
		var err error
		if tx, err = tenant.Begin(finish, w.queue.db, d.Tenant, nil); err != nil {
			w.databaseFailures.Add(1)
			return false
		}
	}
	defer func() { _ = tx.Rollback() }()
	ok, m, err := w.queue.move(finish, tx, &d.Receipt, to, c)
	if !w.counted(ok, err) {
		return false
	}
	if reg.StateHook != nil && hooked(from, to) {
		if err := callHook(finish, reg.StateHook, tx, StateChange{ID: d.ID, Group: d.Group, Destination: d.Destination,
			Tenant: d.Tenant, State: to}); err != nil {
			_ = tx.Rollback()
			// The rollback undid the move: the message is in its old state under the same lease.
			d.Receipt.State = from
			w.hookFailed(ctx, d, reg, from)
			return false
		}
	}
	if err := tenant.Commit(finish, tx, d.Tenant); err != nil {
		w.databaseFailures.Add(1)
		return false
	}
	switch to {
	case Done:
		w.delivered.Add(1)
		w.metrics.delivered(finish, d.Destination, m.delay)
	case Pending:
		w.retried.Add(1)
	case Parked:
		w.parked.Add(1)
		w.metrics.parked(finish, d.Destination, from)
	}
	return true
}

// park moves a claimed message to parked, records the state it left, and raises one alert event and metric that
// name the destination and that state (ST-SUP-RP01-LC12).
func (w *Worker) park(ctx context.Context, d Delivery, reg Destination, c change) {
	w.apply(ctx, nil, d, reg, Parked, c)
}

// releaseLease ends the lease of a claimed message in outcome unknown or reconciling and keeps its state, in a
// transaction of the message's tenant.
func (w *Worker) releaseLease(ctx context.Context, d Delivery, delay time.Duration, code string) {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	tx, err := tenant.Begin(finish, w.queue.db, d.Tenant, nil)
	if err != nil {
		w.databaseFailures.Add(1)
		return
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := w.queue.release(finish, tx, d.Receipt, delay, code)
	if !w.counted(ok, err) {
		return
	}
	if err := tenant.Commit(finish, tx, d.Tenant); err != nil {
		w.databaseFailures.Add(1)
	}
}

// callHook calls a state hook and turns a panic into an error, so a hook never stops the worker.
func callHook(ctx context.Context, hook StateHook, tx *sql.Tx, change StateChange) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("outbox: the state hook panicked")
		}
	}()
	return hook(ctx, tx, change)
}

// hookFailed counts and alerts on a state hook that refused a change, which is rolled back (ST-SUP-RP01-LC25), and
// lets the message wait out a backoff instead of being claimed again at once. A message that was dispatching
// goes to outcome unknown, because its handler may have acted; one in outcome unknown or reconciling keeps its
// state.
func (w *Worker) hookFailed(ctx context.Context, d Delivery, reg Destination, from State) {
	w.hookFailures.Add(1)
	w.metrics.hookFailed(ctx, d.Destination)
	delay := w.retryDelay(max(d.Attempts, 1))
	switch from {
	case Dispatching:
		w.apply(ctx, nil, d, reg, OutcomeUnknown, change{delay: delay, failureCode: "hook-failed"})
	case OutcomeUnknown, Reconciling:
		w.releaseLease(ctx, d, delay, "hook-failed")
	}
}

// counted counts a database failure or a lost lease, and reports whether the move took place. A message that moved
// on from the receipt's state, such as one another holder finished, is a lost lease, not a database failure.
func (w *Worker) counted(ok bool, err error) bool {
	var movedOn *movedOnError
	if errors.As(err, &movedOn) {
		w.lostLeases.Add(1)
		return false
	}
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

// Principal is who resolves a message: a principal of the authorization runtime, such as authz.Person makes. Only
// the authorization runtime makes one with a subject (ST-SUP-RP01-LC17), so a caller cannot name a subject of its
// own choice.
type Principal = seal.Principal

// ResolveRequest is one resolution of a parked message by a person: the message, the destination that the caller
// names for it, such as the list of dead letters gives, the principal of the person and the chosen resolution.
type ResolveRequest struct {
	ID          uuid.UUID
	Destination string
	Principal   Principal
	Resolution  Resolution
}

// ErrNotAllowed is the error of Resolve when the decision port does not allow the principal to resolve messages of
// the named destination. Resolve decides before it reads the message, so the answer is the same for every message:
// one that is not there, one of another tenant or destination, and one in any state.
var ErrNotAllowed = errors.New("outbox: the subject may not resolve messages of this destination")

// ErrNotFound is the error of Resolve, after an allow, for a message that the transaction's tenant does not hold
// under the named destination.
var ErrNotFound = errors.New("outbox: the tenant of the transaction holds no message with this ID and destination")

// ErrNotParked is the error of Resolve, after an allow, for a message that is not parked.
var ErrNotParked = errors.New("outbox: only a parked message can be resolved")

// Resolve applies a person's resolution of one parked message in the caller's transaction tx (ST-SUP-RP01-LC13).
// It takes the subject only from a principal of the authorization runtime. Before it reads the message, it asks the
// decision port itself whether the subject has the permission resolve on outbox_destination:<destination>, for the
// destination that the request names and the tenant of tx, and refuses a deny, a port error or a worker with no port
// (ST-SUP-RP01-LC17); it takes no decision from its caller. So a refusal of the permission gives one answer for every
// message, and only an allowed caller learns whether the message is there and parked. acted moves the message to done, not acted to pending with the same
// identity and outgoing key, and discarded to discarded, which releases its key. For an idempotent destination it
// accepts only not acted, or discarded when the destination must land; discarded is refused for a destination that
// need not land. The audit row and the destination's state hook are written in tx with the state change. A refused
// resolution changes nothing. tx is a transaction of one tenant, which tenant.Begin opened; Resolve runs under the
// tenant policy, so it answers ErrNotFound for a message of another tenant (ST-SUP-RP01-LC19). The caller commits
// tx with tenant.Commit.
//
// Resolve returns a Resolved. The caller calls its Committed after its own commit of tx succeeds, and only then, so
// a message resolved as acted records its delivery delay only once it is done (ST-SUP-RP01-LC07).
func (w *Worker) Resolve(ctx context.Context, tx *sql.Tx, request ResolveRequest) (Resolved, error) {
	var resolved Resolved
	err := w.resolve(ctx, tx, request, &resolved)
	if err != nil {
		return Resolved{}, err
	}
	return resolved, nil
}

// Resolved is a resolution that Resolve applied in the caller's transaction.
type Resolved struct {
	worker      *Worker
	destination string
	delay       time.Duration
	done        bool
}

// Committed records what the resolution changed once the caller's transaction committed: for a message resolved
// as acted, its delivery delay under its destination. A caller that rolls back does not call it. A second call
// records nothing.
func (r *Resolved) Committed(ctx context.Context) {
	if r == nil || r.worker == nil || !r.done {
		return
	}
	r.done = false
	r.worker.metrics.delivered(ctx, r.destination, r.delay)
}

func (w *Worker) resolve(ctx context.Context, tx *sql.Tx, request ResolveRequest, out *Resolved) error {
	if w.decider == nil {
		return errors.New("outbox: the worker has no decision port, so it refuses every resolution")
	}
	if w.auditor == nil {
		return errors.New("outbox: the worker has no auditor, so it refuses every resolution")
	}
	subject, ok := seal.SubjectOf(request.Principal)
	if !ok || !validText(subject, MaxKeyBytes) {
		return errors.New("outbox: a resolution needs a principal of the authorization runtime")
	}
	if tx == nil || request.ID == uuid.Nil || !labelPattern.MatchString(request.Destination) {
		return errors.New("outbox: a resolution needs a transaction, a message ID and a destination name")
	}
	if request.Resolution != Acted && request.Resolution != NotActed && request.Resolution != Discard {
		return fmt.Errorf("outbox: %q is not a resolution", request.Resolution)
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	destination := request.Destination
	// The tenant of the decision is the tenant of tx, which tenant.Begin set; the message is read only after the
	// decision, under the tenant policy, so a message of another tenant is not found (ST-SUP-RP01-LC19).
	owner, err := scanTenant(ctx, tx, `SELECT pg_catalog.current_setting('app.tenant', true)`)
	if err != nil {
		return err
	}
	allowed, err := w.decider.Decide(ctx, DecisionRequest{Subject: subject, Permission: "resolve",
		Resource: resource + destination, Tenant: owner})
	if err != nil {
		return fmt.Errorf("outbox: the decision port failed, so the resolution is refused: %w", err)
	}
	if !allowed {
		return ErrNotAllowed
	}
	var state, group string
	err = tx.QueryRowContext(ctx, `SELECT state, group_id FROM stego_outbox.messages WHERE id = $1 AND destination = $2`,
		request.ID, destination).Scan(&state, &group)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return canceled(ctx, err)
	}
	if State(state) != Parked {
		return ErrNotParked
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
	// The state changes only through the relay function resolve, which requires the transaction's tenant
	// (ST-SUP-RP01-LC19).
	err = tx.QueryRowContext(ctx, `SELECT o_resolved_at, o_age_seconds FROM stego_outbox.resolve($1, $2, $3, $4, $5)`,
		w.queue.now(), request.ID, string(to), string(request.Resolution), subject).Scan(&at, &seconds)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotParked
	}
	if err != nil {
		return canceled(ctx, err)
	}
	if reg.StateHook != nil {
		if err := callHook(ctx, reg.StateHook, tx, StateChange{ID: request.ID, Group: group, Destination: destination,
			Tenant: owner, State: to}); err != nil {
			w.hookFailures.Add(1)
			w.metrics.hookFailed(ctx, destination)
			return fmt.Errorf("outbox: the state hook of %s refused the resolution: %w", destination, err)
		}
	}
	// The audit row is the last write before the caller's commit, as the audit runtime asks (ST-SUP-RP05-LC12).
	if err := w.auditor.Record(ctx, tx, AuditEntry{MessageID: request.ID, Tenant: owner, Destination: destination,
		Subject: subject, Resolution: request.Resolution, At: at}); err != nil {
		return fmt.Errorf("outbox: write the audit row of the resolution: %w", err)
	}
	*out = Resolved{worker: w, destination: destination, delay: time.Duration(seconds * float64(time.Second)),
		done: to == Done}
	return nil
}

// scanTenant reads the tenant of one row of one column that query gives in tx, through tenant.ScanRows, and refuses
// no row or an empty tenant.
func scanTenant(ctx context.Context, tx *sql.Tx, query string, args ...any) (tenant.Tenant, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return tenant.Tenant{}, canceled(ctx, err)
	}
	var t tenant.Tenant
	if rows.Next() {
		t, err = tenant.ScanRows(rows)
	} else {
		err = errors.New("outbox: the transaction has no tenant")
	}
	closeErr := rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return tenant.Tenant{}, canceled(ctx, err)
	}
	return t, nil
}
