package controller

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPendingResultQueueRetainsCapacityAndDelay(t *testing.T) {
	q := newKeyQueue[string](2)
	q.setReady(true)
	if err := q.add("pending"); err != nil {
		t.Fatal(err)
	}
	keyTake(t, q)
	q.finishResult("pending", false, time.Hour, time.Millisecond, time.Second)
	due := q.entries["pending"].due
	if q.retrying != 0 || len(q.pending) != 1 {
		t.Fatal("pending work was counted as a failure or lost")
	}
	for range 3 {
		if err := q.add("pending"); err != nil {
			t.Fatal(err)
		}
	}
	q.restart(time.Millisecond, time.Second)
	if !q.entries["pending"].due.Equal(due) {
		t.Fatal("event or reconnect shortened the recheck delay")
	}
	if err := q.add("healthy"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(q.add("overflow"), ErrOverflow) {
		t.Fatal("pending work left the capacity bound")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := q.take(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("reconnect bypassed readiness", err)
	}
	q.setReady(true)
	if keyTake(t, q) != "healthy" {
		t.Fatal("pending work blocked another key")
	}
	q.finish("healthy", false, time.Millisecond, time.Second)
	// Make the retained key due without a long sleep. No queue goroutine is active.
	q.entries["pending"].due = time.Now().Add(-time.Second)
	if keyTake(t, q) != "pending" {
		t.Fatal("pending key was lost")
	}
	q.finishResult("pending", true, time.Millisecond, 20*time.Millisecond, time.Second)
	if q.entries["pending"].delay != 20*time.Millisecond || q.retrying != 1 {
		t.Fatal("failure did not select the error schedule")
	}
}

func TestPendingResultFailureAndContextTakePrecedence(t *testing.T) {
	failure := errors.New("provider failed")
	for _, item := range []struct {
		name    string
		after   time.Duration
		failure error
		cancel  bool
		want    error
	}{
		{"complete", 0, nil, false, nil},
		{"minimum", time.Millisecond, nil, false, nil},
		{"maximum", time.Hour, nil, false, nil},
		{"negative", -time.Second, nil, false, ErrReconcileResult},
		{"too-short", time.Nanosecond, nil, false, ErrReconcileResult},
		{"too-long", time.Hour + time.Nanosecond, nil, false, ErrReconcileResult},
		{"provider", time.Second, failure, false, failure},
		{"failed-commit", time.Second, errors.Join(failure, denied), false, denied},
		{"error-with-invalid-delay", -time.Second, failure, false, failure},
		{"canceled", time.Second, nil, true, context.Canceled},
	} {
		t.Run(item.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, err, finish := controllerResultWork(ctx, func(context.Context) (ReconcileResult, error) {
				if item.cancel {
					cancel()
				}
				return ReconcileResult{RecheckAfter: item.after}, item.failure
			})
			finish(false)
			if !errors.Is(err, item.want) {
				t.Fatal("result changed the error", err)
			}
			if err != nil && result != (ReconcileResult{}) {
				t.Fatal("failed work retained a pending result")
			}
			if err == nil && result.RecheckAfter != item.after {
				t.Fatal("valid result changed")
			}
		})
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	result, err, finish := controllerResultWork(ctx, func(context.Context) (ReconcileResult, error) { return ReconcileResult{RecheckAfter: time.Second}, nil })
	finish(true)
	if !errors.Is(err, context.DeadlineExceeded) || result != (ReconcileResult{}) {
		t.Fatal("deadline became pending", result, err)
	}
}

func TestPendingResultRechecksWithoutFailureAndExportsMetrics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m := new(Metrics)
	options := keyOptions()
	options.Metrics = m
	options.Workers = 1
	var attempts atomic.Int32
	source := KeyedSource[string]{
		Observe: func(ctx context.Context, sink *KeySink[string]) error {
			if err := sink.Add("pending"); err != nil {
				return err
			}
			if err := sink.Add("healthy"); err != nil {
				return err
			}
			sink.SetReady(true)
			<-ctx.Done()
			return ctx.Err()
		},
		Scan: func(context.Context, func(string) error) error { return nil },
	}
	done := make(chan error, 1)
	go func() {
		done <- RunKeyedWithResult(ctx, source, func(_ context.Context, stringKey string) (ReconcileResult, error) {
			if stringKey == "pending" && attempts.Add(1) < 3 {
				return ReconcileResult{RecheckAfter: 20 * time.Millisecond}, nil
			}
			return ReconcileResult{}, nil
		}, options)
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("controller did not join after cancellation")
		}
	}()
	snapshot := awaitMetrics(t, m, func(s MetricsSnapshot) bool { return s.Pending == 2 && s.Succeeded == 2 })
	if snapshot.Failed != 0 || snapshot.Retries != 0 || snapshot.Queue.Retrying != 0 || snapshot.Queue.Queued != 0 {
		t.Fatal("pending work used failure accounting", snapshot)
	}
	response := httptest.NewRecorder()
	m.ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(response.Body.String(), "stego_controller_actions_total{outcome=\"pending\"} 2\n") || !strings.Contains(response.Body.String(), "stego_controller_action_duration_seconds_count 4\n") {
		t.Fatal("pending outcome is absent from metrics")
	}
}

func TestPendingResultInvalidWatchActionStopsAndJoins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var stopped atomic.Bool
	receiving := make(chan struct{})
	source := Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
		return func() (string, error) { close(receiving); <-ctx.Done(); stopped.Store(true); return "", ctx.Err() }, nil
	}, Scan: func(ctx context.Context, emit func(string) error) error {
		select {
		case <-receiving:
			return emit("record")
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	err := RunKeyedWatchWithResult(ctx, source, func(context.Context, string) (ReconcileResult, error) {
		return ReconcileResult{RecheckAfter: -time.Second}, nil
	}, KeyedWatchOptions{KeyedOptions: keyOptions(), ReconnectDelay: time.Millisecond})
	if !errors.Is(err, ErrReconcileResult) || !stopped.Load() {
		t.Fatal("invalid result did not stop and join the watch", err)
	}
}
