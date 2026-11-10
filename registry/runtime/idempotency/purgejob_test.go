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
		j, err := NewPurgeJob(rec, PurgeJobConfig{Interval: time.Minute, Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- j.Run(ctx) }()
		clock.waitWaiters(t, 1)
		if j.Runs() != 0 || len(rec.deletes) != 0 {
			t.Fatal("the job purged before its first tick")
		}
		clock.Advance(59 * time.Second)
		time.Sleep(20 * time.Millisecond)
		if j.Runs() != 0 {
			t.Fatal("the job purged before the interval passed")
		}
		clock.Advance(time.Second)
		clock.waitWaiters(t, 1)
		if j.Runs() != 1 || countRows(t, db, `SELECT count(*) FROM idempotency_keys WHERE key LIKE 'old%'`) != 0 ||
			countRows(t, db, `SELECT count(*) FROM idempotency_keys`) != 7 {
			t.Fatalf("after the first tick: %d runs, deletes %v; want the 7 expired rows gone", j.Runs(), rec.deletes)
		}
		clock.Advance(time.Minute)
		clock.waitWaiters(t, 1)
		if j.Runs() != 2 || len(rec.deletes) != 2 || rec.deletes[1] != 3 {
			t.Fatalf("after the second tick: %d runs, deletes %v; want one purge per tick", j.Runs(), rec.deletes)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run gave %v after the cancel", err)
		}
		clock.Advance(time.Minute)
		time.Sleep(20 * time.Millisecond)
		if j.Runs() != 2 {
			t.Fatal("the job purged after its context ended")
		}

		// A failed purge is counted and reported, and the next tick runs again.
		failing, err := NewPurgeJob(failingExecer{}, PurgeJobConfig{Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = failing.Run(ctx) }()
		for i := 1; i <= 2; i++ {
			clock.waitWaiters(t, 1)
			clock.Advance(DefaultPurgeInterval)
		}
		clock.waitWaiters(t, 1)
		if failing.Failures() != 2 || failing.Runs() != 2 {
			t.Fatalf("the failing job has %d runs and %d failures, want 2 and 2", failing.Runs(), failing.Failures())
		}
		if ok, why := failing.Health(); ok || why == "" {
			t.Fatal("the failed purge is not reported")
		}

		// Two runs of one job never purge at once.
		slow := &slowExecer{}
		both, err := NewPurgeJob(slow, PurgeJobConfig{Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = both.Run(ctx) }()
		go func() { _ = both.Run(ctx) }()
		clock.waitWaiters(t, 3)
		clock.Advance(DefaultPurgeInterval)
		clock.waitWaiters(t, 3)
		if slow.calls != 2 || slow.max != 1 {
			t.Fatalf("the two runs made %d purges with at most %d at once, want 2 one after the other", slow.calls, slow.max)
		}
	})
}

type failingExecer struct{}

func (failingExecer) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("the database is down")
}

// slowExecer takes 50 ms for each statement and records the most statements that ran at once.
type slowExecer struct {
	mu          sync.Mutex
	active, max int
	calls       int
}

func (s *slowExecer) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	s.mu.Lock()
	s.active++
	s.calls++
	s.max = max(s.max, s.active)
	s.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return driverResult(0), nil
}

type driverResult int64

func (driverResult) LastInsertId() (int64, error)   { return 0, nil }
func (r driverResult) RowsAffected() (int64, error) { return int64(r), nil }
