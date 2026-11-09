package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ErrScanContract reports a scan, stream or cycle that breaks its contract: an invalid option, cursor or page, a
// stream past its item limit, or a page budget that cannot cover the source.
var ErrScanContract = errors.New("controller scan contract is invalid")

// CursorItem is one value of a page with the opaque cursor after it.
type CursorItem[T any] struct {
	Cursor string
	Value  T
}

// CursorPage is one page of a cursor source. More tells that a later page exists.
type CursorPage[T any] struct {
	Items []CursorItem[T]
	More  bool
}

// CursorSource reads the page after a cursor, with at most the given number of items.
type CursorSource[T any] func(context.Context, string, int) (CursorPage[T], error)

// ScanOptions bound a scan: the page size, the page budget of one call and the timeout of each page read.
type ScanOptions struct {
	PageSize    int
	MaxPages    int
	PageTimeout time.Duration
}

func scanError(message string) error { return fmt.Errorf("%w: %s", ErrScanContract, message) }

// Scan reads retained state in bounded pages from the beginning. It validates each whole page before dispatch. A
// page budget that cannot cover the source is the scan contract error, not partial progress (ST-SUP-RP02-LC17).
// Emit must respect the parent context. Failure stops the scan; previous emits are not undone.
func Scan[T any](ctx context.Context, source CursorSource[T], emit func(T) error, options ScanOptions) error {
	progress, err := ScanFrom(ctx, "", source, emit, options)
	if err != nil {
		return err
	}
	if !progress.Complete {
		return scanError("source exceeded its page limit")
	}
	return nil
}

// ScanProgress identifies the last item whose emitter returned success. Complete means the source reported its end.
// It does not prove convergence.
type ScanProgress struct {
	After    string
	Complete bool
}

// ScanFrom resumes after an opaque cursor and reads at most MaxPages pages. It refuses an oversized or non-UTF-8
// cursor before any read. A page budget returns incomplete progress without an error; the caller must arrange
// another pass (ST-SUP-RP02-LC05). Progress is returned with errors and cancellation too: a failed emitter keeps its
// item eligible, and success followed by cancellation advances the cursor (ST-SUP-RP02-LC17). The caller owns
// persistence and retry policy. This function creates no durable acknowledgment and starts no background work.
func ScanFrom[T any](ctx context.Context, after string, source CursorSource[T], emit func(T) error, options ScanOptions) (ScanProgress, error) {
	var progress ScanProgress
	err := controllerHelperWork(ctx, "scan", func(ctx context.Context) error {
		var err error
		progress, err = scanFromUnchecked(ctx, after, source, emit, options)
		return err
	})
	return progress, err
}

func scanFromUnchecked[T any](ctx context.Context, after string, source CursorSource[T], emit func(T) error, options ScanOptions) (ScanProgress, error) {
	if emit == nil {
		return ScanProgress{After: after}, scanError("context, source, and emitter are required")
	}
	return scanFromPages(ctx, after, source, func(ctx context.Context, items []CursorItem[T]) (int, error) {
		for i, item := range items {
			if err := ctx.Err(); err != nil {
				return i, err
			}
			if err := emit(item.Value); err != nil {
				return i, err
			}
		}
		return len(items), nil
	}, options)
}

// scanFromPages keeps page validation and cursor progress common to both action modes. The page action returns
// only its contiguous accepted prefix. The page context ends before the page action runs, so a wait for queue
// admission is not part of the page read (ST-SUP-RP02-LC11).
func scanFromPages[T any](ctx context.Context, after string, source CursorSource[T], emitPage func(context.Context, []CursorItem[T]) (int, error), options ScanOptions) (ScanProgress, error) {
	progress := ScanProgress{After: after}
	if ctx == nil || source == nil || emitPage == nil {
		return progress, scanError("context, source, and emitter are required")
	}
	if !validCursor(after, true) {
		return progress, scanError("initial cursor is invalid")
	}
	if options.PageSize < 1 || options.PageSize > 1000 {
		return progress, scanError("page size must be between 1 and 1000")
	}
	if options.MaxPages < 1 || options.MaxPages > 1000000 {
		return progress, scanError("page count must be between 1 and 1000000")
	}
	if options.PageTimeout < time.Millisecond || options.PageTimeout > time.Minute {
		return progress, scanError("page timeout is outside its limits")
	}
	for n := 0; n < options.MaxPages; n++ {
		if err := ctx.Err(); err != nil {
			return progress, err
		}
		operation, cancel := context.WithTimeout(ctx, options.PageTimeout)
		page, err := source(operation, progress.After, options.PageSize)
		deadlineErr := operation.Err()
		cancel()
		if err != nil {
			return progress, err
		}
		if deadlineErr != nil {
			return progress, deadlineErr
		}
		if err := validateCursorPage(page, progress.After, options.PageSize); err != nil {
			return progress, scanError(err.Error())
		}
		prefix, err := emitPage(ctx, page.Items)
		if prefix < 0 || prefix > len(page.Items) {
			return progress, scanError("page action returned an invalid prefix")
		}
		if prefix > 0 {
			progress.After = page.Items[prefix-1].Cursor
		}
		if err != nil {
			return progress, err
		}
		if prefix != len(page.Items) {
			return progress, scanError("page action stopped without an error")
		}
		if err := ctx.Err(); err != nil {
			return progress, err
		}
		if !page.More {
			progress.Complete = true
			return progress, nil
		}
	}
	return progress, nil
}

// validCursor accepts at most 1,024 bytes of UTF-8 with no NUL; empty only where it names the beginning.
func validCursor(cursor string, empty bool) bool {
	if cursor == "" {
		return empty
	}
	return len(cursor) <= 1024 && utf8.ValidString(cursor) && strings.IndexByte(cursor, 0) < 0
}

func validateCursorPage[T any](page CursorPage[T], after string, limit int) error {
	if len(page.Items) > limit || page.More && len(page.Items) == 0 {
		return errors.New("page size or continuation is invalid")
	}
	seen := make(map[string]bool, len(page.Items))
	for _, item := range page.Items {
		if !validCursor(item.Cursor, false) || item.Cursor == after || seen[item.Cursor] {
			return errors.New("page cursor is invalid or duplicated")
		}
		seen[item.Cursor] = true
	}
	return nil
}

// StreamSource opens a finite stream and returns its receive function.
type StreamSource[T any] func(context.Context) (func() (T, error), error)

// StreamScanOptions bound a stream: the item limit and the separate open and receive timeouts.
type StreamScanOptions struct {
	MaxItems       int
	OpenTimeout    time.Duration
	ReceiveTimeout time.Duration
}

// ScanStream reads a finite stream until io.EOF. Open and receive must honor the supplied stream context and release
// their resources when it is cancelled. The runtime calls them synchronously; it does not detach blocked callbacks,
// and a value that a callback returns after its timeout never reaches emit. The stream is cancelled when the scan
// returns. MaxItems permits one extra receive to establish EOF; an excess value is the scan contract error before
// emit (ST-SUP-RP02-LC06). Emit has no stream idle timer: it may wait for queue capacity and must honor the parent
// context.
func ScanStream[T any](ctx context.Context, source StreamSource[T], emit func(T) error, options StreamScanOptions) error {
	return controllerHelperWork(ctx, "scan", func(ctx context.Context) error {
		return scanStreamUnchecked(ctx, source, emit, options)
	})
}

func scanStreamUnchecked[T any](ctx context.Context, source StreamSource[T], emit func(T) error, options StreamScanOptions) error {
	if ctx == nil || source == nil || emit == nil {
		return scanError("context, stream source, and emitter are required")
	}
	if options.MaxItems < 1 || options.MaxItems > 1000000 {
		return scanError("stream item count must be between 1 and 1000000")
	}
	if options.OpenTimeout < time.Millisecond || options.OpenTimeout > time.Minute || options.ReceiveTimeout < time.Millisecond || options.ReceiveTimeout > time.Minute {
		return scanError("stream timeout is outside its limits")
	}
	stream, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	receive, err := streamCall(ctx, stream, cancel, options.OpenTimeout, func() (func() (T, error), error) { return source(stream) })
	if err != nil {
		return err
	}
	if receive == nil {
		return scanError("stream has no receiver")
	}
	for count := 0; ; count++ {
		value, err := streamCall(ctx, stream, cancel, options.ReceiveTimeout, receive)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if count == options.MaxItems {
			return scanError("stream exceeded its item limit")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(value); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func streamCall[T any](parent, stream context.Context, cancel context.CancelCauseFunc, timeout time.Duration, call func() (T, error)) (T, error) {
	var zero T
	if err := parent.Err(); err != nil {
		return zero, err
	}
	timer := time.AfterFunc(timeout, func() { cancel(context.DeadlineExceeded) })
	value, err := call()
	stopped := timer.Stop()
	if err := parent.Err(); err != nil {
		return zero, err
	}
	// A fired timer can still be entering its callback. Cancel here as well and return without another call, so a
	// late callback cannot cancel a later read.
	if !stopped {
		cancel(context.DeadlineExceeded)
		return zero, context.DeadlineExceeded
	}
	if cause := context.Cause(stream); cause != nil {
		return zero, cause
	}
	return value, err
}

// ErrSweepContract reports a sweep definition, option or page that breaks its contract.
var ErrSweepContract = errors.New("controller sweep contract is invalid")

// SweepItem is one item of a sweep page.
type SweepItem[T any] = CursorItem[T]

// SweepPage is one page of a sweep stream.
type SweepPage[T any] = CursorPage[T]

// SweepStream must return retained, cursor-ordered work after the supplied cursor. Failed actions must remain
// eligible on a later cycle. Cursor progress is not a durable acknowledgment. Empty pages end a cycle.
type SweepStream[T any] struct {
	Name string
	Page func(context.Context, string, int) (SweepPage[T], error)
}

// SweepGroup is a set of streams that share a time budget and initially run in declared order. A pass that exhausts
// its budget leaves the next stream first in line for the next pass. Groups rotate after each pass.
type SweepGroup[T any] struct {
	Name    string
	Streams []SweepStream[T]
}

// SweepIntervalMode selects where the sweep interval applies.
type SweepIntervalMode uint8

const (
	// SweepIntervalAfterGroup waits after each group pass.
	SweepIntervalAfterGroup SweepIntervalMode = iota
	// SweepIntervalAfterRound waits after all groups have had one pass.
	SweepIntervalAfterRound
)

// SweepOptions bound a sweep.
type SweepOptions struct {
	Workers          int
	PageSize         int
	MaxPagesPerCycle int
	PassTimeout      time.Duration
	Interval         time.Duration
	// IntervalMode selects where Interval applies. Empty groups still count as passes.
	IntervalMode SweepIntervalMode
	// Terminal can run on several workers. It must be safe for concurrent calls.
	Terminal func(error) bool
	// Observe runs after the workers stop. It must return promptly. Error details and cursors can contain private
	// data; the runtime does not log them.
	Observe func(SweepEvent)
}

// SweepEvent is the result of one stream page of a pass.
type SweepEvent struct {
	Group, Stream   string
	Started, Failed int
	Err             error
}

type sweepCursor struct {
	after string
	pages int
}

func sweepName(name string) bool {
	if len(name) < 1 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func sweepError(message string) error { return fmt.Errorf("%w: %s", ErrSweepContract, message) }

func (o SweepOptions) validate() error {
	if o.Workers < 1 || o.Workers > 64 {
		return sweepError("worker count must be between 1 and 64")
	}
	if o.PageSize < 1 || o.PageSize > 1000 {
		return sweepError("page size must be between 1 and 1000")
	}
	if o.MaxPagesPerCycle < 1 || o.MaxPagesPerCycle > 1000000 {
		return sweepError("page count must be between 1 and 1000000")
	}
	if o.PassTimeout < time.Millisecond || o.PassTimeout > time.Minute {
		return sweepError("pass timeout is outside its limits")
	}
	if o.Interval < time.Millisecond || o.Interval > time.Hour {
		return sweepError("pass interval is outside its limits")
	}
	if o.IntervalMode != SweepIntervalAfterGroup && o.IntervalMode != SweepIntervalAfterRound {
		return sweepError("interval mode is invalid")
	}
	if o.Terminal == nil {
		return sweepError("terminal error policy is required")
	}
	return nil
}

// RunSweep owns cursors, bounded worker pools, page checks, deadlines, and fair group rotation. It validates every
// definition before it starts telemetry, and starts no provider when its context is already cancelled; it registers
// no keyed queue (ST-SUP-RP02-LC16). It validates the whole page before any action starts. Cursor progress covers
// only the contiguous prefix whose actions started. Workers always stop before the next page or group. Actions must
// tolerate repetition.
func RunSweep[T any](ctx context.Context, groups []SweepGroup[T], reconcile func(context.Context, T) error, options SweepOptions) error {
	if ctx == nil || reconcile == nil {
		return sweepError("context and action are required")
	}
	if err := options.validate(); err != nil {
		return err
	}
	if len(groups) < 1 || len(groups) > 64 {
		return sweepError("group count must be between 1 and 64")
	}
	definitions := make([]SweepGroup[T], len(groups))
	cursors := make([][]sweepCursor, len(groups))
	nextStreams := make([]int, len(groups))
	names := map[string]bool{}
	total := 0
	for i, group := range groups {
		if !sweepName(group.Name) || names[group.Name] {
			return sweepError(fmt.Sprintf("group %d: name is invalid or duplicated", i+1))
		}
		names[group.Name] = true
		if len(group.Streams) < 1 || len(group.Streams) > 16 {
			return sweepError(fmt.Sprintf("group %s: stream count must be between 1 and 16", group.Name))
		}
		total += len(group.Streams)
		if total > 256 {
			return sweepError("total stream count exceeds 256")
		}
		definitions[i] = SweepGroup[T]{Name: group.Name, Streams: append([]SweepStream[T](nil), group.Streams...)}
		cursors[i] = make([]sweepCursor, len(group.Streams))
		streams := map[string]bool{}
		for j, stream := range group.Streams {
			if !sweepName(stream.Name) || streams[stream.Name] || stream.Page == nil {
				return sweepError(fmt.Sprintf("group %s: stream %d: definition is invalid", group.Name, j+1))
			}
			streams[stream.Name] = true
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ctx, closeTelemetry, err := startControllerTelemetry(ctx)
	if err != nil {
		return err
	}
	defer closeTelemetry()
	position := 0
	for ctx.Err() == nil {
		err := sweepGroup(ctx, definitions[position], cursors[position], &nextStreams[position], reconcile, options)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		position = (position + 1) % len(definitions)
		if options.IntervalMode == SweepIntervalAfterRound && position != 0 {
			continue
		}
		timer := time.NewTimer(options.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func validateSweepPage[T any](page SweepPage[T], after string, limit int) error {
	if err := validateCursorPage(page, after, limit); err != nil {
		return sweepError(err.Error())
	}
	return nil
}

func sweepGroup[T any](parent context.Context, group SweepGroup[T], cursors []sweepCursor, nextStream *int, reconcile func(context.Context, T) error, options SweepOptions) error {
	ctx, cancel := context.WithTimeout(parent, options.PassTimeout)
	defer cancel()
	observe := func(event SweepEvent) {
		if options.Observe != nil {
			options.Observe(event)
		}
	}
	start := *nextStream
	for offset := 0; offset < len(group.Streams); offset++ {
		i := (start + offset) % len(group.Streams)
		stream := group.Streams[i]
		if ctx.Err() != nil {
			return nil
		}
		*nextStream = (i + 1) % len(group.Streams)
		cursor := &cursors[i]
		if cursor.pages >= options.MaxPagesPerCycle {
			return sweepError("stream exceeded its cycle page limit")
		}
		var page SweepPage[T]
		var sourceError, validationError error
		observedError, finish := controllerWork(ctx, "scan", func(operation context.Context) error {
			page, sourceError = stream.Page(operation, cursor.after, options.PageSize)
			if sourceError != nil {
				return sourceError
			}
			validationError = validateSweepPage(page, cursor.after, options.PageSize)
			return validationError
		})
		// Telemetry can record a deadline after a successful callback. Keep that observation separate from the
		// source error and its terminal policy (ST-SUP-RP02-LC16).
		terminalScan := validationError != nil || (sourceError != nil && (errors.Is(sourceError, ErrSweepContract) || options.Terminal(sourceError)))
		finish(observedError != nil && !terminalScan && parent.Err() == nil && ctx.Err() != context.Canceled)
		if sourceError != nil {
			observe(SweepEvent{Group: group.Name, Stream: stream.Name, Err: sourceError})
			if terminalScan {
				return sourceError
			}
			continue
		}
		if validationError != nil {
			return validationError
		}
		if ctx.Err() != nil {
			return nil
		}
		started, failures := sweepPage(parent, ctx, cancel, page, reconcile, options)
		prefix, count, failed := 0, 0, 0
		var result []error
		var terminal error
		for j, didStart := range started {
			if didStart {
				count++
			}
			if j == prefix && didStart {
				prefix++
			}
			if err := failures[j]; err != nil {
				failed++
				result = append(result, err)
				if errors.Is(err, ErrSweepContract) || options.Terminal(err) {
					terminal = err
				}
			}
		}
		if prefix > 0 {
			cursor.after = page.Items[prefix-1].Cursor
			cursor.pages++
		}
		if prefix == len(page.Items) && !page.More {
			*cursor = sweepCursor{}
		}
		observe(SweepEvent{Group: group.Name, Stream: stream.Name, Started: count, Failed: failed, Err: errors.Join(result...)})
		if terminal != nil {
			return terminal
		}
	}
	if ctx.Err() == nil {
		*nextStream = 0
	}
	return nil
}

// sweepPage runs the actions of one page on at most Workers workers and joins them all before it returns.
func sweepPage[T any](parent, ctx context.Context, cancel context.CancelFunc, page SweepPage[T], reconcile func(context.Context, T) error, options SweepOptions) ([]bool, []error) {
	started := make([]bool, len(page.Items))
	failures := make([]error, len(page.Items))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for n := 0; n < min(options.Workers, len(page.Items)); n++ {
		workers.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				started[i] = true
				err, finish := controllerWork(ctx, "reconcile", func(operation context.Context) error { return reconcile(operation, page.Items[i].Value) })
				failures[i] = err
				terminal := err != nil && (errors.Is(err, ErrSweepContract) || options.Terminal(err))
				// A pass deadline permits a later pass. Parent cancellation ends the run.
				finish(err != nil && !terminal && parent.Err() == nil && ctx.Err() != context.Canceled)
				if terminal {
					cancel()
					return
				}
			}
		})
	}
dispatch:
	for i := range page.Items {
		if ctx.Err() != nil {
			break
		}
		select {
		case jobs <- i:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()
	return started, failures
}
