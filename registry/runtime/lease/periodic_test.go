package lease

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"stego.dev/runtime/modes"
)

func TestPeriodic(t *testing.T) {
	t.Run("ST-SUP-RP09-LC13-T01 A periodic job runs at its interval only while the lease is held", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		client := newClient(t, d.db, "stego.controller", "controller-a", Settings{})
		held := acquire(t, client)
		var current atomic.Pointer[Held]
		current.Store(held)
		clock := newFakeClock()
		var runs, withLease atomic.Int64
		var deadlineOK atomic.Bool
		deadlineOK.Store(true)
		p, err := NewPeriodic(PeriodicConfig{Lease: current.Load, Clock: clock, Retry: 100 * time.Millisecond,
			Jobs: []Job{{Name: "audit-anchor", Interval: time.Second, Run: func(ctx context.Context, h *Held) error {
				n := runs.Add(1)
				if h == current.Load() && h.Err() == nil {
					withLease.Add(1)
				}
				if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(h.Expires()) {
					deadlineOK.Store(false)
				}
				// Each run takes 300 ms of the process clock: the next start is 1 s after this start.
				clock.Advance(300 * time.Millisecond)
				if n == 2 {
					return errors.New("the sink is down")
				}
				return nil
			}}}})
		if err != nil {
			t.Fatal(err)
		}
		registry := modes.NewRegistry()
		if err := registry.Register(modes.Controller, modes.Component{Name: PeriodicName, Run: p.Run}); err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		running, err := registry.Start(runCtx, modes.Selection{modes.Controller})
		if err != nil {
			t.Fatal(err)
		}
		stats := func() JobStats {
			s, ok := p.Stats("audit-anchor")
			if !ok {
				t.Fatal("no counts of the job")
			}
			return s
		}

		clock.waitWaiters(t, 1)
		if n := runs.Load(); n != 1 {
			t.Fatalf("%d runs at start, want one", n)
		}
		for i := 2; i <= 4; i++ {
			// The last run started 1 s ago less 1 ms: 300 ms of it went in the run, so 699 ms are left to wait.
			clock.Advance(699 * time.Millisecond)
			time.Sleep(20 * time.Millisecond)
			if n := runs.Load(); n != int64(i-1) {
				t.Fatalf("%d runs before the interval from the last start passed, want %d", n, i-1)
			}
			clock.Advance(time.Millisecond)
			clock.waitWaiters(t, 1)
			if n := runs.Load(); n != int64(i) {
				t.Fatalf("%d runs after %d intervals, want %d", n, i-1, i)
			}
			if i == 2 {
				if ok, why := p.Health(); ok || why == "" {
					t.Fatal("the failed run is not reported in the health")
				}
			}
		}
		if s := stats(); s.Runs != 4 || s.Failures != 1 || s.Skipped != 0 || s.LastFailed {
			t.Fatalf("the counts are %+v, want 4 runs, 1 failure and no skip", s)
		}
		if ok, _ := p.Health(); !ok {
			t.Fatal("the health still reports the job after a run that succeeded")
		}
		if withLease.Load() != 4 || !deadlineOK.Load() {
			t.Fatalf("%d runs got the held lease, and the deadline was the lease's expiry: %t", withLease.Load(), deadlineOK.Load())
		}

		// Another holder takes the lease: the renew fails, the holder stops, and the job runs no more.
		d.exec(t, "UPDATE leases SET holder = 'controller-b', token = token + 1 WHERE name = 'stego.controller'")
		if err := held.Renew(ctx); !errors.Is(err, ErrLost) {
			t.Fatalf("the renew gave %v, want ErrLost", err)
		}
		clock.Advance(700 * time.Millisecond)
		clock.waitWaiters(t, 1)
		// After a skip the job tries again at the retry period, not at the interval.
		for range 2 {
			clock.Advance(100 * time.Millisecond)
			clock.waitWaiters(t, 1)
		}
		if s := stats(); runs.Load() != 4 || s.Skipped != 3 {
			t.Fatalf("after the lease was lost the counts are %+v with %d runs, want no run and 3 skips", s, runs.Load())
		}
		// The controller acquires the lease again: the job runs at the next retry, not one interval later.
		d.expire(t, "stego.controller")
		again := acquire(t, client)
		current.Store(again)
		clock.Advance(100 * time.Millisecond)
		clock.waitWaiters(t, 1)
		if n := runs.Load(); n != 5 || withLease.Load() != 5 {
			t.Fatalf("%d runs (%d with the lease) after the lease was acquired again, want 5", n, withLease.Load())
		}
		cancel()
		if err := running.Wait(); err != nil {
			t.Fatalf("the component ended with %v", err)
		}
		if _, err := NewPeriodic(PeriodicConfig{Lease: current.Load, Jobs: []Job{{Name: "x", Run: func(context.Context, *Held) error { return nil }}}}); err == nil {
			t.Fatal("a job with no interval was accepted")
		}
	})
}
