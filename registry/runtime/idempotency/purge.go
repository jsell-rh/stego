package idempotency

import (
	"context"
	"database/sql"
	"time"
)

// DefaultRetention is how long Wrap keeps a completed key when the request names no retention (ST-SUP-RP07-LC07).
const DefaultRetention = 24 * time.Hour

// DefaultPurgeBatch and DefaultPurgePause are the batch size and the pause of Purge when its options name none.
const (
	DefaultPurgeBatch = 1000
	DefaultPurgePause = 100 * time.Millisecond
)

// Execer runs one statement on its own. *sql.DB satisfies it, and then each batch of Purge is a short transaction
// of its own.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

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

const purgeSQL = `DELETE FROM idempotency_keys
WHERE (tenant_id, principal_id, key) IN (
	SELECT tenant_id, principal_id, key FROM idempotency_keys
	WHERE expires_at <= $1
	ORDER BY expires_at
	LIMIT $2
	FOR UPDATE SKIP LOCKED)`

// Purge deletes the key rows whose expires_at has passed, in batches of at most opts.Batch rows with a pause of
// opts.Pause between two batches, and returns how many rows it deleted (ST-SUP-RP07-LC07). It stops after the first
// batch that removes fewer rows than the batch size. It never deletes row by row, and needs no TTL index.
func Purge(ctx context.Context, db Execer, opts PurgeOptions) (int64, error) {
	batch := opts.Batch
	if batch <= 0 {
		batch = DefaultPurgeBatch
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
		res, err := db.ExecContext(ctx, purgeSQL, clock(), batch)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(batch) {
			return total, nil
		}
	}
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
