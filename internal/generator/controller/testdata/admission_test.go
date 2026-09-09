package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeyedScanBackpressureReachesLaterKeysWithOnePersistentFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var scans atomic.Int32
	source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
		return func() (string, error) { <-ctx.Done(); return "", ctx.Err() }, nil
	}, Scan: func(ctx context.Context, emit func(string) error) error {
		scans.Add(1)
		for _, key := range []string{"failed", "first", "second", "last"} {
			if err := emit(key); err != nil {
				return err
			}
		}
		return nil
	}}
	o := watchKeyOptions()
	o.Capacity = 2
	o.Workers = 1
	err := RunKeyedWatch(ctx, source, func(ctx context.Context, key string) error {
		if key == "failed" {
			return errors.New("provider unavailable")
		}
		if key == "last" {
			return denied
		}
		select {
		case <-time.After(5 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, o)
	if !errors.Is(err, denied) || scans.Load() != 1 {
		t.Fatal("discovery did not reach its last key in one scan", err, scans.Load())
	}
}

func TestKeyedWatchBackpressurePreservesKeys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var watches atomic.Int32
	keys := []string{"failed", "first", "second", "last"}
	source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
		watches.Add(1)
		next := 0
		return func() (string, error) {
			if next < len(keys) {
				key := keys[next]
				next++
				return key, nil
			}
			<-ctx.Done()
			return "", ctx.Err()
		}, nil
	}, Scan: func(context.Context, func(string) error) error { return nil }}
	o := watchKeyOptions()
	o.Capacity = 2
	o.Workers = 1
	err := RunKeyedWatch(ctx, source, func(ctx context.Context, key string) error {
		if key == "failed" {
			return errors.New("provider unavailable")
		}
		if key == "last" {
			return denied
		}
		select {
		case <-time.After(5 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, o)
	if !errors.Is(err, denied) || watches.Load() != 1 {
		t.Fatal("watch lost keys or reconnected under queue pressure", err, watches.Load())
	}
}

func TestWaitingAdmissionIsBoundedAndCanceled(t *testing.T) {
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
	if err := q.addWait(nil, "next"); err == nil {
		t.Fatal("nil context was accepted")
	}
	if err := q.addWait(context.Background(), ""); !errors.Is(err, ErrKey) {
		t.Fatal("invalid key was admitted", err)
	}
	if err := q.addWait(context.Background(), "next"); err != nil {
		t.Fatal("released capacity was not available", err)
	}
}

func BenchmarkKeyAdmission(b *testing.B) {
	for _, wait := range []bool{false, true} {
		name := "nonblocking"
		if wait {
			name = "backpressure"
		}
		b.Run(name, func(b *testing.B) {
			q := newKeyQueue[string](1)
			q.setReady(true)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				if wait {
					err = q.addWait(ctx, "record")
				} else {
					err = q.add("record")
				}
				if err != nil {
					b.Fatal(err)
				}
				key, err := q.take(ctx)
				if err != nil {
					b.Fatal(err)
				}
				q.finish(key, false, time.Millisecond, time.Second)
			}
		})
	}
}
