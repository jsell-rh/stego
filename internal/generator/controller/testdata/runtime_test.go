package controller

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

var denied = errors.New("denied")

func options() Options {
	return Options{QueueCapacity: 4, ResyncInterval: 5 * time.Millisecond, ReconcileTimeout: 100 * time.Millisecond, ReconnectDelay: time.Millisecond, Terminal: func(err error) bool { return errors.Is(err, denied) }}
}
func idle[T any](ctx context.Context) (func() (T, error), error) {
	return func() (T, error) { <-ctx.Done(); var zero T; return zero, ctx.Err() }, nil
}
func run[T any](t *testing.T, source Source[T], action func(context.Context, T) error, o Options) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, source, action, o) }()
	return cancel, done
}
func result(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("controller did not stop")
		return nil
	}
}
func TestWatchBeforeScanAndSerialActions(t *testing.T) {
	var opened, active atomic.Bool
	var scanned atomic.Int32
	o := options()
	type record struct {
		ID      string
		Deleted bool
	}
	source := Source[record]{Watch: func(ctx context.Context) (func() (record, error), error) {
		opened.Store(true)
		return idle[record](ctx)
	}, Scan: func(ctx context.Context, emit func(record) error) error {
		if !opened.Load() {
			t.Error("scan started before watch")
		}
		if scanned.Add(1) == 1 {
			if err := emit(record{"live", false}); err != nil {
				return err
			}
			return emit(record{"retained", true})
		}
		return nil
	}}
	count := 0
	stop, done := run(t, source, func(ctx context.Context, item record) error {
		if active.Swap(true) {
			t.Error("parallel action")
		}
		defer active.Store(false)
		count++
		if (count == 1 && (item.ID != "live" || item.Deleted)) || (count == 2 && (item.ID != "retained" || !item.Deleted)) {
			t.Error("item order or content changed")
		}
		if count == 2 {
			return denied
		}
		return nil
	}, o)
	defer stop()
	if !errors.Is(result(t, done), denied) || count != 2 {
		t.Fatal("typed actions did not finish")
	}
}
func TestTransientFailureIsRecoveredByScan(t *testing.T) {
	source := Source[string]{Watch: idle[string], Scan: func(ctx context.Context, emit func(string) error) error { return emit("record") }}
	attempts := 0
	_, done := run(t, source, func(ctx context.Context, id string) error {
		attempts++
		if attempts == 1 {
			return io.ErrUnexpectedEOF
		}
		return denied
	}, options())
	if !errors.Is(result(t, done), denied) || attempts != 2 {
		t.Fatal("failed work was not retried through retained state")
	}
}
func TestSlowScanCompletesWithoutOverlap(t *testing.T) {
	var scanning atomic.Bool
	source := Source[string]{Watch: idle[string], Scan: func(ctx context.Context, emit func(string) error) error {
		if scanning.Swap(true) {
			t.Error("overlapping scan")
		}
		defer scanning.Store(false)
		timer := time.NewTimer(25 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return emit("late")
		}
	}}
	_, done := run(t, source, func(context.Context, string) error { return denied }, options())
	if !errors.Is(result(t, done), denied) {
		t.Fatal("resync interval canceled a slow scan")
	}
}
func TestOverflowCancelsActionJoinsWorkersAndRescans(t *testing.T) {
	var sessions, scans atomic.Int32
	var live atomic.Int32
	actionStarted := make(chan struct{})
	o := options()
	o.QueueCapacity = 1
	source := Source[int]{Watch: func(ctx context.Context) (func() (int, error), error) {
		if live.Load() != 0 {
			t.Error("old receiver still active")
		}
		n := sessions.Add(1)
		if n > 1 {
			return idle[int](ctx)
		}
		i := 0
		return func() (int, error) {
			live.Add(1)
			defer live.Add(-1)
			i++
			if i == 1 {
				return 1, nil
			}
			<-actionStarted
			return i, nil
		}, nil
	}, Scan: func(ctx context.Context, emit func(int) error) error {
		scans.Add(1)
		if sessions.Load() > 1 {
			return emit(99)
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	attempts := 0
	_, done := run(t, source, func(ctx context.Context, item int) error {
		attempts++
		if attempts == 1 {
			close(actionStarted)
			<-ctx.Done()
			return ctx.Err()
		}
		if item != 99 {
			t.Errorf("stale queue item survived reconnect: %d", item)
		}
		return denied
	}, o)
	if !errors.Is(result(t, done), denied) || sessions.Load() != 2 || scans.Load() < 1 {
		t.Fatal("overflow did not recover")
	}
}
func TestPermissionFailureIsNotHiddenByCancellation(t *testing.T) {
	for _, where := range []string{"watch", "scan", "action"} {
		t.Run(where, func(t *testing.T) {
			var opens atomic.Int32
			source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
				opens.Add(1)
				if where == "watch" {
					return nil, denied
				}
				return idle[string](ctx)
			}, Scan: func(ctx context.Context, emit func(string) error) error {
				if where == "scan" {
					return denied
				}
				return emit("x")
			}}
			_, done := run(t, source, func(context.Context, string) error { return denied }, options())
			if !errors.Is(result(t, done), denied) || opens.Load() != 1 {
				t.Fatal("permission failure retried or hidden")
			}
		})
	}
}
func TestActionDeadlineAndCancellationJoin(t *testing.T) {
	o := options()
	o.ReconcileTimeout = 5 * time.Millisecond
	var scansStopped atomic.Int32
	var calls atomic.Int32
	o.Observe = func(e Event) {
		if e.Phase == "reconcile_failed" && !errors.Is(e.Err, context.DeadlineExceeded) {
			t.Error("action timeout was lost")
		}
	}
	source := Source[string]{Watch: idle[string], Scan: func(ctx context.Context, emit func(string) error) error {
		defer scansStopped.Add(1)
		if err := emit("x"); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	err := Run(ctx, source, func(call context.Context, _ string) error {
		calls.Add(1)
		<-call.Done()
		cancel()
		return call.Err()
	}, o)
	if err != nil || calls.Load() != 1 || scansStopped.Load() != 1 {
		t.Fatal("cancel did not join scan", err)
	}
}
func TestDisconnectRecoversBeforeRetry(t *testing.T) {
	var opens atomic.Int32
	source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
		if opens.Add(1) == 1 {
			return func() (string, error) { return "", io.EOF }, nil
		}
		return idle[string](ctx)
	}, Scan: func(ctx context.Context, emit func(string) error) error {
		if opens.Load() > 1 {
			return emit("retained")
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	_, done := run(t, source, func(context.Context, string) error { return denied }, options())
	if !errors.Is(result(t, done), denied) || opens.Load() != 2 {
		t.Fatal("disconnect did not rescan")
	}
}
func TestInvalidOptionsDoNotOpenSource(t *testing.T) {
	valid := options()
	cases := []Options{valid, valid, valid, valid, valid, valid}
	cases[0].QueueCapacity = 0
	cases[1].QueueCapacity = 65537
	cases[2].Terminal = nil
	cases[3].ResyncInterval = -1
	cases[4].ReconcileTimeout = 0
	cases[5].ReconnectDelay = 0
	for _, o := range cases {
		source := Source[int]{Watch: func(context.Context) (func() (int, error), error) {
			t.Error("invalid options opened source")
			return nil, nil
		}, Scan: func(context.Context, func(int) error) error { return nil }}
		if Run(context.Background(), source, func(context.Context, int) error { return nil }, o) == nil {
			t.Fatal("invalid options accepted")
		}
	}
}

func TestActionTimeoutSchedulesAnotherPass(t *testing.T) {
	o := options()
	o.ReconcileTimeout = 5 * time.Millisecond
	notices := 0
	o.Observe = func(e Event) {
		if e.Phase == "reconcile_failed" {
			notices++
			if !errors.Is(e.Err, context.DeadlineExceeded) {
				t.Error("missing deadline error")
			}
		}
	}
	source := Source[string]{Watch: idle[string], Scan: func(ctx context.Context, emit func(string) error) error { return emit("x") }}
	attempts := 0
	_, done := run(t, source, func(ctx context.Context, _ string) error {
		attempts++
		if attempts == 1 {
			<-ctx.Done()
			return nil
		}
		return denied
	}, o)
	if !errors.Is(result(t, done), denied) || attempts != 2 || notices != 1 {
		t.Fatal("timeout did not retain retry")
	}
}

func TestWatchSetupHasDeadline(t *testing.T) {
	o := options()
	o.ReconcileTimeout = 5 * time.Millisecond
	opens := 0
	source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
		opens++
		if opens == 2 {
			return nil, denied
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}, Scan: func(context.Context, func(string) error) error { t.Error("scan after failed setup"); return nil }}
	_, done := run(t, source, func(context.Context, string) error { return nil }, o)
	if !errors.Is(result(t, done), denied) || opens != 2 {
		t.Fatal("watch setup did not time out")
	}
}
func TestEmptyReceiverStops(t *testing.T) {
	opens := 0
	source := Source[string]{Watch: func(context.Context) (func() (string, error), error) { opens++; return nil, nil }, Scan: func(context.Context, func(string) error) error { t.Error("invalid watch scanned"); return nil }}
	_, done := run(t, source, func(context.Context, string) error { return nil }, options())
	if !errors.Is(result(t, done), ErrWatch) || opens != 1 {
		t.Fatal("empty watch was retried")
	}
}
