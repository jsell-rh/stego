package lease

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jsell-rh/stego/registry/runtime/modes"
)

const backfillChange = "ST-CHG-01"

// observations records the schema backfill observations, as the probe recorder does.
type observations struct {
	mu        sync.Mutex
	rows      int64
	failed    int64
	groups    int64
	completed int
	stalled   []time.Time
	changes   []string
	onRows    func(batch int)
	batches   int
	throttled []string
	// onThrottle runs after each throttle with its count.
	onThrottle func(n int)
}

func (o *observations) BackfillRows(change string, rows int64) {
	o.mu.Lock()
	o.rows += rows
	o.changes = append(o.changes, change)
	o.batches++
	batch, on := o.batches, o.onRows
	o.mu.Unlock()
	if on != nil {
		on(batch)
	}
}

func (o *observations) BackfillFailedRows(change string, rows int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failed += rows
	o.changes = append(o.changes, change)
}

func (o *observations) BackfillDuplicateGroups(change string, groups int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.groups += groups
	o.changes = append(o.changes, change)
}

func (o *observations) BackfillCompleted(change string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.completed++
	o.changes = append(o.changes, change)
}

func (o *observations) BackfillStalled(change string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stalled = append(o.stalled, time.Now())
	o.changes = append(o.changes, change)
}

func (o *observations) BackfillThrottled(change, reason string) {
	o.mu.Lock()
	o.throttled = append(o.throttled, reason)
	o.changes = append(o.changes, change)
	n, on := len(o.throttled), o.onThrottle
	o.mu.Unlock()
	if on != nil {
		on(n)
	}
}

func (o *observations) throttledReasons() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.throttled...)
}

func (o *observations) stalls() []time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]time.Time(nil), o.stalled...)
}

// call is one call of the test step: the key it started after and the keys it wrote.
type call struct {
	after   sql.NullString
	written []int64
	err     error
}

// items is the table of a retype from text to bigint: old is the old column, new_value the new one. The step is
// the stand-in for the generated step: it reads the rows after the key whose new value is unset, in key order,
// converts old, and gives the fill value to a row that does not convert when the declaration has one.
type items struct {
	d    *database
	fill *int64

	mu    sync.Mutex
	calls []call
	gate  func(n int)
}

func newItems(t *testing.T, d *database, rows int, bad ...int64) *items {
	t.Helper()
	d.exec(t, "CREATE TABLE items (id bigint PRIMARY KEY, old text NOT NULL, new_value bigint)")
	d.exec(t, "INSERT INTO items (id, old) SELECT n, n::text FROM generate_series(1, $1) AS n", rows)
	for _, id := range bad {
		d.exec(t, "UPDATE items SET old='not a number' WHERE id=$1", id)
	}
	d.exec(t, "INSERT INTO stego_schema.backfills (change_id) VALUES ($1)", backfillChange)
	return &items{d: d}
}

func (it *items) step(ctx context.Context, tx *sql.Tx, key *Key, limit int) (Batch, error) {
	var after sql.NullString
	var arg any
	if key != nil {
		after = sql.NullString{String: key.Value, Valid: true}
		arg = key.Args()[0]
	}
	it.mu.Lock()
	n := len(it.calls)
	it.calls = append(it.calls, call{after: after})
	gate := it.gate
	it.mu.Unlock()
	if gate != nil {
		gate(n)
	}
	result, written, err := it.move(ctx, tx, arg, limit)
	it.mu.Lock()
	it.calls[n].written, it.calls[n].err = written, err
	it.mu.Unlock()
	return result, err
}

func (it *items) move(ctx context.Context, tx *sql.Tx, after any, limit int) (Batch, []int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id, old FROM items WHERE ($1::bigint IS NULL OR id > $1::bigint) AND new_value IS NULL ORDER BY id LIMIT $2 FOR UPDATE", after, limit)
	if err != nil {
		return Batch{}, nil, err
	}
	type row struct {
		id  int64
		old string
	}
	var picked []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.old); err != nil {
			rows.Close()
			return Batch{}, nil, err
		}
		picked = append(picked, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Batch{}, nil, err
	}
	var result Batch
	var written []int64
	for _, r := range picked {
		result.Read++
		result.LastKey = IntegerKey(r.id)
		value, err := strconv.ParseInt(r.old, 10, 64)
		if err != nil {
			if it.fill == nil {
				result.Failed++
				continue
			}
			value = *it.fill
		}
		if _, err := tx.ExecContext(ctx, "UPDATE items SET new_value=$1 WHERE id=$2", value, r.id); err != nil {
			return Batch{}, nil, err
		}
		result.Moved++
		written = append(written, r.id)
	}
	return result, written, nil
}

func (it *items) allCalls() []call {
	it.mu.Lock()
	defer it.mu.Unlock()
	return append([]call(nil), it.calls...)
}

// row is the checkpoint row of the change as the second session reads it.
type row struct {
	lastKey    sql.NullString
	keyType    sql.NullString
	stored     []byte
	moved      int64
	failed     int64
	fillDigest string
	token      int64
	completed  sql.NullTime
}

func checkpointRow(t *testing.T, d *database) row {
	t.Helper()
	var r row
	var stored []byte
	err := d.other.QueryRowContext(context.Background(), "SELECT last_key, last_key_type, rows_moved, failed_rows, fill_digest, token, completed_at FROM stego_schema.backfills WHERE change_id=$1", backfillChange).
		Scan(&stored, &r.keyType, &r.moved, &r.failed, &r.fillDigest, &r.token, &r.completed)
	if err != nil {
		t.Fatal(err)
	}
	r.stored = stored
	r.lastKey = sql.NullString{String: string(stored), Valid: stored != nil}
	return r
}

func newBackfill(t *testing.T, it *items, settings BackfillSettings) *Backfill {
	t.Helper()
	if settings.Key == "" {
		settings.Key = KeyInteger
	}
	b, err := NewBackfill(backfillChange, it.step, settings)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func heldLease(t *testing.T, d *database, holder string, settings Settings) *Held {
	t.Helper()
	if err := Declare(context.Background(), d.db, "stego.backfill."+backfillChange); err != nil {
		t.Fatal(err)
	}
	return acquire(t, newClient(t, d.db, "stego.backfill."+backfillChange, holder, settings))
}

func TestBackfillCheckpoint(t *testing.T) {
	t.Run("ST-SUP-RP09-LC03-T04 A backfill batch of a holder whose lease was taken writes nothing", func(t *testing.T) {
		d := openDatabase(t)
		it := newItems(t, d, 250)
		ctx := context.Background()
		a := heldLease(t, d, "a", Settings{})
		// B takes the lease before it writes its first checkpoint; the checkpoint row still holds no higher token.
		d.expire(t, "stego.backfill."+backfillChange)
		b := acquire(t, newClient(t, d.db, "stego.backfill."+backfillChange, "b", Settings{}))
		err := newBackfill(t, it, BackfillSettings{BatchSize: 100}).Run(ctx, d.db, a)
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("the backfill of A gave %v, want ErrFenced", err)
		}
		if calls := it.allCalls(); len(calls) != 0 {
			t.Fatalf("the step of A ran %d times, want none", len(calls))
		}
		var moved int
		if err := d.other.QueryRowContext(ctx, "SELECT count(*) FROM items WHERE new_value IS NOT NULL").Scan(&moved); err != nil || moved != 0 {
			t.Fatalf("%d rows hold the new value (%v), want none", moved, err)
		}
		if got := checkpointRow(t, d); got.token != 0 || got.lastKey.Valid {
			t.Fatalf("the checkpoint row is %+v, want it unchanged", got)
		}
		if err := newBackfill(t, it, BackfillSettings{BatchSize: 100}).Run(ctx, d.db, b); err != nil {
			t.Fatalf("the backfill of B gave %v", err)
		}
		if got := checkpointRow(t, d); got.token != b.Token() || got.moved != 250 || !got.completed.Valid {
			t.Fatalf("after B the checkpoint row is %+v, want B's token, 250 rows and complete", got)
		}
	})

	t.Run("ST-SUP-RP09-LC07-T01 A checkpoint write with a lower token than the row's fails as fenced and changes nothing", func(t *testing.T) {
		d := openDatabase(t)
		it := newItems(t, d, 250)
		ctx := context.Background()
		a := heldLease(t, d, "a", Settings{})

		// A commits one batch, then waits before its next batch until B has committed one.
		bCommitted := make(chan struct{})
		aStep := make(chan struct{})
		aInStep := make(chan struct{})
		aObserved := &observations{onRows: func(batch int) {
			if batch == 1 {
				<-bCommitted
			}
		}}
		it.gate = func(n int) {
			if n == 2 {
				close(aInStep)
				<-aStep
			}
		}
		aBackfill := newBackfill(t, it, BackfillSettings{BatchSize: 100, Observer: aObserved})
		aResult := make(chan error, 1)
		go func() { aResult <- aBackfill.Run(ctx, d.db, a) }()

		// B commits one batch with a higher token. B holds another lease row than A, so that the lease fence of
		// each batch does not stop A first: the checkpoint statement is then the only guard, and this test proves it.
		deadline := time.Now().Add(10 * time.Second)
		for checkpointRow(t, d).moved != 100 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		const other = "stego.backfill." + backfillChange + ".other"
		if err := Declare(ctx, d.db, other); err != nil {
			t.Fatal(err)
		}
		d.exec(t, "UPDATE leases SET token=1000 WHERE name=$1", other)
		b := acquire(t, newClient(t, d.db, other, "b", Settings{}))
		bWaits := make(chan struct{})
		bObserved := &observations{onRows: func(batch int) {
			if batch == 1 {
				close(bCommitted)
				<-bWaits
			}
		}}
		bBackfill := newBackfill(t, it, BackfillSettings{BatchSize: 100, Observer: bObserved})
		bResult := make(chan error, 1)
		go func() { bResult <- bBackfill.Run(ctx, d.db, b) }()

		// A runs its next batch and holds the checkpoint row; then B's next batch waits on the row lock.
		within(t, aInStep, "the next batch of A")
		close(bWaits)
		waitForLockWait(t, d)
		close(aStep)

		if err := within(t, aResult, "the end of A"); !errors.Is(err, ErrFenced) {
			t.Fatalf("the batch of A gave %v, want ErrFenced", err)
		}
		if err := within(t, bResult, "the end of B"); err != nil {
			t.Fatalf("the backfill of B gave %v", err)
		}
		got := checkpointRow(t, d)
		if got.token != b.Token() {
			t.Fatalf("the row holds token %d, want B's %d", got.token, b.Token())
		}
		if bObserved.batches < 2 {
			t.Fatalf("B committed %d batches, want its waiting batch committed too", bObserved.batches)
		}
		var moved int
		if err := d.other.QueryRowContext(ctx, "SELECT count(*) FROM items WHERE new_value IS NOT NULL").Scan(&moved); err != nil || moved != 250 || got.moved != 250 {
			t.Fatalf("%d rows hold the new value and the row counts %d moved (%v), want 250 each: A's batch rolled back", moved, got.moved, err)
		}
	})
}

// waitForLockWait waits until a session of the test database waits on a lock.
func waitForLockWait(t *testing.T, d *database) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		err := d.other.QueryRowContext(context.Background(), "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'").Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no session waits on the row lock")
}

func TestBackfillProgress(t *testing.T) {
	t.Run("ST-SUP-RP09-LC08-T01 Backfill progress is recorded as counts with no row value", func(t *testing.T) {
		d := openDatabase(t)
		it := newItems(t, d, 250, 17, 101, 230)
		observed := &observations{}
		b := newBackfill(t, it, BackfillSettings{Observer: observed})
		if err := b.Run(context.Background(), d.db, heldLease(t, d, "h1", Settings{})); err != nil {
			t.Fatal(err)
		}
		if observed.rows != 247 || observed.failed != 3 || observed.completed != 1 {
			t.Fatalf("the counts are rows %d, failed rows %d, completed %d, want 247, 3 and 1", observed.rows, observed.failed, observed.completed)
		}
		for _, change := range observed.changes {
			if change != backfillChange {
				t.Fatalf("an emission carries %q, want only the change ID", change)
			}
		}
	})
}

// keyedStep is a stand-in for a generated step that orders by the key alone, with no filter on the new value, so
// that only the checkpoint key keeps a row from being moved twice. It records the order of the moves.
type keyedStep struct {
	mu    sync.Mutex
	moved []string
}

func (k *keyedStep) record(row string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.moved = append(k.moved, row)
}

func (k *keyedStep) all() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.moved...)
}

func newKeyedBackfill(t *testing.T, step Step, settings BackfillSettings) *Backfill {
	t.Helper()
	b, err := NewBackfill(backfillChange, step, settings)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBackfillKeyTypes(t *testing.T) {
	t.Run("ST-SUP-RP09-LC09-T02 An integer checkpoint compares as an integer", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		d.exec(t, "CREATE TABLE numbers (id bigint PRIMARY KEY, moves int NOT NULL DEFAULT 0)")
		d.exec(t, "INSERT INTO numbers (id) SELECT generate_series(1, 20)")
		d.exec(t, "INSERT INTO stego_schema.backfills (change_id) VALUES ($1)", backfillChange)
		k := &keyedStep{}
		step := func(ctx context.Context, tx *sql.Tx, after *Key, limit int) (Batch, error) {
			var arg any
			if after != nil {
				arg = after.Args()[0]
			}
			rows, err := tx.QueryContext(ctx, "UPDATE numbers SET moves=moves+1 WHERE id IN (SELECT id FROM numbers WHERE ($1::bigint IS NULL OR id > $1::bigint) ORDER BY id LIMIT $2 FOR UPDATE) RETURNING id", arg, limit)
			if err != nil {
				return Batch{}, err
			}
			var ids []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return Batch{}, err
				}
				ids = append(ids, id)
			}
			rows.Close()
			slices.Sort(ids)
			var batch Batch
			for _, id := range ids {
				k.record(strconv.FormatInt(id, 10))
				batch.Read++
				batch.Moved++
				batch.LastKey = IntegerKey(id)
			}
			return batch, rows.Err()
		}
		var checkpoints []string
		observed := &observations{}
		observed.onRows = func(int) {
			r := checkpointRow(t, d)
			checkpoints = append(checkpoints, r.keyType.String+":"+r.lastKey.String)
		}
		b := newKeyedBackfill(t, step, BackfillSettings{Key: KeyInteger, BatchSize: 9, Observer: observed})
		// A key that compares wrong moves the same rows again and again; the bound ends such a run.
		bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := b.Run(bounded, d.db, heldLease(t, d, "h1", Settings{})); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(checkpoints) != "[integer:9 integer:18 integer:20]" {
			t.Fatalf("the checkpoints are %v, want 9, 18 and 20 stored as integers", checkpoints)
		}
		var wrong int
		if err := d.other.QueryRowContext(ctx, "SELECT count(*) FROM numbers WHERE moves <> 1").Scan(&wrong); err != nil || wrong != 0 {
			t.Fatalf("%d rows were not moved exactly once (%v)", wrong, err)
		}
		if got := checkpointRow(t, d); !got.completed.Valid || got.moved != 20 || observed.completed != 1 {
			t.Fatalf("the change is complete %v with %d rows moved, want complete after key 20 with 20", got.completed.Valid, got.moved)
		}
	})

	t.Run("ST-SUP-RP09-LC09-T03 A composite checkpoint resumes between two rows of one leading part", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, "CREATE TABLE pairs (a integer NOT NULL, b text NOT NULL, moves int NOT NULL DEFAULT 0, PRIMARY KEY (a, b))")
		d.exec(t, "INSERT INTO pairs (a, b) VALUES (1, 'x'), (1, 'y'), (2, 'a'), (2, 'b'), (2, 'c'), (3, 'a'), (3, 'b')")
		d.exec(t, "INSERT INTO stego_schema.backfills (change_id) VALUES ($1)", backfillChange)
		k := &keyedStep{}
		step := func(ctx context.Context, tx *sql.Tx, after *Key, limit int) (Batch, error) {
			query := "SELECT a, b FROM pairs ORDER BY a, b LIMIT $1 FOR UPDATE"
			args := []any{limit}
			if after != nil {
				query = "SELECT a, b FROM pairs WHERE (a, b) > ($2::integer, $3::text) ORDER BY a, b LIMIT $1 FOR UPDATE"
				args = append(args, after.Args()...)
			}
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return Batch{}, err
			}
			type pair struct {
				a int64
				b string
			}
			var picked []pair
			for rows.Next() {
				var p pair
				if err := rows.Scan(&p.a, &p.b); err != nil {
					rows.Close()
					return Batch{}, err
				}
				picked = append(picked, p)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return Batch{}, err
			}
			var batch Batch
			for _, p := range picked {
				if _, err := tx.ExecContext(ctx, "UPDATE pairs SET moves=moves+1 WHERE a=$1 AND b=$2", p.a, p.b); err != nil {
					return Batch{}, err
				}
				k.record(fmt.Sprintf("%d%s", p.a, p.b))
				batch.Read++
				batch.Moved++
				batch.LastKey = CompositeKey(IntegerKey(p.a), TextKey(p.b))
			}
			return batch, nil
		}
		settings := BackfillSettings{Key: KeyComposite, KeyParts: []KeyType{KeyInteger, KeyText}, BatchSize: 3}
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		first := settings
		first.Observer = &observations{onRows: func(int) { stop() }}
		if err := newKeyedBackfill(t, step, first).Run(ctx, d.db, heldLease(t, d, "h1", Settings{})); !errors.Is(err, context.Canceled) {
			t.Fatalf("the first runner gave %v, want its stop after one batch", err)
		}
		got := checkpointRow(t, d)
		part := func(field string) []byte {
			return append(binary.BigEndian.AppendUint32(nil, uint32(len(field))), field...)
		}
		var want []byte
		for _, field := range []string{"integer", "2", "text", "a"} {
			want = append(want, part(field)...)
		}
		if got.keyType.String != "composite" || !bytes.Equal(got.stored, want) {
			t.Fatalf("the checkpoint is %q of type %q, want the two typed, length-prefixed parts %q", got.stored, got.keyType.String, want)
		}
		bounded, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := newKeyedBackfill(t, step, settings).Run(bounded, d.db, heldLease(t, d, "h1", Settings{})); err != nil {
			t.Fatal(err)
		}
		if moved := fmt.Sprint(k.all()); moved != "[1x 1y 2a 2b 2c 3a 3b]" {
			t.Fatalf("the rows moved in the order %s, want each once in (a, b) order", moved)
		}
		var wrong int
		if err := d.other.QueryRowContext(context.Background(), "SELECT count(*) FROM pairs WHERE moves <> 1").Scan(&wrong); err != nil || wrong != 0 {
			t.Fatalf("%d rows were not moved exactly once (%v)", wrong, err)
		}
	})
}

func TestBackfillBatch(t *testing.T) {
	t.Run("ST-SUP-RP09-LC09-T01 A batch is bounded, in key order, and times out on a lock with the checkpoint unchanged", func(t *testing.T) {
		d := openDatabase(t)
		it := newItems(t, d, 250)
		ctx := context.Background()
		lock, err := d.other.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Rollback() }()
		var lockedCheckpoint row
		observed := &observations{onRows: func(batch int) {
			if batch == 1 {
				if _, err := lock.ExecContext(ctx, "SELECT id FROM items WHERE id=150 FOR UPDATE"); err != nil {
					t.Error(err)
				}
			}
		}}
		it.gate = func(n int) {
			if n == 2 {
				// The second batch met the lock and timed out; the third runs after the lock is gone.
				lockedCheckpoint = checkpointRow(t, d)
				_ = lock.Rollback()
			}
		}
		b := newBackfill(t, it, BackfillSettings{BatchSize: 100, LockTimeout: 300 * time.Millisecond, Observer: observed})
		d.rec.reset()
		if err := b.Run(ctx, d.db, heldLease(t, d, "h1", Settings{Retry: 50 * time.Millisecond})); err != nil {
			t.Fatal(err)
		}
		calls := it.allCalls()
		if len(calls) < 3 {
			t.Fatalf("the step ran %d times, want the lock timeout and a retry", len(calls))
		}
		var pgErr *pgconn.PgError
		if !errors.As(calls[1].err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("the batch that met the lock gave %v, want the lock timeout 55P03", calls[1].err)
		}
		if lockedCheckpoint.lastKey.String != "100" || lockedCheckpoint.moved != 100 {
			t.Fatalf("after the lock timeout the checkpoint is %v with %d moved, want 100 and 100 unchanged", lockedCheckpoint.lastKey, lockedCheckpoint.moved)
		}
		var last int64
		for i, c := range calls {
			if len(c.written) > 100 {
				t.Fatalf("batch %d moved %d rows, more than 100", i, len(c.written))
			}
			if c.err != nil {
				continue
			}
			for _, id := range c.written {
				if id <= last {
					t.Fatalf("batch %d wrote %d after %d, not in key order", i, id, last)
				}
				last = id
			}
		}
		checkpointInTransaction(t, d.rec.all())
	})
}

// checkpointInTransaction checks that each transaction of the backfill that moved rows and committed wrote the
// checkpoint between its begin and its commit, on its own connection.
func checkpointInTransaction(t *testing.T, statements []statement) {
	t.Helper()
	type transaction struct{ moved, checkpoint bool }
	open := make(map[any]*transaction)
	committed := 0
	for _, s := range statements {
		text := strings.ToLower(strings.TrimSpace(s.sql))
		switch {
		case strings.HasPrefix(text, "begin"):
			open[s.conn] = &transaction{}
		case strings.HasPrefix(text, "update items"):
			if tx := open[s.conn]; tx != nil {
				tx.moved = true
			}
		case strings.HasPrefix(text, "select stego_schema.backfill_checkpoint("):
			if tx := open[s.conn]; tx != nil {
				tx.checkpoint = true
			}
		case text == "commit":
			tx := open[s.conn]
			if tx != nil && tx.moved {
				committed++
				if !tx.checkpoint {
					t.Fatal("a batch committed rows with no checkpoint in its transaction")
				}
			}
			delete(open, s.conn)
		case text == "rollback":
			delete(open, s.conn)
		}
	}
	if committed != 3 {
		t.Fatalf("%d batches committed rows, want 3", committed)
	}
}

func TestBackfillResumes(t *testing.T) {
	t.Run("ST-SUP-RP09-LC10-T01 A new runner starts after the checkpoint and marks the change complete", func(t *testing.T) {
		d := openDatabase(t)
		it := newItems(t, d, 250)
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		first := newBackfill(t, it, BackfillSettings{BatchSize: 100, Observer: &observations{onRows: func(batch int) {
			if batch == 2 {
				stop()
			}
		}}})
		if err := first.Run(ctx, d.db, heldLease(t, d, "h1", Settings{})); !errors.Is(err, context.Canceled) {
			t.Fatalf("the first runner gave %v, want its stop", err)
		}
		if err := first.Flush(context.Background()); !errors.Is(err, ErrNotFlushed) {
			t.Fatalf("the flush of a runner that ended with no flush gave %v, want ErrNotFlushed", err)
		}
		if got := checkpointRow(t, d); got.lastKey.String != "200" || got.completed.Valid {
			t.Fatalf("after two batches the checkpoint is %v, complete %v, want 200 and not complete", got.lastKey, got.completed.Valid)
		}
		started := len(it.allCalls())
		lastBatch := time.Now()
		d.expire(t, "stego.backfill."+backfillChange)
		second := newBackfill(t, it, BackfillSettings{BatchSize: 100})
		if err := second.Run(context.Background(), d.db, heldLease(t, d, "h2", Settings{})); err != nil {
			t.Fatal(err)
		}
		calls := it.allCalls()[started:]
		if len(calls) == 0 || calls[0].after.String != "200" {
			t.Fatalf("the new runner started after %v, want 200", calls)
		}
		got := checkpointRow(t, d)
		if !got.completed.Valid || got.moved != 250 {
			t.Fatalf("the change is complete %v with %d rows moved, want complete with 250", got.completed.Valid, got.moved)
		}
		if got.completed.Time.Before(lastBatch.Add(-time.Second)) {
			t.Fatalf("the completed time %s is before the last batch at %s", got.completed.Time, lastBatch)
		}
	})
}

func TestBackfillRepair(t *testing.T) {
	t.Run("ST-SUP-RP09-LC11-T01 A new fill value passes again over the unset rows only", func(t *testing.T) {
		d := openDatabase(t)
		it := newItems(t, d, 250, 3, 120, 249)
		ctx := context.Background()
		if err := newBackfill(t, it, BackfillSettings{}).Run(ctx, d.db, heldLease(t, d, "h1", Settings{})); err != nil {
			t.Fatal(err)
		}
		if got := checkpointRow(t, d); !got.completed.Valid || got.failed != 3 {
			t.Fatalf("the first pass is complete %v with %d failed rows, want complete with 3", got.completed.Valid, got.failed)
		}
		fill := int64(-1)
		it.fill = &fill
		started := len(it.allCalls())
		var cleared row
		it.gate = func(n int) {
			if n == started {
				cleared = checkpointRow(t, d)
			}
		}
		const digest = "sha256:fill-minus-one"
		if err := newBackfill(t, it, BackfillSettings{FillDigest: digest}).Run(ctx, d.db, heldLease(t, d, "h1", Settings{})); err != nil {
			t.Fatal(err)
		}
		if cleared.completed.Valid || cleared.lastKey.Valid || cleared.fillDigest != digest {
			t.Fatalf("before the repair pass the row is %+v, want no completed time, no key and the new digest", cleared)
		}
		var written []int64
		for _, c := range it.allCalls()[started:] {
			written = append(written, c.written...)
		}
		if fmt.Sprint(written) != "[3 120 249]" {
			t.Fatalf("the repair pass wrote %v, want only 3, 120 and 249", written)
		}
		var filled int
		if err := d.other.QueryRowContext(ctx, "SELECT count(*) FROM items WHERE new_value=-1").Scan(&filled); err != nil || filled != 3 {
			t.Fatalf("%d rows hold the fill value (%v), want 3", filled, err)
		}
		if got := checkpointRow(t, d); !got.completed.Valid || got.fillDigest != digest {
			t.Fatalf("after the repair the row is %+v, want complete with the new digest", got)
		}
		// A declaration with no fill value starts no repair pass.
		started = len(it.allCalls())
		if err := newBackfill(t, it, BackfillSettings{}).Run(ctx, d.db, heldLease(t, d, "h1", Settings{})); err != nil {
			t.Fatal(err)
		}
		if calls := it.allCalls()[started:]; len(calls) != 0 {
			t.Fatalf("a run with no fill value ran the step %d times, want none", len(calls))
		}
		if got := checkpointRow(t, d); !got.completed.Valid || got.fillDigest != digest {
			t.Fatalf("after a run with no fill value the row is %+v, want it unchanged", got)
		}
	})
}

func TestBackfillStall(t *testing.T) {
	t.Run("ST-SUP-RP09-LC12-T01 A stall makes the controller not ready and keeps it live", func(t *testing.T) {
		d := openDatabase(t)
		it := newItems(t, d, 250)
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		ttl := time.Second
		renew := 600 * time.Millisecond
		lock, err := d.other.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Rollback() }()
		var lastCommit time.Time
		var commitMu sync.Mutex
		locked := make(chan struct{})
		nextBatch := make(chan struct{})
		checked := make(chan struct{})
		observed := &observations{}
		observed.onRows = func(batch int) {
			commitMu.Lock()
			lastCommit = time.Now()
			commitMu.Unlock()
			if batch == 1 {
				if _, err := lock.ExecContext(ctx, "SELECT id FROM items WHERE id=101 FOR UPDATE"); err != nil {
					t.Error(err)
				}
				close(locked)
			}
			if batch == 2 {
				// The first batch after the stall committed: hold the next one until the test read readiness.
				close(nextBatch)
				<-checked
			}
		}
		b := newBackfill(t, it, BackfillSettings{BatchSize: 100, LockTimeout: 200 * time.Millisecond, Observer: observed})
		c := newClient(t, d.db, "stego.backfill."+backfillChange, "h1", Settings{TTL: ttl, Renew: renew, Retry: 100 * time.Millisecond})
		if err := Declare(ctx, d.db, c.Name()); err != nil {
			t.Fatal(err)
		}
		readiness := modes.NewReadiness(d.other)
		readiness.WatchQueue(readyQueue{})
		readiness.Watch(b)
		server := httptest.NewServer(readiness.Handler(modes.Selection{modes.Controller}))
		defer server.Close()
		result := make(chan error, 1)
		go func() {
			result <- c.Hold(ctx, func(ctx context.Context, h *Held) error { return b.Run(ctx, d.db, h) })
		}()
		within(t, locked, "the first batch")
		commitMu.Lock()
		stallFrom := lastCommit
		commitMu.Unlock()
		var notReadyAt time.Time
		for time.Since(stallFrom) < 4*ttl {
			select {
			case err := <-result:
				t.Fatalf("the holder stopped during the stall: %v", err)
			default:
			}
			if probe(t, server.URL+"/livez") != http.StatusOK {
				t.Fatal("the controller is not live during the stall")
			}
			if probe(t, server.URL+"/readyz") != http.StatusOK && notReadyAt.IsZero() {
				notReadyAt = time.Now()
			}
			time.Sleep(200 * time.Millisecond)
		}
		if notReadyAt.IsZero() || notReadyAt.Sub(stallFrom) > 3*ttl+renew+200*time.Millisecond {
			t.Fatalf("the backfill was stalled at %v after the last commit, want within three TTLs and one renew interval", notReadyAt.Sub(stallFrom))
		}
		stalls := observed.stalls()
		if len(stalls) != 1 || stalls[0].Sub(stallFrom) > 3*ttl+renew+200*time.Millisecond {
			t.Fatalf("schema.backfill.stalled was emitted %d times (%v), want once within three TTLs and one renew interval", len(stalls), stalls)
		}
		_ = lock.Rollback()
		within(t, nextBatch, "the next batch")
		code := probe(t, server.URL+"/readyz")
		complete := checkpointRow(t, d).completed.Valid
		close(checked)
		if code != http.StatusOK || complete {
			t.Fatalf("after the next batch, with the change complete %v, readiness answered %d, want 200 before the change is complete", complete, code)
		}
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("the holder gave %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the backfill did not complete")
		}
		if !checkpointRow(t, d).completed.Valid {
			t.Fatal("the change is not complete")
		}
	})
}

// probe answers the status of one probe request.
func probe(t *testing.T, url string) int {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

// readyQueue is a controller queue that runs and is ready, so the readiness of the controller mode reads the
// backfill only.
type readyQueue struct{}

func (readyQueue) QueueReady() bool { return true }

func TestBackfillThrottle(t *testing.T) {
	const id = "ST-SUP-RP09-LC09-T04 A backfill pauses between batches, halves a slow batch, waits for a lagging replica and bounds each statement"
	t.Run(id+": pause", func(t *testing.T) {
		d := openDatabase(t)
		it := newItems(t, d, 30)
		var mu sync.Mutex
		var commits []time.Time
		b := newBackfill(t, it, BackfillSettings{BatchSize: 10, Pause: 300 * time.Millisecond, Observer: &observations{onRows: func(int) {
			mu.Lock()
			commits = append(commits, time.Now())
			mu.Unlock()
		}}})
		if err := b.Run(context.Background(), d.db, heldLease(t, d, "h1", Settings{})); err != nil {
			t.Fatal(err)
		}
		if len(commits) != 3 {
			t.Fatalf("%d batches committed, want 3", len(commits))
		}
		for i := 1; i < len(commits); i++ {
			if gap := commits[i].Sub(commits[i-1]); gap < 300*time.Millisecond {
				t.Fatalf("batch %d committed %s after the one before, less than the pause", i+1, gap)
			}
		}
	})
	t.Run(id+": target batch time", func(t *testing.T) {
		d := openDatabase(t)
		it := newItems(t, d, 40)
		var limits []int
		slow := func(ctx context.Context, tx *sql.Tx, key *Key, limit int) (Batch, error) {
			limits = append(limits, limit)
			if len(limits) <= 2 {
				if _, err := tx.ExecContext(ctx, "SELECT pg_sleep(0.3)"); err != nil {
					return Batch{}, err
				}
			}
			return it.step(ctx, tx, key, limit)
		}
		observed := &observations{}
		b, err := NewBackfill(backfillChange, slow, BackfillSettings{Key: KeyInteger, BatchSize: 16, TargetBatchTime: 200 * time.Millisecond, Observer: observed})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Run(context.Background(), d.db, heldLease(t, d, "h1", Settings{})); err != nil {
			t.Fatal(err)
		}
		if len(limits) < 4 || limits[0] != 16 || limits[1] != 8 || limits[2] != 4 || limits[3] != 8 {
			t.Fatalf("the batch sizes were %v, want 16, 8, 4 and then 8 after a fast batch", limits)
		}
		if got := observed.throttledReasons(); len(got) != 2 || got[0] != ThrottleBatchTime {
			t.Fatalf("schema.backfill.throttled was %v, want batch_time twice", got)
		}
	})
	t.Run(id+": maximum replica lag", func(t *testing.T) {
		d := openDatabase(t)
		it := newItems(t, d, 20)
		lagging := "SELECT 5000::bigint"
		saved := replicaLagStatement
		replicaLagStatement = lagging
		defer func() { replicaLagStatement = saved }()
		observed := &observations{}
		var once sync.Once
		observed.onThrottle = func(n int) {
			if n == 3 {
				once.Do(func() { replicaLagStatement = "SELECT 10::bigint" })
			}
		}
		b := newBackfill(t, it, BackfillSettings{BatchSize: 10, MaxReplicaLag: time.Second, Observer: observed})
		if err := b.Run(context.Background(), d.db, heldLease(t, d, "h1", Settings{Retry: 50 * time.Millisecond})); err != nil {
			t.Fatal(err)
		}
		calls := it.allCalls()
		if got := observed.throttledReasons(); len(got) != 3 || got[0] != ThrottleReplicaLag {
			t.Fatalf("schema.backfill.throttled was %v, want replica_lag three times before the first batch", got)
		}
		if len(calls) != 3 || observed.rows != 20 {
			t.Fatalf("the step ran %d times and moved %d rows, want 3 runs and 20 rows after the lag dropped", len(calls), observed.rows)
		}
	})
	t.Run(id+": statement timeout", func(t *testing.T) {
		d := openDatabase(t)
		it := newItems(t, d, 10)
		var runs int
		hang := func(ctx context.Context, tx *sql.Tx, key *Key, limit int) (Batch, error) {
			runs++
			if runs == 1 {
				if _, err := tx.ExecContext(ctx, "SELECT pg_sleep(2)"); err != nil {
					return Batch{}, err
				}
			}
			return it.step(ctx, tx, key, limit)
		}
		var first error
		check := func(ctx context.Context, tx *sql.Tx, key *Key, limit int) (Batch, error) {
			b, err := hang(ctx, tx, key, limit)
			if runs == 1 {
				first = err
			}
			return b, err
		}
		b, err := NewBackfill(backfillChange, check, BackfillSettings{Key: KeyInteger, BatchSize: 10, StatementTimeout: 200 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Run(context.Background(), d.db, heldLease(t, d, "h1", Settings{Retry: 50 * time.Millisecond})); err != nil {
			t.Fatal(err)
		}
		var pgErr *pgconn.PgError
		if !errors.As(first, &pgErr) || pgErr.Code != "57014" {
			t.Fatalf("the first batch gave %v, want the statement timeout 57014", first)
		}
		if r := checkpointRow(t, d); r.moved != 10 || !r.completed.Valid {
			t.Fatalf("the checkpoint moved %d rows, complete %v; want 10 and complete after the retry", r.moved, r.completed.Valid)
		}
	})
}
