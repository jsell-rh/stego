package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func parallelOptions(workers int) ParallelCycleOptions[int] {
	return ParallelCycleOptions[int]{Workers: workers, ActionTimeout: 20 * time.Millisecond, Key: func(n int) string { return fmt.Sprint(n) }}
}
func parallelScan(size int) ScanOptions {
	return ScanOptions{PageSize: size, MaxPages: 1, PageTimeout: time.Second}
}
func parallelBudget() ObservationOptions {
	return ObservationOptions{WorkTimeout: 50 * time.Millisecond, CommitTimeout: time.Second}
}

func TestCycleParallelBudgetResumesWithoutFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saved := Checkpoint{}
		var lock sync.Mutex
		calls := map[int]int{}
		var active atomic.Int32
		for pass := 0; pass < 3; pass++ {
			access := budgetCheckpoint(t, &saved)
			save := access.Save
			access.Save = func(ctx context.Context, version int64, data string) error {
				if active.Load() != 0 {
					t.Error("save started before actions stopped")
				}
				return save(ctx, version, data)
			}
			state, err := ScanCycleParallel(context.Background(), "input", access, budgetSource(24), func(ctx context.Context, n int) error {
				active.Add(1)
				defer active.Add(-1)
				end, ok := ctx.Deadline()
				if !ok || time.Until(end) != 20*time.Millisecond {
					t.Error("action did not receive its budget")
				}
				time.Sleep(12 * time.Millisecond)
				lock.Lock()
				calls[n]++
				lock.Unlock()
				return nil
			}, nil, parallelScan(24), parallelBudget(), parallelOptions(3))
			want := min((pass+1)*9, 24)
			if err != nil || state.Failed || state.Complete != (pass == 2) || state.After != fmt.Sprint(want) || len(calls) != want {
				t.Fatal(pass, state, err, len(calls))
			}
		}
		for n := 1; n <= 24; n++ {
			if calls[n] != 1 {
				t.Fatal("resumed prefix was repeated or omitted", n, calls[n])
			}
		}
		if saved.Version != 3 {
			t.Fatal("progress was not saved", saved)
		}
	})
}

type parallelRecord struct{ id, stage int }

func TestCycleParallelSequenceKeysStaySerial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sourceFor := func(stage int) CursorSource[parallelRecord] {
			return func(ctx context.Context, after string, limit int) (CursorPage[parallelRecord], error) {
				page, err := budgetSource(6)(ctx, after, limit)
				out := CursorPage[parallelRecord]{More: page.More}
				for _, item := range page.Items {
					out.Items = append(out.Items, CursorItem[parallelRecord]{Cursor: item.Cursor, Value: parallelRecord{item.Value, stage}})
				}
				return out, err
			}
		}
		source, err := SequenceCursorSources([]NamedCursorSource[parallelRecord]{{Name: "records", Read: sourceFor(0)}, {Name: "journals", Read: sourceFor(1)}})
		if err != nil {
			t.Fatal(err)
		}
		var lock sync.Mutex
		active := map[int]bool{}
		stages := map[int][]int{}
		running, peak := 0, 0
		saved := Checkpoint{}
		state, err := ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), source, func(_ context.Context, item parallelRecord) error {
			lock.Lock()
			if active[item.id] {
				t.Error("the same resource ran concurrently")
			}
			active[item.id] = true
			running++
			peak = max(peak, running)
			stages[item.id] = append(stages[item.id], item.stage)
			lock.Unlock()
			time.Sleep(10 * time.Millisecond)
			lock.Lock()
			delete(active, item.id)
			running--
			lock.Unlock()
			return nil
		}, nil, parallelScan(12), parallelBudget(), ParallelCycleOptions[parallelRecord]{Workers: 3, ActionTimeout: 20 * time.Millisecond, Key: func(item parallelRecord) string { return fmt.Sprint(item.id) }})
		if err != nil || !state.Complete || state.Failed || peak != 3 || running != 0 {
			t.Fatal(state, err, peak, running)
		}
		for id := 1; id <= 6; id++ {
			if fmt.Sprint(stages[id]) != "[0 1]" {
				t.Fatal("resource order changed", id, stages[id])
			}
		}
	})
}

func TestCycleParallelStoppedPrefixReplaysLaterEffects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saved := Checkpoint{}
		later := make(chan struct{})
		failed := errors.New("action failed")
		var lock sync.Mutex
		calls := map[int]int{}
		count := func(n int) { lock.Lock(); calls[n]++; lock.Unlock() }
		state, err := ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(3), func(_ context.Context, n int) error {
			count(n)
			switch n {
			case 1:
				time.Sleep(time.Millisecond)
			case 2:
				<-later
				time.Sleep(time.Millisecond)
				return failed
			case 3:
				time.Sleep(2 * time.Millisecond)
				close(later)
			}
			return nil
		}, nil, parallelScan(3), parallelBudget(), parallelOptions(3))
		if !errors.Is(err, failed) || !errors.Is(err, ErrCycleFailed) || state.After != "1" || state.Complete || !state.Failed {
			t.Fatal(state, err)
		}
		state, err = ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(3), func(_ context.Context, n int) error { count(n); return nil }, nil, parallelScan(3), parallelBudget(), parallelOptions(3))
		if !state.Complete || !state.Failed || !errors.Is(err, ErrCycleFailed) || calls[1] != 1 || calls[2] != 2 || calls[3] != 2 {
			t.Fatal("restart lost failure or skipped later effects", state, err, calls)
		}
		state, err = ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(3), func(_ context.Context, n int) error { count(n); return nil }, nil, parallelScan(3), parallelBudget(), parallelOptions(3))
		if err != nil || state.Failed || !state.Complete {
			t.Fatal("a clean full cycle did not finish", state, err)
		}
	})
}

func TestCycleParallelLateNilFailureSurvivesPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saved := Checkpoint{}
		state, err := ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(12), func(ctx context.Context, n int) error {
			if n == 1 {
				<-ctx.Done()
				return nil
			}
			time.Sleep(12 * time.Millisecond)
			return nil
		}, func(error) bool { return true }, parallelScan(12), parallelBudget(), parallelOptions(2))
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrCycleFailed) || !state.Failed || state.Complete || state.After == "" {
			t.Fatal(state, err)
		}
		state, err = ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(12), func(context.Context, int) error { return nil }, nil, parallelScan(12), parallelBudget(), parallelOptions(2))
		if !state.Complete || !state.Failed || !errors.Is(err, ErrCycleFailed) {
			t.Fatal("restart erased the failed action", state, err)
		}
		state, err = ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(12), func(context.Context, int) error { return nil }, nil, parallelScan(12), parallelBudget(), parallelOptions(2))
		if err != nil || !state.Complete || state.Failed {
			t.Fatal(state, err)
		}
	})
}

func TestCycleParallelParentCancellationJoinsWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var entered, active atomic.Int32
		access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error { t.Error("cancelled parent saved progress"); return nil }}
		_, err := ScanCycleParallel(ctx, "input", access, budgetSource(6), func(ctx context.Context, _ int) error {
			active.Add(1)
			defer active.Add(-1)
			if entered.Add(1) == 3 {
				cancel()
			}
			<-ctx.Done()
			return ctx.Err()
		}, nil, parallelScan(6), parallelBudget(), parallelOptions(3))
		if !errors.Is(err, context.Canceled) || active.Load() != 0 || entered.Load() != 3 {
			t.Fatal(err, active.Load(), entered.Load())
		}
	})
}

func TestCycleParallelTerminalFailureCancelsPeersBeforeSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saved := Checkpoint{}
		var active, entered atomic.Int32
		access := budgetCheckpoint(t, &saved)
		save := access.Save
		access.Save = func(ctx context.Context, version int64, data string) error {
			if active.Load() != 0 {
				t.Error("save overlapped an action")
			}
			return save(ctx, version, data)
		}
		state, err := ScanCycleParallel(context.Background(), "input", access, budgetSource(8), func(ctx context.Context, n int) error {
			active.Add(1)
			defer active.Add(-1)
			entered.Add(1)
			if n == 2 {
				time.Sleep(time.Millisecond)
				return ErrScanContract
			}
			<-ctx.Done()
			return ctx.Err()
		}, func(err error) bool { return !errors.Is(err, ErrScanContract) }, parallelScan(8), parallelBudget(), parallelOptions(3))
		if !errors.Is(err, ErrScanContract) || !errors.Is(err, ErrCycleFailed) || state.After != "" || state.Complete || !state.Failed || active.Load() != 0 || entered.Load() != 3 {
			t.Fatal(state, err, entered.Load(), active.Load())
		}
	})
}

func TestCycleParallelFailurePolicyIsSerial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saved := Checkpoint{}
		providerError := errors.New("provider failed")
		var busy atomic.Bool
		calls := 0
		state, err := ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(3), func(context.Context, int) error { time.Sleep(time.Millisecond); return providerError }, func(error) bool {
			if !busy.CompareAndSwap(false, true) {
				t.Error("continuation policy ran concurrently")
			}
			defer busy.Store(false)
			time.Sleep(time.Millisecond)
			calls++
			return true
		}, parallelScan(3), parallelBudget(), parallelOptions(3))
		if !state.Complete || !state.Failed || !errors.Is(err, providerError) || !errors.Is(err, ErrCycleFailed) || calls != 3 {
			t.Fatal(state, err, calls)
		}
	})
}

func TestCycleParallelSaveConflictIsNotSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conflict := errors.New("checkpoint changed")
		var active atomic.Int32
		access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error {
			if active.Load() != 0 {
				t.Error("save overlapped an action")
			}
			return conflict
		}}
		state, err := ScanCycleParallel(context.Background(), "input", access, budgetSource(3), func(context.Context, int) error {
			active.Add(1)
			defer active.Add(-1)
			time.Sleep(time.Millisecond)
			return nil
		}, nil, parallelScan(3), parallelBudget(), parallelOptions(3))
		if !state.Complete || state.Failed || !errors.Is(err, conflict) || active.Load() != 0 {
			t.Fatal(state, err, active.Load())
		}
	})
}

func TestCycleParallelShortParentStartsNoAction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		saved := Checkpoint{}
		state, err := ScanCycleParallel(ctx, "input", budgetCheckpoint(t, &saved), budgetSource(3), func(context.Context, int) error { t.Error("action started without a full reserve"); return nil }, nil, parallelScan(3), ObservationOptions{WorkTimeout: 50 * time.Millisecond, CommitTimeout: 10 * time.Millisecond}, parallelOptions(3))
		if err != nil || state.Failed || state.Complete || state.After != "" || saved.Version != 1 || ctx.Err() != nil {
			t.Fatal(state, err, saved)
		}
	})
}

func TestCycleParallelRejectsInvalidOptionsBeforeWork(t *testing.T) {
	values := []ParallelCycleOptions[int]{parallelOptions(0), parallelOptions(-1), parallelOptions(65), {Workers: 2, ActionTimeout: 20 * time.Millisecond}, {Workers: 2, ActionTimeout: time.Nanosecond, Key: func(int) string { return "key" }}, {Workers: 2, ActionTimeout: 50 * time.Millisecond, Key: func(int) string { return "key" }}}
	for _, options := range values {
		access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) {
			t.Error("invalid options reached storage")
			return Checkpoint{}, nil
		}, Save: func(context.Context, int64, string) error { t.Error("invalid options saved progress"); return nil }}
		_, err := ScanCycleParallel(context.Background(), "input", access, budgetSource(1), func(context.Context, int) error { t.Error("invalid options started effects"); return nil }, nil, parallelScan(1), parallelBudget(), options)
		if !errors.Is(err, ErrScanContract) {
			t.Fatal("invalid options accepted", err)
		}
	}
}

func TestCycleParallelValidatesWholePageBeforeEffects(t *testing.T) {
	for _, bad := range []string{"", strings.Repeat("k", 513), "nul\x00key", string([]byte{0xff})} {
		saved := Checkpoint{}
		options := parallelOptions(3)
		options.Key = func(n int) string {
			if n == 3 {
				return bad
			}
			return fmt.Sprint(n)
		}
		_, err := ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(3), func(context.Context, int) error { t.Error("invalid final key permitted effects"); return nil }, nil, parallelScan(3), parallelBudget(), options)
		if !errors.Is(err, ErrScanContract) {
			t.Fatal(err)
		}
	}
	for _, cursor := range []string{"1", strings.Repeat("c", 513)} {
		saved := Checkpoint{}
		source := func(context.Context, string, int) (CursorPage[int], error) {
			return CursorPage[int]{Items: []CursorItem[int]{{Cursor: "1", Value: 1}, {Cursor: cursor, Value: 2}}}, nil
		}
		_, err := ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), source, func(context.Context, int) error { t.Error("invalid final cursor permitted effects"); return nil }, nil, parallelScan(3), parallelBudget(), parallelOptions(3))
		if !errors.Is(err, ErrScanContract) {
			t.Fatal(err)
		}
	}
}

func TestCycleParallelSourceWindowRetainsFailure(t *testing.T) {
	saved := Checkpoint{}
	var called atomic.Int32
	source := func(ctx context.Context, after string, limit int) (CursorPage[int], error) {
		if after != "" {
			return CursorPage[int]{}, ErrScanWindowLimit
		}
		page, err := budgetSource(3)(ctx, after, limit)
		page.More = true
		return page, err
	}
	scan := parallelScan(3)
	scan.MaxPages = 2
	state, err := ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), source, func(context.Context, int) error { called.Add(1); return nil }, nil, scan, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}, ParallelCycleOptions[int]{Workers: 3, ActionTimeout: 100 * time.Millisecond, Key: func(n int) string { return fmt.Sprint(n) }})
	if !state.Complete || !state.Failed || state.After != "3" || !errors.Is(err, ErrScanWindowLimit) || !errors.Is(err, ErrCycleFailed) || called.Load() != 3 {
		t.Fatal(state, err, called.Load())
	}
}

func TestCycleParallelEmptySourceStartsNoWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saved := Checkpoint{}
		options := parallelOptions(3)
		options.Key = func(int) string { t.Error("empty source requested a key"); return "key" }
		state, err := ScanCycleParallel(context.Background(), "input", budgetCheckpoint(t, &saved), budgetSource(0), func(context.Context, int) error { t.Error("empty source emitted an action"); return nil }, nil, parallelScan(3), parallelBudget(), options)
		if err != nil || !state.Complete || state.Failed || state.After != "" || saved.Version != 1 {
			t.Fatal(state, err, saved)
		}
	})
}
