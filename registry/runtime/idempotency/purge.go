package idempotency

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DefaultRetention is how long Wrap keeps a completed key when the request names no retention (ST-SUP-RP07-LC07).
const DefaultRetention = 24 * time.Hour

// DefaultPurgeBatch and DefaultPurgePause are the batch size and the pause of Purge when its options name none.
const (
	DefaultPurgeBatch = 1000
	DefaultPurgePause = 100 * time.Millisecond
)

// Querier opens a transaction. *sql.DB satisfies it, and then each batch of Purge is a short transaction of its own.
type Querier interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// The limits of one batch of Purge, set with SET LOCAL in its transaction: a batch that waits longer for a row lock
// or runs longer ends with an error, and the next run of the purge job tries again.
const (
	PurgeLockTimeout      = time.Second
	PurgeStatementTimeout = 30 * time.Second
)

// limitsSQL sets PurgeLockTimeout and PurgeStatementTimeout for the transaction of one batch.
const limitsSQL = `SELECT pg_catalog.set_config('lock_timeout', '1s', true), pg_catalog.set_config('statement_timeout', '30s', true)`

// PurgeOptions sets Purge. A zero field takes its default.
type PurgeOptions struct {
	// Batch is the most rows one DELETE removes; zero is DefaultPurgeBatch.
	Batch int
	// Pause is the wait between two batches; zero is DefaultPurgePause.
	Pause time.Duration
	// Clock gives the current time; nil is time.Now.
	Clock func() time.Time
	// Sleep waits d or until ctx ends; nil waits on a timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// purgeSQL calls the purge function of the key table that the search path resolves. The function, not the caller,
// reads and deletes the rows of every tenant (ST-SUP-RP07-LC11). Each parameter has its type, so no overload of the
// function with other parameter types runs in its place.
const purgeSQL = `SELECT ` + PurgeFunction + `($1::timestamptz, $2::integer)`

// Purge deletes the key rows whose expires_at has passed, in batches of at most opts.Batch rows with a pause of
// opts.Pause between two batches, and returns how many rows it deleted (ST-SUP-RP07-LC07). Each batch is one call of
// the purge function idempotency_keys_purge, one DELETE of at most the batch size, which spans tenants under the
// key table's row-level security (ST-SUP-RP07-LC11); the function takes no time later than the database's clock.
// Each batch is a transaction of its own with SET LOCAL lock_timeout of PurgeLockTimeout and statement_timeout of
// PurgeStatementTimeout; a batch that a timeout ends gives its error, and the next run tries again. It stops after the first batch that removes fewer rows than the batch size. It never deletes row by row, and needs
// no TTL index. It refuses a batch above MaxPurgeBatch.
func Purge(ctx context.Context, db Querier, opts PurgeOptions) (int64, error) {
	batch := opts.Batch
	if batch <= 0 {
		batch = DefaultPurgeBatch
	}
	if batch > MaxPurgeBatch {
		return 0, fmt.Errorf("idempotency: the purge batch is at most %d", MaxPurgeBatch)
	}
	pause := opts.Pause
	if pause <= 0 {
		pause = DefaultPurgePause
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepTimer
	}
	var total int64
	for first := true; ; first = false {
		if !first {
			if err := sleep(ctx, pause); err != nil {
				return total, err
			}
		}
		n, err := purgeBatch(ctx, db, clock(), batch)
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(batch) {
			return total, nil
		}
	}
}

// purgeBatch runs one call of the purge function in a transaction of its own, with the limits of a batch.
func purgeBatch(ctx context.Context, db Querier, now time.Time, batch int) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, limitsSQL); err != nil {
		return 0, err
	}
	var n int64
	if err := tx.QueryRowContext(ctx, purgeSQL, now, batch).Scan(&n); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

func sleepTimer(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
