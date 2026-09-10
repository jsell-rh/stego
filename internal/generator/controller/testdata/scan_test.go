package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func scanOptions() ScanOptions {
	return ScanOptions{PageSize: 2, MaxPages: 3, PageTimeout: time.Second}
}

func TestScanChecksWholePageBeforeDispatch(t *testing.T) {
	for _, page := range []CursorPage[int]{
		{Items: []CursorItem[int]{{Cursor: "ok"}, {Cursor: ""}}},
		{Items: []CursorItem[int]{{Cursor: "ok"}, {Cursor: "ok"}}},
		{Items: []CursorItem[int]{{Cursor: "ok"}, {Cursor: "\xff"}}},
		{Items: []CursorItem[int]{{Cursor: "ok"}, {Cursor: strings.Repeat("x", 1025)}}},
		{Items: []CursorItem[int]{{Cursor: "a"}, {Cursor: "b"}, {Cursor: "c"}}},
		{More: true},
	} {
		calls := 0
		err := Scan(context.Background(), func(context.Context, string, int) (CursorPage[int], error) { return page, nil }, func(int) error { calls++; return nil }, scanOptions())
		if !errors.Is(err, ErrScanContract) || calls != 0 {
			t.Fatalf("invalid page caused effects: %d, %v", calls, err)
		}
	}
}

func TestScanFollowsOpaqueCursorsAndRestarts(t *testing.T) {
	// Cursor ordering belongs to the source, including its database collation.
	for range 2 {
		var cursors []string
		var values []int
		source := func(_ context.Context, after string, limit int) (CursorPage[int], error) {
			if limit != 2 {
				t.Fatal("wrong page bound", limit)
			}
			cursors = append(cursors, after)
			switch after {
			case "":
				return CursorPage[int]{Items: []CursorItem[int]{{Cursor: "z", Value: 1}}, More: true}, nil
			case "z":
				return CursorPage[int]{Items: []CursorItem[int]{{Cursor: "A", Value: 2}, {Cursor: "a", Value: 3}}}, nil
			default:
				t.Fatal("unexpected cursor", after)
				return CursorPage[int]{}, nil
			}
		}
		if err := Scan(context.Background(), source, func(v int) error { values = append(values, v); return nil }, scanOptions()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cursors, []string{"", "z"}) || !reflect.DeepEqual(values, []int{1, 2, 3}) {
			t.Fatal(cursors, values)
		}
	}
}

func TestScanBoundsFaultyContinuation(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		calls, emitted := 0, 0
		err := Scan(context.Background(), func(_ context.Context, after string, _ int) (CursorPage[int], error) {
			calls++
			cursor := fmt.Sprint(calls)
			if repeat && after != "" {
				cursor = after
			}
			return CursorPage[int]{Items: []CursorItem[int]{{Cursor: cursor}}, More: true}, nil
		}, func(int) error { emitted++; return nil }, scanOptions())
		if !errors.Is(err, ErrScanContract) {
			t.Fatal(err)
		}
		if repeat && (calls != 2 || emitted != 1) || !repeat && (calls != 3 || emitted != 3) {
			t.Fatal(calls, emitted)
		}
	}
}

func TestScanStopsOnFailureAndCancellation(t *testing.T) {
	denied := errors.New("denied")
	for _, stage := range []string{"source", "emit", "cancel", "deadline"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls, emitted := 0, 0
			options := scanOptions()
			options.PageTimeout = time.Millisecond
			err := Scan(ctx, func(operation context.Context, _ string, _ int) (CursorPage[int], error) {
				calls++
				if stage == "source" {
					return CursorPage[int]{}, denied
				}
				if stage == "deadline" {
					<-operation.Done()
				}
				return CursorPage[int]{Items: []CursorItem[int]{{Cursor: "a"}, {Cursor: "b"}}, More: true}, nil
			}, func(int) error {
				emitted++
				if stage == "cancel" {
					cancel()
					return nil
				}
				return denied
			}, options)
			want := denied
			if stage == "cancel" {
				want = context.Canceled
			}
			if stage == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || calls != 1 {
				t.Fatal(err, calls)
			}
			if (stage == "source" || stage == "deadline") && emitted != 0 || (stage == "emit" || stage == "cancel") && emitted != 1 {
				t.Fatal("wrong dispatch count", emitted)
			}
		})
	}
}

func TestScanRejectsInvalidOptionsBeforeCalls(t *testing.T) {
	source := func(context.Context, string, int) (CursorPage[int], error) {
		t.Fatal("invalid scan called source")
		return CursorPage[int]{}, nil
	}
	emit := func(int) error { t.Fatal("invalid scan emitted work"); return nil }
	for _, options := range []ScanOptions{
		{}, {PageSize: 1001, MaxPages: 1, PageTimeout: time.Second},
		{PageSize: 1, MaxPages: 1000001, PageTimeout: time.Second},
		{PageSize: 1, MaxPages: 1, PageTimeout: time.Minute + 1},
		{PageSize: 1, MaxPages: 1, PageTimeout: time.Millisecond - 1},
	} {
		if err := Scan(context.Background(), source, emit, options); !errors.Is(err, ErrScanContract) {
			t.Fatal(err)
		}
	}
	if err := Scan[int](nil, source, emit, scanOptions()); !errors.Is(err, ErrScanContract) {
		t.Fatal(err)
	}
	if err := Scan[int](context.Background(), nil, emit, scanOptions()); !errors.Is(err, ErrScanContract) {
		t.Fatal(err)
	}
	if err := Scan(context.Background(), source, nil, scanOptions()); !errors.Is(err, ErrScanContract) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Scan(ctx, source, emit, scanOptions()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func BenchmarkScanPage(b *testing.B) {
	page := CursorPage[int]{}
	for i := range 100 {
		page.Items = append(page.Items, CursorItem[int]{Cursor: fmt.Sprint(i), Value: i})
	}
	source := func(context.Context, string, int) (CursorPage[int], error) { return page, nil }
	options := ScanOptions{PageSize: 100, MaxPages: 1, PageTimeout: time.Second}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := Scan(context.Background(), source, func(int) error { return nil }, options); err != nil {
			b.Fatal(err)
		}
	}
}

func TestScanFromResumesPartialPages(t *testing.T) {
	for _, fail := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		source := func(_ context.Context, after string, _ int) (CursorPage[int], error) {
			// Opaque order deliberately differs from Go string order.
			switch after {
			case "":
				return CursorPage[int]{Items: []CursorItem[int]{{Cursor: "z", Value: 1}, {Cursor: "A", Value: 2}}}, nil
			case "z":
				return CursorPage[int]{Items: []CursorItem[int]{{Cursor: "A", Value: 2}}}, nil
			case "A":
				return CursorPage[int]{}, nil
			}
			t.Fatal("wrong saved cursor", after)
			return CursorPage[int]{}, nil
		}
		progress, err := ScanFrom(ctx, "", source, func(int) error {
			cancel()
			if fail {
				return ctx.Err()
			}
			return nil
		}, scanOptions())
		want := "z"
		if fail {
			want = ""
		}
		if !errors.Is(err, context.Canceled) || progress.After != want || progress.Complete {
			t.Fatal(progress, err)
		}
		var values []int
		progress, err = ScanFrom(context.Background(), progress.After, source, func(v int) error { values = append(values, v); return nil }, scanOptions())
		expected := []int{2}
		if fail {
			expected = []int{1, 2}
		}
		if err != nil || !progress.Complete || progress.After != "A" || !reflect.DeepEqual(values, expected) {
			t.Fatal(progress, values, err)
		}
	}
}

func TestScanFromPageBudgetRetainsProgress(t *testing.T) {
	source := func(_ context.Context, after string, _ int) (CursorPage[string], error) {
		next := after + "x"
		return CursorPage[string]{Items: []CursorItem[string]{{Cursor: next, Value: next}}, More: len(next) < 7}, nil
	}
	options := scanOptions()
	options.MaxPages = 2
	var progress ScanProgress
	var values []string
	for passes := 0; !progress.Complete; passes++ {
		if passes >= 4 {
			t.Fatal("scan did not finish")
		}
		var err error
		progress, err = ScanFrom(context.Background(), progress.After, source, func(v string) error { values = append(values, v); return nil }, options)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(values) != 7 || progress.After != "xxxxxxx" {
		t.Fatal(values, progress)
	}
}

func TestScanFromRejectsInvalidContinuationBeforeEffects(t *testing.T) {
	calls := 0
	source := func(context.Context, string, int) (CursorPage[int], error) {
		calls++
		return CursorPage[int]{Items: []CursorItem[int]{{Cursor: "z"}, {Cursor: "saved"}}}, nil
	}
	for _, after := range []string{strings.Repeat("x", 1025), "\xff"} {
		_, err := ScanFrom(context.Background(), after, source, func(int) error { t.Fatal("invalid scan caused effects"); return nil }, scanOptions())
		if !errors.Is(err, ErrScanContract) || calls != 0 {
			t.Fatal(err, calls)
		}
	}
	progress, err := ScanFrom(context.Background(), "saved", source, func(int) error { t.Fatal("invalid page caused effects"); return nil }, scanOptions())
	if !errors.Is(err, ErrScanContract) || progress.After != "saved" || progress.Complete {
		t.Fatal(progress, err)
	}
}
