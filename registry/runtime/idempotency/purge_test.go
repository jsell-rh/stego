package idempotency

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// countingExecer records each batch of Purge, a transaction of its own, and the key rows there were when it began, so
// batches gives the rows that each batch removed.
type countingExecer struct {
	db     *sql.DB
	before []int64
}

func (c *countingExecer) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	var n int64
	if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&n); err != nil {
		return nil, err
	}
	c.before = append(c.before, n)
	return c.db.BeginTx(ctx, opts)
}

// batches gives the rows that each batch removed, once the purge has ended.
func (c *countingExecer) batches() []int64 {
	var now int64
	_ = c.db.QueryRow(`SELECT count(*) FROM idempotency_keys`).Scan(&now)
	out := make([]int64, len(c.before))
	for i, n := range c.before {
		next := now
		if i+1 < len(c.before) {
			next = c.before[i+1]
		}
		out[i] = n - next
	}
	return out
}

func insertExpiring(t *testing.T, db *sql.DB, prefix string, n int, expires time.Time) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO idempotency_keys
		(tenant_id, principal_id, key, fingerprint, status, response_code, locked_at, expires_at)
		SELECT 't1', 'p1', $1 || g, 'f', 'completed', 201, $2, $2
		FROM generate_series(1, $3) AS g`, prefix, expires, n); err != nil {
		t.Fatal(err)
	}
}

func TestPurge(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	t.Run("ST-SUP-RP07-LC07-T01 Purge deletes only expired rows", func(t *testing.T) {
		db, _ := testDB(t)
		insertExpiring(t, db, "old", 10, now.Add(-time.Minute))
		insertExpiring(t, db, "live", 5, now.Add(time.Minute))
		n, err := Purge(ctx, db, PurgeOptions{Schema: testSchema(t, db), Clock: clock, Sleep: func(context.Context, time.Duration) error { return nil }})
		if err != nil || n != 10 {
			t.Fatalf("Purge = %d, %v; want 10", n, err)
		}
		if got := countRows(t, db, `SELECT count(*) FROM idempotency_keys WHERE key LIKE 'live%'`); got != 5 {
			t.Fatalf("%d live rows kept, want 5", got)
		}
		if got := countRows(t, db, `SELECT count(*) FROM idempotency_keys WHERE key LIKE 'old%'`); got != 0 {
			t.Fatalf("%d expired rows kept, want 0", got)
		}
	})

	t.Run("ST-SUP-RP07-LC07-T02 Purge deletes in bounded batches with a pause between them", func(t *testing.T) {
		db, _ := testDB(t)
		insertExpiring(t, db, "old", 2500, now.Add(-time.Hour))
		rec := &countingExecer{db: db}
		var pauses []time.Duration
		n, err := Purge(ctx, rec, PurgeOptions{Schema: testSchema(t, db), Batch: 1000, Pause: 250 * time.Millisecond, Clock: clock,
			Sleep: func(_ context.Context, d time.Duration) error { pauses = append(pauses, d); return nil }})
		if err != nil || n != 2500 {
			t.Fatalf("Purge = %d, %v; want 2500", n, err)
		}
		if len(rec.batches()) < 3 || len(rec.batches()) > 4 {
			t.Fatalf("%d DELETE statements %v, want three or four", len(rec.batches()), rec.batches())
		}
		var sum int64
		for _, d := range rec.batches() {
			if d > 1000 {
				t.Fatalf("a DELETE removed %d rows, more than the batch of 1000", d)
			}
			sum += d
		}
		if sum != 2500 {
			t.Fatalf("the DELETE statements removed %d rows, want 2500", sum)
		}
		if len(pauses) != len(rec.batches())-1 {
			t.Fatalf("%d pauses for %d batches, want one between each two", len(pauses), len(rec.batches()))
		}
		for _, p := range pauses {
			if p != 250*time.Millisecond {
				t.Fatalf("a pause of %v, want 250ms", p)
			}
		}
		if got := countRows(t, db, `SELECT count(*) FROM idempotency_keys`); got != 0 {
			t.Fatalf("%d rows left, want 0", got)
		}
	})
}

func TestPurgeLimits(t *testing.T) {
	t.Run("ST-SUP-RP07-LC07-T04 A batch that waits on a locked key row ends after the lock timeout, and the next run removes it", func(t *testing.T) {
		ctx := context.Background()
		db, _ := testDB(t)
		insertExpiring(t, db, "old", 3, time.Now().Add(-time.Hour))
		holder, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback() }()
		var key string
		if err := holder.QueryRowContext(ctx, `SELECT key FROM idempotency_keys WHERE key = 'old1' FOR UPDATE`).Scan(&key); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = Purge(ctx, db, PurgeOptions{Schema: testSchema(t, db), Sleep: func(context.Context, time.Duration) error { return nil }})
		took := time.Since(start)
		if pgCode(err) != "55P03" || took < PurgeLockTimeout || took > PurgeLockTimeout+5*time.Second {
			t.Fatalf("Purge with a locked row gave %v after %v, want a lock timeout after about %v", err, took, PurgeLockTimeout)
		}
		if n := countRows(t, db, `SELECT count(*) FROM idempotency_keys`); n != 3 {
			t.Fatalf("the batch that timed out removed rows: %d left, want 3", n)
		}
		// The limits hold only in the transaction of the batch.
		var lock, statement string
		if err := db.QueryRowContext(ctx, `SELECT current_setting('lock_timeout'), current_setting('statement_timeout')`).Scan(&lock, &statement); err != nil || lock != "0" || statement != "0" {
			t.Fatalf("the session keeps lock_timeout %q and statement_timeout %q: %v", lock, statement, err)
		}
		if err := holder.Rollback(); err != nil {
			t.Fatal(err)
		}
		if n, err := Purge(ctx, db, PurgeOptions{Schema: testSchema(t, db), Sleep: func(context.Context, time.Duration) error { return nil }}); err != nil || n != 3 {
			t.Fatalf("the next run gave %d, %v; want the 3 expired rows", n, err)
		}
		// The statement timeout is SET LOCAL in the transaction of the batch.
		if limitsSQL != `SELECT pg_catalog.set_config('lock_timeout', '1s', true), pg_catalog.set_config('statement_timeout', '30s', true)` {
			t.Fatalf("the limits of a batch are %s", limitsSQL)
		}
	})
}
