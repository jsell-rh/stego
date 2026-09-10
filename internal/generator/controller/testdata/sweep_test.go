package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sweepOptions() SweepOptions {
	return SweepOptions{Workers: 2, PageSize: 10, MaxPagesPerCycle: 100, PassTimeout: 20 * time.Millisecond, Interval: time.Millisecond, Terminal: func(err error) bool { return errors.Is(err, denied) }}
}
func TestSweepResumesShortPageAfterDeadline(t *testing.T) {
	var mu sync.Mutex
	cursors := []string{}
	completed := map[int]bool{}
	group := SweepGroup[int]{Name: "records", Streams: []SweepStream[int]{{Name: "live", Page: func(ctx context.Context, after string, limit int) (SweepPage[int], error) {
		mu.Lock()
		cursors = append(cursors, after)
		mu.Unlock()
		start := 0
		if after != "" {
			if _, err := fmt.Sscan(after, &start); err != nil {
				return SweepPage[int]{}, err
			}
		}
		page := SweepPage[int]{}
		for i := start + 1; i <= 5; i++ {
			page.Items = append(page.Items, SweepItem[int]{Cursor: fmt.Sprint(i), Value: i})
		}
		return page, nil
	}}}}
	var active, maximum atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := RunSweep(ctx, []SweepGroup[int]{group}, func(ctx context.Context, item int) error {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		if item <= 2 {
			<-ctx.Done()
			return ctx.Err()
		}
		mu.Lock()
		completed[item] = true
		all := len(completed) == 3
		mu.Unlock()
		if all {
			return denied
		}
		return nil
	}, sweepOptions())
	if !errors.Is(err, denied) || len(completed) != 3 || maximum.Load() > 2 || active.Load() != 0 {
		t.Fatal("partial page did not progress or workers remained active", err)
	}
	if len(cursors) < 2 || cursors[1] == "" {
		t.Fatal("short page reset a partial cursor", cursors)
	}
}
func TestSweepValidatesWholePageBeforeEffects(t *testing.T) {
	for _, page := range []SweepPage[int]{
		{Items: []SweepItem[int]{{Cursor: "a"}, {Cursor: "a"}}},
		{Items: []SweepItem[int]{{Cursor: "a"}, {Cursor: ""}}},
		{Items: []SweepItem[int]{{Cursor: strings.Repeat("x", 1025)}}},
		{Items: []SweepItem[int]{{Cursor: string([]byte{255})}}},
		{More: true},
		{Items: make([]SweepItem[int], 11)},
	} {
		groups := []SweepGroup[int]{{Name: "records", Streams: []SweepStream[int]{{Name: "live", Page: func(context.Context, string, int) (SweepPage[int], error) { return page, nil }}}}}
		if err := RunSweep(context.Background(), groups, func(context.Context, int) error { t.Error("invalid page caused effects"); return nil }, sweepOptions()); !errors.Is(err, ErrSweepContract) {
			t.Fatal("invalid page accepted", err)
		}
	}
}
func TestSweepRotatesGroupsAndOrdersStreams(t *testing.T) {
	order := []string{}
	source := func(name string) func(context.Context, string, int) (SweepPage[string], error) {
		return func(context.Context, string, int) (SweepPage[string], error) {
			order = append(order, name)
			return SweepPage[string]{Items: []SweepItem[string]{{Cursor: name, Value: name}}}, nil
		}
	}
	groups := []SweepGroup[string]{{Name: "one", Streams: []SweepStream[string]{{Name: "live", Page: source("one-live")}, {Name: "history", Page: source("one-history")}}}, {Name: "two", Streams: []SweepStream[string]{{Name: "live", Page: source("two-live")}}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := RunSweep(ctx, groups, func(_ context.Context, item string) error {
		if item == "two-live" {
			return denied
		}
		return nil
	}, sweepOptions())
	if !errors.Is(err, denied) || strings.Join(order, ",") != "one-live,one-history,two-live" {
		t.Fatal("group fairness or stream order changed", order, err)
	}
}
func TestSweepRetriesFailedWorkOnNextCycle(t *testing.T) {
	attempts := 0
	group := SweepGroup[string]{Name: "records", Streams: []SweepStream[string]{{Name: "live", Page: func(_ context.Context, after string, _ int) (SweepPage[string], error) {
		if after != "" {
			t.Error("finished cycle did not reset cursor")
		}
		return SweepPage[string]{Items: []SweepItem[string]{{Cursor: "x", Value: "x"}}}, nil
	}}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := RunSweep(ctx, []SweepGroup[string]{group}, func(context.Context, string) error {
		attempts++
		if attempts == 1 {
			return errors.New("temporary")
		}
		return denied
	}, sweepOptions())
	if !errors.Is(err, denied) || attempts != 2 {
		t.Fatal("failed retained work did not receive another turn", err)
	}
}
func TestSweepRejectsRepeatedCursorAndExcessPages(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(fmt.Sprint(repeat), func(t *testing.T) {
			options := sweepOptions()
			options.MaxPagesPerCycle = 2
			calls, actions := 0, 0
			group := SweepGroup[string]{Name: "records", Streams: []SweepStream[string]{{Name: "live", Page: func(_ context.Context, after string, _ int) (SweepPage[string], error) {
				calls++
				cursor := fmt.Sprint(calls)
				if repeat && after != "" {
					cursor = after
				}
				return SweepPage[string]{Items: []SweepItem[string]{{Cursor: cursor}}, More: true}, nil
			}}}}
			err := RunSweep(context.Background(), []SweepGroup[string]{group}, func(context.Context, string) error { actions++; return nil }, options)
			if !errors.Is(err, ErrSweepContract) || calls != 2 || actions > 2 {
				t.Fatal("unbounded or repeated cursor", err, calls, actions)
			}
		})
	}
}
func TestSweepCancellationJoinsWorkers(t *testing.T) {
	options := sweepOptions()
	options.Workers = 4
	options.PassTimeout = time.Second
	group := SweepGroup[int]{Name: "records", Streams: []SweepStream[int]{{Name: "live", Page: func(context.Context, string, int) (SweepPage[int], error) {
		page := SweepPage[int]{}
		for i := 0; i < 10; i++ {
			page.Items = append(page.Items, SweepItem[int]{Cursor: fmt.Sprint(i), Value: i})
		}
		return page, nil
	}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var started, stopped atomic.Int32
	err := RunSweep(ctx, []SweepGroup[int]{group}, func(ctx context.Context, _ int) error {
		if started.Add(1) == 4 {
			cancel()
		}
		<-ctx.Done()
		stopped.Add(1)
		return ctx.Err()
	}, options)
	if err != nil || started.Load() != 4 || stopped.Load() != 4 {
		t.Fatal("canceled sweep leaked or started extra workers", err, started.Load(), stopped.Load())
	}
}
func TestInvalidSweepDefinitionsHaveNoEffects(t *testing.T) {
	source := func(context.Context, string, int) (SweepPage[int], error) {
		t.Error("invalid definition called source")
		return SweepPage[int]{}, nil
	}
	group := SweepGroup[int]{Name: "records", Streams: []SweepStream[int]{{Name: "live", Page: source}}}
	for _, groups := range [][]SweepGroup[int]{nil, {group, group}, {{Name: "invalid name", Streams: group.Streams}}, {{Name: "records"}}, {{Name: "records", Streams: []SweepStream[int]{{Name: "live"}}}}} {
		if err := RunSweep(context.Background(), groups, func(context.Context, int) error { return nil }, sweepOptions()); !errors.Is(err, ErrSweepContract) {
			t.Fatal("invalid definition accepted", err)
		}
	}
	for _, change := range []func(*SweepOptions){func(o *SweepOptions) { o.Workers = 0 }, func(o *SweepOptions) { o.Workers = 65 }, func(o *SweepOptions) { o.PageSize = 1001 }, func(o *SweepOptions) { o.MaxPagesPerCycle = 0 }, func(o *SweepOptions) { o.PassTimeout = 0 }, func(o *SweepOptions) { o.Interval = 0 }, func(o *SweepOptions) { o.Terminal = nil }} {
		options := sweepOptions()
		change(&options)
		if err := RunSweep(context.Background(), []SweepGroup[int]{group}, func(context.Context, int) error { return nil }, options); !errors.Is(err, ErrSweepContract) {
			t.Fatal("invalid options accepted", err)
		}
	}
}

func TestSweepSlowStreamCannotStarveHistory(t *testing.T) {
	source := func(value string) func(context.Context, string, int) (SweepPage[string], error) {
		return func(context.Context, string, int) (SweepPage[string], error) {
			return SweepPage[string]{Items: []SweepItem[string]{{Cursor: value, Value: value}}}, nil
		}
	}
	groups := []SweepGroup[string]{{Name: "records", Streams: []SweepStream[string]{{Name: "live", Page: source("live")}, {Name: "history", Page: source("history")}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := RunSweep(ctx, groups, func(ctx context.Context, value string) error {
		if value == "live" {
			<-ctx.Done()
			return ctx.Err()
		}
		return denied
	}, sweepOptions())
	if !errors.Is(err, denied) {
		t.Fatal("slow first stream starved retained history", err)
	}
}

func BenchmarkSweepDispatch(b *testing.B) {
	page := SweepPage[int]{}
	for i := 0; i < 100; i++ {
		page.Items = append(page.Items, SweepItem[int]{Cursor: fmt.Sprint(i), Value: i})
	}
	for _, workers := range []int{1, 8, 32} {
		b.Run(fmt.Sprint(workers), func(b *testing.B) {
			options := sweepOptions()
			options.Workers = workers
			action := func(context.Context, int) error { return nil }
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				sweepPage(ctx, ctx, cancel, page, action, options)
				cancel()
			}
		})
	}
}

func TestSweepDeadlineAfterSuccessfulPagePreservesTerminalPolicy(t *testing.T) {
	options := sweepOptions()
	calls, classified := 0, 0
	options.Terminal = func(error) bool { classified++; return true }
	groups := []SweepGroup[int]{{Name: "records", Streams: []SweepStream[int]{{Name: "live", Page: func(ctx context.Context, _ string, _ int) (SweepPage[int], error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return SweepPage[int]{}, nil
		}
		return SweepPage[int]{}, denied
	}}}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := RunSweep(ctx, groups, func(context.Context, int) error { t.Error("empty page started an action"); return nil }, options)
	if !errors.Is(err, denied) || calls != 2 || classified != 1 {
		t.Fatal("telemetry changed the source terminal policy", err, calls, classified)
	}
}
