package audit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Tx is the part of a database transaction that the package uses. A pgx.Tx satisfies it. The package never
// begins, commits or rolls back a Tx it is given.
type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// RunTx runs fn in a new short transaction of the service, and commits it when fn returns nil. The service gives
// it, so that its own transaction helper (tenant setting, audit connections, lock and statement timeouts) opens
// the transaction.
type RunTx func(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error

// Enqueuer writes one outbox message inside the transaction tx. The outbox runtime's queue satisfies it; the
// package names only the part it uses.
type Enqueuer interface {
	Enqueue(ctx context.Context, tx Tx, destination, key string, payload []byte) error
}

// Writer writes audit rows and enqueues each of them for the audit sink.
type Writer struct {
	outbox Enqueuer
	now    func() time.Time
}

// NewWriter returns a Writer that enqueues sink messages through outbox and reads the time from now; a nil now
// is time.Now.
func NewWriter(outbox Enqueuer, now func() time.Time) (*Writer, error) {
	if outbox == nil {
		return nil, errors.New("audit: a writer needs an outbox")
	}
	if now == nil {
		now = time.Now
	}
	return &Writer{outbox: outbox, now: now}, nil
}

// Record inserts the audit row of a change through the caller's transaction tx and enqueues it for the audit
// sink in the same transaction, keyed by the event ID (ST-SUP-RP05-LC02, ST-SUP-RP05-LC07). It does not commit:
// the row commits with the change, or is gone with it. It returns the event as stored, with its ID, times,
// prev_hash and hash.
func (w *Writer) Record(ctx context.Context, tx Tx, e Event) (Event, error) {
	return w.write(ctx, tx, e)
}

// RecordOutcome writes the audit row of a denied or failed mutation in its own short transaction, after the
// caller's transaction rolled back (ST-SUP-RP05-LC02). The outcome must be denied or failed, with an error code,
// such as ErrorCodeConflict or ErrorCodeCommitOutcomeUnknown.
func (w *Writer) RecordOutcome(ctx context.Context, run RunTx, e Event) (Event, error) {
	if e.Outcome != OutcomeDenied && e.Outcome != OutcomeFailed {
		return Event{}, fmt.Errorf("audit: an outcome row has the outcome denied or failed, not %q", string(e.Outcome))
	}
	if e.ErrorCode == "" {
		return Event{}, errors.New("audit: an outcome row needs an error_code")
	}
	return w.alone(ctx, run, e)
}

// RecordRead writes the audit row of a read of an entity that holds a pii field in its own short transaction,
// with the action read and the outcome ok, so the row stays when the caller's transaction rolls back
// (ST-SUP-RP05-LC02).
func (w *Writer) RecordRead(ctx context.Context, run RunTx, e Event) (Event, error) {
	e.Action = ActionRead
	if e.Outcome == "" {
		e.Outcome = OutcomeOK
	}
	return w.alone(ctx, run, e)
}

func (w *Writer) alone(ctx context.Context, run RunTx, e Event) (Event, error) {
	if run == nil {
		return Event{}, errors.New("audit: an own transaction needs a RunTx")
	}
	var stored Event
	err := run(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		stored, err = w.write(ctx, tx, e)
		return err
	})
	if err != nil {
		return Event{}, err
	}
	return stored, nil
}

const (
	lockHead = `INSERT INTO ` + HeadsTable + ` (service, head, row_count) VALUES ($1, $2, 0)
 ON CONFLICT (service) DO UPDATE SET service = EXCLUDED.service RETURNING head, row_count`
	insertEvent = `INSERT INTO ` + EventsTable + ` (seq, id, occurred_at, recorded_at, tenant_id, actor_id, actor_kind,
 actor_ip, user_agent, action, resource_type, resource_id, outcome, error_code, request_id, trace_id, span_id,
 idempotency_key, before, after, patch, service, service_version, declaration_version, prev_hash, hash)
 VALUES ($1, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19::jsonb,
 $20::jsonb, $21::jsonb, $22, $23, $24, $25, $26)`
	moveHead = `UPDATE ` + HeadsTable + ` SET head = $2, row_count = $3 WHERE service = $1`
)

// write links the event to the head of its service chain and stores it. The head row lock orders the writers of
// one chain: the next writer waits until this transaction ends.
func (w *Writer) write(ctx context.Context, tx Tx, e Event) (Event, error) {
	now := w.now()
	if e.RecordedAt.IsZero() {
		e.RecordedAt = now
	}
	e, err := New(e, now)
	if err != nil {
		return Event{}, err
	}
	var (
		prev  []byte
		count int64
	)
	if err := tx.QueryRow(ctx, lockHead, e.Service, ZeroHash()).Scan(&prev, &count); err != nil {
		return Event{}, fmt.Errorf("audit: lock the chain head of %s: %w", e.Service, err)
	}
	e, err = Link(e, prev)
	if err != nil {
		return Event{}, err
	}
	if _, err := tx.Exec(ctx, insertEvent, count+1, e.ID.String(), e.OccurredAt, e.RecordedAt, e.TenantID,
		e.ActorID, string(e.ActorKind), string(e.ActorIP), string(e.UserAgent), e.Action, e.ResourceType,
		e.ResourceID, string(e.Outcome), e.ErrorCode, e.RequestID, e.TraceID, e.SpanID, e.IdempotencyKey,
		jsonArg(e.Before), jsonArg(e.After), jsonArg(e.Patch), e.Service, e.ServiceVersion, e.DeclarationVersion,
		e.PrevHash, e.Hash); err != nil {
		return Event{}, fmt.Errorf("audit: insert the audit row: %w", err)
	}
	if _, err := tx.Exec(ctx, moveHead, e.Service, e.Hash, count+1); err != nil {
		return Event{}, fmt.Errorf("audit: move the chain head of %s: %w", e.Service, err)
	}
	payload, err := eventMessage(e)
	if err != nil {
		return Event{}, err
	}
	if err := w.outbox.Enqueue(ctx, tx, SinkDestination, e.ID.String(), payload); err != nil {
		return Event{}, fmt.Errorf("audit: enqueue the audit row for the sink: %w", err)
	}
	return e, nil
}

func jsonArg(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
