package lease

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"stego.dev/runtime/modes"
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

func (it *items) step(ctx context.Context, tx *sql.Tx, after sql.NullString, limit int) (Batch, error) {
	it.mu.Lock()
	n := len(it.calls)
	it.calls = append(it.calls, call{after: after})
	gate := it.gate
	it.mu.Unlock()
	if gate != nil {
		gate(n)
	}
	result, written, err := it.move(ctx, tx, after, limit)
	it.mu.Lock()
	it.calls[n].written, it.calls[n].err = written, err
	it.mu.Unlock()
	return result, err
}

func (it *items) move(ctx context.Context, tx *sql.Tx, after sql.NullString, limit int) (Batch, []int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id, old FROM items WHERE ($1::text IS NULL OR id > $1::bigint) AND new_value IS NULL ORDER BY id LIMIT $2 FOR UPDATE", after, limit)
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
		result.LastKey = strconv.FormatInt(r.id, 10)
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
	moved      int64
	failed     int64
	fillDigest string
	token      int64
	completed  sql.NullTime
}

func checkpointRow(t *testing.T, d *database) row {
	t.Helper()
	var r row
	err := d.other.QueryRowContext(context.Background(), "SELECT last_key, rows_moved, failed_rows, fill_digest, token, completed_at FROM stego_schema.backfills WHERE change_id=$1", backfillChange).
		Scan(&r.lastKey, &r.moved, &r.failed, &r.fillDigest, &r.token, &r.completed)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newBackfill(t *testing.T, it *items, settings BackfillSettings) *Backfill {
	t.Helper()
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

		// B takes the expired lease with a higher token and commits one batch.
		deadline := time.Now().Add(10 * time.Second)
		for checkpointRow(t, d).moved != 100 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		d.expire(t, "stego.backfill."+backfillChange)
		b := acquire(t, newClient(t, d.db, "stego.backfill."+backfillChange, "b", Settings{}))
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
		case strings.HasPrefix(text, "update stego_schema.backfills"):
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
		}
		b := newBackfill(t, it, BackfillSettings{BatchSize: 100, LockTimeout: 200 * time.Millisecond, Observer: observed})
		c := newClient(t, d.db, "stego.backfill."+backfillChange, "h1", Settings{TTL: ttl, Renew: renew, Retry: 100 * time.Millisecond})
		if err := Declare(ctx, d.db, c.Name()); err != nil {
			t.Fatal(err)
		}
		readiness := modes.NewReadiness(d.other)
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
		deadline := time.Now().Add(5 * time.Second)
		ready := false
		for !ready && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			commitMu.Lock()
			committed := lastCommit.After(stallFrom)
			commitMu.Unlock()
			ready = committed && probe(t, server.URL+"/readyz") == http.StatusOK
		}
		if !ready {
			t.Fatal("the controller stayed not ready after the next batch committed")
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
