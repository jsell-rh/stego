package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCycleRetainsFailureAcrossProcessReplacement(t *testing.T) {
	saved := Checkpoint{}
	access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return saved, nil }, Save: func(_ context.Context, version int64, value string) error {
		if version != saved.Version {
			t.Fatal("stale save")
		}
		saved = Checkpoint{Version: version + 1, After: value}
		return nil
	}}
	source := func(_ context.Context, after string, _ int) (CursorPage[string], error) {
		if after == "a" {
			return CursorPage[string]{Items: []CursorItem[string]{{"b", "b"}}}, nil
		}
		return CursorPage[string]{Items: []CursorItem[string]{{"a", "a"}}, More: true}, nil
	}
	scan := ScanOptions{PageSize: 1, MaxPages: 1, PageTimeout: time.Second}
	budget := ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}
	failed := errors.New("private provider failure")
	first, err := ScanCycle(context.Background(), "desired-1", access, source, func(context.Context, string) error { return failed }, func(error) bool { return true }, scan, budget)
	if !errors.Is(err, failed) || !errors.Is(err, ErrCycleFailed) || first.Complete || !first.Failed || strings.Contains(saved.After, "private") {
		t.Fatal(first, saved, err)
	}
	// The next call has no shared error accumulator from the earlier call.
	second, err := ScanCycle(context.Background(), "desired-1", access, source, func(context.Context, string) error { return nil }, nil, scan, budget)
	if !second.Complete || !second.Failed || !errors.Is(err, ErrCycleFailed) {
		t.Fatal("earlier failure was lost", second, err)
	}
	third, err := ScanCycle(context.Background(), "desired-1", access, source, func(context.Context, string) error { return nil }, nil, scan, budget)
	if err != nil || third.Complete || third.Failed || third.After != "a" {
		t.Fatal("next full cycle did not restart", third, err)
	}
	fourth, err := ScanCycle(context.Background(), "desired-1", access, source, func(context.Context, string) error { return nil }, nil, scan, budget)
	if err != nil || !fourth.Complete || fourth.Failed {
		t.Fatal(fourth, err)
	}
}

func TestCycleRejectsUnpersistablePageBeforeEffects(t *testing.T) {
	for _, cursor := range []string{strings.Repeat("a", 513), "a\x00b", strings.Repeat("\n", 512)} {
		effects := 0
		_, err := ScanCycle(context.Background(), "source", CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error { return nil }}, func(context.Context, string, int) (CursorPage[int], error) {
			return CursorPage[int]{Items: []CursorItem[int]{{"valid", 1}, {cursor, 2}}}, nil
		}, func(context.Context, int) error { effects++; return nil }, nil, ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second})
		if !errors.Is(err, ErrScanContract) || effects != 0 {
			t.Fatal("invalid page caused effects", effects, err)
		}
	}
	for _, value := range []string{`[1,"source","a",null,false]`, `[2,"source","a",false,false]`, `[1,"source","a",false,false,0]`, `[1, "source","a",false,false]`} {
		if _, err := DecodeCycle(value); err == nil {
			t.Fatal("invalid cycle accepted", value)
		}
	}
}

func TestCycleChangedSourceRestartsAndConflictIsPreserved(t *testing.T) {
	encoded, _ := EncodeCycle(CycleState{Source: "old", After: "a", Failed: true})
	conflict := errors.New("checkpoint changed")
	access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return Checkpoint{Version: 2, After: encoded}, nil }, Save: func(context.Context, int64, string) error { return conflict }}
	result, err := ScanCycle(context.Background(), "new", access, func(_ context.Context, after string, _ int) (CursorPage[int], error) {
		if after != "" {
			t.Fatal("old source resumed", after)
		}
		return CursorPage[int]{Items: []CursorItem[int]{{"a", 1}}}, nil
	}, func(context.Context, int) error { return nil }, nil, ScanOptions{PageSize: 1, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second})
	if !result.Complete || result.Failed || !errors.Is(err, conflict) {
		t.Fatal(result, err)
	}
}

func TestCycleTimeoutSavesFailureButParentCancellationDoesNot(t *testing.T) {
	for _, parentCancellation := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		saves := 0
		access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil }, Save: func(ctx context.Context, _ int64, data string) error {
			saves++
			value, err := DecodeCycle(data)
			if err != nil || !value.Failed || value.Complete || value.After != "a" || ctx.Err() != nil {
				t.Fatal("invalid saved timeout state", value, err, ctx.Err())
			}
			return nil
		}}
		result, err := ScanCycle(ctx, "1", access, func(context.Context, string, int) (CursorPage[int], error) {
			return CursorPage[int]{Items: []CursorItem[int]{{"a", 1}, {"b", 2}}}, nil
		}, func(work context.Context, value int) error {
			if value == 1 {
				return nil
			}
			if parentCancellation {
				cancel()
			}
			<-work.Done()
			// Even a nil return after the deadline is not a timely confirmation.
			return nil
		}, func(error) bool { return true }, ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: 20 * time.Millisecond, CommitTimeout: time.Second})
		cancel()
		if result.After != "a" || !result.Failed || err == nil || parentCancellation && saves != 0 || !parentCancellation && saves != 1 {
			t.Fatal(result, saves, err)
		}
	}
}

func TestCycleTransitionCannotEraseEarlierFailure(t *testing.T) {
	previous := CycleState{Source: "1", After: "opaque-cursor", Failed: true}
	next := CycleState{Source: "1", After: "next-cursor", Complete: true}
	if err := ValidateCycleTransition(previous, next); !errors.Is(err, ErrScanContract) {
		t.Fatal("failure was erased", err)
	}
	next.Failed = true
	if err := ValidateCycleTransition(previous, next); err != nil {
		t.Fatal(err)
	}
	previous.Complete = true
	next.Failed = false
	if err := ValidateCycleTransition(previous, next); err != nil {
		t.Fatal("new cycle was rejected", err)
	}
	previous.Complete = false
	next.Source = "2"
	if err := ValidateCycleTransition(previous, next); err != nil {
		t.Fatal("changed source was rejected", err)
	}
}
