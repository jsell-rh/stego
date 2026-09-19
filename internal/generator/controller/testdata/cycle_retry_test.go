package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func retryCycle(parallel bool, ctx context.Context, access CheckpointAccess, count int, emit func(context.Context, int) error, allow func(error) bool, budget ObservationOptions, timeout time.Duration, retry CycleRetryOptions) (CycleState, error) {
	scan := ScanOptions{PageSize: count, MaxPages: 1, PageTimeout: time.Second}
	if parallel {
		return ScanCycleParallel(ctx, "input", access, budgetSource(count), emit, allow, scan, budget, ParallelCycleOptions[int]{Workers: 1, ActionTimeout: timeout, Retry: retry, Key: func(n int) string { return fmt.Sprint(n) }})
	}
	return ScanCycleWithOptions(ctx, "input", access, budgetSource(count), emit, allow, scan, budget, CycleOptions{ActionTimeout: timeout, Retry: retry})
}

func TestCycleRetryResolvesCurrentFailureBeforeCursorAdvance(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		for _, priorFailed := range []bool{false, true} {
			t.Run(fmt.Sprintf("parallel=%t/prior=%t", parallel, priorFailed), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					saved := Checkpoint{}
					if priorFailed {
						encoded, err := EncodeCycle(CycleState{Source: "input", Failed: true})
						if err != nil {
							t.Fatal(err)
						}
						saved = Checkpoint{Version: 1, After: encoded}
					}
					transient := errors.New("private provider response")
					called := []int{}
					var previous context.Context
					var ended time.Time
					state, err := retryCycle(parallel, context.Background(), budgetCheckpoint(t, &saved), 2, func(ctx context.Context, n int) error {
						called = append(called, n)
						end, ok := ctx.Deadline()
						if !ok || time.Until(end) != 20*time.Millisecond {
							return errors.New("incomplete action budget")
						}
						if len(called) == 1 {
							previous = ctx
							ended = time.Now()
							return transient
						}
						if previous.Err() != context.Canceled || time.Since(ended) < time.Millisecond {
							return errors.New("retry did not release its prior context or wait")
						}
						return nil
					}, nil, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}, 20*time.Millisecond, CycleRetryOptions{MaxAttempts: 2, Delay: time.Millisecond, Retryable: func(err error) bool { return errors.Is(err, transient) }})
					if !state.Complete || state.Failed != priorFailed || errors.Is(err, ErrCycleFailed) != priorFailed || (!priorFailed && err != nil) || !reflect.DeepEqual(called, []int{1, 1, 2}) {
						t.Fatal(state, err, called)
					}
				})
			})
		}
	}
}

func TestCycleRetryLimitsAndErrorSelection(t *testing.T) {
	transient := errors.New("transient")
	for _, parallel := range []bool{false, true} {
		for _, row := range []struct {
			name        string
			policy      CycleRetryOptions
			actionError error
			attempts    int
		}{
			{"default", CycleRetryOptions{}, transient, 1},
			{"one", CycleRetryOptions{MaxAttempts: 1}, transient, 1},
			{"exhausted", CycleRetryOptions{MaxAttempts: 3, Delay: time.Millisecond, Retryable: func(error) bool { return true }}, transient, 3},
			{"denied", CycleRetryOptions{MaxAttempts: 2, Delay: time.Millisecond, Retryable: func(error) bool { return false }}, transient, 1},
			{"contract", CycleRetryOptions{MaxAttempts: 2, Delay: time.Millisecond, Retryable: func(error) bool { return true }}, ErrScanContract, 1},
			{"window", CycleRetryOptions{MaxAttempts: 2, Delay: time.Millisecond, Retryable: func(error) bool { return true }}, ErrScanWindowLimit, 1},
		} {
			t.Run(fmt.Sprintf("parallel=%t/%s", parallel, row.name), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					saved, calls := Checkpoint{}, 0
					state, err := retryCycle(parallel, context.Background(), budgetCheckpoint(t, &saved), 1, func(context.Context, int) error { calls++; return row.actionError }, nil, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}, 20*time.Millisecond, row.policy)
					if !state.Failed || !errors.Is(err, row.actionError) || !errors.Is(err, ErrCycleFailed) || calls != row.attempts {
						t.Fatal(state, err, calls)
					}
				})
			})
		}
	}
}

func TestCycleRetryDeadlineRequiresFullReserve(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		for _, enough := range []bool{false, true} {
			t.Run(fmt.Sprintf("parallel=%t/enough=%t", parallel, enough), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					saved, calls := Checkpoint{}, 0
					work := 50 * time.Millisecond
					if enough {
						work = 100 * time.Millisecond
					}
					state, err := retryCycle(parallel, context.Background(), budgetCheckpoint(t, &saved), 1, func(ctx context.Context, _ int) error {
						calls++
						if calls == 1 {
							<-ctx.Done()
							return nil
						}
						return nil
					}, func(error) bool { return true }, ObservationOptions{WorkTimeout: work, CommitTimeout: time.Second}, 30*time.Millisecond, CycleRetryOptions{MaxAttempts: 2, Delay: time.Millisecond, Retryable: func(err error) bool { return errors.Is(err, context.DeadlineExceeded) }})
					if enough {
						if err != nil || !state.Complete || state.Failed || calls != 2 {
							t.Fatal(state, err, calls)
						}
					} else if !errors.Is(err, context.DeadlineExceeded) || !state.Failed || calls != 1 {
						t.Fatal(state, err, calls)
					}
				})
			})
		}
	}
}

func TestCycleRetryCancellationStopsDelayAndSave(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprint(parallel), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls, saves := 0, 0
				access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error { saves++; return nil }}
				_, err := retryCycle(parallel, ctx, access, 1, func(context.Context, int) error {
					calls++
					time.AfterFunc(time.Millisecond, cancel)
					return errors.New("retry")
				}, nil, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}, 20*time.Millisecond, CycleRetryOptions{MaxAttempts: 2, Delay: 10 * time.Millisecond, Retryable: func(error) bool { return true }})
				if !errors.Is(err, context.Canceled) || calls != 1 || saves != 0 {
					t.Fatal(err, calls, saves)
				}
			})
		})
	}
}

func TestCycleRetryDoesNotRetryCheckpointConflict(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprint(parallel), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				conflict := errors.New("checkpoint conflict")
				calls, saves := 0, 0
				access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error { saves++; return conflict }}
				_, err := retryCycle(parallel, context.Background(), access, 1, func(context.Context, int) error { calls++; return nil }, nil, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}, 20*time.Millisecond, CycleRetryOptions{MaxAttempts: 2, Delay: time.Millisecond, Retryable: func(error) bool { return true }})
				if !errors.Is(err, conflict) || calls != 1 || saves != 1 {
					t.Fatal(err, calls, saves)
				}
			})
		})
	}
}

func TestCycleRetryRejectsInvalidPolicyBeforeStorage(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		for i, policy := range []CycleRetryOptions{
			{MaxAttempts: -1}, {MaxAttempts: MaxCycleAttempts + 1},
			{Delay: time.Millisecond}, {Retryable: func(error) bool { return true }},
			{MaxAttempts: 2, Delay: time.Millisecond},
			{MaxAttempts: 2, Retryable: func(error) bool { return true }},
			{MaxAttempts: 2, Delay: time.Nanosecond, Retryable: func(error) bool { return true }},
			{MaxAttempts: 2, Delay: time.Hour, Retryable: func(error) bool { return true }},
			{MaxAttempts: 2, Delay: 30 * time.Millisecond, Retryable: func(error) bool { return true }},
		} {
			t.Run(fmt.Sprintf("parallel=%t/%d", parallel, i), func(t *testing.T) {
				calls := 0
				access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { calls++; return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error { calls++; return nil }}
				_, err := retryCycle(parallel, context.Background(), access, 1, func(context.Context, int) error { calls++; return nil }, nil, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}, 20*time.Millisecond, policy)
				if !errors.Is(err, ErrScanContract) || calls != 0 {
					t.Fatal(err, calls)
				}
			})
		}
	}
}

func TestCycleRetryRetainsKeyOrderWithIndependentWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		order := []int{}
		attempts := map[int]int{}
		var activeA atomic.Int32
		var overlapped atomic.Bool
		transient := errors.New("retry")
		saved := Checkpoint{}
		state, err := ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(3), func(ctx context.Context, n int) error {
			if n != 2 {
				if activeA.Add(1) != 1 {
					overlapped.Store(true)
				}
				defer activeA.Add(-1)
			}
			mu.Lock()
			attempts[n]++
			attempt := attempts[n]
			if n != 2 {
				order = append(order, n)
			}
			mu.Unlock()
			if n == 2 {
				time.Sleep(10 * time.Millisecond)
				return nil
			}
			if n == 1 && attempt == 1 {
				return transient
			}
			return nil
		}, nil, ScanOptions{PageSize: 3, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}, ParallelCycleOptions[int]{Workers: 2, ActionTimeout: 20 * time.Millisecond, Key: func(n int) string {
			if n == 2 {
				return "b"
			}
			return "a"
		}, Retry: CycleRetryOptions{MaxAttempts: 2, Delay: time.Millisecond, Retryable: func(err error) bool { return errors.Is(err, transient) }}})
		if err != nil || !state.Complete || state.Failed || overlapped.Load() || !reflect.DeepEqual(order, []int{1, 1, 3}) || attempts[2] != 1 {
			t.Fatal(state, err, order, attempts)
		}
	})
}

func TestCycleRetryPeerFailureCancelsWaitingRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transient, terminal := errors.New("retry"), errors.New("stop")
		waiting := make(chan struct{})
		var calls atomic.Int32
		saved := Checkpoint{}
		_, err := ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(2), func(_ context.Context, n int) error {
			if n == 1 {
				calls.Add(1)
				return transient
			}
			<-waiting
			return terminal
		}, nil, ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}, ParallelCycleOptions[int]{Workers: 2, ActionTimeout: 20 * time.Millisecond, Key: func(n int) string { return fmt.Sprint(n) }, Retry: CycleRetryOptions{MaxAttempts: 2, Delay: 10 * time.Millisecond, Retryable: func(err error) bool {
			if errors.Is(err, transient) {
				close(waiting)
				return true
			}
			return false
		}}})
		if !errors.Is(err, terminal) || calls.Load() != 1 {
			t.Fatal(err, calls.Load())
		}
	})
}
