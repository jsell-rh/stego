package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestObservationReservesACommitAfterWorkDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	end, _ := parent.Deadline()
	options := ObservationOptions{WorkTimeout: time.Second, CommitTimeout: 200 * time.Millisecond}
	var operation, write context.Context
	err := RunObservation(parent, func(ctx context.Context) error {
		operation = ctx
		deadline, _ := ctx.Deadline()
		if !deadline.Equal(end.Add(-options.CommitTimeout)) {
			t.Fatal("work did not reserve commit time")
		}
		<-ctx.Done()
		return nil // A late success must be rejected.
	}, func(ctx context.Context, err error) error {
		write = ctx
		if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("deadline prevented failure commit", ctx.Err(), err)
		}
		if operation.Err() == nil {
			t.Fatal("work context stayed active during commit")
		}
		return nil
	}, options)
	if !errors.Is(err, context.DeadlineExceeded) || write == nil {
		t.Fatal("work failure was lost", err)
	}
	if write.Err() == nil {
		t.Fatal("commit context was not closed")
	}
}

func TestObservationPreservesBothFailures(t *testing.T) {
	failed, conflict := errors.New("provider failed"), errors.New("revision conflict")
	err := RunObservation(context.Background(), func(context.Context) error { return failed }, func(_ context.Context, err error) error {
		if !errors.Is(err, failed) {
			t.Fatal("missing work error")
		}
		return conflict
	}, ObservationOptions{time.Second, time.Second})
	if !errors.Is(err, failed) || !errors.Is(err, conflict) {
		t.Fatal("work or commit failure was lost", err)
	}
}

func TestObservationUsesRemainingTimeForFailureCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	workCalls, commits := 0, 0
	err := RunObservation(ctx, func(context.Context) error { workCalls++; return nil }, func(ctx context.Context, err error) error {
		commits++
		if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("invalid reserved failure", ctx.Err(), err)
		}
		return nil
	}, ObservationOptions{time.Second, 2 * time.Second})
	if workCalls != 0 || commits != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("work used commit reserve", workCalls, commits, err)
	}
}

func TestObservationStopsOnParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	commits := 0
	err := RunObservation(ctx, func(context.Context) error { cancel(); return nil }, func(context.Context, error) error { commits++; return nil }, ObservationOptions{time.Second, time.Second})
	if !errors.Is(err, context.Canceled) || commits != 0 {
		t.Fatal("canceled parent permitted a commit", err, commits)
	}
}

func TestObservationChecksLateCommitSuccess(t *testing.T) {
	err := RunObservation(context.Background(), func(context.Context) error { return nil }, func(ctx context.Context, _ error) error {
		<-ctx.Done()
		return nil
	}, ObservationOptions{time.Second, 5 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("late commit returned success", err)
	}
}

func TestObservationWaitsForCallbacksToReturn(t *testing.T) {
	for _, phase := range []string{"work", "commit"} {
		t.Run(phase, func(t *testing.T) {
			canceled, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			call := func(ctx context.Context) error { <-ctx.Done(); close(canceled); <-release; return nil }
			go func() {
				done <- RunObservation(context.Background(), func(ctx context.Context) error {
					if phase == "work" {
						return call(ctx)
					}
					return nil
				}, func(ctx context.Context, _ error) error {
					if phase == "commit" {
						return call(ctx)
					}
					return nil
				}, ObservationOptions{5 * time.Millisecond, 5 * time.Millisecond})
			}()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("callback was not canceled")
			}
			select {
			case <-done:
				close(release)
				t.Fatal("callback was detached")
			case <-time.After(5 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("callback did not join")
			}
		})
	}
}

func TestObservationRejectsInvalidContracts(t *testing.T) {
	work := func(context.Context) error { t.Fatal("invalid work ran"); return nil }
	commit := func(context.Context, error) error { t.Fatal("invalid commit ran"); return nil }
	for _, options := range []ObservationOptions{{}, {-1, time.Second}, {time.Minute + 1, time.Second}, {time.Second, 0}, {time.Second, time.Minute + 1}} {
		if err := RunObservation(context.Background(), work, commit, options); !errors.Is(err, ErrObservationContract) {
			t.Fatal(err)
		}
	}
	options := ObservationOptions{time.Second, time.Second}
	for _, err := range []error{RunObservation(nil, work, commit, options), RunObservation(context.Background(), nil, commit, options), RunObservation(context.Background(), work, nil, options)} {
		if !errors.Is(err, ErrObservationContract) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunObservation(ctx, work, commit, options); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	delayedTimer := observationDeadlineContext{Context: context.Background(), end: time.Now().Add(-time.Second)}
	if err := RunObservation(delayedTimer, work, commit, options); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired clock deadline was ignored", err)
	}
}

type observationDeadlineContext struct {
	context.Context
	end time.Time
}

func (ctx observationDeadlineContext) Deadline() (time.Time, bool) { return ctx.end, true }

func TestObservationCallsAreIndependent(t *testing.T) {
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 10 {
				called := false
				err := RunObservation(context.Background(), func(context.Context) error { return nil }, func(ctx context.Context, err error) error {
					if err != nil || ctx.Err() != nil {
						t.Error("invalid success context", err, ctx.Err())
					}
					called = true
					return nil
				}, ObservationOptions{time.Second, time.Second})
				if err != nil || !called {
					t.Error("observation failed", err)
				}
			}
		})
	}
	workers.Wait()
}

func TestObservationClosesWorkContextOnPanic(t *testing.T) {
	var operation context.Context
	func() {
		defer func() {
			if recover() != "provider panic" {
				t.Error("provider panic changed")
			}
		}()
		_ = RunObservation(context.Background(), func(ctx context.Context) error {
			operation = ctx
			panic("provider panic")
		}, func(context.Context, error) error { t.Fatal("panic started a commit"); return nil }, ObservationOptions{time.Second, time.Second})
	}()
	if operation == nil || !errors.Is(operation.Err(), context.Canceled) {
		t.Fatal("panic left work context active")
	}
}

func BenchmarkObservation(b *testing.B) {
	work := func(context.Context) error { return nil }
	commit := func(context.Context, error) error { return nil }
	options := ObservationOptions{time.Second, time.Second}
	b.ReportAllocs()
	for range b.N {
		if err := RunObservation(context.Background(), work, commit, options); err != nil {
			b.Fatal(err)
		}
	}
}
