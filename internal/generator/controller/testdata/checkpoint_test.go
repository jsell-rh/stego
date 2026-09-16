package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCheckpointedScanReservesCommitAndRetriesFailedItem(t *testing.T) {
	saved := Checkpoint{}
	writes := 0
	access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return saved, nil }, Save: func(ctx context.Context, version int64, after string) error {
		if ctx.Err() != nil || version != saved.Version {
			t.Fatal("invalid commit", ctx.Err(), version, saved)
		}
		writes++
		saved = Checkpoint{After: after, Version: version + 1}
		return nil
	}}
	source := func(_ context.Context, after string, _ int) (CursorPage[string], error) {
		if after == "a" {
			return CursorPage[string]{Items: []CursorItem[string]{{"b", "b"}}}, nil
		}
		return CursorPage[string]{Items: []CursorItem[string]{{"a", "a"}, {"b", "b"}}}, nil
	}
	calls := []string{}
	first := true
	emit := func(ctx context.Context, value string) error {
		calls = append(calls, value)
		if value == "b" && first {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	opts := ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}
	budget := ObservationOptions{WorkTimeout: 20 * time.Millisecond, CommitTimeout: time.Second}
	progress, err := ScanCheckpointed(context.Background(), access, source, emit, opts, budget)
	if !errors.Is(err, context.DeadlineExceeded) || progress.Complete || saved.After != "a" || writes != 1 {
		t.Fatal(progress, saved, writes, err)
	}
	first = false
	progress, err = ScanCheckpointed(context.Background(), access, source, emit, opts, budget)
	if err != nil || !progress.Complete || saved.After != "" || saved.Version != 2 || writes != 2 || len(calls) != 3 || calls[2] != "b" {
		t.Fatal(progress, saved, calls, err)
	}
}
func TestCheckpointedScanDoesNotCommitAfterParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writes := 0
	access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error { writes++; return nil }}
	_, err := ScanCheckpointed(ctx, access, func(context.Context, string, int) (CursorPage[string], error) {
		return CursorPage[string]{Items: []CursorItem[string]{{"a", "a"}, {"b", "b"}}}, nil
	}, func(context.Context, string) error { cancel(); return nil }, ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second})
	if !errors.Is(err, context.Canceled) || writes != 0 {
		t.Fatal("cancelled parent saved progress", err, writes)
	}
}
func TestCheckpointedScanPreservesCommitConflict(t *testing.T) {
	conflict := errors.New("stale checkpoint")
	access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil }, Save: func(context.Context, int64, string) error { return conflict }}
	progress, err := ScanCheckpointed(context.Background(), access, func(context.Context, string, int) (CursorPage[int], error) {
		return CursorPage[int]{Items: []CursorItem[int]{{"a", 1}}, More: true}, nil
	}, func(context.Context, int) error { return nil }, ScanOptions{PageSize: 1, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second})
	if !errors.Is(err, conflict) || progress.Complete {
		t.Fatal(progress, err)
	}
}

// Every emitted cursor must also be a valid stored checkpoint. Reject the
// complete page before effects, even when the invalid cursor follows a valid one.
func TestCheckpointedScanRejectsUnstorableCursorBeforeEffects(t *testing.T) {
	for _, cursor := range []string{"\x00", "prefix\x00suffix"} {
		t.Run(fmt.Sprintf("%q", cursor), func(t *testing.T) {
			effects, writes := 0, 0
			access := CheckpointAccess{
				Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil },
				Save: func(context.Context, int64, string) error { writes++; return nil },
			}
			progress, err := ScanCheckpointed(context.Background(), access,
				func(context.Context, string, int) (CursorPage[int], error) {
					return CursorPage[int]{Items: []CursorItem[int]{{Cursor: "valid", Value: 1}, {Cursor: cursor, Value: 2}}, More: true}, nil
				},
				func(context.Context, int) error { effects++; return nil },
				ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second},
				ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second})
			if !errors.Is(err, ErrScanContract) || effects != 0 || writes != 0 || progress.After != "" || progress.Complete {
				t.Fatalf("unstorable cursor reached work or storage: effects=%d writes=%d progress=%+v error=%v", effects, writes, progress, err)
			}
		})
	}
}

func TestScanRejectsUnstorableInitialCursor(t *testing.T) {
	calls := 0
	_, err := ScanFrom(context.Background(), "prefix\x00suffix",
		func(context.Context, string, int) (CursorPage[int], error) { calls++; return CursorPage[int]{}, nil },
		func(int) error { calls++; return nil }, ScanOptions{PageSize: 1, MaxPages: 1, PageTimeout: time.Second})
	if !errors.Is(err, ErrScanContract) || calls != 0 {
		t.Fatalf("invalid initial cursor reached a callback: calls=%d error=%v", calls, err)
	}
}
