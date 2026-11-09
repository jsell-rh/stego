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

func keyOptions() KeyedOptions {
	return KeyedOptions{Capacity: 8, ResyncInterval: time.Hour, Timeout: time.Second, RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond, Terminal: func(err error) bool { return errors.Is(err, denied) }}
}

func watchKeyOptions() KeyedWatchOptions {
	o := keyOptions()
	o.Workers = 2
	return KeyedWatchOptions{KeyedOptions: o, ReconnectDelay: time.Millisecond}
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

// within fails the test when call does not return inside limit, so a call that blocks fails rather than hangs.
func within(t *testing.T, limit time.Duration, call func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); call() }()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("the call did not return within %s", limit)
	}
}

func TestQueueItems(t *testing.T) {
	t.Run("ST-SUP-RP02-LC01-T01 The emitted runtime delivers a live record and a deletion record as two items", func(t *testing.T) {
		var opened, active atomic.Bool
		var scanned atomic.Int32
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
		var items []record
		stop, done := run(t, source, func(ctx context.Context, item record) error {
			if active.Swap(true) {
				t.Error("parallel action")
			}
			defer active.Store(false)
			items = append(items, item)
			if len(items) == 2 {
				return denied
			}
			return nil
		}, options())
		defer stop()
		if err := result(t, done); !errors.Is(err, denied) {
			t.Fatal("typed actions did not finish", err)
		}
		if len(items) != 2 || items[0] != (record{"live", false}) || items[1] != (record{"retained", true}) {
			t.Fatalf("item order or content changed: %v", items)
		}
	})

	t.Run("ST-SUP-RP02-LC01-T02 Duplicate events share one entry and a change during the action is kept", func(t *testing.T) {
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
	})
}

func TestQueueBounds(t *testing.T) {
	t.Run("ST-SUP-RP02-LC02-T01 Invalid worker and reconnect options open no source", func(t *testing.T) {
		for i, change := range []func(*KeyedWatchOptions){
			func(o *KeyedWatchOptions) { o.Workers = -1 }, func(o *KeyedWatchOptions) { o.Capacity = 100; o.Workers = 65 },
			func(o *KeyedWatchOptions) { o.Workers = o.Capacity + 1 }, func(o *KeyedWatchOptions) { o.ReconnectDelay = 0 },
		} {
			o := watchKeyOptions()
			change(&o)
			var calls atomic.Int32
			source := Source[string]{Watch: func(context.Context) (func() (string, error), error) {
				calls.Add(1)
				return nil, nil
			}, Scan: func(context.Context, func(string) error) error { calls.Add(1); return nil }}
			if RunKeyedWatch(context.Background(), source, func(context.Context, string) error { return nil }, o) == nil {
				t.Fatalf("option set %d was accepted", i)
			}
			if calls.Load() != 0 {
				t.Fatalf("option set %d called the source", i)
			}
		}
	})

	t.Run("ST-SUP-RP02-LC02-T02 Waiting admission is bounded and canceled", func(t *testing.T) {
		q := newKeyQueue[string](1)
		q.setReady(true)
		if err := q.add("active"); err != nil {
			t.Fatal(err)
		}
		if keyTake(t, q) != "active" {
			t.Fatal("wrong active key")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if err := q.addWait(ctx, "next"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("full queue did not wait", err)
		}
		if len(q.entries) != 1 || q.entries["active"] == nil {
			t.Fatal("waiting admission evicted or added a key")
		}
		q.finish("active", false, time.Millisecond, time.Second)
		if err := q.addWait(ctx, "canceled"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("canceled admission succeeded", err)
		}
		if len(q.entries) != 0 {
			t.Fatal("canceled admission changed the queue")
		}
		var none context.Context
		if err := q.addWait(none, "next"); err == nil {
			t.Fatal("nil context was accepted")
		}
		if err := q.addWait(context.Background(), ""); !errors.Is(err, ErrKey) {
			t.Fatal("invalid key was admitted", err)
		}
		if err := q.addWait(context.Background(), "next"); err != nil {
			t.Fatal("released capacity was not available", err)
		}
	})
}

func TestQueueReconnect(t *testing.T) {
	t.Run("ST-SUP-RP02-LC04-T01 A queue restart keeps due times, capacity and interrupted work", func(t *testing.T) {
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
	})

	t.Run("ST-SUP-RP02-LC04-T02 A watch reconnect cannot bypass the scan retry delay", func(t *testing.T) {
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
	})
}

func TestQueueBackoff(t *testing.T) {
	t.Run("ST-SUP-RP02-LC10-T01 A failed connection attempt does not delay keys already queued", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		options := watchKeyOptions()
		options.Workers = 1
		options.ReconnectDelay = 10 * time.Second
		var watches atomic.Int32
		queued := make(chan struct{})
		source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
			watches.Add(1)
			keys := []string{"a", "b", "c"}
			return func() (string, error) {
				if len(keys) > 0 {
					key := keys[0]
					keys = keys[1:]
					return key, nil
				}
				<-queued
				return "", io.ErrUnexpectedEOF
			}, nil
		}, Scan: func(context.Context, func(string) error) error { return nil }}
		reached := make(chan string, 8)
		var seen atomic.Int32
		done := make(chan error, 1)
		go func() {
			done <- RunKeyedWatch(ctx, source, func(ctx context.Context, key string) error {
				if seen.Add(1) == 1 {
					// Hold the first action until all three keys are queued, then fail the connection.
					close(queued)
					<-ctx.Done()
					return ctx.Err()
				}
				reached <- key
				return nil
			}, options)
		}()
		got := map[string]bool{}
		deadline := time.After(time.Second)
		for len(got) < 3 {
			select {
			case key := <-reached:
				got[key] = true
			case <-deadline:
				t.Fatalf("the queued keys waited for the reconnect backoff: %v", got)
			}
		}
		if watches.Load() != 1 {
			t.Fatal("the reconnect did not wait its backoff", watches.Load())
		}
		cancel()
		if err := result(t, done); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ST-SUP-RP02-LC10-T02 An interrupted action takes the next capped delay, not a reset delay", func(t *testing.T) {
		q := newKeyQueue[string](1)
		q.setReady(true)
		_ = q.add("key")
		keyTake(t, q)
		q.finish("key", true, time.Second, time.Minute)
		q.entries["key"].due = time.Now().Add(-time.Millisecond)
		keyTake(t, q)
		q.finish("key", true, time.Second, time.Minute)
		if q.entries["key"].delay != 2*time.Second {
			t.Fatal("the second failure did not take the second delay", q.entries["key"].delay)
		}
		q.entries["key"].due = time.Now().Add(-time.Millisecond)
		keyTake(t, q)
		q.restart(time.Second, time.Minute)
		if delay := q.entries["key"].delay; delay != 4*time.Second {
			t.Fatal("the interrupted third attempt did not take the third delay", delay)
		}
	})

	t.Run("ST-SUP-RP02-LC10-T03 A successful scan clears the scan backoff", func(t *testing.T) {
		options := keyOptions()
		options.RetryMin = time.Second
		options.RetryMax = time.Minute
		now := time.Now()
		var schedule scanSchedule
		for _, failed := range []bool{true, true, false, true} {
			schedule.active = true
			schedule.finish(failed, now, options)
		}
		if schedule.delay != options.RetryMin || !schedule.due.Equal(now.Add(options.RetryMin)) {
			t.Fatal("the delay before the fifth scan is not the first backoff step", schedule.delay)
		}
	})

	t.Run("ST-SUP-RP02-LC10-T04 A queued action runs while the next scan waits for its due time", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		options := keyOptions()
		options.ResyncInterval = time.Minute
		scanned := make(chan struct{})
		var scans atomic.Int32
		source := KeyedSource[string]{Observe: func(ctx context.Context, sink *KeySink[string]) error {
			sink.SetReady(true)
			<-scanned
			if err := sink.Add("key"); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		}, Scan: func(context.Context, func(string) error) error {
			if scans.Add(1) == 1 {
				close(scanned)
			}
			return nil
		}}
		acted := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- RunKeyed(ctx, source, func(context.Context, string) error { close(acted); return nil }, options)
		}()
		select {
		case <-acted:
		case <-time.After(time.Second):
			t.Fatal("the action waited behind the scan timer")
		}
		if scans.Load() != 1 {
			t.Fatal("the next scan did not wait for its due time")
		}
		cancel()
		if err := result(t, done); err != nil {
			t.Fatal(err)
		}
	})
}

func TestQueueAdmission(t *testing.T) {
	page := func(cursors ...string) CursorSource[string] {
		return func(_ context.Context, after string, _ int) (CursorPage[string], error) {
			if after != "" {
				return CursorPage[string]{}, nil
			}
			items := make([]CursorItem[string], len(cursors))
			for i, c := range cursors {
				items[i] = CursorItem[string]{Cursor: c, Value: c}
			}
			return CursorPage[string]{Items: items}, nil
		}
	}
	busy := func(t *testing.T, capacity int) *keyQueue[string] {
		q := newKeyQueue[string](capacity)
		q.setReady(true)
		if err := q.add("active"); err != nil {
			t.Fatal(err)
		}
		keyTake(t, q)
		return q
	}

	t.Run("ST-SUP-RP02-LC11-T01 The scan cursor advances only after the item is admitted", func(t *testing.T) {
		q := busy(t, 2)
		ctx, cancel := context.WithCancel(context.Background())
		var waiting atomic.Int32
		go func() {
			for q.waiting.Load() == 0 || q.metrics().Queued+q.metrics().Active < 2 {
				time.Sleep(time.Millisecond)
			}
			waiting.Store(1)
			cancel()
		}()
		progress, err := ScanFrom(ctx, "", page("first", "second"), func(key string) error { return q.addWait(ctx, key) }, scanOptions())
		if !errors.Is(err, context.Canceled) || waiting.Load() != 1 {
			t.Fatal("the scan did not wait at admission", err)
		}
		if progress.After != "first" {
			t.Fatalf("the stored cursor is %q, not the first item", progress.After)
		}
	})

	t.Run("ST-SUP-RP02-LC11-T02 The page request context ends before the scan waits for admission", func(t *testing.T) {
		q := busy(t, 1)
		var pageContext context.Context
		source := func(ctx context.Context, after string, limit int) (CursorPage[string], error) {
			pageContext = ctx
			return page("next")(ctx, after, limit)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		ended := false
		_, err := ScanFrom(ctx, "", source, func(key string) error {
			ended = pageContext.Err() != nil
			return q.addWait(ctx, key)
		}, scanOptions())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("the full queue did not hold the scan", err)
		}
		if !ended {
			t.Fatal("the page context was still open when the scan waited for admission")
		}
	})

	t.Run("ST-SUP-RP02-LC11-T03 Admission stops on cancellation", func(t *testing.T) {
		q := busy(t, 1)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := ScanFrom(ctx, "", page("next"), func(key string) error { return q.addWait(ctx, key) }, scanOptions())
			done <- err
		}()
		for q.waiting.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("the scan did not return the cancellation", err)
			}
		case <-time.After(time.Second):
			t.Fatal("the scan did not stop within one second")
		}
		q.finish("active", false, time.Millisecond, time.Second)
		if len(q.entries) != 0 {
			t.Fatal("an item was admitted after the cancellation", q.entries)
		}
	})

	t.Run("ST-SUP-RP02-LC11-T04 Adding a key does not block the caller", func(t *testing.T) {
		q := newKeyQueue[string](2)
		q.setReady(true)
		sink := &KeySink[string]{queue: q}
		for _, key := range []string{"one", "two"} {
			if err := sink.Add(key); err != nil {
				t.Fatal(err)
			}
		}
		var err error
		within(t, 100*time.Millisecond, func() { err = sink.Add("three") })
		if !errors.Is(err, ErrOverflow) {
			t.Fatal("a full queue did not report backpressure", err)
		}
		within(t, 100*time.Millisecond, func() { err = sink.Add("one") })
		if err != nil || len(q.entries) != 2 || q.entries["one"] == nil || q.entries["two"] == nil {
			t.Fatal("a key was lost", err, q.entries)
		}
	})
}
