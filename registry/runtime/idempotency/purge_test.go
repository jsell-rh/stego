package idempotency

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// countingExecer records each call of the purge function that Purge runs and the rows it removed.
type countingExecer struct {
	db      *sql.DB
	deletes []int64
}

func (c *countingExecer) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	row := c.db.QueryRowContext(ctx, q, args...)
	if strings.Contains(q, PurgeFunction+"(") {
		var n int64
		if row.Scan(&n) == nil {
			c.deletes = append(c.deletes, n)
		}
		return c.db.QueryRowContext(ctx, `SELECT $1::bigint`, n)
	}
	return row
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
		n, err := Purge(ctx, db, PurgeOptions{Clock: clock, Sleep: func(context.Context, time.Duration) error { return nil }})
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
		n, err := Purge(ctx, rec, PurgeOptions{Batch: 1000, Pause: 250 * time.Millisecond, Clock: clock,
			Sleep: func(_ context.Context, d time.Duration) error { pauses = append(pauses, d); return nil }})
		if err != nil || n != 2500 {
			t.Fatalf("Purge = %d, %v; want 2500", n, err)
		}
		if len(rec.deletes) < 3 || len(rec.deletes) > 4 {
			t.Fatalf("%d DELETE statements %v, want three or four", len(rec.deletes), rec.deletes)
		}
		var sum int64
		for _, d := range rec.deletes {
			if d > 1000 {
				t.Fatalf("a DELETE removed %d rows, more than the batch of 1000", d)
			}
			sum += d
		}
		if sum != 2500 {
			t.Fatalf("the DELETE statements removed %d rows, want 2500", sum)
		}
		if len(pauses) != len(rec.deletes)-1 {
			t.Fatalf("%d pauses for %d batches, want one between each two", len(pauses), len(rec.deletes))
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
