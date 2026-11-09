package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"stego.dev/runtime/tenant"
)

// Tx is the part of a store transaction that the package uses (ST-STD-RUNTIME-R06). *sql.Tx satisfies it. The
// package never commits or rolls back a Tx it is given.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Enqueuer writes one outbox message, with its tenant on the outbox row, inside the transaction tx. The outbox
// runtime's queue satisfies it; the package names only the part it uses.
type Enqueuer interface {
	Enqueue(ctx context.Context, tx Tx, tenant, destination, key string, payload []byte) error
}

// StatementError is the error of a statement of the package. It names the step and the SQLSTATE and holds no
// field value and no text of the database (ST-SUP-RP05-LC13). It gives the SQLSTATE through SQLState, as a
// database driver does, so the command runtime can tell a conflict (ST-SUP-RP10-LC25).
type StatementError struct {
	Step string
	Code string
}

func (e *StatementError) Error() string {
	if e.Code == "" {
		return "audit: the " + e.Step + " statement failed"
	}
	return "audit: the " + e.Step + " statement failed with SQLSTATE " + e.Code
}

// SQLState returns the SQLSTATE of the database error, or empty.
func (e *StatementError) SQLState() string { return e.Code }

// stmtError turns a database error into an error with no value: a cancel or a deadline stays a context error.
func stmtError(ctx context.Context, step string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("audit: the %s statement: %w", step, ctxErr)
	}
	return &StatementError{Step: step, Code: sqlState(err)}
}

// sqlState returns the SQLSTATE of a database error, as the PostgreSQL drivers give it, or empty.
func sqlState(err error) string {
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		return coded.SQLState()
	}
	return ""
}

// Writer writes audit rows and enqueues each of them for the audit sink.
type Writer struct {
	outbox         Enqueuer
	now            func() time.Time
	failedOutcomes atomic.Int64

	mu       sync.Mutex
	recorded map[string]time.Time // request IDs that called Record, for ErrReadAfterRecord
}

// ErrReadAfterRecord is the error of RecordRead for a request whose command already called Record: the command
// holds the chain lock until it commits, so the read row would wait on its own command. A read row of a fill is
// written before the command's Record (ST-SUP-RP05-LC12).
var ErrReadAfterRecord = errors.New("audit: RecordRead after Record in the same request")

// recordedTTL is how long a Writer remembers that a request called Record.
const recordedTTL = 10 * time.Minute

func (w *Writer) markRecorded(requestID string, now time.Time) {
	if requestID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, at := range w.recorded {
		if now.Sub(at) > recordedTTL {
			delete(w.recorded, id)
		}
	}
	w.recorded[requestID] = now
}

func (w *Writer) hasRecorded(requestID string, now time.Time) bool {
	if requestID == "" {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	at, ok := w.recorded[requestID]
	return ok && now.Sub(at) <= recordedTTL
}

// NewWriter returns a Writer that enqueues sink messages through outbox and reads the occurred time from now; a
// nil now is time.Now.
func NewWriter(outbox Enqueuer, now func() time.Time) (*Writer, error) {
	if outbox == nil {
		return nil, errors.New("audit: a writer needs an outbox")
	}
	if now == nil {
		now = time.Now
	}
	return &Writer{outbox: outbox, now: now, recorded: map[string]time.Time{}}, nil
}

// FailedOutcomes returns how many RecordOutcome calls failed (ST-SUP-RP05-LC13).
func (w *Writer) FailedOutcomes() int64 { return w.failedOutcomes.Load() }

// Record writes all the audit events of one transaction as one batch through the caller's transaction tx and
// enqueues each for the audit sink in the same transaction, keyed by its ID (ST-SUP-RP05-LC02,
// ST-SUP-RP05-LC07, ST-SUP-RP05-LC12). The caller calls it once, as its last statement before COMMIT
// (ST-SUP-RP10-LC06): Record locks the head of the tenant's chain once, so that lock is the last one the
// transaction takes. The events get consecutive seq values in the order given, and recorded_at is the greater of
// the database clock and the chain's last recorded_at, read after the lock; a recorded_at that the caller gives is
// ignored. Record does not commit. t is the tenant of the transaction, which tenant.Begin opened; each event gets
// it as its tenant_id, and an event that names another tenant is refused. It returns the events as stored.
func (w *Writer) Record(ctx context.Context, tx Tx, t tenant.Tenant, events []Event) ([]Event, error) {
	for _, e := range events {
		w.markRecorded(clean(e.RequestID), w.now())
	}
	return w.write(ctx, tx, t, events)
}

// RecordOutcome writes the audit row of a denied or failed mutation in its own short transaction, after the
// caller's transaction rolled back (ST-SUP-RP05-LC02). The outcome must be denied or failed, with an error code,
// such as ErrorCodeConflict or ErrorCodeCommitOutcomeUnknown. The event always gets a new ID. A failure is counted
// (FailedOutcomes) and returned as an error that names no value, so the command runtime can log the event's
// metadata (ST-SUP-RP10-LC06).
//
// db is the pool of the audit connections (ST-SUP-RP10-LC29): RecordOutcome opens the transaction with
// tenant.Begin, sets a lock timeout of 1.5 s and a statement timeout of 2 s, and commits with tenant.Commit, which checks
// the tenant setting (ST-SUP-RP04-LC03).
func (w *Writer) RecordOutcome(ctx context.Context, db *sql.DB, t tenant.Tenant, e Event) (Event, error) {
	if e.Outcome != OutcomeDenied && e.Outcome != OutcomeFailed {
		w.failedOutcomes.Add(1)
		return Event{}, errors.New("audit: an outcome row has the outcome denied or failed")
	}
	if e.ErrorCode == "" {
		w.failedOutcomes.Add(1)
		return Event{}, errors.New("audit: an outcome row needs an error_code")
	}
	stored, err := w.alone(ctx, db, t, e)
	if err != nil {
		w.failedOutcomes.Add(1)
		return Event{}, err
	}
	return stored, nil
}

// RecordRead writes the audit row of a read of an entity that holds a pii field in its own short transaction,
// with the action read and the outcome ok, so the row stays when the caller's transaction rolls back
// (ST-SUP-RP05-LC02). The event always gets a new ID. It fails at once with ErrReadAfterRecord, and waits on no
// lock, when the event's request already called Record. It opens its transaction on db as RecordOutcome does.
func (w *Writer) RecordRead(ctx context.Context, db *sql.DB, t tenant.Tenant, e Event) (Event, error) {
	if w.hasRecorded(clean(e.RequestID), w.now()) {
		return Event{}, ErrReadAfterRecord
	}
	e.Action = ActionRead
	e.Outcome = OutcomeOK
	return w.alone(ctx, db, t, e)
}

// ownTimeouts are the timeouts of each short audit transaction (ST-SUP-RP10-LC29): both at most 2 s, and the lock
// timeout shorter, so a wait on the chain lock fails as a lock timeout.
var ownTimeouts = []string{`SET LOCAL lock_timeout = '1500ms'`, `SET LOCAL statement_timeout = '2s'`}

func (w *Writer) alone(ctx context.Context, db *sql.DB, t tenant.Tenant, e Event) (Event, error) {
	if db == nil {
		return Event{}, errors.New("audit: an own transaction needs the audit connections")
	}
	e.ID = UUID{}
	stored, err := func() ([]Event, error) {
		tx, err := tenant.Begin(ctx, db, t, nil)
		if err != nil {
			return nil, err
		}
		for _, q := range ownTimeouts {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				_ = tx.Rollback()
				return nil, stmtError(ctx, "timeout setting", err)
			}
		}
		stored, err := w.write(ctx, tx, t, []Event{e})
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		return stored, tenant.Commit(ctx, tx, t)
	}()
	if err != nil {
		var se *StatementError
		if errors.As(err, &se) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Event{}, err
		}
		return Event{}, &StatementError{Step: "own transaction", Code: sqlState(err)}
	}
	return stored[0], nil
}

const (
	// lockHead takes the row lock of the tenant's chain head and reads the head and the recorded_at to use: the
	// greater of the database clock and the chain's last recorded_at, read after the lock.
	lockHead = `INSERT INTO ` + HeadsTable + ` AS h (tenant_id, seq, head, last_recorded_at)
 VALUES ($1, 0, $2, '-infinity')
 ON CONFLICT (tenant_id) DO UPDATE SET tenant_id = EXCLUDED.tenant_id
 RETURNING h.seq, h.head, greatest(date_trunc('microseconds', clock_timestamp()), h.last_recorded_at)`
	insertEvent = `INSERT INTO ` + EventsTable + ` (tenant_id, seq, id, occurred_at, recorded_at, actor_id, actor_kind,
 actor_ip, user_agent, action, resource_type, resource_id, outcome, error_code, request_id, trace_id, span_id,
 idempotency_key, before, after, patch, service, service_version, declaration_version, prev_hash, hash)
 VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19::jsonb,
 $20::jsonb, $21::jsonb, $22, $23, $24, $25, $26)`
	moveHead = `UPDATE ` + HeadsTable + ` SET seq = $2, head = $3, last_recorded_at = $4 WHERE tenant_id = $1`
)

// write links the events to the head of their tenant's chain and stores them. The head row lock orders the
// writers of one chain: the next writer waits until this transaction ends.
func (w *Writer) write(ctx context.Context, tx Tx, t tenant.Tenant, events []Event) ([]Event, error) {
	if t.IsZero() {
		return nil, tenant.ErrZero
	}
	if len(events) == 0 {
		return nil, nil
	}
	now := w.now()
	prepared := make([]Event, len(events))
	for i, e := range events {
		e.RecordedAt = time.Time{}
		if e.TenantID != "" && e.TenantID != t.ID() {
			return nil, errors.New("audit: an event names another tenant than its transaction")
		}
		e.TenantID = t.ID()
		p, err := New(e, now)
		if err != nil {
			return nil, err
		}
		prepared[i] = p
	}
	tid := t.ID()
	var (
		seq        int64
		prev       []byte
		recordedAt time.Time
	)
	if err := tx.QueryRowContext(ctx, lockHead, tid, ZeroHash()).Scan(&seq, &prev, &recordedAt); err != nil {
		return nil, stmtError(ctx, "chain lock", err)
	}
	recordedAt = storedTime(recordedAt)
	for i := range prepared {
		e := prepared[i]
		e.RecordedAt = recordedAt
		seq++
		e, err := Link(e, prev)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, insertEvent, tid, seq, e.ID.String(), e.OccurredAt, e.RecordedAt,
			e.ActorID, string(e.ActorKind), string(e.ActorIP), string(e.UserAgent), e.Action, e.ResourceType,
			e.ResourceID, string(e.Outcome), e.ErrorCode, e.RequestID, e.TraceID, e.SpanID, e.IdempotencyKey,
			jsonArg(e.Before.raw), jsonArg(e.After.raw), jsonArg(e.Patch.raw), e.Service, e.ServiceVersion,
			e.DeclarationVersion, e.PrevHash, e.Hash); err != nil {
			return nil, stmtError(ctx, "audit row insert", err)
		}
		prev = e.Hash
		prepared[i] = e
	}
	if _, err := tx.ExecContext(ctx, moveHead, tid, seq, prev, recordedAt); err != nil {
		return nil, stmtError(ctx, "chain head update", err)
	}
	for _, e := range prepared {
		payload, err := eventMessage(e)
		if err != nil {
			return nil, err
		}
		if err := w.outbox.Enqueue(ctx, tx, tid, SinkDestination, e.ID.String(), payload); err != nil {
			return nil, stmtError(ctx, "sink enqueue", err)
		}
	}
	return prepared, nil
}

func jsonArg(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
