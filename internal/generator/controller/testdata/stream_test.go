package controller

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

func streamOptions() StreamScanOptions {
	return StreamScanOptions{MaxItems: 2, OpenTimeout: time.Second, ReceiveTimeout: time.Second}
}

func TestStreamScanBoundsAndCloses(t *testing.T) {
	for _, size := range []int{0, 2, 3} {
		var stream context.Context
		calls := 0
		values := []int{}
		err := ScanStream(context.Background(), func(ctx context.Context) (func() (int, error), error) {
			stream = ctx
			return func() (int, error) {
				calls++
				if calls > size {
					return 0, io.EOF
				}
				return calls, nil
			}, nil
		}, func(value int) error { values = append(values, value); return nil }, streamOptions())
		if size <= 2 && err != nil || size > 2 && !errors.Is(err, ErrScanContract) {
			t.Fatal("stream bound", size, err)
		}
		expected := []int{}
		if size > 0 {
			expected = []int{1, 2}
		}
		if !reflect.DeepEqual(values, expected) || stream.Err() != context.Canceled {
			t.Fatal("stream output or closure", size, values, stream.Err())
		}
		if calls != min(size+1, 3) {
			t.Fatal("extra receive", calls)
		}
	}
}

func TestStreamScanTimeoutJoinsCallbacks(t *testing.T) {
	for _, phase := range []string{"open", "receive"} {
		t.Run(phase, func(t *testing.T) {
			parent, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			timedOut := make(chan struct{})
			release := make(chan struct{})
			done := make(chan error, 1)
			wait := func(ctx context.Context) {
				<-ctx.Done()
				close(timedOut)
				select {
				case <-release:
				case <-parent.Done():
				}
			}
			options := streamOptions()
			options.OpenTimeout = 20 * time.Millisecond
			options.ReceiveTimeout = 20 * time.Millisecond
			go func() {
				done <- ScanStream(parent, func(ctx context.Context) (func() (int, error), error) {
					if phase == "open" {
						wait(ctx)
						return nil, ctx.Err()
					}
					return func() (int, error) { wait(ctx); return 1, nil }, nil
				}, func(int) error { t.Error("late value was emitted"); return nil }, options)
			}()
			select {
			case <-timedOut:
			case <-parent.Done():
				t.Fatal("callback was not cancelled")
			}
			select {
			case err := <-done:
				t.Fatal("callback was detached", err)
			default:
			}
			close(release)
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("timeout classification", err)
				}
			case <-parent.Done():
				t.Fatal("callback did not join")
			}
		})
	}
}

func TestStreamScanDoesNotTimeQueueAdmission(t *testing.T) {
	options := streamOptions()
	options.ReceiveTimeout = 30 * time.Millisecond
	calls := 0
	var stream context.Context
	err := ScanStream(context.Background(), func(ctx context.Context) (func() (int, error), error) {
		stream = ctx
		return func() (int, error) {
			calls++
			if calls == 1 {
				return 1, nil
			}
			return 0, io.EOF
		}, nil
	}, func(int) error {
		select {
		case <-stream.Done():
			t.Fatal("stream expired during admission")
		case <-time.After(4 * options.ReceiveTimeout):
		}
		return nil
	}, options)
	if err != nil || calls != 2 {
		t.Fatal("queue admission changed the stream", err, calls)
	}
}

func TestStreamScanErrorsAndCancellation(t *testing.T) {
	sentinel := errors.New("source failed")
	for _, phase := range []string{"open", "receive", "emit", "cancel"} {
		parent, cancel := context.WithCancel(context.Background())
		var stream context.Context
		calls := 0
		err := ScanStream(parent, func(ctx context.Context) (func() (int, error), error) {
			stream = ctx
			if phase == "open" {
				return nil, sentinel
			}
			return func() (int, error) {
				calls++
				if phase == "receive" {
					return 0, sentinel
				}
				return 1, nil
			}, nil
		}, func(int) error {
			if phase == "cancel" {
				cancel()
				return nil
			}
			return sentinel
		}, streamOptions())
		cancel()
		want := sentinel
		if phase == "cancel" {
			want = context.Canceled
		}
		if !errors.Is(err, want) || stream.Err() == nil || calls > 1 {
			t.Fatal("error or cancellation lost", phase, err, calls)
		}
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	err := ScanStream(parent, func(context.Context) (func() (int, error), error) {
		t.Error("cancelled source opened")
		return nil, nil
	}, func(int) error { return nil }, streamOptions())
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestStreamScanRejectsInvalidOptions(t *testing.T) {
	valid := streamOptions()
	for _, options := range []StreamScanOptions{
		{}, {MaxItems: -1, OpenTimeout: valid.OpenTimeout, ReceiveTimeout: valid.ReceiveTimeout},
		{MaxItems: 1000001, OpenTimeout: valid.OpenTimeout, ReceiveTimeout: valid.ReceiveTimeout},
		{MaxItems: 1, OpenTimeout: time.Nanosecond, ReceiveTimeout: valid.ReceiveTimeout},
		{MaxItems: 1, OpenTimeout: valid.OpenTimeout, ReceiveTimeout: time.Minute + 1},
	} {
		err := ScanStream(context.Background(), func(context.Context) (func() (int, error), error) { t.Error("invalid source opened"); return nil, nil }, func(int) error { return nil }, options)
		if !errors.Is(err, ErrScanContract) {
			t.Fatal("invalid options accepted", err)
		}
	}
	empty := func(context.Context) (func() (int, error), error) { return nil, nil }
	for _, err := range []error{
		ScanStream(nil, empty, func(int) error { return nil }, valid),
		ScanStream[int](context.Background(), nil, func(int) error { return nil }, valid),
		ScanStream(context.Background(), empty, nil, valid),
		ScanStream(context.Background(), empty, func(int) error { return nil }, valid),
	} {
		if !errors.Is(err, ErrScanContract) {
			t.Fatal("invalid callback accepted", err)
		}
	}
}

func BenchmarkStreamScan(b *testing.B) {
	options := StreamScanOptions{MaxItems: 1000, OpenTimeout: time.Second, ReceiveTimeout: time.Second}
	b.ReportAllocs()
	for b.Loop() {
		err := ScanStream(context.Background(), func(context.Context) (func() (int, error), error) {
			count := 0
			return func() (int, error) {
				if count == 1000 {
					return 0, io.EOF
				}
				count++
				return count, nil
			}, nil
		}, func(int) error { return nil }, options)
		if err != nil {
			b.Fatal(err)
		}
	}
}
