package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// cycleCheckpoint is a checkpoint store in memory that refuses a save that breaks the cycle transition rule.
func cycleCheckpoint(t *testing.T, saved *Checkpoint) CheckpointAccess {
	t.Helper()
	return CheckpointAccess{
		Load: func(context.Context) (Checkpoint, error) { return *saved, nil },
		Save: func(ctx context.Context, version int64, encoded string) error {
			before, err := DecodeCycle(saved.After)
			if err != nil {
				t.Fatal(err)
			}
			after, err := DecodeCycle(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if ctx.Err() != nil || version != saved.Version || ValidateCycleTransition(before, after) != nil {
				t.Fatal("invalid cycle checkpoint transition")
			}
			*saved = Checkpoint{Version: version + 1, After: encoded}
			return nil
		},
	}
}

func retryCycle(parallel bool, ctx context.Context, access CheckpointAccess, count int, emit func(context.Context, int) error, allow func(error) bool, budget ObservationOptions, timeout time.Duration, retry CycleRetryOptions) (CycleState, error) {
	scan := ScanOptions{PageSize: count, MaxPages: 1, PageTimeout: time.Second}
	if parallel {
		return ScanCycleParallel(ctx, "input", access, numbered(count), emit, allow, scan, budget, ParallelCycleOptions[int]{Workers: 1, ActionTimeout: timeout, Retry: retry, Key: func(n int) string { return fmt.Sprint(n) }})
	}
	return ScanCycleWithOptions(ctx, "input", access, numbered(count), emit, allow, scan, budget, CycleOptions{ActionTimeout: timeout, Retry: retry})
}

func savedCycle(t *testing.T, saved Checkpoint) CycleState {
	t.Helper()
	state, err := DecodeCycle(saved.After)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

var longBudget = ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}

func TestRetryPolicy(t *testing.T) {
	transient := errors.New("transient")
	always := func(error) bool { return true }
	type row struct {
		name        string
		policy      CycleRetryOptions
		actionError error
	}
	attempts := func(t *testing.T, rows []row) {
		for _, parallel := range []bool{false, true} {
			for _, r := range rows {
				t.Run(fmt.Sprintf("parallel=%t/%s", parallel, r.name), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						saved, calls := Checkpoint{}, 0
						state, err := retryCycle(parallel, context.Background(), cycleCheckpoint(t, &saved), 1, func(context.Context, int) error { calls++; return r.actionError }, nil, longBudget, 20*time.Millisecond, r.policy)
						if !state.Failed || !errors.Is(err, r.actionError) || !errors.Is(err, ErrCycleFailed) || calls != 1 {
							t.Fatal(state, err, calls)
						}
					})
				})
			}
		}
	}

	t.Run("ST-SUP-RP02-LC03-T01 The zero retry policy makes one attempt", func(t *testing.T) {
		attempts(t, []row{{"zero", CycleRetryOptions{}, transient}, {"one", CycleRetryOptions{MaxAttempts: 1}, transient}})
	})

	t.Run("ST-SUP-RP02-LC03-T02 An out-of-bounds retry policy reaches no storage", func(t *testing.T) {
		for _, parallel := range []bool{false, true} {
			for i, policy := range []CycleRetryOptions{
				{MaxAttempts: -1},
				{MaxAttempts: MaxCycleAttempts + 1, Delay: time.Millisecond, Retryable: always},
				{MaxAttempts: 2, Delay: time.Nanosecond, Retryable: always},
				{MaxAttempts: 2, Delay: time.Hour, Retryable: always},
				{MaxAttempts: 2, Delay: 30 * time.Millisecond, Retryable: always},
			} {
				t.Run(fmt.Sprintf("parallel=%t/%d", parallel, i), func(t *testing.T) {
					calls := 0
					access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { calls++; return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error { calls++; return nil }}
					_, err := retryCycle(parallel, context.Background(), access, 1, func(context.Context, int) error { calls++; return nil }, nil, longBudget, 20*time.Millisecond, policy)
					if !errors.Is(err, ErrScanContract) || calls != 0 {
						t.Fatal(err, calls)
					}
				})
			}
		}
		// Four attempts is the bound itself.
		saved := Checkpoint{}
		if _, err := retryCycle(false, context.Background(), cycleCheckpoint(t, &saved), 1, func(context.Context, int) error { return nil }, nil, longBudget, 20*time.Millisecond, CycleRetryOptions{MaxAttempts: MaxCycleAttempts, Delay: time.Millisecond, Retryable: always}); err != nil {
			t.Fatal("four attempts were refused", err)
		}
	})

	t.Run("ST-SUP-RP02-LC03-T03 Contract, window and denied errors are not retried", func(t *testing.T) {
		attempts(t, []row{
			{"denied", CycleRetryOptions{MaxAttempts: 2, Delay: time.Millisecond, Retryable: func(error) bool { return false }}, transient},
			{"contract", CycleRetryOptions{MaxAttempts: 2, Delay: time.Millisecond, Retryable: always}, ErrScanContract},
			{"window", CycleRetryOptions{MaxAttempts: 2, Delay: time.Millisecond, Retryable: always}, ErrScanWindowLimit},
		})
	})
}

func TestRetryBudget(t *testing.T) {
	transient := errors.New("transient")
	always := func(error) bool { return true }

	t.Run("ST-SUP-RP02-LC09-T01 A retry delay longer than the action timeout is refused", func(t *testing.T) {
		timeout := 20 * time.Millisecond
		for _, parallel := range []bool{false, true} {
			opens := 0
			access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { opens++; return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error { return nil }}
			_, err := retryCycle(parallel, context.Background(), access, 1, func(context.Context, int) error { return nil }, nil, longBudget, timeout, CycleRetryOptions{MaxAttempts: 2, Delay: timeout + time.Millisecond, Retryable: always})
			if !errors.Is(err, ErrScanContract) || !strings.Contains(err.Error(), "21ms") || !strings.Contains(err.Error(), "20ms") || opens != 0 {
				t.Fatal("the long delay was not refused with its bounds before the source opened", err, opens)
			}
			saved := Checkpoint{}
			if _, err := retryCycle(parallel, context.Background(), cycleCheckpoint(t, &saved), 1, func(context.Context, int) error { return nil }, nil, longBudget, timeout, CycleRetryOptions{MaxAttempts: 2, Delay: timeout, Retryable: always}); err != nil {
				t.Fatal("a delay equal to the timeout was refused", err)
			}
		}
	})

	t.Run("ST-SUP-RP02-LC09-T02 A failed attempt is cancelled and its callback joined before the retry delay starts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			delay := time.Second
			var cancelled time.Time
			joined := make(chan struct{})
			calls := 0
			saved := Checkpoint{}
			_, err := retryCycle(false, context.Background(), cycleCheckpoint(t, &saved), 1, func(ctx context.Context, _ int) error {
				calls++
				if calls == 1 {
					go func() {
						<-ctx.Done()
						cancelled = time.Now()
						close(joined)
					}()
					return transient
				}
				select {
				case <-joined:
				default:
					return errors.New("the first callback had not returned")
				}
				if waited := time.Since(cancelled); waited < delay {
					return fmt.Errorf("the delay started %s before the first attempt was cancelled", delay-waited)
				}
				return nil
			}, nil, ObservationOptions{WorkTimeout: 10 * time.Second, CommitTimeout: time.Second}, 2*time.Second, CycleRetryOptions{MaxAttempts: 2, Delay: delay, Retryable: always})
			if err != nil || calls != 2 {
				t.Fatal(err, calls)
			}
		})
	})

	t.Run("ST-SUP-RP02-LC09-T03 No attempt starts when the remaining budget cannot cover the delay and a full action", func(t *testing.T) {
		delay, timeout := 10*time.Millisecond, 30*time.Millisecond
		for _, c := range []struct {
			name   string
			budget time.Duration
			calls  int
		}{{"short", delay + timeout - time.Millisecond, 1}, {"exact", delay + timeout, 2}} {
			t.Run(c.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					saved, calls := Checkpoint{}, 0
					start := time.Now()
					state, err := retryCycle(false, context.Background(), cycleCheckpoint(t, &saved), 1, func(context.Context, int) error {
						calls++
						if calls == 1 {
							return transient
						}
						return nil
					}, nil, ObservationOptions{WorkTimeout: c.budget, CommitTimeout: time.Second}, timeout, CycleRetryOptions{MaxAttempts: 2, Delay: delay, Retryable: always})
					if calls != c.calls {
						t.Fatal("wrong attempt count", calls, state, err)
					}
					stored := savedCycle(t, saved)
					if c.calls == 1 && (!stored.Failed || !errors.Is(err, transient)) {
						t.Fatal("the failure was not stored", stored, err)
					}
					if c.calls == 1 && time.Since(start) != 0 {
						t.Fatal("the cycle waited the delay of an attempt that it could not start", time.Since(start))
					}
					if c.calls == 2 && (stored.Failed || err != nil) {
						t.Fatal("the second attempt did not resolve the failure", stored, err)
					}
				})
			})
		}
	})

	t.Run("ST-SUP-RP02-LC09-T04 A terminal peer failure cancels the pending retries of a parallel cycle", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			terminal := errors.New("stop")
			waiting := make(chan struct{})
			var calls, active atomic.Int32
			saved := Checkpoint{}
			access := cycleCheckpoint(t, &saved)
			save := access.Save
			saves := 0
			access.Save = func(ctx context.Context, version int64, data string) error {
				saves++
				if active.Load() != 0 {
					t.Error("the save ran before both workers joined")
				}
				return save(ctx, version, data)
			}
			_, err := ScanCycleParallel(context.Background(), "input", access, numbered(2), func(_ context.Context, n int) error {
				active.Add(1)
				defer active.Add(-1)
				if n == 1 {
					calls.Add(1)
					return transient
				}
				<-waiting
				return terminal
			}, nil, ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}, longBudget, ParallelCycleOptions[int]{Workers: 2, ActionTimeout: 20 * time.Millisecond, Key: func(n int) string { return fmt.Sprint(n) }, Retry: CycleRetryOptions{MaxAttempts: 2, Delay: 10 * time.Millisecond, Retryable: func(err error) bool {
				if errors.Is(err, transient) {
					close(waiting)
					return true
				}
				return false
			}}})
			if !errors.Is(err, terminal) || calls.Load() != 1 || saves != 1 {
				t.Fatal(err, calls.Load(), saves)
			}
		})
	})

	t.Run("ST-SUP-RP02-LC09-T05 A later success does not clear a failure already stored in the cycle checkpoint", func(t *testing.T) {
		saved := Checkpoint{}
		var seen []int
		pass := func() (CycleState, error) {
			return ScanCycle(context.Background(), "input", cycleCheckpoint(t, &saved), numbered(2), func(_ context.Context, n int) error {
				seen = append(seen, n)
				if n == 1 && len(seen) == 1 {
					return transient
				}
				return nil
			}, func(error) bool { return true }, ScanOptions{PageSize: 1, MaxPages: 1, PageTimeout: time.Second}, longBudget)
		}
		if _, err := pass(); !errors.Is(err, transient) || !savedCycle(t, saved).Failed {
			t.Fatal("item 1's failure was not saved", err, saved)
		}
		state, err := pass()
		stored := savedCycle(t, saved)
		if !stored.Failed || !stored.Complete || !state.Failed || !errors.Is(err, ErrCycleFailed) {
			t.Fatal("item 2's success cleared the stored failure", stored, err)
		}
		if _, err := pass(); err != nil || len(seen) != 3 || seen[2] != 1 {
			t.Fatal("the next cycle did not start from item 1", seen, err)
		}
		if err := ValidateCycleTransition(CycleState{Source: "input", After: "1", Failed: true}, CycleState{Source: "input", After: "2", Complete: true}); !errors.Is(err, ErrScanContract) {
			t.Fatal("a transition that erases a failure was accepted", err)
		}
	})
}
