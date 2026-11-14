package lease

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The defaults of ST-SUP-RP09-LC09: a batch moves at most 1,000 rows, waits at most 2 s for a lock, and runs each
// statement for at most 10 s (decision on finding F6 of the schema-change research, orchestrator under the owner's
// delegation, 2026-11-13).
const (
	DefaultBatchSize        = 1000
	DefaultLockTimeout      = 2 * time.Second
	DefaultStatementTimeout = 10 * time.Second
	// DefaultValidateTimeout bounds the VALIDATE CONSTRAINT of the completeness CHECK, which reads every row of the
	// table in one statement.
	DefaultValidateTimeout = 10 * time.Minute
)

// ErrIncomplete is the error of Run when the completeness CHECK of the change does not validate: a row has the old
// column set and the new column unset, such as a row of a tenant that the tenant list does not hold. Run then sets
// the checkpoint key back to the start and returns, and the next Run passes over the rows again (decision on
// ST-SUP-Q120 (c), orchestrator under the owner's delegation, 2026-11-14).
var ErrIncomplete = errors.New("the change is not complete")

// ErrValidate is the error of Run when the validate statement of the completeness CHECK fails with an error that a
// run again cannot fix, such as a statement that names no constraint: an error with a SQLSTATE other than the lock
// timeout 55P03, the statement timeout 57014, the serialization failure 40001 and the deadlock 40P01. Run returns it
// at once, with the cause, instead of running the batch again until its time limit (review of PR #300, L1).
var ErrValidate = errors.New("the completeness CHECK cannot be validated")

// transientStates are the SQLSTATEs of a validate statement that a run again can pass: a lock or statement timeout,
// a serialization failure, a deadlock, and an administrator shutdown or a server that cannot take connections yet, as
// at a failover (review of PR #305).
var transientStates = map[string]bool{"55P03": true, "57014": true, "40001": true, "40P01": true, "57P01": true, "57P03": true}

// ErrSession is the error of Run when the session of its batches breaks: an administrator shutdown (SQLSTATE 57P01),
// a server that cannot take connections yet (57P03), a connection exception (class 08) or a connection that the
// driver or database/sql marks as bad or done. A run again on the same session cannot pass, so Run returns it at once,
// and the caller holds the lease again with new sessions, as after a lost lease (review of PR #305, L3, orchestrator
// under the owner's delegation, 2026-11-14).
var ErrSession = errors.New("the session of the backfill is broken")

// brokenSession tells whether err ends the session of the backfill.
func brokenSession(err error) bool {
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		code := state.SQLState()
		return code == "57P01" || code == "57P03" || strings.HasPrefix(code, "08")
	}
	return false
}

// The reasons of schema.backfill.throttled: the backfill waited for a replica whose replay lag is above the
// maximum, or it made its batches smaller because a batch took longer than the target batch time.
const (
	ThrottleReplicaLag = "replica_lag"
	ThrottleBatchTime  = "batch_time"
)

// BackfillsTable is the table of the backfill checkpoints, one row for each schema change. The migration of a step
// at expand creates the table and the row of its change as the owner role (ST-SUP-RC01-OC25); the backfill, as the
// owner role, changes the row only through BackfillFunctions.
const BackfillsTable = `CREATE TABLE IF NOT EXISTS stego_schema.backfills (
	change_id text PRIMARY KEY,
	last_key bytea,
	last_key_type text,
	rows_moved bigint NOT NULL DEFAULT 0,
	failed_rows bigint NOT NULL DEFAULT 0,
	duplicate_groups bigint NOT NULL DEFAULT 0,
	fill_digest text NOT NULL DEFAULT '',
	token bigint NOT NULL DEFAULT 0,
	completed_at timestamptz
)`

// BackfillFunctions are the two functions through which the backfill reads and changes a row of
// stego_schema.backfills: backfill_read reads the change's row and locks it FOR UPDATE for the batch, so the row
// lock orders two writers; backfill_checkpoint is the fenced checkpoint statement of ST-SUP-RP09-LC07, which changes
// the row only when the lease row that its arguments name is the lease of the change, stego.backfill.<change ID>,
// holds the holder with the holder's token and has not expired, and the row's token is not higher than the
// holder's (the lease of the change by the recheck of the security review of PR #291, orchestrator under the
// owner's delegation, 2026-11-14); it sets the row's token to the holder's and gives
// the count of rows it changed. Both are SECURITY DEFINER functions of the owner role with the search path
// pg_catalog, pg_temp, so no object of the caller's search path runs in them, and they name each table with its
// schema. The text revokes EXECUTE from PUBLIC and grants it to no role, so only the owner role, which runs the
// backfill in the migration job, can run them, and the runtime role cannot read, lock or change a row
// (ST-SUP-RC01-OC25; decision on finding M1 of the security review of PR #291, orchestrator under the owner's
// delegation, 2026-11-14). The migration of a step at expand with a backfill runs it after BackfillsTable, as the
// owner role. backfill_checkpoint is PL/pgSQL, so it reads public.leases only when it runs, and a migration can make
// it before the lease table. Each statement runs again with no error on a database that it made.
const BackfillFunctions = `CREATE OR REPLACE FUNCTION stego_schema.backfill_read(text)
 RETURNS TABLE (last_key bytea, last_key_type text, rows_moved bigint, failed_rows bigint, duplicate_groups bigint, fill_digest text, token bigint, completed boolean)
 LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
 AS $stego_backfill$SELECT b.last_key, b.last_key_type, b.rows_moved, b.failed_rows, b.duplicate_groups, b.fill_digest, b.token, b.completed_at IS NOT NULL
  FROM stego_schema.backfills b WHERE b.change_id = $1 FOR UPDATE$stego_backfill$;
CREATE OR REPLACE FUNCTION stego_schema.backfill_checkpoint(text, bytea, bigint, bigint, bigint, text, boolean, bigint, text, text, text)
 RETURNS bigint LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
 AS $stego_backfill$DECLARE n bigint;
BEGIN
  PERFORM 1 FROM public.leases l WHERE l.name = $10 AND $10 = 'stego.backfill.' || $1 AND l.holder = $11 AND l.token = $8 AND l.expires_at > pg_catalog.clock_timestamp() FOR SHARE;
  IF NOT FOUND THEN
    RETURN 0;
  END IF;
  WITH changed AS (UPDATE stego_schema.backfills b SET last_key = $2, last_key_type = $9, rows_moved = $3, failed_rows = $4,
   duplicate_groups = $5, fill_digest = $6, completed_at = CASE WHEN $7 THEN coalesce(b.completed_at, pg_catalog.now()) END, token = $8
   WHERE b.change_id = $1 AND b.token <= $8 RETURNING 1) SELECT pg_catalog.count(*) INTO n FROM changed;
  RETURN n;
END$stego_backfill$;
REVOKE ALL ON FUNCTION stego_schema.backfill_read(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.backfill_checkpoint(text, bytea, bigint, bigint, bigint, text, boolean, bigint, text, text, text) FROM PUBLIC;
`

// readCheckpointStatement reads and locks the change's row through backfill_read.
const readCheckpointStatement = `SELECT last_key, last_key_type, rows_moved, failed_rows, duplicate_groups, fill_digest, token, completed FROM stego_schema.backfill_read($1)`

// checkpointStatement is the fenced checkpoint statement of ST-SUP-RP09-LC07, through backfill_checkpoint, with the
// lease name and the holder after the key type.
const checkpointStatement = `SELECT stego_schema.backfill_checkpoint($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

// KeyType is the type of a checkpoint key (ST-SUP-RP09-LC09).
type KeyType string

// The types of a checkpoint key. A composite key is the ordered list of its parts, each of one of the other types.
const (
	KeyInteger   KeyType = "integer"
	KeyUUID      KeyType = "uuid"
	KeyText      KeyType = "text"
	KeyComposite KeyType = "composite"
)

// Key is a checkpoint key with its type. A scalar key holds its value as text: the decimal digits of an integer,
// the canonical form of a uuid, or the text. A composite key holds its parts.
type Key struct {
	Type  KeyType
	Value string
	Parts []Key
}

// IntegerKey is the key of an integer column.
func IntegerKey(v int64) Key { return Key{Type: KeyInteger, Value: strconv.FormatInt(v, 10)} }

// UUIDKey is the key of a uuid column, in its canonical form.
func UUIDKey(v string) Key { return Key{Type: KeyUUID, Value: v} }

// TextKey is the key of a text column.
func TextKey(v string) Key { return Key{Type: KeyText, Value: v} }

// CompositeKey is the key of several columns, in the order of the key.
func CompositeKey(parts ...Key) Key { return Key{Type: KeyComposite, Parts: parts} }

// Args gives the values of the key for the step's comparison in key order, one for each column, each of its own
// type: an int64 for an integer, and the text of a uuid or a text, which the step casts to its column's type. The
// step compares a composite key as a row, such as (a, b) > ($1, $2), so a text part compares in the collation of
// its column, as the ORDER BY of the batch does.
func (k Key) Args() []any {
	if k.Type == KeyComposite {
		var args []any
		for _, part := range k.Parts {
			args = append(args, part.Args()...)
		}
		return args
	}
	if k.Type == KeyInteger {
		v, err := strconv.ParseInt(k.Value, 10, 64)
		if err == nil {
			return []any{v}
		}
	}
	return []any{k.Value}
}

// check refuses a key whose type or parts are not those of the shape.
func (k Key) check(shape KeyType, parts []KeyType) error {
	if k.Type != shape {
		return fmt.Errorf("the key has the type %q, not %q", k.Type, shape)
	}
	switch k.Type {
	case KeyInteger:
		if _, err := strconv.ParseInt(k.Value, 10, 64); err != nil {
			return fmt.Errorf("the integer key %q: %w", k.Value, err)
		}
	case KeyUUID, KeyText:
	case KeyComposite:
		if len(k.Parts) != len(parts) {
			return fmt.Errorf("the composite key has %d parts, not %d", len(k.Parts), len(parts))
		}
		for i, part := range k.Parts {
			if part.Type == KeyComposite {
				return errors.New("a part of a composite key cannot be composite")
			}
			if err := part.check(parts[i], nil); err != nil {
				return fmt.Errorf("part %d: %w", i+1, err)
			}
		}
	default:
		return fmt.Errorf("unknown key type %q", k.Type)
	}
	return nil
}

// encodeKey is the stored form of a key. A scalar key is the bytes of its value. A composite key is, for each
// part in order, its type and then its value, each prefixed by its length in bytes as a 4-byte big-endian unsigned
// integer.
func encodeKey(k Key) []byte {
	if k.Type != KeyComposite {
		return []byte(k.Value)
	}
	var out []byte
	for _, part := range k.Parts {
		for _, field := range []string{string(part.Type), part.Value} {
			out = binary.BigEndian.AppendUint32(out, uint32(len(field)))
			out = append(out, field...)
		}
	}
	return out
}

// decodeKey reads a stored key of the type typ.
func decodeKey(typ KeyType, stored []byte) (Key, error) {
	if typ != KeyComposite {
		return Key{Type: typ, Value: string(stored)}, nil
	}
	key := Key{Type: KeyComposite}
	for len(stored) > 0 {
		var fields [2]string
		for i := range fields {
			if len(stored) < 4 {
				return Key{}, errors.New("a composite key ends inside a length")
			}
			n := binary.BigEndian.Uint32(stored)
			stored = stored[4:]
			if uint64(len(stored)) < uint64(n) {
				return Key{}, errors.New("a composite key ends inside a part")
			}
			fields[i] = string(stored[:n])
			stored = stored[n:]
		}
		key.Parts = append(key.Parts, Key{Type: KeyType(fields[0]), Value: fields[1]})
	}
	return key, nil
}

// lockTimeoutStatement sets the lock timeout and the statement timeout of the batch's transaction only.
const lockTimeoutStatement = `SELECT pg_catalog.set_config('lock_timeout', $1, true), pg_catalog.set_config('statement_timeout', $2, true)`

// replicaLagStatement reads the longest replay lag of the replicas, in milliseconds, or 0 when there is none. A role
// that does not have the privileges of pg_read_all_stats reads no lag (PostgreSQL shows it NULL), and the
// backfill then sees no lag; the owner role needs pg_monitor for the maximum replica lag to apply. A test of the
// package replaces it, as the test server has no replica.
var replicaLagStatement = `SELECT coalesce(pg_catalog.max(pg_catalog.date_part('epoch', replay_lag) * 1000), 0)::bigint FROM pg_catalog.pg_stat_replication`

// Observer receives the built-in schema backfill observations of the probe runtime (ST-SUP-RP12-LC23): counts and
// the change ID, never a row value (ST-SUP-RP09-LC08).
type Observer interface {
	BackfillRows(change string, rows int64)
	BackfillFailedRows(change string, rows int64)
	BackfillDuplicateGroups(change string, groups int64)
	BackfillCompleted(change string)
	BackfillStalled(change string)
	BackfillThrottled(change, reason string)
}

// Batch is what one step of a backfill did: the rows it read after the checkpoint key, in key order, the key of
// the last of them, and the counts of the rows it moved, the rows that failed the conversion and the duplicate
// groups it found.
type Batch struct {
	Read       int
	LastKey    Key
	Moved      int64
	Failed     int64
	Duplicates int64
}

// Step is the generated part of one batch: in tx, it reads at most limit rows after the key after (nil at the
// start) in key order, compared in the order of the key's type with after.Args, whose new value is unset, and
// moves them. It writes no checkpoint; the batch does.
type Step func(ctx context.Context, tx *sql.Tx, after *Key, limit int) (Batch, error)

// BackfillSettings are the settings of a backfill. A zero value takes its default.
type BackfillSettings struct {
	// FillDigest is the digest of the declared fill value of a retype, or "" when it declares none.
	FillDigest string
	// Key is the type of the key the batches order by, and KeyParts the types of its parts when it is composite.
	Key         KeyType
	KeyParts    []KeyType
	BatchSize   int
	LockTimeout time.Duration
	Observer    Observer
	// StatementTimeout bounds each statement of a batch. Pause is the wait after each batch that commits, and zero
	// is none. TargetBatchTime, when set, halves the rows of the next batch, down to one, after a batch that took
	// longer, and doubles them, up to BatchSize, after one that took less than half of it. MaxReplicaLag, when set,
	// makes the backfill wait before a batch while a replica's replay lag is above it. Both report
	// schema.backfill.throttled (decision on finding F6, orchestrator under the owner's delegation, 2026-11-13).
	StatementTimeout time.Duration
	Pause            time.Duration
	TargetBatchTime  time.Duration
	MaxReplicaLag    time.Duration
	// Validate is the statement that validates the completeness CHECK of the change, named Check, such as ALTER
	// TABLE public.boards VALIDATE CONSTRAINT boards_name_complete. When a batch finds no row left, the backfill runs
	// it in its own transaction, with the lock timeout and ValidateTimeout, and marks the change complete only after
	// it passes. Empty marks the change complete when a batch finds no row left.
	Validate, Check string
	ValidateTimeout time.Duration
}

// Backfill moves the rows of one schema change in short fenced batches under the lease of the change.
type Backfill struct {
	change      string
	step        Step
	fillDigest  string
	key         KeyType
	keyParts    []KeyType
	batchSize   int
	lockTimeout time.Duration
	observer    Observer
	statement   time.Duration
	pause       time.Duration
	target      time.Duration
	maxLag      time.Duration
	validate    string
	check       string
	validateFor time.Duration

	mu         sync.Mutex
	limit      int
	db         DB
	held       *Held
	state      checkpoint
	running    bool
	complete   bool
	lastCommit time.Time
	stalled    bool
	stopping   bool
	flushErr   error
	flushed    bool
	ran        bool
	runErr     error
	idle       chan struct{}
	wake       chan struct{}
}

type checkpoint struct {
	lastKey    *Key
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
	b := &Backfill{change: change, step: step, fillDigest: settings.FillDigest, key: settings.Key, keyParts: append([]KeyType(nil), settings.KeyParts...), batchSize: settings.BatchSize, lockTimeout: settings.LockTimeout, observer: settings.Observer,
		statement: settings.StatementTimeout, pause: settings.Pause, target: settings.TargetBatchTime, maxLag: settings.MaxReplicaLag,
		validate: settings.Validate, check: settings.Check, validateFor: settings.ValidateTimeout}
	if (b.validate == "") != (b.check == "") {
		return nil, fmt.Errorf("backfill %s: the validate statement and the name of its CHECK go together", change)
	}
	switch b.key {
	case KeyInteger, KeyUUID, KeyText:
		if len(b.keyParts) > 0 {
			return nil, fmt.Errorf("backfill %s: a %s key has no parts", change, b.key)
		}
	case KeyComposite:
		if len(b.keyParts) < 2 {
			return nil, fmt.Errorf("backfill %s: a composite key needs at least two parts", change)
		}
		for _, part := range b.keyParts {
			if part != KeyInteger && part != KeyUUID && part != KeyText {
				return nil, fmt.Errorf("backfill %s: a part of a composite key must be integer, uuid or text, not %q", change, part)
			}
		}
	default:
		return nil, fmt.Errorf("backfill %s: the key type must be integer, uuid, text or composite, not %q", change, b.key)
	}
	if b.batchSize == 0 {
		b.batchSize = DefaultBatchSize
	}
	if b.lockTimeout == 0 {
		b.lockTimeout = DefaultLockTimeout
	}
	if b.statement == 0 {
		b.statement = DefaultStatementTimeout
	}
	if b.validateFor == 0 {
		b.validateFor = DefaultValidateTimeout
	}
	if b.batchSize < 0 || b.lockTimeout < 0 || b.statement < 0 || b.pause < 0 || b.target < 0 || b.maxLag < 0 || b.validateFor < 0 {
		return nil, fmt.Errorf("backfill %s: the batch size, the timeouts, the pause, the target batch time and the maximum replica lag must not be negative", change)
	}
	b.limit = b.batchSize
	return b, nil
}

// Change is the change ID.
func (b *Backfill) Change() string { return b.change }

// Stalled tells if the backfill has rows left and committed no batch for three lease durations (ST-SUP-RP09-LC12).
// The readiness of the controller mode can read it; the migration job, which runs the backfills, emits the stall.
func (b *Backfill) Stalled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stalled
}

// Run moves the rows of the change under held: one batch after the other from the checkpoint key, until a batch
// reads no row, and then it marks the change complete. When the declared fill digest differs from the row's, it
// first clears the completed time and the key and records the new digest, so the step passes again over the rows
// whose new value is unset (ST-SUP-RP09-LC11). A batch that fails, such as at the lock timeout, is rolled back
// and run again after the retry period; a fenced batch, a stopped holder or a broken session (ErrSession) ends Run. A watcher marks the backfill
// stalled when no batch commits for three lease durations, and emits the stall once.
func (b *Backfill) Run(ctx context.Context, db DB, held *Held) (runErr error) {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return fmt.Errorf("backfill %s: already running", b.change)
	}
	b.db, b.held, b.running, b.stopping, b.flushErr = db, held, true, false, nil
	b.ran, b.flushed, b.complete, b.runErr = true, false, false, nil
	b.lastCommit = held.client.clock.Now()
	b.idle = make(chan struct{})
	b.wake = make(chan struct{})
	b.mu.Unlock()
	watched := make(chan struct{})
	defer func() {
		<-watched
		b.mu.Lock()
		b.running, b.stalled, b.runErr = false, false, runErr
		close(b.idle)
		b.mu.Unlock()
	}()

	if err := b.prepare(ctx); err != nil {
		close(watched)
		return err
	}
	watch, stopWatch := context.WithCancel(ctx)
	// The watcher ends before Run returns, so a later Run of the same backfill shares nothing with it.
	defer stopWatch()
	go func() {
		defer close(watched)
		b.watch(watch, held)
	}()
	b.mu.Lock()
	wake := b.wake
	b.mu.Unlock()
	for {
		b.mu.Lock()
		complete, stopping := b.complete, b.stopping
		b.mu.Unlock()
		if stopping {
			// The flush: write the checkpoint of the last batch again with the holder's token, while the lease
			// still holds, before Run returns and the holder can release the lease.
			err := b.flush(context.WithoutCancel(ctx))
			b.mu.Lock()
			b.flushErr, b.flushed = err, true
			b.mu.Unlock()
			return err
		}
		if complete {
			return nil
		}
		lagging, err := b.lagging(ctx)
		if err == nil && lagging {
			if b.observer != nil {
				b.observer.BackfillThrottled(b.change, ThrottleReplicaLag)
			}
			err = errReplicaLag
		}
		if err == nil {
			err = b.batch(ctx)
		}
		switch {
		case err == nil:
			if b.pause > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-held.Done():
					return held.Err()
				case <-wake:
				case <-held.client.clock.After(b.pause):
				}
			}
			continue
		case errors.Is(err, ErrFenced), errors.Is(err, ErrStopped), errors.Is(err, ErrIncomplete), errors.Is(err, ErrValidate), ctx.Err() != nil:
			return err
		case brokenSession(err):
			return fmt.Errorf("backfill %s: %w: %w", b.change, ErrSession, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-held.Done():
			return held.Err()
		case <-wake:
		case <-held.client.clock.After(held.client.retry):
		}
	}
}

// errReplicaLag is the wait of a backfill for a replica whose replay lag is above the maximum.
var errReplicaLag = errors.New("backfill: a replica lags more than the maximum replica lag")

// lagging tells whether a replica's replay lag is above the maximum replica lag; with no maximum it reads nothing.
func (b *Backfill) lagging(ctx context.Context) (bool, error) {
	if b.maxLag <= 0 {
		return false, nil
	}
	var lag int64
	if err := b.db.QueryRowContext(ctx, replicaLagStatement).Scan(&lag); err != nil {
		return false, fmt.Errorf("backfill %s: read the replica lag: %w", b.change, err)
	}
	return time.Duration(lag)*time.Millisecond > b.maxLag, nil
}

// prepare reads the checkpoint and, for a declared fill value whose digest differs from the row's, starts the
// repair pass. A declaration with no fill value starts none.
func (b *Backfill) prepare(ctx context.Context) error {
	return b.inTx(ctx, func(ctx context.Context, tx *sql.Tx, row checkpoint) (checkpoint, bool, error) {
		if b.fillDigest == "" || row.fillDigest == b.fillDigest {
			return row, false, nil
		}
		row.lastKey = nil
		row.completed = false
		row.failed = 0
		row.fillDigest = b.fillDigest
		return row, true, nil
	})
}

// batch runs one batch: one transaction with the lock timeout that locks the checkpoint row, runs the step after
// the checkpoint key and writes the checkpoint (ST-SUP-RP09-LC09, ST-SUP-RP09-LC10). When the step finds no row
// left and the change has a completeness CHECK, the batch writes nothing and finish validates the CHECK first.
func (b *Backfill) batch(ctx context.Context) error {
	var result Batch
	drained := false
	b.mu.Lock()
	limit := b.limit
	b.mu.Unlock()
	started := b.held.client.clock.Now()
	err := b.inTx(ctx, func(ctx context.Context, tx *sql.Tx, row checkpoint) (checkpoint, bool, error) {
		if row.completed {
			return row, false, nil
		}
		var err error
		result, err = b.step(ctx, tx, row.lastKey, limit)
		if err != nil {
			return row, false, fmt.Errorf("backfill %s: step: %w", b.change, err)
		}
		if result.Read < 0 || result.Read > limit {
			return row, false, fmt.Errorf("backfill %s: the step read %d rows, more than the batch size %d", b.change, result.Read, limit)
		}
		if result.Read == 0 {
			if b.validate != "" {
				drained = true
				return row, false, nil
			}
			row.completed = true
			return row, true, nil
		}
		if err := result.LastKey.check(b.key, b.keyParts); err != nil {
			return row, false, fmt.Errorf("backfill %s: the step's last key: %w", b.change, err)
		}
		last := result.LastKey
		row.lastKey = &last
		row.moved += result.Moved
		row.failed += result.Failed
		row.duplicates += result.Duplicates
		return row, true, nil
	})
	if err != nil {
		return err
	}
	if drained {
		if err := b.finish(ctx); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.lastCommit = b.held.client.clock.Now()
	throttled := false
	if took := b.lastCommit.Sub(started); b.target > 0 && took > b.target && b.limit > 1 {
		b.limit, throttled = max(1, b.limit/2), true
	} else if b.target > 0 && took < b.target/2 {
		b.limit = min(b.batchSize, b.limit*2)
	}
	b.stalled = false
	completed := b.state.completed
	b.complete = completed
	b.mu.Unlock()
	if b.observer != nil {
		if throttled {
			b.observer.BackfillThrottled(b.change, ThrottleBatchTime)
		}
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

// finish marks the change complete only after its completeness CHECK validates: it runs the validate statement in
// a transaction of its own, which holds no lease row and no checkpoint row, with the lock timeout and the validate
// timeout, and then marks the change complete in a fenced checkpoint. VALIDATE CONSTRAINT reads every row of the
// table past row-level security and takes only a SHARE UPDATE EXCLUSIVE lock, so it blocks no read or write of the
// table, and it proves that no row of any tenant has the old column set and the new column unset. When a row breaks
// the CHECK (SQLSTATE 23514), finish sets the checkpoint key back to the start in a fenced checkpoint and fails with
// ErrIncomplete, which names the change and the CHECK and no row value; the next Run passes over the rows again and
// moves those that are unset. A lock timeout, a statement timeout, a serialization failure, a deadlock and another
// error with no SQLSTATE are a failed batch, which Run runs again (decision on ST-SUP-Q120 (c), orchestrator under
// the owner's delegation, 2026-11-14); a broken session, such as an administrator shutdown (57P01), ends Run with
// ErrSession (review of PR #305, L3); any other SQL error fails with ErrValidate and its cause, and Run returns it at
// once (review of PR #300, L1).
func (b *Backfill) finish(ctx context.Context) error {
	err := b.validateCheck(ctx)
	var state interface{ SQLState() string }
	if errors.As(err, &state) && state.SQLState() == "23514" {
		if err := b.inTx(ctx, func(_ context.Context, _ *sql.Tx, row checkpoint) (checkpoint, bool, error) {
			row.lastKey, row.completed = nil, false
			return row, true, nil
		}); err != nil {
			return err
		}
		return fmt.Errorf("backfill %s: %w: the CHECK %s finds a row whose old column is set and whose new column is unset, such as a row of a tenant that the tenant list does not hold; the next run passes over the rows again from the start", b.change, ErrIncomplete, b.check)
	}
	if errors.As(err, &state) && !transientStates[state.SQLState()] && !brokenSession(err) {
		return fmt.Errorf("backfill %s: %w %s: %w", b.change, ErrValidate, b.check, err)
	}
	if err != nil {
		return fmt.Errorf("backfill %s: validate the CHECK %s: %w", b.change, b.check, err)
	}
	return b.inTx(ctx, func(_ context.Context, _ *sql.Tx, row checkpoint) (checkpoint, bool, error) {
		if row.completed {
			return row, false, nil
		}
		row.completed = true
		return row, true, nil
	})
}

// validateCheck runs the validate statement in its own transaction with the lock timeout and the validate timeout.
func (b *Backfill) validateCheck(ctx context.Context) error {
	held := b.held
	held.act.RLock()
	defer held.act.RUnlock()
	if err := held.active(); err != nil {
		return err
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, lockTimeoutStatement, fmt.Sprintf("%dms", b.lockTimeout.Milliseconds()), fmt.Sprintf("%dms", b.validateFor.Milliseconds())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, b.validate); err != nil {
		return err
	}
	return tx.Commit()
}

// inTx runs change in one transaction with the lock timeout, under the lease row locked by Fence, over the
// checkpoint row locked FOR UPDATE, and writes
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
	if _, err := tx.ExecContext(ctx, lockTimeoutStatement, fmt.Sprintf("%dms", b.lockTimeout.Milliseconds()), fmt.Sprintf("%dms", b.statement.Milliseconds())); err != nil {
		return fmt.Errorf("backfill %s: lock timeout: %w", b.change, err)
	}
	// The lease row first: a holder whose lease another holder took writes nothing (ST-SUP-RP09-LC03). The
	// checkpoint token below stays as the second guard (ST-SUP-RP09-LC07).
	token, err := held.Fence(ctx, tx)
	if err != nil {
		return err
	}
	var row checkpoint
	var storedKey []byte
	var storedType sql.NullString
	err = tx.QueryRowContext(ctx, readCheckpointStatement, b.change).Scan(&storedKey, &storedType, &row.moved, &row.failed, &row.duplicates, &row.fillDigest, &row.token, &row.completed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("backfill %s: the change has no checkpoint row", b.change)
	}
	if err != nil {
		return fmt.Errorf("backfill %s: read the checkpoint: %w", b.change, err)
	}
	if storedKey != nil {
		// The stored key is read in its own type, and refused when it is not the type the batches order by: a key
		// is never compared as the text of another type.
		key, err := decodeKey(KeyType(storedType.String), storedKey)
		if err == nil {
			err = key.check(b.key, b.keyParts)
		}
		if err != nil {
			return fmt.Errorf("backfill %s: the stored checkpoint key: %w", b.change, err)
		}
		row.lastKey = &key
	}
	next, write, err := change(ctx, tx, row)
	if err != nil {
		return err
	}
	if write {
		if next, err = b.checkpoint(ctx, tx, next, token); err != nil {
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

// checkpoint runs the fenced checkpoint statement with the token that Fence read from the lease row in tx, the
// lease name and the holder, so the function itself checks the lease row (ST-SUP-RP09-LC07).
func (b *Backfill) checkpoint(ctx context.Context, tx *sql.Tx, row checkpoint, token int64) (checkpoint, error) {
	var storedKey []byte
	var storedType sql.NullString
	if row.lastKey != nil {
		storedKey = encodeKey(*row.lastKey)
		storedType = sql.NullString{String: string(row.lastKey.Type), Valid: true}
	}
	var changed int64
	if err := tx.QueryRowContext(ctx, checkpointStatement, b.change, storedKey, row.moved, row.failed, row.duplicates, row.fillDigest, row.completed, token, storedType, b.held.client.name, b.held.client.holder).Scan(&changed); err != nil {
		return row, fmt.Errorf("backfill %s: checkpoint: %w", b.change, err)
	}
	if changed != 1 {
		err := fmt.Errorf("backfill %s: %w: the lease row does not hold the holder with the token %d unexpired, or the checkpoint row holds a higher token", b.change, ErrFenced, token)
		b.held.halt(err)
		return row, err
	}
	row.token = token
	return row, nil
}

// ErrNotFlushed is the error of Flush when Run ended before it could flush, with the change not complete.
var ErrNotFlushed = errors.New("backfill: Run ended with no flush")

// Flush makes Run start no new batch, waits for the batch that runs, and waits for Run to write the checkpoint of
// the last batch again with the holder's token and return. The controller's shutdown runs it under the lease,
// before it releases the lease (ST-SUP-RP09-LC06). It returns nil when Run never ran, such as on a replica that
// never held the lease, and when the change is complete. When Run ended with no flush and the change is not
// complete, such as when Run's context ended first, it returns ErrNotFlushed with Run's error.
func (b *Backfill) Flush(ctx context.Context) error {
	b.mu.Lock()
	if b.running {
		if !b.stopping {
			b.stopping = true
			close(b.wake)
		}
		idle := b.idle
		b.mu.Unlock()
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
		b.mu.Lock()
	}
	defer b.mu.Unlock()
	switch {
	case !b.ran:
		return nil
	case b.flushed:
		return b.flushErr
	case b.complete:
		return nil
	}
	return fmt.Errorf("backfill %s: %w: %w", b.change, ErrNotFlushed, b.runErr)
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
func (b *Backfill) watch(ctx context.Context, held *Held) {
	clock := held.client.clock
	bound := 3 * held.client.ttl
	for {
		select {
		case <-ctx.Done():
			return
		case <-clock.After(held.client.renew):
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

// Copy is the batch of a rename at expand (ST-SUP-RC01-OC36): in the table public.Table it copies the old column
// From into the new column To for each row after the checkpoint key whose new value is unset and whose old value is
// set, in the order of the key. The key is (TenantColumn, id) for an owned table, a composite key of a text and a
// uuid, and id for a shared one, a uuid key. Tenants is the qualified tenant list, such as public.stego_tenants.
//
// The batch runs as the owner role, which row-level security binds to the tenant of the transaction, so a batch of
// an owned table reads the rows of one tenant: it sets app.tenant to the tenant of the checkpoint key and reads the
// rows of that tenant after the key; when there is none, it goes on with the next tenant of the tenant list, in the
// order of the key. It sets BackfillSetting first, so the sync trigger of the change passes the batch's rows
// (ST-SUP-RC01-OC32). Check is the completeness CHECK of the step on the table, which Validate validates before the
// change is marked complete, since the walk over the tenant list does not read a row of a tenant that the list does
// not hold (decision on ST-SUP-Q120 (c), orchestrator under the owner's delegation, 2026-11-14).
type Copy struct {
	Change, Table, TenantColumn, From, To, Tenants, Check string
}

// Validate gives the statement that validates the completeness CHECK of c.
func (c Copy) Validate() string {
	return "ALTER TABLE public." + quoteName(c.Table) + " VALIDATE CONSTRAINT " + quoteName(c.Check)
}

// copySettingStatement sets the backfill setting and the tenant of the batch's transaction only.
const copySettingStatement = `SELECT pg_catalog.set_config('stego.backfill_change', $1, true), pg_catalog.set_config('app.tenant', $2, true)`

// Settings gives the key type of the batches of c.
func (c Copy) Settings() (KeyType, []KeyType) {
	if c.TenantColumn == "" {
		return KeyUUID, nil
	}
	return KeyComposite, []KeyType{KeyText, KeyUUID}
}

// Step gives the step of c, or an error for a name that is empty or not a plain lower-case SQL name.
func (c Copy) Step() (Step, error) {
	for _, n := range []string{c.Change, c.Table, c.From, c.To, c.Check} {
		if n == "" {
			return nil, errors.New("backfill: a copy needs its change, table, columns and completeness CHECK")
		}
	}
	if c.TenantColumn != "" && c.Tenants == "" {
		return nil, errors.New("backfill: the copy of an owned table needs the tenant list")
	}
	table := "public." + quoteName(c.Table)
	from, to := quoteName(c.From), quoteName(c.To)
	where := ""
	if c.TenantColumn != "" {
		where = quoteName(c.TenantColumn) + " = $3 AND "
	}
	batch := `WITH batch AS (SELECT id FROM ` + table + ` WHERE ` + where + `($1::text IS NULL OR id > $1::uuid) ORDER BY id LIMIT $2),
moved AS (UPDATE ` + table + ` t SET ` + to + ` = t.` + from + ` FROM batch b WHERE ` + strings.ReplaceAll(where, quoteName(c.TenantColumn), "t."+quoteName(c.TenantColumn)) +
		`t.id = b.id AND t.` + to + ` IS NULL AND t.` + from + ` IS NOT NULL RETURNING 1)
SELECT (SELECT pg_catalog.count(*) FROM batch), (SELECT pg_catalog.count(*) FROM moved), (SELECT id::text FROM batch ORDER BY id DESC LIMIT 1)`
	next := `SELECT pg_catalog.min(tenant_id) FROM ` + c.Tenants + ` WHERE $1::text IS NULL OR tenant_id > $1::text`
	return func(ctx context.Context, tx *sql.Tx, after *Key, limit int) (Batch, error) {
		var tenant, id sql.NullString
		if after != nil {
			if c.TenantColumn == "" {
				id = sql.NullString{String: after.Value, Valid: true}
			} else if len(after.Parts) == 2 {
				tenant = sql.NullString{String: after.Parts[0].Value, Valid: true}
				id = sql.NullString{String: after.Parts[1].Value, Valid: true}
			}
		}
		if c.TenantColumn != "" && !tenant.Valid {
			if err := tx.QueryRowContext(ctx, next, nil).Scan(&tenant); err != nil {
				return Batch{}, err
			}
		}
		for {
			if c.TenantColumn != "" && !tenant.Valid {
				return Batch{}, nil
			}
			if _, err := tx.ExecContext(ctx, copySettingStatement, c.Change, tenant.String); err != nil {
				return Batch{}, err
			}
			args := []any{id, limit}
			if c.TenantColumn != "" {
				args = append(args, tenant.String)
			}
			var read, moved int64
			var last sql.NullString
			if err := tx.QueryRowContext(ctx, batch, args...).Scan(&read, &moved, &last); err != nil {
				return Batch{}, err
			}
			if read > 0 {
				key := UUIDKey(last.String)
				if c.TenantColumn != "" {
					key = CompositeKey(TextKey(tenant.String), key)
				}
				return Batch{Read: int(read), LastKey: key, Moved: moved}, nil
			}
			if c.TenantColumn == "" {
				return Batch{}, nil
			}
			if err := tx.QueryRowContext(ctx, next, tenant.String).Scan(&tenant); err != nil {
				return Batch{}, err
			}
			id = sql.NullString{}
		}
	}, nil
}

// quoteName quotes one SQL name.
func quoteName(n string) string { return `"` + strings.ReplaceAll(n, `"`, `""`) + `"` }
