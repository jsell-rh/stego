package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

func budgetCheckpoint(t *testing.T, saved *Checkpoint) CheckpointAccess {
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
				t.Fatal("invalid budget checkpoint transition")
			}
			*saved = Checkpoint{Version: version + 1, After: encoded}
			return nil
		},
	}
}
func budgetSource(count int) CursorSource[int] {
	return func(_ context.Context, after string, limit int) (CursorPage[int], error) {
		first := 0
		if after != "" {
			var err error
			first, err = strconv.Atoi(after)
			if err != nil {
				return CursorPage[int]{}, err
			}
		}
		page := CursorPage[int]{}
		for n := first + 1; n <= count && len(page.Items) < limit; n++ {
			page.Items = append(page.Items, CursorItem[int]{Cursor: fmt.Sprint(n), Value: n})
		}
		page.More = first+len(page.Items) < count
		return page, nil
	}
}

func TestCycleActionBudgetResumesWithoutDeadlineFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saved := Checkpoint{}
		called := []int{}
		for pass := 0; pass < 4; pass++ {
			// Each pass constructs new callbacks and reads only the saved checkpoint.
			state, err := ScanCycleWithOptions(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(10), func(ctx context.Context, n int) error {
				end, ok := ctx.Deadline()
				if !ok || time.Until(end) != 20*time.Millisecond {
					t.Fatal("action did not receive its full budget")
				}
				time.Sleep(12 * time.Millisecond)
				called = append(called, n)
				return nil
			}, nil, ScanOptions{PageSize: 10, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: 50 * time.Millisecond, CommitTimeout: time.Second}, CycleOptions{ActionTimeout: 20 * time.Millisecond})
			if err != nil || state.Failed || state.Complete != (pass == 3) {
				t.Fatal("budget pause changed the cycle outcome", pass, state, err)
			}
			want := min((pass+1)*3, 10)
			if state.After != fmt.Sprint(want) || len(called) != want {
				t.Fatal("budget pause lost or repeated work", state, called)
			}
		}
		if saved.Version != 4 {
			t.Fatal("progress was not saved on every pass", saved)
		}
		for i, n := range called {
			if n != i+1 {
				t.Fatal("unexpected action order", called)
			}
		}
	})
}

func TestCycleActionBudgetRetainsRealFailureAcrossPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saved := Checkpoint{}
		scan := ScanOptions{PageSize: 3, MaxPages: 1, PageTimeout: time.Second}
		budget := ObservationOptions{WorkTimeout: 50 * time.Millisecond, CommitTimeout: time.Second}
		options := CycleOptions{ActionTimeout: 20 * time.Millisecond}
		state, err := ScanCycleWithOptions(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(3), func(ctx context.Context, n int) error {
			if n == 1 {
				<-ctx.Done()
				return nil
			} // A late nil is still a failed action.
			time.Sleep(12 * time.Millisecond)
			return nil
		}, func(error) bool { return true }, scan, budget, options)
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrCycleFailed) || !state.Failed || state.Complete || state.After != "2" {
			t.Fatal(state, err)
		}
		state, err = ScanCycleWithOptions(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(3), func(context.Context, int) error { return nil }, nil, scan, budget, options)
		if !state.Complete || !state.Failed || !errors.Is(err, ErrCycleFailed) {
			t.Fatal("resumption erased a provider failure", state, err)
		}
		state, err = ScanCycleWithOptions(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(3), func(context.Context, int) error { return nil }, nil, scan, budget, options)
		if !state.Complete || state.Failed || err != nil {
			t.Fatal("next clean cycle did not complete", state, err)
		}
	})
}

func TestCycleActionBudgetSaveConflictIsNotCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conflict := errors.New("checkpoint changed")
		access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error { return conflict }}
		state, err := ScanCycleWithOptions(context.Background(), "input", access, budgetSource(5), func(context.Context, int) error { time.Sleep(12 * time.Millisecond); return nil }, nil, ScanOptions{PageSize: 5, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: 50 * time.Millisecond, CommitTimeout: time.Second}, CycleOptions{ActionTimeout: 20 * time.Millisecond})
		if !errors.Is(err, conflict) || state.Complete || state.Failed || state.After != "3" {
			t.Fatal(state, err)
		}
	})
}

func TestCycleActionBudgetParentCancellationPreventsSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error { t.Fatal("cancelled parent saved a checkpoint"); return nil }}
		_, err := ScanCycleWithOptions(ctx, "input", access, budgetSource(2), func(context.Context, int) error { cancel(); return nil }, nil, ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: 50 * time.Millisecond, CommitTimeout: time.Second}, CycleOptions{ActionTimeout: 20 * time.Millisecond})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestCycleActionBudgetRejectsInvalidLimitsBeforeWork(t *testing.T) {
	for _, duration := range []time.Duration{-time.Second, 0, time.Nanosecond, 50 * time.Millisecond, time.Hour} {
		_, err := ScanCycleWithOptions(context.Background(), "input", CheckpointAccess{Load: func(context.Context) (Checkpoint, error) {
			t.Fatal("invalid budget reached storage")
			return Checkpoint{}, nil
		}, Save: func(context.Context, int64, string) error { t.Fatal("invalid budget saved progress"); return nil }}, budgetSource(1), func(context.Context, int) error { t.Fatal("invalid budget started work"); return nil }, nil, ScanOptions{PageSize: 1, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: 50 * time.Millisecond, CommitTimeout: time.Second}, CycleOptions{ActionTimeout: duration})
		if !errors.Is(err, ErrScanContract) {
			t.Fatal("invalid action budget accepted", duration, err)
		}
	}
}

func TestCycleActionBudgetUsesParentTimeReserve(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		saved := Checkpoint{}
		state, err := ScanCycleWithOptions(ctx, "input", budgetCheckpoint(t, &saved), budgetSource(1), func(context.Context, int) error { t.Fatal("action started without a full time reserve"); return nil }, nil, ScanOptions{PageSize: 1, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: 50 * time.Millisecond, CommitTimeout: 10 * time.Millisecond}, CycleOptions{ActionTimeout: 20 * time.Millisecond})
		if err != nil || state.Failed || state.Complete || state.After != "" || saved.Version != 1 || ctx.Err() != nil {
			t.Fatal("short parent budget changed the outcome", state, saved, err)
		}
	})
}
