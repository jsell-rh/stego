package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func scanOptions() ScanOptions {
	return ScanOptions{PageSize: 2, MaxPages: 3, PageTimeout: time.Second}
}

func streamOptions() StreamScanOptions {
	return StreamScanOptions{MaxItems: 2, OpenTimeout: time.Second, ReceiveTimeout: time.Second}
}

func sweepOptions() SweepOptions {
	return SweepOptions{Workers: 2, PageSize: 10, MaxPagesPerCycle: 100, PassTimeout: 20 * time.Millisecond, Interval: time.Millisecond, Terminal: func(err error) bool { return errors.Is(err, denied) }}
}

// numbered is a source of the values 1 to count, size items to a page, with the decimal value as the cursor.
func numbered(count int) CursorSource[int] {
	return func(_ context.Context, after string, limit int) (CursorPage[int], error) {
		start := 0
		if after != "" {
			if _, err := fmt.Sscan(after, &start); err != nil {
				return CursorPage[int]{}, err
			}
		}
		page := CursorPage[int]{}
		for i := start + 1; i <= count && len(page.Items) < limit; i++ {
			page.Items = append(page.Items, CursorItem[int]{Cursor: fmt.Sprint(i), Value: i})
		}
		page.More = start+len(page.Items) < count
		return page, nil
	}
}

func TestScanResume(t *testing.T) {
	t.Run("ST-SUP-RP02-LC05-T01 A partial page resumes after the last successful item", func(t *testing.T) {
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
	})

	t.Run("ST-SUP-RP02-LC05-T02 An invalid starting cursor reaches no source", func(t *testing.T) {
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
		if !errors.Is(err, ErrScanContract) || progress.After != "saved" || progress.Complete || calls != 1 {
			t.Fatal(progress, err, calls)
		}
	})

	t.Run("ST-SUP-RP02-LC05-T03 A page budget returns progress for the next pass", func(t *testing.T) {
		source := func(_ context.Context, after string, _ int) (CursorPage[string], error) {
			next := after + "x"
			return CursorPage[string]{Items: []CursorItem[string]{{Cursor: next, Value: next}}, More: len(next) < 7}, nil
		}
		options := scanOptions()
		options.MaxPages = 2
		var progress ScanProgress
		var values []string
		passes := 0
		for !progress.Complete {
			if passes == 10 {
				t.Fatal("scan did not finish")
			}
			passes++
			var err error
			progress, err = ScanFrom(context.Background(), progress.After, source, func(v string) error { values = append(values, v); return nil }, options)
			if err != nil {
				t.Fatal(err)
			}
		}
		if passes != 4 || len(values) != 7 || progress.After != "xxxxxxx" {
			t.Fatal(passes, values, progress)
		}
	})
}

func TestScanStream(t *testing.T) {
	t.Run("ST-SUP-RP02-LC06-T01 An open or receive timeout cancels and joins the callback", func(t *testing.T) {
		for _, phase := range []string{"open", "receive"} {
			t.Run(phase, func(t *testing.T) {
				parent, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				timedOut := make(chan struct{})
				release := make(chan struct{})
				done := make(chan error, 1)
				wait := func(ctx context.Context) {
					<-ctx.Done()
					close(timedOut)
					select {
					case <-release:
					case <-parent.Done():
					}
				}
				options := streamOptions()
				options.OpenTimeout = 20 * time.Millisecond
				options.ReceiveTimeout = 20 * time.Millisecond
				go func() {
					done <- ScanStream(parent, func(ctx context.Context) (func() (int, error), error) {
						if phase == "open" {
							wait(ctx)
							return nil, ctx.Err()
						}
						return func() (int, error) { wait(ctx); return 1, nil }, nil
					}, func(int) error { t.Error("late value was emitted"); return nil }, options)
				}()
				select {
				case <-timedOut:
				case <-parent.Done():
					t.Fatal("callback was not cancelled")
				}
				select {
				case err := <-done:
					t.Fatal("callback was detached", err)
				case <-time.After(20 * time.Millisecond):
				}
				close(release)
				select {
				case err := <-done:
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("timeout classification", err)
					}
				case <-parent.Done():
					t.Fatal("callback did not join")
				}
			})
		}
	})

	t.Run("ST-SUP-RP02-LC06-T02 The item limit stops the stream and closes it", func(t *testing.T) {
		for _, size := range []int{0, 2, 3} {
			var stream context.Context
			calls := 0
			values := []int{}
			err := ScanStream(context.Background(), func(ctx context.Context) (func() (int, error), error) {
				stream = ctx
				return func() (int, error) {
					calls++
					if calls > size {
						return 0, io.EOF
					}
					return calls, nil
				}, nil
			}, func(value int) error { values = append(values, value); return nil }, streamOptions())
			if size <= 2 && err != nil || size > 2 && !errors.Is(err, ErrScanContract) {
				t.Fatal("stream bound", size, err)
			}
			expected := []int{}
			if size > 0 {
				expected = []int{1, 2}
			}
			if !reflect.DeepEqual(values, expected) || stream.Err() != context.Canceled {
				t.Fatal("stream output or closure", size, values, stream.Err())
			}
			if calls != min(size+1, 3) {
				t.Fatal("extra receive", calls)
			}
		}
	})
}

func TestScanSweep(t *testing.T) {
	stream := func(page func(context.Context, string, int) (SweepPage[int], error)) []SweepGroup[int] {
		return []SweepGroup[int]{{Name: "records", Streams: []SweepStream[int]{{Name: "live", Page: page}}}}
	}

	t.Run("ST-SUP-RP02-LC16-T01 A sweep validates every definition before it starts telemetry", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		ctx := stoppedTelemetry(t, telemetry)
		var reads atomic.Int32
		page := func(context.Context, string, int) (SweepPage[int], error) { reads.Add(1); return SweepPage[int]{}, nil }
		groups := []SweepGroup[int]{
			{Name: "first", Streams: []SweepStream[int]{{Name: "live", Page: page}}},
			{Name: "second", Streams: []SweepStream[int]{{Name: "live", Page: page}}},
			{Name: "third", Streams: []SweepStream[int]{{Name: "live"}}},
		}
		err := RunSweep(ctx, groups, func(context.Context, int) error { return nil }, sweepOptions())
		if !errors.Is(err, ErrSweepContract) || !strings.Contains(err.Error(), "third") {
			t.Fatal("the error does not name the third definition", err)
		}
		if providers.starts.Load() != 1 || reads.Load() != 0 {
			t.Fatal("an invalid sweep started a provider or read a source", providers.starts.Load(), reads.Load())
		}
	})

	t.Run("ST-SUP-RP02-LC16-T02 A sweep with a cancelled context starts no provider", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		ctx, cancel := context.WithCancel(stoppedTelemetry(t, telemetry))
		cancel()
		err := RunSweep(ctx, stream(func(context.Context, string, int) (SweepPage[int], error) { return SweepPage[int]{}, nil }), func(context.Context, int) error { return nil }, sweepOptions())
		if err != nil || providers.starts.Load() != 1 {
			t.Fatal("a cancelled sweep started a provider", err, providers.starts.Load())
		}
	})

	t.Run("ST-SUP-RP02-LC16-T03 A sweep registers no keyed queue", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		running := int64(-1)
		_, err := Monitor(context.Background(), "", telemetry, func(ctx context.Context, _ *Metrics) (struct{}, error) {
			return struct{}{}, RunSweep(ctx, stream(numberedSweep(1)), func(context.Context, int) error {
				running = providers.gauge(t, "stego.controller.queue.running")
				return denied
			}, sweepOptions())
		})
		if !errors.Is(err, denied) || running != 0 {
			t.Fatal("the sweep registered a queue", err, running)
		}
	})

	t.Run("ST-SUP-RP02-LC16-T04 A pass deadline counts as a retry only while the run is active and the error is not terminal", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			retry bool
		}{{"deadline", true}, {"cancelled", false}, {"terminal", false}} {
			t.Run(c.name, func(t *testing.T) {
				providers, telemetry := newTestTelemetry(t)
				parent, release, err := telemetry.Start(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				ctx, cancel := context.WithCancel(parent)
				defer cancel()
				calls := 0
				_ = RunSweep(ctx, stream(numberedSweep(1)), func(work context.Context, _ int) error {
					calls++
					switch {
					case calls > 1:
						cancel()
						return nil
					case c.name == "terminal":
						return denied
					case c.name == "cancelled":
						cancel()
					}
					<-work.Done()
					return work.Err()
				}, sweepOptions())
				records := providers.completions("reconcile")
				if len(records) == 0 || records[0].retry != c.retry {
					t.Fatal("the first pass has the wrong retry flag", records)
				}
			})
		}
	})

	t.Run("ST-SUP-RP02-LC16-T05 A page callback that returns no error after its deadline is a timeout, not a source error", func(t *testing.T) {
		providers, telemetry := newTestTelemetry(t)
		parent, release, err := telemetry.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		options := sweepOptions()
		calls, classified := 0, 0
		options.Terminal = func(error) bool { classified++; return true }
		ctx, cancel := context.WithTimeout(parent, time.Second)
		defer cancel()
		err = RunSweep(ctx, stream(func(ctx context.Context, _ string, _ int) (SweepPage[int], error) {
			calls++
			if calls == 1 {
				<-ctx.Done()
				time.Sleep(time.Millisecond)
				return SweepPage[int]{}, nil
			}
			return SweepPage[int]{}, denied
		}), func(context.Context, int) error { t.Error("empty page started an action"); return nil }, options)
		if !errors.Is(err, denied) || calls != 2 || classified != 1 {
			t.Fatal("the late page became a source error for the terminal policy", err, calls, classified)
		}
		scans := providers.completions("scan")
		if len(scans) == 0 || scans[0].outcome != "timeout" {
			t.Fatal("the late page was not recorded as a timeout", scans)
		}
	})

	t.Run("ST-SUP-RP02-LC16-T06 The sweep joins its workers before the next page and before close", func(t *testing.T) {
		options := sweepOptions()
		options.Workers = 4
		options.PassTimeout = time.Second
		var running, reads, readsWhileRunning atomic.Int32
		page := func(context.Context, string, int) (SweepPage[int], error) {
			reads.Add(1)
			if running.Load() != 0 {
				readsWhileRunning.Add(1)
			}
			page := SweepPage[int]{More: true}
			for i := 0; i < 10; i++ {
				page.Items = append(page.Items, SweepItem[int]{Cursor: fmt.Sprint(reads.Load()*100 + int32(i)), Value: i})
			}
			return page, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var started, stopped atomic.Int32
		err := RunSweep(ctx, stream(page), func(ctx context.Context, value int) error {
			running.Add(1)
			defer running.Add(-1)
			if started.Add(1) == 14 {
				cancel()
				<-ctx.Done()
				time.Sleep(20 * time.Millisecond)
			} else {
				time.Sleep(2 * time.Millisecond)
			}
			stopped.Add(1)
			return nil
		}, options)
		if err != nil || stopped.Load() != started.Load() || running.Load() != 0 {
			t.Fatal("the sweep returned before its workers", err, started.Load(), stopped.Load())
		}
		if reads.Load() < 2 || readsWhileRunning.Load() != 0 {
			t.Fatal("the sweep read a page while a worker ran", reads.Load(), readsWhileRunning.Load())
		}
	})
}

// numberedSweep is a sweep stream page function of the values 1 to count.
func numberedSweep(count int) func(context.Context, string, int) (SweepPage[int], error) {
	source := numbered(count)
	return func(ctx context.Context, after string, limit int) (SweepPage[int], error) {
		return source(ctx, after, limit)
	}
}

func TestScanProgress(t *testing.T) {
	t.Run("ST-SUP-RP02-LC17-T01 A resumed scan returns the completed prefix with the error", func(t *testing.T) {
		failure := errors.New("emitter failed")
		progress, err := ScanFrom(context.Background(), "", numbered(5), func(v int) error {
			if v == 4 {
				return failure
			}
			return nil
		}, scanOptions())
		if !errors.Is(err, failure) || progress.After != "3" || progress.Complete {
			t.Fatal("the emitter error lost its prefix", progress, err)
		}
		source := numbered(5)
		broken := errors.New("source failed")
		progress, err = ScanFrom(context.Background(), "", func(ctx context.Context, after string, limit int) (CursorPage[int], error) {
			if after != "" {
				return CursorPage[int]{}, broken
			}
			return source(ctx, after, limit)
		}, func(int) error { return nil }, scanOptions())
		if !errors.Is(err, broken) || progress.After != "2" || progress.Complete {
			t.Fatal("the source error lost the first page", progress, err)
		}
	})

	t.Run("ST-SUP-RP02-LC17-T02 A page above the size bound is refused", func(t *testing.T) {
		options := scanOptions()
		emitted := 0
		_, err := ScanFrom(context.Background(), "", func(context.Context, string, int) (CursorPage[int], error) {
			page := CursorPage[int]{}
			for i := 0; i <= options.PageSize; i++ {
				page.Items = append(page.Items, CursorItem[int]{Cursor: fmt.Sprint(i), Value: i})
			}
			return page, nil
		}, func(int) error { emitted++; return nil }, options)
		if !errors.Is(err, ErrScanContract) || emitted != 0 {
			t.Fatal("an oversized page was accepted", err, emitted)
		}
	})

	t.Run("ST-SUP-RP02-LC17-T03 Each page read is bounded by the operation timeout", func(t *testing.T) {
		options := scanOptions()
		options.PageTimeout = 30 * time.Millisecond
		var page context.Context
		started := time.Now()
		_, err := ScanFrom(context.Background(), "", func(ctx context.Context, _ string, _ int) (CursorPage[int], error) {
			page = ctx
			<-ctx.Done()
			return CursorPage[int]{}, ctx.Err()
		}, func(int) error { return nil }, options)
		elapsed := time.Since(started)
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(page.Err(), context.DeadlineExceeded) {
			t.Fatal("the page read was not cancelled by its deadline", err)
		}
		if elapsed < options.PageTimeout || elapsed > options.PageTimeout+500*time.Millisecond {
			t.Fatal("the page read did not end at its timeout", elapsed)
		}
	})

	t.Run("ST-SUP-RP02-LC17-T04 A scan from the beginning returns the contract error when its page budget cannot cover the source", func(t *testing.T) {
		options := scanOptions()
		options.MaxPages = 2
		emitted := 0
		err := Scan(context.Background(), numbered(6), func(int) error { emitted++; return nil }, options)
		if !errors.Is(err, ErrScanContract) {
			t.Fatal("a short page budget did not give the scan contract error", err)
		}
		if err := Scan(context.Background(), numbered(4), func(int) error { return nil }, options); err != nil {
			t.Fatal("a budget that covers the source failed", err)
		}
	})

	t.Run("ST-SUP-RP02-LC17-T05 A scan starts no background work", func(t *testing.T) {
		// Count only the goroutines that run code of the package's own files, so other tests cannot change it.
		own := func() int {
			buffer := make([]byte, 1<<20)
			stacks := string(buffer[:runtime.Stack(buffer, true)])
			count := 0
			for _, stack := range strings.Split(stacks, "\n\n") {
				for _, file := range []string{"queue.go:", "scan.go:", "retry.go:", "telemetry.go:"} {
					if strings.Contains(stack, "/controller/"+file) {
						count++
						break
					}
				}
			}
			return count
		}
		settle := func() int {
			for range 20 {
				if own() == 0 {
					return 0
				}
				time.Sleep(time.Millisecond)
			}
			return own()
		}
		_, err := ScanFrom(context.Background(), "", numbered(5), func(int) error { return nil }, scanOptions())
		if err != nil {
			t.Fatal(err)
		}
		if after := settle(); after != 0 {
			t.Fatalf("the scan left %d goroutines", after)
		}
	})
}
