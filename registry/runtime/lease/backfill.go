package lease

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The defaults of ST-SUP-RP09-LC09: a batch moves at most 1,000 rows and waits at most 2 s for a lock.
const (
	DefaultBatchSize   = 1000
	DefaultLockTimeout = 2 * time.Second
)

// BackfillsTable is the table of the backfill checkpoints, one row for each schema change. The migration of a step
// at expand creates the table and the row of its change as the owner role (ST-SUP-RC01-OC25); the runtime role
// changes the row only through the checkpoint statement.
const BackfillsTable = `CREATE TABLE IF NOT EXISTS stego_schema.backfills (
	change_id text PRIMARY KEY,
	last_key text,
	rows_moved bigint NOT NULL DEFAULT 0,
	failed_rows bigint NOT NULL DEFAULT 0,
	duplicate_groups bigint NOT NULL DEFAULT 0,
	fill_digest text NOT NULL DEFAULT '',
	token bigint NOT NULL DEFAULT 0,
	completed_at timestamptz
)`

// readCheckpointStatement locks the change's row for the batch, so the row lock orders two writers.
const readCheckpointStatement = `SELECT last_key, rows_moved, failed_rows, duplicate_groups, fill_digest, token, completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id=$1 FOR UPDATE`

// checkpointStatement is the fenced checkpoint statement of ST-SUP-RP09-LC07: it changes the row only when the
// row's token is not higher than the holder's, and sets the row's token to the holder's.
const checkpointStatement = `UPDATE stego_schema.backfills SET last_key=$2, rows_moved=$3, failed_rows=$4, duplicate_groups=$5, fill_digest=$6, completed_at=CASE WHEN $7::boolean THEN coalesce(completed_at, now()) END, token=$8 WHERE change_id=$1 AND token<=$8`

// lockTimeoutStatement sets the lock timeout of the batch's transaction only.
const lockTimeoutStatement = `SELECT set_config('lock_timeout', $1, true)`

// Observer receives the built-in schema backfill observations of the probe runtime (ST-SUP-RP12-LC23): counts and
// the change ID, never a row value (ST-SUP-RP09-LC08).
type Observer interface {
	BackfillRows(change string, rows int64)
	BackfillFailedRows(change string, rows int64)
	BackfillDuplicateGroups(change string, groups int64)
	BackfillCompleted(change string)
	BackfillStalled(change string)
}

// Batch is what one step of a backfill did: the rows it read after the checkpoint key, in key order, the key of
// the last of them, and the counts of the rows it moved, the rows that failed the conversion and the duplicate
// groups it found.
type Batch struct {
	Read       int
	LastKey    string
	Moved      int64
	Failed     int64
	Duplicates int64
}

// Step is the generated part of one batch: in tx, it reads at most limit rows after the key after (none at the
// start) in key order, whose new value is unset, and moves them. It writes no checkpoint; the batch does.
type Step func(ctx context.Context, tx *sql.Tx, after sql.NullString, limit int) (Batch, error)

// BackfillSettings are the settings of a backfill. A zero value takes its default.
type BackfillSettings struct {
	// FillDigest is the digest of the declared fill value of a retype, or "" when it declares none.
	FillDigest  string
	BatchSize   int
	LockTimeout time.Duration
	Observer    Observer
}

// Backfill moves the rows of one schema change in short fenced batches under the lease of the change.
type Backfill struct {
	change      string
	step        Step
	fillDigest  string
	batchSize   int
	lockTimeout time.Duration
	observer    Observer

	mu         sync.Mutex
	db         DB
	held       *Held
	state      checkpoint
	running    bool
	complete   bool
	lastCommit time.Time
	stalled    bool
	stopping   bool
	flushErr   error
	idle       chan struct{}
}

type checkpoint struct {
	lastKey    sql.NullString
	moved      int64
	failed     int64
	duplicates int64
	fillDigest string
	token      int64
	completed  bool
}

// NewBackfill returns the backfill of the change with its generated step.
func NewBackfill(change string, step Step, settings BackfillSettings) (*Backfill, error) {
	if change == "" || step == nil {
		return nil, errors.New("backfill: a backfill needs a change ID and a step")
	}
	b := &Backfill{change: change, step: step, fillDigest: settings.FillDigest, batchSize: settings.BatchSize, lockTimeout: settings.LockTimeout, observer: settings.Observer}
	if b.batchSize == 0 {
		b.batchSize = DefaultBatchSize
	}
	if b.lockTimeout == 0 {
		b.lockTimeout = DefaultLockTimeout
	}
	if b.batchSize < 0 || b.lockTimeout < 0 {
		return nil, fmt.Errorf("backfill %s: the batch size and the lock timeout must be positive", change)
	}
	return b, nil
}

// Change is the change ID.
func (b *Backfill) Change() string { return b.change }

// Stalled tells if the backfill has rows left and committed no batch for three lease durations (ST-SUP-RP09-LC12).
// The readiness of the controller mode reads it.
func (b *Backfill) Stalled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stalled
}

// Run moves the rows of the change under held: one batch after the other from the checkpoint key, until a batch
// reads no row, and then it marks the change complete. When the declared fill digest differs from the row's, it
// first clears the completed time and the key and records the new digest, so the step passes again over the rows
// whose new value is unset (ST-SUP-RP09-LC11). A batch that fails, such as at the lock timeout, is rolled back
// and run again after the retry period; a fenced batch or a stopped holder ends Run. A watcher marks the backfill
// stalled when no batch commits for three lease durations, and emits the stall once.
func (b *Backfill) Run(ctx context.Context, db DB, held *Held) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return fmt.Errorf("backfill %s: already running", b.change)
	}
	b.db, b.held, b.running, b.stopping, b.flushErr = db, held, true, false, nil
	b.lastCommit = held.client.clock.Now()
	b.idle = make(chan struct{})
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.running, b.stalled = false, false
		close(b.idle)
		b.mu.Unlock()
	}()

	if err := b.prepare(ctx); err != nil {
		return err
	}
	watch, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go b.watch(watch)
	for {
		b.mu.Lock()
		complete, stopping := b.complete, b.stopping
		b.mu.Unlock()
		if stopping {
			// The flush: write the checkpoint of the last batch again with the holder's token, while the lease
			// still holds, before Run returns and the holder can release the lease.
			err := b.flush(context.WithoutCancel(ctx))
			b.mu.Lock()
			b.flushErr = err
			b.mu.Unlock()
			return err
		}
		if complete {
			return nil
		}
		err := b.batch(ctx)
		switch {
		case err == nil:
			continue
		case errors.Is(err, ErrFenced), errors.Is(err, ErrStopped), ctx.Err() != nil:
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-held.Done():
			return held.Err()
		case <-held.client.clock.After(held.client.retry):
		}
	}
}

// prepare reads the checkpoint and, for a new fill value, starts the repair pass.
func (b *Backfill) prepare(ctx context.Context) error {
	return b.inTx(ctx, func(ctx context.Context, tx *sql.Tx, row checkpoint) (checkpoint, bool, error) {
		if row.fillDigest == b.fillDigest {
			return row, false, nil
		}
		row.lastKey = sql.NullString{}
		row.completed = false
		row.failed = 0
		row.fillDigest = b.fillDigest
		return row, true, nil
	})
}

// batch runs one batch: one transaction with the lock timeout that locks the checkpoint row, runs the step after
// the checkpoint key and writes the checkpoint (ST-SUP-RP09-LC09, ST-SUP-RP09-LC10).
func (b *Backfill) batch(ctx context.Context) error {
	var result Batch
	err := b.inTx(ctx, func(ctx context.Context, tx *sql.Tx, row checkpoint) (checkpoint, bool, error) {
		if row.completed {
			return row, false, nil
		}
		var err error
		result, err = b.step(ctx, tx, row.lastKey, b.batchSize)
		if err != nil {
			return row, false, fmt.Errorf("backfill %s: step: %w", b.change, err)
		}
		if result.Read < 0 || result.Read > b.batchSize {
			return row, false, fmt.Errorf("backfill %s: the step read %d rows, more than the batch size %d", b.change, result.Read, b.batchSize)
		}
		if result.Read == 0 {
			row.completed = true
			return row, true, nil
		}
		row.lastKey = sql.NullString{String: result.LastKey, Valid: true}
		row.moved += result.Moved
		row.failed += result.Failed
		row.duplicates += result.Duplicates
		return row, true, nil
	})
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.lastCommit = b.held.client.clock.Now()
	b.stalled = false
	completed := b.state.completed
	b.complete = completed
	b.mu.Unlock()
	if b.observer != nil {
		if result.Read > 0 {
			b.observer.BackfillRows(b.change, result.Moved)
			if result.Failed > 0 {
				b.observer.BackfillFailedRows(b.change, result.Failed)
			}
			if result.Duplicates > 0 {
				b.observer.BackfillDuplicateGroups(b.change, result.Duplicates)
			}
		}
		if completed && result.Read == 0 {
			b.observer.BackfillCompleted(b.change)
		}
	}
	return nil
}

// inTx runs change in one transaction with the lock timeout, over the checkpoint row locked FOR UPDATE, and writes
// the row change gives with the fenced checkpoint statement when it asks to, then commits.
func (b *Backfill) inTx(ctx context.Context, change func(ctx context.Context, tx *sql.Tx, row checkpoint) (checkpoint, bool, error)) error {
	held := b.held
	held.act.RLock()
	defer held.act.RUnlock()
	if err := held.active(); err != nil {
		return err
	}
	ctx, cancel := held.deadline(ctx)
	defer cancel()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backfill %s: begin: %w", b.change, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, lockTimeoutStatement, fmt.Sprintf("%dms", b.lockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("backfill %s: lock timeout: %w", b.change, err)
	}
	var row checkpoint
	err = tx.QueryRowContext(ctx, readCheckpointStatement, b.change).Scan(&row.lastKey, &row.moved, &row.failed, &row.duplicates, &row.fillDigest, &row.token, &row.completed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("backfill %s: the change has no checkpoint row", b.change)
	}
	if err != nil {
		return fmt.Errorf("backfill %s: read the checkpoint: %w", b.change, err)
	}
	next, write, err := change(ctx, tx, row)
	if err != nil {
		return err
	}
	if write {
		if next, err = b.checkpoint(ctx, tx, next); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backfill %s: commit: %w", b.change, err)
	}
	b.mu.Lock()
	b.state = next
	b.complete = next.completed
	b.mu.Unlock()
	return nil
}

// checkpoint runs the fenced checkpoint statement with the holder's token (ST-SUP-RP09-LC07).
func (b *Backfill) checkpoint(ctx context.Context, tx *sql.Tx, row checkpoint) (checkpoint, error) {
	token := b.held.Token()
	result, err := tx.ExecContext(ctx, checkpointStatement, b.change, row.lastKey, row.moved, row.failed, row.duplicates, row.fillDigest, row.completed, token)
	if err != nil {
		return row, fmt.Errorf("backfill %s: checkpoint: %w", b.change, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return row, fmt.Errorf("backfill %s: checkpoint: %w", b.change, err)
	}
	if changed != 1 {
		err := fmt.Errorf("backfill %s: %w: the checkpoint row holds a token higher than %d", b.change, ErrFenced, token)
		b.held.halt(err)
		return row, err
	}
	row.token = token
	return row, nil
}

// Flush makes Run start no new batch, waits for the batch that runs, and waits for Run to write the checkpoint of
// the last batch again with the holder's token and return. The controller's shutdown runs it under the lease,
// before it releases the lease (ST-SUP-RP09-LC06). With no Run, it has nothing to flush.
func (b *Backfill) Flush(ctx context.Context) error {
	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return nil
	}
	b.stopping = true
	idle := b.idle
	b.mu.Unlock()
	select {
	case <-idle:
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flushErr
}

// flush writes the checkpoint of the last batch again with the holder's token.
func (b *Backfill) flush(ctx context.Context) error {
	b.mu.Lock()
	last := b.state
	b.mu.Unlock()
	return b.inTx(ctx, func(context.Context, *sql.Tx, checkpoint) (checkpoint, bool, error) {
		return last, true, nil
	})
}

// watch marks the backfill stalled, and emits schema.backfill.stalled once, when it has rows left and no batch
// committed for three lease durations. It looks once every renew interval, so it sees a stall within three lease
// durations and one renew interval. It never stops the process (ST-SUP-RP09-LC12).
func (b *Backfill) watch(ctx context.Context) {
	clock := b.held.client.clock
	bound := 3 * b.held.client.ttl
	for {
		select {
		case <-ctx.Done():
			return
		case <-clock.After(b.held.client.renew):
		}
		b.mu.Lock()
		stall := !b.complete && !b.stalled && clock.Now().Sub(b.lastCommit) >= bound
		if stall {
			b.stalled = true
		}
		b.mu.Unlock()
		if stall && b.observer != nil {
			b.observer.BackfillStalled(b.change)
		}
	}
}
