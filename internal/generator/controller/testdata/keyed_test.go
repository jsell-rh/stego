package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func keyOptions() KeyedOptions {
	return KeyedOptions{Capacity: 8, ResyncInterval: time.Hour, Timeout: time.Second, RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond, Terminal: func(err error) bool { return errors.Is(err, denied) }}
}
func keyTake(t *testing.T, q *keyQueue[string]) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	key, err := q.take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func TestKeysCombineAndRetainChangesDuringAction(t *testing.T) {
	q := newKeyQueue[string](1)
	q.setReady(true)
	for i := 0; i < 10000; i++ {
		if err := q.add("record"); err != nil {
			t.Fatal(err)
		}
	}
	if len(q.pending) != 1 || len(q.entries) != 1 {
		t.Fatal("duplicates consumed queue space")
	}
	if keyTake(t, q) != "record" {
		t.Fatal("wrong key")
	}
	if !errors.Is(q.add("other"), ErrOverflow) {
		t.Fatal("active key did not consume capacity")
	}
	if err := q.add("record"); err != nil {
		t.Fatal(err)
	}
	q.finish("record", false, time.Millisecond, time.Second)
	if keyTake(t, q) != "record" {
		t.Fatal("change during action was lost")
	}
	q.finish("record", false, time.Millisecond, time.Second)
	if len(q.entries) != 0 || len(q.pending) != 0 {
		t.Fatal("completed key retained memory")
	}
	if err := q.add("other"); err != nil {
		t.Fatal("capacity was not released")
	}
}
func TestKeyRetryDelayCannotBeBypassed(t *testing.T) {
	q := newKeyQueue[string](2)
	q.setReady(true)
	_ = q.add("failed")
	keyTake(t, q)
	q.finish("failed", true, 40*time.Millisecond, 80*time.Millisecond)
	due := q.entries["failed"].due
	for i := 0; i < 1000; i++ {
		_ = q.add("failed")
	}
	if !q.entries["failed"].due.Equal(due) {
		t.Fatal("new event bypassed retry delay")
	}
	_ = q.add("healthy")
	if keyTake(t, q) != "healthy" {
		t.Fatal("failed key blocked healthy work")
	}
	q.finish("healthy", false, time.Millisecond, time.Second)
	if keyTake(t, q) != "failed" || time.Now().Before(due) {
		t.Fatal("retry ran early")
	}
	q.finish("failed", true, 40*time.Millisecond, 80*time.Millisecond)
	if q.entries["failed"].delay != 80*time.Millisecond {
		t.Fatal("retry delay did not grow")
	}
	if nextDelay(80*time.Millisecond, 40*time.Millisecond, 80*time.Millisecond) != 80*time.Millisecond {
		t.Fatal("retry cap was exceeded")
	}
}
func TestDueRetryPrecedesNewKeys(t *testing.T) {
	q := newKeyQueue[string](2)
	q.setReady(true)
	_ = q.add("retry")
	keyTake(t, q)
	q.finish("retry", true, time.Millisecond, time.Millisecond)
	// Make the retry due without relying on scheduler timing.
	q.entries["retry"].due = time.Now().Add(-time.Second)
	_ = q.add("new")
	if keyTake(t, q) != "retry" {
		t.Fatal("new traffic starved a due retry")
	}
}
func TestKeyQueueStartsPausedAndPreservesPendingWork(t *testing.T) {
	q := newKeyQueue[string](1)
	_ = q.add("record")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	if _, err := q.take(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("action started before baseline")
	}
	cancel()
	q.setReady(true)
	if keyTake(t, q) != "record" {
		t.Fatal("baseline lost work")
	}
	_ = q.add("record")
	q.setReady(false)
	q.finish("record", false, time.Millisecond, time.Second)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
	if _, err := q.take(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("action started during reset")
	}
	cancel()
	q.setReady(true)
	if keyTake(t, q) != "record" {
		t.Fatal("reset lost pending work")
	}
}
func TestInvalidKeysDoNotConsumeCapacity(t *testing.T) {
	q := newKeyQueue[string](1)
	for _, key := range []string{"", strings.Repeat("a", 1025), string([]byte{255})} {
		if !errors.Is(q.add(key), ErrKey) {
			t.Fatal("invalid key accepted")
		}
	}
	if len(q.entries) != 0 {
		t.Fatal("invalid key consumed capacity")
	}
}
func TestKeyedLatestStateAndSerializedRetry(t *testing.T) {
	var current atomic.Int32
	var sink *KeySink[string]
	var stopped atomic.Bool
	started := make(chan struct{})
	source := KeyedSource[string]{Observe: func(ctx context.Context, s *KeySink[string]) error {
		sink = s
		current.Store(1)
		if err := s.Add("x"); err != nil {
			return err
		}
		s.SetReady(true)
		close(started)
		<-ctx.Done()
		stopped.Store(true)
		return ctx.Err()
	}, Scan: func(context.Context, func(string) error) error { return nil }}
	attempts := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := RunKeyed(ctx, source, func(context.Context, string) error {
		<-started
		attempts++
		switch attempts {
		case 1:
			if current.Load() != 1 {
				t.Error("wrong baseline")
			}
			current.Store(2)
			_ = sink.Add("x")
			return errors.New("retry")
		case 2:
			if current.Load() != 2 {
				t.Error("retry used stale data")
			}
			return denied
		default:
			t.Fatal("unexpected attempt")
			return nil
		}
	}, keyOptions())
	if !errors.Is(err, denied) || attempts != 2 || !stopped.Load() {
		t.Fatal("keyed lifecycle failed", err)
	}
}
func TestKeyedScanWakesWithBaselineAndRepairsZeroState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var scans atomic.Int32
	source := KeyedSource[string]{Observe: func(ctx context.Context, s *KeySink[string]) error { s.SetReady(true); <-ctx.Done(); return ctx.Err() }, Scan: func(ctx context.Context, emit func(string) error) error { scans.Add(1); return emit("zero") }}
	err := RunKeyed(ctx, source, func(_ context.Context, key string) error {
		if key != "zero" {
			t.Error("wrong scan key")
		}
		return denied
	}, keyOptions())
	if !errors.Is(err, denied) || scans.Load() != 1 {
		t.Fatal("scan did not wake with action worker", err)
	}
}
func TestKeyedSourceFailureStopsAndJoins(t *testing.T) {
	for _, stage := range []string{"observe", "scan", "overflow"} {
		t.Run(stage, func(t *testing.T) {
			started := make(chan struct{})
			var stopped atomic.Bool
			source := KeyedSource[string]{Observe: func(ctx context.Context, s *KeySink[string]) error {
				defer stopped.Store(true)
				s.SetReady(true)
				close(started)
				if stage == "observe" {
					return denied
				}
				if stage == "overflow" {
					for i := 0; i < 20; i++ {
						if err := s.Add(fmt.Sprint(i)); err != nil {
							return err
						}
					}
				}
				<-ctx.Done()
				return ctx.Err()
			}, Scan: func(ctx context.Context, emit func(string) error) error {
				<-started
				if stage == "scan" {
					return denied
				}
				return nil
			}}
			o := keyOptions()
			o.Capacity = 1
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := RunKeyed(ctx, source, func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }, o)
			if stage == "overflow" {
				if !errors.Is(err, ErrOverflow) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, denied) {
				t.Fatal(err)
			}
			if !stopped.Load() {
				t.Fatal("observer remained active")
			}
		})
	}
}
func TestKeyedActionDeadlineRetries(t *testing.T) {
	o := keyOptions()
	o.Timeout = 5 * time.Millisecond
	source := KeyedSource[string]{Observe: func(ctx context.Context, s *KeySink[string]) error {
		_ = s.Add("x")
		s.SetReady(true)
		<-ctx.Done()
		return ctx.Err()
	}, Scan: func(context.Context, func(string) error) error { return nil }}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	attempts := 0
	err := RunKeyed(ctx, source, func(ctx context.Context, _ string) error {
		attempts++
		if attempts == 1 {
			<-ctx.Done()
			return nil
		}
		return denied
	}, o)
	if !errors.Is(err, denied) || attempts != 2 {
		t.Fatal("action timeout lost work", err)
	}
}
func TestInvalidKeyedOptionsHaveNoEffects(t *testing.T) {
	for _, change := range []func(*KeyedOptions){func(o *KeyedOptions) { o.Capacity = 0 }, func(o *KeyedOptions) { o.Capacity = 65537 }, func(o *KeyedOptions) { o.Terminal = nil }, func(o *KeyedOptions) { o.RetryMax = o.RetryMin - 1 }, func(o *KeyedOptions) { o.Timeout = 0 }, func(o *KeyedOptions) { o.ResyncInterval = 0 }} {
		o := keyOptions()
		change(&o)
		source := KeyedSource[string]{Observe: func(context.Context, *KeySink[string]) error { t.Error("invalid options opened observer"); return nil }, Scan: func(context.Context, func(string) error) error { return nil }}
		if RunKeyed(context.Background(), source, func(context.Context, string) error { return nil }, o) == nil {
			t.Fatal("invalid options accepted")
		}
	}
}
func BenchmarkKeyQueue(b *testing.B) {
	for _, size := range []int{1, 1024, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			q := newKeyQueue[string](size)
			q.setReady(true)
			for i := 0; i < size; i++ {
				_ = q.add(fmt.Sprint(i))
			}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				key, err := q.take(ctx)
				if err != nil {
					b.Fatal(err)
				}
				_ = q.add(key)
				q.finish(key, false, time.Millisecond, time.Second)
			}
		})
	}
}

func TestKeyedScanRetriesAndReportsFailure(t *testing.T) {
	o := keyOptions()
	var notices atomic.Int32
	o.Observe = func(event Event) {
		if event.Phase == "scan_failed" {
			notices.Add(1)
		}
	}
	attempts := 0
	source := KeyedSource[string]{Observe: func(ctx context.Context, s *KeySink[string]) error { s.SetReady(true); <-ctx.Done(); return ctx.Err() }, Scan: func(ctx context.Context, emit func(string) error) error {
		attempts++
		if attempts == 1 {
			return errors.New("temporary scan failure")
		}
		return emit("repair")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := RunKeyed(ctx, source, func(context.Context, string) error { return denied }, o)
	if !errors.Is(err, denied) || attempts != 2 || notices.Load() != 1 {
		t.Fatal("scan failure was hidden or not retried", err)
	}
}

// Keep a fixed backlog while workers invalidate their active key and complete it.
// This measures queue contention without network calls or retry delays.
func BenchmarkKeyQueueWorkers(b *testing.B) {
	for _, size := range []int{1024, 10000} {
		for _, count := range []int{1, 4} {
			b.Run(fmt.Sprintf("keys_%d/workers_%d", size, count), func(b *testing.B) {
				q := newKeyQueue[string](size)
				q.setReady(true)
				for i := 0; i < size; i++ {
					if err := q.add(fmt.Sprint(i)); err != nil {
						b.Fatal(err)
					}
				}
				ctx := context.Background()
				var workers sync.WaitGroup
				b.ReportAllocs()
				b.ResetTimer()
				for worker := 0; worker < count; worker++ {
					workers.Go(func() {
						for i := worker; i < b.N; i += count {
							key, err := q.take(ctx)
							if err != nil {
								b.Error(err)
								return
							}
							if err := q.add(key); err != nil {
								b.Error(err)
								return
							}
							q.finish(key, false, time.Millisecond, time.Second)
						}
					})
				}
				workers.Wait()
			})
		}
	}
}

func TestReconnectRetainsCapacityAndInterruptedWork(t *testing.T) {
	q := newKeyQueue[string](3)
	q.setReady(true)
	for _, key := range []string{"delayed", "active", "pending"} {
		if err := q.add(key); err != nil {
			t.Fatal(err)
		}
	}
	if keyTake(t, q) != "delayed" {
		t.Fatal("wrong first key")
	}
	q.finish("delayed", true, time.Second, time.Minute)
	due := q.entries["delayed"].due
	if keyTake(t, q) != "active" {
		t.Fatal("wrong active key")
	}
	before := time.Now()
	q.restart(time.Second, time.Minute)
	if q.ready || q.metrics().Active != 0 || q.metrics().Queued != 3 || q.metrics().Retrying != 2 {
		t.Fatal("invalid resumed queue", q.metrics())
	}
	if !q.entries["delayed"].due.Equal(due) {
		t.Fatal("reconnect changed an existing delay")
	}
	active := q.entries["active"]
	if active.delay != time.Second || active.due.Before(before.Add(time.Second)) || !active.dirty {
		t.Fatal("interrupted work lost its delay")
	}
	if !errors.Is(q.add("overflow"), ErrOverflow) {
		t.Fatal("reconnect lost the capacity bound")
	}
	q.restart(time.Second, time.Minute)
	if active.delay != time.Second || !q.entries["delayed"].due.Equal(due) || q.metrics().Queued != 3 {
		t.Fatal("failed reconnect duplicated or postponed pending work")
	}
	q.setReady(true)
	if keyTake(t, q) != "pending" {
		t.Fatal("a delayed key blocked independent pending work")
	}
}

func BenchmarkKeyReconnect(b *testing.B) {
	for _, count := range []int{1024, 10000, 65536} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			q := newKeyQueue[string](count)
			for i := 0; i < count; i++ {
				if err := q.add(fmt.Sprint(i)); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				q.restart(time.Second, time.Minute)
			}
		})
	}
}
