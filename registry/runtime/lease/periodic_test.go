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
		held := acquire(t, newClient(t, d.db, "stego.controller", "controller-a", Settings{}))
		var current atomic.Pointer[Held]
		current.Store(held)
		clock := newFakeClock()
		var runs atomic.Int64
		var withLease atomic.Int64
		p, err := NewPeriodic(PeriodicConfig{Lease: current.Load, Clock: clock, Jobs: []Job{{Name: "audit-anchor",
			Interval: time.Second, Run: func(ctx context.Context, h *Held) error {
				n := runs.Add(1)
				if h == held && h.Err() == nil {
					withLease.Add(1)
				}
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
			clock.Advance(999 * time.Millisecond)
			time.Sleep(20 * time.Millisecond)
			if n := runs.Load(); n != int64(i-1) {
				t.Fatalf("%d runs before the interval passed, want %d", n, i-1)
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
		if withLease.Load() != 4 {
			t.Fatalf("%d runs got the held lease, want 4", withLease.Load())
		}

		// Another holder takes the lease: the renew fails, the holder stops, and the job runs no more.
		d.exec(t, "UPDATE leases SET holder = 'controller-b', token = token + 1 WHERE name = 'stego.controller'")
		if err := held.Renew(ctx); !errors.Is(err, ErrLost) {
			t.Fatalf("the renew gave %v, want ErrLost", err)
		}
		for range 2 {
			clock.Advance(time.Second)
			clock.waitWaiters(t, 1)
		}
		if s := stats(); runs.Load() != 4 || s.Runs != 4 || s.Skipped != 2 {
			t.Fatalf("after the lease was lost the counts are %+v with %d runs, want no run and 2 skips", s, runs.Load())
		}
		current.Store(nil)
		clock.Advance(time.Second)
		clock.waitWaiters(t, 1)
		if s := stats(); runs.Load() != 4 || s.Skipped != 3 {
			t.Fatalf("with no lease the counts are %+v, want a third skip", s)
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
