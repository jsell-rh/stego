package idempotency

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

// jobClock is a process clock that moves only when the test advances it.
type jobClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []jobWaiter
}

type jobWaiter struct {
	at time.Time
	ch chan time.Time
}

func (c *jobClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *jobClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, jobWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *jobClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
}

// count is the number of timers that wait now.
func (c *jobClock) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// waitWaiters waits on the real clock until n timers wait.
func (c *jobClock) waitWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		count := len(c.waiters)
		c.mu.Unlock()
		if count >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %d timers wait on the clock", n)
}

func TestPurgeJob(t *testing.T) {
	t.Run("ST-SUP-RP07-LC09-T01 The purge job runs on its interval and stops with its context", func(t *testing.T) {
		db, _ := testDB(t)
		clock := &jobClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
		insertExpiring(t, db, "old", 7, clock.Now().Add(-time.Second))
		insertExpiring(t, db, "later", 3, clock.Now().Add(90*time.Second))
		insertExpiring(t, db, "live", 4, clock.Now().Add(time.Hour))
		rec := &countingExecer{db: db}
		j, err := NewPurgeJob(rec, PurgeJobConfig{Schema: testSchema(t, db), Interval: time.Minute, Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- j.Run(ctx) }()
		clock.waitWaiters(t, 1)
		if j.Runs() != 1 || countRows(t, db, `SELECT count(*) FROM idempotency_keys WHERE key LIKE 'old%'`) != 0 ||
			countRows(t, db, `SELECT count(*) FROM idempotency_keys`) != 7 {
			t.Fatalf("at start: %d runs, deletes %v; want the 7 expired rows gone", j.Runs(), rec.batches())
		}
		clock.Advance(59 * time.Second)
		time.Sleep(20 * time.Millisecond)
		if j.Runs() != 1 {
			t.Fatal("the job purged before the interval passed")
		}
		clock.Advance(time.Second)
		clock.waitWaiters(t, 1)
		if j.Runs() != 2 {
			t.Fatalf("after the first tick: %d runs, want 2", j.Runs())
		}
		clock.Advance(time.Minute)
		clock.waitWaiters(t, 1)
		if j.Runs() != 3 || len(rec.batches()) != 3 || rec.batches()[2] != 3 {
			t.Fatalf("after the second tick: %d runs, deletes %v; want one purge per tick, the third of the 3 rows due by then", j.Runs(), rec.batches())
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run gave %v after the cancel", err)
		}
		clock.Advance(time.Minute)
		time.Sleep(20 * time.Millisecond)
		if j.Runs() != 3 {
			t.Fatal("the job purged after its context ended")
		}

		// A purge whose pause waits on the job's clock ends with ctx, and is not counted as a run.
		insertExpiring(t, db, "many", 5, clock.Now().Add(-time.Second))
		paused, err := NewPurgeJob(db, PurgeJobConfig{Schema: testSchema(t, db), Batch: 2, Pause: time.Hour, Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		pctx, pcancel := context.WithCancel(context.Background())
		done = make(chan error, 1)
		go func() { done <- paused.Run(pctx) }()
		clock.waitWaiters(t, 1)
		pcancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run gave %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the pause of a purge did not end with its context")
		}
		if paused.Runs() != 0 || paused.Failures() != 0 {
			t.Fatalf("the cancelled purge counts %d runs and %d failures, want none", paused.Runs(), paused.Failures())
		}

		// A failed purge is counted and reported, and the next tick runs again.
		failing, err := NewPurgeJob(failingQuerier{db: closedDB(t)}, PurgeJobConfig{Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		// Abandoned timers of the jobs above stay on the clock, so this part waits for the runs.
		waitRuns := func(j *PurgeJob, n int64) {
			t.Helper()
			deadline := time.Now().Add(10 * time.Second)
			for j.Runs() < n && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
		}
		go func() { _ = failing.Run(ctx) }()
		waitRuns(failing, 1)
		time.Sleep(20 * time.Millisecond)
		clock.Advance(DefaultPurgeInterval)
		waitRuns(failing, 2)
		if failing.Failures() != 2 || failing.Runs() != 2 {
			t.Fatalf("the failing job has %d runs and %d failures, want 2 and 2", failing.Runs(), failing.Failures())
		}
		if ok, why := failing.Health(); ok || why == "" {
			t.Fatal("the failed purge is not reported")
		}

		// Two runs of one job never purge at once.
		slow := &slowQuerier{db: db}
		both, err := NewPurgeJob(slow, PurgeJobConfig{Schema: testSchema(t, db), Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = both.Run(ctx) }()
		go func() { _ = both.Run(ctx) }()
		waitRuns(both, 2)
		if slow.calls != 2 || slow.max != 1 {
			t.Fatalf("the two runs made %d purges with at most %d at once, want 2 one after the other", slow.calls, slow.max)
		}
	})
}

// failingQuerier is a database that is down: each statement fails.
type failingQuerier struct{ db *sql.DB }

func (f failingQuerier) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return f.db.BeginTx(ctx, opts)
}

// closedDB is a pool that is closed, so each statement on it fails at once.
func closedDB(t *testing.T) *sql.DB {
	t.Helper()
	closed, err := sql.Open("pgx", "postgres://127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	return closed
}

// slowQuerier takes 50 ms to begin each batch and records the most batches that began at once.
type slowQuerier struct {
	db          *sql.DB
	mu          sync.Mutex
	active, max int
	calls       int
}

func (s *slowQuerier) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	s.mu.Lock()
	s.active++
	s.calls++
	s.max = max(s.max, s.active)
	s.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return s.db.BeginTx(ctx, opts)
}
