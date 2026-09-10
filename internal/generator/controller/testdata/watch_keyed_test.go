package controller

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func watchKeyOptions() KeyedWatchOptions {
	o := keyOptions()
	o.Workers = 2
	return KeyedWatchOptions{KeyedOptions: o, ReconnectDelay: time.Millisecond}
}

func TestKeyedWorkersKeepIndependentProgressAndSerializeEachKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	active := make(chan struct{})
	release := make(chan struct{})
	var calls, overlap atomic.Int32
	var firstActive atomic.Bool
	source := KeyedSource[string]{Observe: func(ctx context.Context, sink *KeySink[string]) error {
		if err := sink.Add("slow"); err != nil {
			return err
		}
		sink.SetReady(true)
		select {
		case <-active:
		case <-ctx.Done():
			return ctx.Err()
		}
		for i := 0; i < 10000; i++ {
			if err := sink.Add("slow"); err != nil {
				return err
			}
		}
		if err := sink.Add("other"); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}, Scan: func(context.Context, func(string) error) error { return nil }}
	o := keyOptions()
	o.Workers = 2
	o.Capacity = 2
	err := RunKeyed(ctx, source, func(ctx context.Context, key string) error {
		if key == "other" {
			close(release)
			return nil
		}
		if firstActive.Swap(true) {
			overlap.Add(1)
		}
		defer firstActive.Store(false)
		if calls.Add(1) == 1 {
			close(active)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return denied
	}, o)
	if !errors.Is(err, denied) || calls.Load() != 2 || overlap.Load() != 0 {
		t.Fatal("key exclusion or independent progress failed", err, calls.Load(), overlap.Load())
	}
}

func TestKeyedWatchReconnectJoinsActionsAndRepeatsDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	actionStarted := make(chan struct{})
	secondScan := make(chan struct{})
	var watches, scans, active atomic.Int32
	var actionStopped atomic.Bool
	source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
		count := watches.Add(1)
		if count > 1 && !actionStopped.Load() {
			t.Error("reconnect preceded action shutdown")
		}
		return func() (string, error) {
			if count == 1 {
				select {
				case <-actionStarted:
					return "", io.EOF
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			<-ctx.Done()
			return "", ctx.Err()
		}, nil
	}, Scan: func(ctx context.Context, emit func(string) error) error {
		if scans.Add(1) == 2 {
			close(secondScan)
		}
		return emit("retained")
	}}
	err := RunKeyedWatch(ctx, source, func(ctx context.Context, _ string) error {
		if active.Add(1) != 1 {
			t.Error("old action overlapped new session")
		}
		defer active.Add(-1)
		if watches.Load() == 1 {
			close(actionStarted)
			<-ctx.Done()
			actionStopped.Store(true)
			return ctx.Err()
		}

		// Retained actions may resume before discovery. Keep this test alive until
		// it has observed the second scan, then test terminal action shutdown.
		select {
		case <-secondScan:
		case <-ctx.Done():
			return ctx.Err()
		}
		return denied
	}, watchKeyOptions())
	if !errors.Is(err, denied) || watches.Load() != 2 || scans.Load() != 2 || !actionStopped.Load() {
		t.Fatal("recovery failed", err, watches.Load(), scans.Load())
	}
}

func TestKeyedWatchTerminalScanCancelsReceiver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var stopped atomic.Bool
	started := make(chan struct{})
	source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
		return func() (string, error) { close(started); <-ctx.Done(); stopped.Store(true); return "", ctx.Err() }, nil
	}, Scan: func(ctx context.Context, _ func(string) error) error {
		select {
		case <-started:
			return denied
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	err := RunKeyedWatch(ctx, source, func(context.Context, string) error { t.Error("unexpected action"); return nil }, watchKeyOptions())
	if !errors.Is(err, denied) || !stopped.Load() {
		t.Fatal("terminal scan did not stop receiver", err)
	}
}

func TestKeyedWatchRejectsInvalidOptionsBeforeOpening(t *testing.T) {
	for _, change := range []func(*KeyedWatchOptions){
		func(o *KeyedWatchOptions) { o.Workers = -1 }, func(o *KeyedWatchOptions) { o.Workers = 65 },
		func(o *KeyedWatchOptions) { o.Workers = o.Capacity + 1 }, func(o *KeyedWatchOptions) { o.ReconnectDelay = 0 },
	} {
		o := watchKeyOptions()
		change(&o)
		source := Source[string]{Watch: func(context.Context) (func() (string, error), error) {
			t.Error("invalid options opened source")
			return nil, nil
		}, Scan: func(context.Context, func(string) error) error { return nil }}
		if RunKeyedWatch(context.Background(), source, func(context.Context, string) error { return nil }, o) == nil {
			t.Fatal("invalid options accepted")
		}
	}
}

func TestKeyedWatchPreservesDelayedKeysAcrossReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	failed := make(chan struct{})
	var watches, calls atomic.Int32
	var first time.Time
	options := watchKeyOptions()
	options.RetryMin = 150 * time.Millisecond
	options.RetryMax = time.Second
	options.Observe = func(event Event) {
		if event.Phase == "reconcile_failed" && calls.Load() == 1 {
			close(failed)
		}
	}
	source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
		count := watches.Add(1)
		return func() (string, error) {
			if count == 1 {
				select {
				case <-failed:
					return "", io.EOF
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			<-ctx.Done()
			return "", ctx.Err()
		}, nil
	}, Scan: func(ctx context.Context, emit func(string) error) error { return emit("failed") }}
	err := RunKeyedWatch(ctx, source, func(context.Context, string) error {
		if calls.Add(1) == 1 {
			first = time.Now()
			return errors.New("provider unavailable")
		}
		if time.Since(first) < options.RetryMin {
			t.Error("reconnect bypassed the key retry delay")
		}
		return denied
	}, options)
	if !errors.Is(err, denied) || watches.Load() != 2 || calls.Load() != 2 {
		t.Fatal("retry recovery failed", err, watches.Load(), calls.Load())
	}
}

func TestKeyedWatchPreservesFailedScanDelayAcrossReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	failed, acted := make(chan struct{}), make(chan struct{})
	var watches, scans atomic.Int32
	var first time.Time
	options := watchKeyOptions()
	options.RetryMin = 150 * time.Millisecond
	options.RetryMax = time.Second
	options.Observe = func(event Event) {
		if event.Phase == "scan_failed" {
			close(failed)
		}
	}
	source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
		count := watches.Add(1)
		sent := false
		return func() (string, error) {
			if count == 1 {
				select {
				case <-failed:
					return "", io.EOF
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			if !sent {
				sent = true
				return "independent", nil
			}
			<-ctx.Done()
			return "", ctx.Err()
		}, nil
	}, Scan: func(context.Context, func(string) error) error {
		if scans.Add(1) == 1 {
			first = time.Now()
			return errors.New("inventory unavailable")
		}
		if elapsed := time.Since(first); elapsed < options.RetryMin {
			t.Error("reconnect bypassed inventory retry delay", elapsed)
		}
		select {
		case <-acted:
		default:
			t.Error("scan backoff blocked a resource action")
		}
		return denied
	}}
	err := RunKeyedWatch(ctx, source, func(context.Context, string) error { close(acted); return nil }, options)
	if !errors.Is(err, denied) || watches.Load() != 2 || scans.Load() != 2 {
		t.Fatal("scan recovery failed", err, watches.Load(), scans.Load())
	}
}

func TestKeyedWatchSuccessfulScanPermitsImmediateRediscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	scanned := make(chan struct{})
	var watches, scans atomic.Int32
	options := watchKeyOptions()
	options.Observe = func(event Event) {
		if event.Phase == "scan_completed" {
			close(scanned)
		}
	}
	source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
		count := watches.Add(1)
		return func() (string, error) {
			if count == 1 {
				select {
				case <-scanned:
					return "", io.EOF
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			<-ctx.Done()
			return "", ctx.Err()
		}, nil
	}, Scan: func(context.Context, func(string) error) error {
		if scans.Add(1) == 1 {
			return nil
		}
		return denied
	}}
	err := RunKeyedWatch(ctx, source, func(context.Context, string) error { t.Error("unexpected action"); return nil }, options)
	if !errors.Is(err, denied) || scans.Load() != 2 {
		t.Fatal("successful scan delayed fresh discovery", err, scans.Load())
	}
}

func TestScanScheduleRetainsFailureAndResetsAfterSuccess(t *testing.T) {
	options := watchKeyOptions().KeyedOptions
	options.RetryMin = time.Second
	options.RetryMax = 4 * time.Second
	now := time.Now()
	schedule := scanSchedule{active: true}
	schedule.finish(true, now, options)
	due := schedule.due
	schedule.reconnect(now.Add(time.Millisecond), options.RetryMin, options.RetryMax)
	if schedule.active || schedule.delay != time.Second || !schedule.due.Equal(due) {
		t.Fatal("failed scan lost its due time")
	}
	schedule.active = true
	schedule.reconnect(now, options.RetryMin, options.RetryMax)
	if schedule.active || schedule.delay != 2*time.Second || !schedule.due.Equal(now.Add(2*time.Second)) {
		t.Fatal("interrupted scan lost its next delay")
	}
	schedule.active = true
	schedule.reconnect(now, options.RetryMin, options.RetryMax)
	schedule.active = true
	schedule.reconnect(now, options.RetryMin, options.RetryMax)
	if schedule.delay != options.RetryMax {
		t.Fatal("scan retry exceeded its cap")
	}
	schedule.finish(false, now, options)
	if schedule.delay != 0 || !schedule.due.Equal(now.Add(options.ResyncInterval)) {
		t.Fatal("successful scan did not reset periodic discovery")
	}
	schedule.reconnect(now, options.RetryMin, options.RetryMax)
	if !schedule.due.IsZero() {
		t.Fatal("successful scan delayed new discovery")
	}
	schedule.active = true
	schedule.reconnect(now, options.RetryMin, options.RetryMax)
	if schedule.delay != options.RetryMin {
		t.Fatal("successful scan did not reset retry growth")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	schedule.due = now.Add(time.Hour)
	if !errors.Is(schedule.wait(ctx), context.Canceled) {
		t.Fatal("cancellation did not stop the scan wait")
	}
}
