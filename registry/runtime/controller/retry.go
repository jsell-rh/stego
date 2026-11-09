package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var ErrObservationContract = errors.New("invalid observation callback contract")

type ObservationOptions struct {
	WorkTimeout   time.Duration
	CommitTimeout time.Duration
}

// RunObservation reserves time for a conditional observation commit after work.
// Work receives the earlier of WorkTimeout and the parent deadline minus
// CommitTimeout. Commit receives the work error and a separate bounded context.
// A work timeout does not cancel that commit context. Parent cancellation does.
//
// Callbacks run synchronously and must honor their contexts. No callback is
// detached. A late nil return is a deadline error. The result preserves both
// work and commit errors. There is no automatic retry or rollback.
//
// The application owns observation values, authorization, revision checks, and
// atomic events. Read the revision before work; do not fetch a newer revision
// to commit an older result. A successful commit does not imply successful work.
func RunObservation(ctx context.Context, work func(context.Context) error, commit func(context.Context, error) error, options ObservationOptions) error {
	return controllerHelperWork(ctx, "reconcile", func(ctx context.Context) error {
		return runObservationUnchecked(ctx, work, commit, options)
	})
}
func runObservationUnchecked(ctx context.Context, work func(context.Context) error, commit func(context.Context, error) error, options ObservationOptions) error {
	if ctx == nil || work == nil || commit == nil || options.WorkTimeout < time.Millisecond || options.WorkTimeout > time.Minute || options.CommitTimeout < time.Millisecond || options.CommitTimeout > time.Minute {
		return ErrObservationContract
	}
	if err := observationContextError(ctx); err != nil {
		return err
	}
	workEnd := time.Now().Add(options.WorkTimeout)
	if end, ok := ctx.Deadline(); ok {
		reserved := end.Add(-options.CommitTimeout)
		if reserved.Before(workEnd) {
			workEnd = reserved
		}
	}
	operation, stop := context.WithDeadline(ctx, workEnd)
	defer stop()
	workErr := observationContextError(operation)
	if workErr == nil {
		workErr = work(operation)
	}
	workErr = errors.Join(workErr, observationContextError(operation))
	stop()
	if err := observationContextError(ctx); err != nil {
		return errors.Join(workErr, err)
	}
	write, closeWrite := context.WithTimeout(ctx, options.CommitTimeout)
	defer closeWrite()
	writeErr := observationContextError(write)
	if writeErr == nil {
		writeErr = commit(write, workErr)
	}
	return errors.Join(workErr, writeErr, observationContextError(write))
}

// Timer callbacks can be delayed. Check the clock as well as cancellation.
func observationContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if end, ok := ctx.Deadline(); ok && !time.Now().Before(end) {
		return context.DeadlineExceeded
	}
	return nil
}

// Checkpoint is scan progress, not proof of convergence or exclusive ownership.
// Zero version means absent. Reset keeps a positive version to reject late writes.
type Checkpoint struct {
	After   string
	Version int64
}
type CheckpointAccess struct {
	Load func(context.Context) (Checkpoint, error)
	// Save must compare the supplied version and return a conflict without retry.
	Save func(context.Context, int64, string) error
}

// ScanCheckpointed loads progress, runs a bounded pass, and saves its completed
// prefix with time reserved for the write. Source and emit receive the work
// context. A work timeout can save progress; parent cancellation cannot.
// A crash can repeat work since the last save. Effects must tolerate repeats.
// Completion resets the cursor for a later full scan. Unchanged cursors cause
// no write. Errors from work and save are preserved; there is no automatic retry.
func ScanCheckpointed[T any](ctx context.Context, access CheckpointAccess, source CursorSource[T], emit func(context.Context, T) error, scan ScanOptions, budget ObservationOptions) (ScanProgress, error) {
	var progress ScanProgress
	err := controllerHelperWork(ctx, "scan", func(ctx context.Context) error {
		var err error
		progress, err = scanCheckpointedBody(ctx, access, source, emit, scan, budget)
		return err
	})
	return progress, err
}
func scanCheckpointedBody[T any](ctx context.Context, access CheckpointAccess, source CursorSource[T], emit func(context.Context, T) error, scan ScanOptions, budget ObservationOptions) (ScanProgress, error) {
	var progress ScanProgress
	if access.Load == nil || access.Save == nil || source == nil || emit == nil {
		return progress, ErrScanContract
	}
	var checkpoint Checkpoint
	loaded := false
	err := RunObservation(ctx, func(work context.Context) error {
		var err error
		checkpoint, err = access.Load(work)
		if err != nil {
			return err
		}
		if checkpoint.Version < 0 || (checkpoint.Version == 0 && checkpoint.After != "") || len(checkpoint.After) > 1024 || !utf8.ValidString(checkpoint.After) || strings.IndexByte(checkpoint.After, 0) >= 0 {
			return ErrScanContract
		}
		loaded = true
		progress, err = ScanFrom(work, checkpoint.After, source, func(value T) error { return emit(work, value) }, scan)
		return err
	}, func(commit context.Context, _ error) error {
		if !loaded {
			return nil
		}
		after := progress.After
		if progress.Complete {
			after = ""
		}
		if after == checkpoint.After {
			return nil
		}
		return access.Save(commit, checkpoint.Version, after)
	}, budget)
	return progress, err
}

var ErrCycleFailed = errors.New("scan cycle contains failed actions")

// ErrScanWindowLimit ends a bounded source window without proving completion.
// A source can return it to require a later full scan from the beginning. The
// current cycle is saved as complete and failed, and always returns an error.
// An emitter cannot use this error to end the source window.
var ErrScanWindowLimit = errors.New("scan source window limit reached")

// CycleState retains failure evidence across bounded passes. Complete means the
// source ended or reached an explicit window limit. A successful cycle also requires Failed=false and a successful
// conditional save. Source identifies all desired inputs relevant to the work.
type CycleState struct {
	Source, After    string
	Failed, Complete bool
}

// EncodeCycle uses the existing opaque checkpoint storage contract. Its versioned
// format has a 1024-byte limit. It contains no provider error text.
func EncodeCycle(value CycleState) (string, error) {
	if value.Source == "" || len(value.Source) > 128 || len(value.After) > 512 || !utf8.ValidString(value.Source) || !utf8.ValidString(value.After) || strings.IndexByte(value.Source, 0) >= 0 || strings.IndexByte(value.After, 0) >= 0 {
		return "", ErrScanContract
	}
	data, err := json.Marshal([]any{1, value.Source, value.After, value.Failed, value.Complete})
	if err != nil || len(data) > 1024 {
		return "", ErrScanContract
	}
	return string(data), nil
}

// DecodeCycle accepts only the canonical format written by EncodeCycle. An empty
// checkpoint has no earlier cycle. Unknown formats fail before provider work.
func DecodeCycle(encoded string) (CycleState, error) {
	var value CycleState
	if encoded == "" {
		return value, nil
	}
	if len(encoded) > 1024 || !utf8.ValidString(encoded) {
		return value, ErrScanContract
	}
	var fields []json.RawMessage
	if json.Unmarshal([]byte(encoded), &fields) != nil || len(fields) != 5 {
		return value, ErrScanContract
	}
	var format int
	if json.Unmarshal(fields[0], &format) != nil || format != 1 || json.Unmarshal(fields[1], &value.Source) != nil || json.Unmarshal(fields[2], &value.After) != nil || json.Unmarshal(fields[3], &value.Failed) != nil || json.Unmarshal(fields[4], &value.Complete) != nil {
		return CycleState{}, ErrScanContract
	}
	canonical, err := EncodeCycle(value)
	if err != nil || canonical != encoded {
		return CycleState{}, ErrScanContract
	}
	return value, nil
}

// ValidateCycleTransition prevents loss of earlier failure evidence within a
// cycle. The caller must also check its input version and checkpoint revision.
// Cursor ordering belongs to the source contract; this function does not order
// opaque cursors. A complete cycle or changed source starts a new cycle.
func ValidateCycleTransition(previous, next CycleState) error {
	if _, err := EncodeCycle(next); err != nil {
		return err
	}
	if previous.Source == "" {
		if previous != (CycleState{}) {
			return ErrScanContract
		}
		return nil
	}
	if _, err := EncodeCycle(previous); err != nil {
		return err
	}
	if previous.Source == next.Source && !previous.Complete && previous.Failed && !next.Failed {
		return ErrScanContract
	}
	return nil
}

// CycleOptions gives each action a full time budget. The runtime stops the pass
// before a new action if the remaining work time is shorter than ActionTimeout.
// A stopped pass retains its successful prefix and does not add a failure.
// An unresolved action error or late return still adds failure evidence.
type CycleOptions struct {
	ActionTimeout time.Duration
	Retry         CycleRetryOptions
}

// CycleRetryOptions permits a bounded retry before the cursor advances. Zero
// values retain one attempt. Retryable must return promptly and be safe for
// concurrent calls. It must select only errors for which repeating the whole
// action is safe, including when an earlier result was lost. A successful retry
// resolves this action's failure; it does not clear a saved cycle failure.
// No checkpoint save or source read is retried by this policy.
type CycleRetryOptions struct {
	MaxAttempts int
	Delay       time.Duration
	Retryable   func(error) bool
}

// MaxCycleAttempts is the most attempts a cycle retry policy may make (ST-SUP-RP02-LC03).
const MaxCycleAttempts = 4

// validate refuses a policy beyond four attempts, outside 1 ms to 1 s of delay, or with a delay above the action
// timeout. The error names the bound it breaks (ST-SUP-RP02-LC09).
func (r CycleRetryOptions) validate(timeout time.Duration) error {
	if r.MaxAttempts < 0 || r.MaxAttempts > MaxCycleAttempts {
		return fmt.Errorf("%w: retry attempts %d are outside 0 to %d", ErrScanContract, r.MaxAttempts, MaxCycleAttempts)
	}
	if r.MaxAttempts <= 1 {
		if r.Delay != 0 || r.Retryable != nil {
			return fmt.Errorf("%w: a single attempt takes no retry delay or predicate", ErrScanContract)
		}
		return nil
	}
	if r.Retryable == nil {
		return fmt.Errorf("%w: a retry policy requires a predicate", ErrScanContract)
	}
	if r.Delay < time.Millisecond || r.Delay > time.Second {
		return fmt.Errorf("%w: retry delay %s is outside 1ms to 1s", ErrScanContract, r.Delay)
	}
	if r.Delay > timeout {
		return fmt.Errorf("%w: retry delay %s exceeds the action timeout %s", ErrScanContract, r.Delay, timeout)
	}
	return nil
}

// Each retry starts only after the previous callback returns and its context
// is canceled. The pass must retain the delay and one full action budget.
// Parallel workers retain their resource key throughout the retry sequence.
func cycleActionWithRetry(work context.Context, timeout time.Duration, retry CycleRetryOptions, emit func(context.Context) error) error {
	for attempt := 1; ; attempt++ {
		action := work
		cancel := func() {}
		if timeout > 0 {
			action, cancel = context.WithTimeout(work, timeout)
		}
		err := errors.Join(emit(action), observationContextError(action))
		cancel()
		if err == nil {
			return nil
		}
		if attempt >= max(1, retry.MaxAttempts) || observationContextError(work) != nil || errors.Is(err, ErrScanContract) || errors.Is(err, ErrScanWindowLimit) {
			return err
		}
		if !retry.Retryable(err) {
			return err
		}
		end, ok := work.Deadline()
		if !ok || time.Until(end) < retry.Delay+timeout {
			return err
		}
		timer := time.NewTimer(retry.Delay)
		select {
		case <-work.Done():
			timer.Stop()
			return errors.Join(err, observationContextError(work))
		case <-timer.C:
		}
		if observationContextError(work) != nil {
			return errors.Join(err, observationContextError(work))
		}
		if time.Until(end) < timeout {
			return err
		}
	}
}

var errCycleBudget = errors.New("scan action budget is not available")

// ScanCycleWithOptions requires a positive action timeout below WorkTimeout.
// Callbacks must honor their contexts. A source read still uses PageTimeout.
// The checkpoint format and all failure and conditional-save rules are unchanged.
func ScanCycleWithOptions[T any](ctx context.Context, sourceVersion string, access CheckpointAccess, source CursorSource[T], emit func(context.Context, T) error, continueOnError func(error) bool, scan ScanOptions, budget ObservationOptions, options CycleOptions) (CycleState, error) {
	if options.ActionTimeout < time.Millisecond || options.ActionTimeout >= budget.WorkTimeout {
		return CycleState{Source: sourceVersion}, ErrScanContract
	}
	if err := options.Retry.validate(options.ActionTimeout); err != nil {
		return CycleState{Source: sourceVersion}, err
	}
	return scanCycle(ctx, sourceVersion, access, source, emit, continueOnError, scan, budget, options, nil)
}

// ScanCycle retains earlier action failures when the next process resumes work.
// A changed Source or a completed cycle starts again at the beginning. The caller
// must invalidate the checkpoint atomically when a dependency changes, or supply
// a changed Source and check it during Save. Version conflicts are not retried.
// continueOnError permits independent later items after an action error. It must
// not permit errors that make later work unsafe. Context errors always stop work.
// Parent cancellation prevents saving. A work timeout can save its completed
// prefix. A completed failed cycle returns ErrCycleFailed, including when the
// failed action ran in an earlier process. Effects must be safe to repeat.
func ScanCycle[T any](ctx context.Context, sourceVersion string, access CheckpointAccess, source CursorSource[T], emit func(context.Context, T) error, continueOnError func(error) bool, scan ScanOptions, budget ObservationOptions) (CycleState, error) {
	return scanCycle(ctx, sourceVersion, access, source, emit, continueOnError, scan, budget, CycleOptions{}, nil)
}
func scanCycle[T any](ctx context.Context, sourceVersion string, access CheckpointAccess, source CursorSource[T], emit func(context.Context, T) error, continueOnError func(error) bool, scan ScanOptions, budget ObservationOptions, options CycleOptions, parallel *ParallelCycleOptions[T]) (CycleState, error) {
	var result CycleState
	err := controllerHelperWork(ctx, "scan", func(ctx context.Context) error {
		var err error
		result, err = scanCycleBody(ctx, sourceVersion, access, source, emit, continueOnError, scan, budget, options, parallel)
		return err
	})
	return result, err
}
func scanCycleBody[T any](ctx context.Context, sourceVersion string, access CheckpointAccess, source CursorSource[T], emit func(context.Context, T) error, continueOnError func(error) bool, scan ScanOptions, budget ObservationOptions, options CycleOptions, parallel *ParallelCycleOptions[T]) (CycleState, error) {
	result := CycleState{Source: sourceVersion}
	if _, err := EncodeCycle(result); err != nil || access.Load == nil || access.Save == nil || source == nil || emit == nil {
		return result, ErrScanContract
	}
	var checkpoint Checkpoint
	loaded := false
	var actionFailure error
	err := RunObservation(ctx, func(work context.Context) error {
		var err error
		checkpoint, err = access.Load(work)
		if err != nil {
			return err
		}
		if checkpoint.Version < 0 || (checkpoint.Version == 0 && checkpoint.After != "") {
			return ErrScanContract
		}
		prior, err := DecodeCycle(checkpoint.After)
		if err != nil {
			return err
		}
		if prior.Source == sourceVersion && !prior.Complete {
			result = prior
		}
		loaded = true
		windowEnded := false
		budgetEnded := false
		read := func(pageContext context.Context, after string, limit int) (CursorPage[T], error) {
			page, err := source(pageContext, after, limit)
			if err != nil {
				if errors.Is(err, ErrScanWindowLimit) && observationContextError(pageContext) == nil {
					windowEnded = true
				}
				return page, err
			}
			// Validate persistence bounds for the entire page before its first effect.
			for _, item := range page.Items {
				if _, err := EncodeCycle(CycleState{Source: sourceVersion, After: item.Cursor}); err != nil {
					return CursorPage[T]{}, err
				}
			}
			return page, nil
		}
		action := func(value T) error {
			if options.ActionTimeout > 0 {
				end, ok := work.Deadline()
				if !ok {
					return ErrScanContract
				}
				if time.Until(end) < options.ActionTimeout {
					budgetEnded = true
					return errCycleBudget
				}
			}
			err := cycleActionWithRetry(work, options.ActionTimeout, options.Retry, func(action context.Context) error { return emit(action, value) })
			if err == nil {
				return nil
			}
			result.Failed = true
			if actionFailure == nil {
				actionFailure = err
			}
			if work.Err() != nil || continueOnError == nil || !continueOnError(err) {
				return err
			}
			return nil
		}
		var progress ScanProgress
		if parallel == nil {
			progress, err = ScanFrom(work, result.After, read, action, scan)
		} else {
			progress, err = scanFromPages(work, result.After, read, func(ctx context.Context, items []CursorItem[T]) (int, error) {
				page := runParallelCyclePage(ctx, items, emit, continueOnError, *parallel)
				result.Failed = result.Failed || page.failed
				if actionFailure == nil {
					actionFailure = page.failure
				}
				budgetEnded = budgetEnded || page.budgetEnded
				return page.prefix, page.err
			}, scan)
		}
		result.After = progress.After
		result.Complete = progress.Complete
		if budgetEnded && err == errCycleBudget {
			err = nil
		}
		if windowEnded && observationContextError(work) == nil {
			result.Complete = true
			result.Failed = true
		}
		return err
	}, func(commit context.Context, _ error) error {
		if !loaded {
			return nil
		}
		encoded, err := EncodeCycle(result)
		if err != nil {
			return err
		}
		if encoded == checkpoint.After {
			return nil
		}
		return access.Save(commit, checkpoint.Version, encoded)
	}, budget)
	if result.Failed {
		err = errors.Join(err, actionFailure, ErrCycleFailed)
	}
	return result, err
}

// MaxParallelCycleWorkers bounds callbacks and result storage per page.
// Consumers can use this limit when validating operator settings at startup.
const MaxParallelCycleWorkers = 64

// ParallelCycleOptions selects bounded concurrency for independent resource
// keys. Key must return promptly. Equal keys run in source order, one at a time,
// within this invocation. Different keys can run at the same time. Keys are not
// stored or logged. This is not a distributed lock or a provider lease.
type ParallelCycleOptions[T any] struct {
	Workers       int
	ActionTimeout time.Duration
	Retry         CycleRetryOptions
	Key           func(T) string
}

// ScanCycleParallel retains the same source, failure, checkpoint, and commit
// rules as ScanCycleWithOptions. It validates all page keys before effects.
// The emitter must be safe for concurrent calls on different keys, honor its
// context, and tolerate repeats. All admitted callbacks return before saving.
// This does not fence external effects that finish after a callback returns.
// Effects beyond a stopped prefix can repeat after restart. Continuation policy
// calls are serial; it must not depend on the order in which actions finish.
func ScanCycleParallel[T any](ctx context.Context, sourceVersion string, access CheckpointAccess, source CursorSource[T], emit func(context.Context, T) error, continueOnError func(error) bool, scan ScanOptions, budget ObservationOptions, options ParallelCycleOptions[T]) (CycleState, error) {
	if options.Workers < 1 || options.Workers > MaxParallelCycleWorkers || options.Key == nil || options.ActionTimeout < time.Millisecond || options.ActionTimeout >= budget.WorkTimeout {
		return CycleState{Source: sourceVersion}, ErrScanContract
	}
	if err := options.Retry.validate(options.ActionTimeout); err != nil {
		return CycleState{Source: sourceVersion}, err
	}
	return scanCycle(ctx, sourceVersion, access, source, emit, continueOnError, scan, budget, CycleOptions{ActionTimeout: options.ActionTimeout, Retry: options.Retry}, &options)
}

type cyclePageResult struct {
	prefix              int
	failed, budgetEnded bool
	failure, err        error
}
type cycleActionResult struct {
	index                int
	started, budgetEnded bool
	err                  error
}
type cycleKeyQueue struct {
	indexes []int
	next    int
}

func cycleActionReserve(ctx context.Context, timeout time.Duration) (bool, error) {
	if err := observationContextError(ctx); err != nil {
		return false, err
	}
	end, ok := ctx.Deadline()
	if !ok {
		return false, ErrScanContract
	}
	return time.Until(end) >= timeout, nil
}

func runParallelCyclePage[T any](work context.Context, items []CursorItem[T], emit func(context.Context, T) error, continueOnError func(error) bool, options ParallelCycleOptions[T]) cyclePageResult {
	result := cyclePageResult{}
	if len(items) == 0 {
		return result
	}
	// A page can contain the same resource under different source cursors.
	// Validate every key before starting any worker or external action.
	groups := []cycleKeyQueue{}
	groupFor := make([]int, len(items))
	byKey := map[string]int{}
	for i, item := range items {
		key := options.Key(item.Value)
		if len(key) == 0 || len(key) > 512 || !utf8.ValidString(key) || strings.IndexByte(key, 0) >= 0 {
			result.err = ErrScanContract
			return result
		}
		group, ok := byKey[key]
		if !ok {
			group = len(groups)
			byKey[key] = group
			groups = append(groups, cycleKeyQueue{})
		}
		groupFor[i] = group
		groups[group].indexes = append(groups[group].indexes, i)
	}
	workers := min(options.Workers, len(groups))
	jobs := make(chan int)
	results := make(chan cycleActionResult, workers)
	page, cancel := context.WithCancel(work)
	defer cancel()
	var running sync.WaitGroup
	for range workers {
		running.Go(func() {
			for index := range jobs {
				event := cycleActionResult{index: index}
				available, err := cycleActionReserve(page, options.ActionTimeout)
				if err != nil {
					event.err = err
				} else if !available {
					event.budgetEnded = true
				} else {
					event.started = true
					event.err = cycleActionWithRetry(page, options.ActionTimeout, options.Retry, func(action context.Context) error { return emit(action, items[index].Value) })
				}
				results <- event
			}
		})
	}
	ready := make([]int, len(groups))
	for i := range ready {
		ready[i] = i
	}
	head, inFlight := 0, 0
	stopped := false
	accepted := make([]bool, len(items))
	failures := make([]error, len(items))
	var stopError error
	for {
		if !stopped && observationContextError(work) != nil {
			stopped = true
			cancel()
		}
		for !stopped && inFlight < workers && head < len(ready) {
			available, err := cycleActionReserve(work, options.ActionTimeout)
			if err != nil {
				stopError = err
				stopped = true
				cancel()
				break
			}
			if !available {
				result.budgetEnded = true
				stopped = true
				break
			}
			group := ready[head]
			index := groups[group].indexes[groups[group].next]
			select {
			case jobs <- index:
				head++
				groups[group].next++
				inFlight++
			case <-work.Done():
				stopped = true
				cancel()
			}
		}
		if inFlight == 0 {
			break
		}
		event := <-results
		inFlight--
		if event.started && event.err != nil {
			failures[event.index] = event.err
		}
		if event.budgetEnded {
			result.budgetEnded = true
			stopped = true
		}
		if event.err != nil {
			// A peer-abort cancellation cannot acknowledge interrupted work, even if
			// application policy permits independent provider timeouts.
			allow := event.started && observationContextError(work) == nil && observationContextError(page) == nil && continueOnError != nil && continueOnError(event.err)
			if allow {
				accepted[event.index] = true
			} else {
				stopError = errors.Join(stopError, event.err)
				stopped = true
				cancel()
			}
		} else if event.started {
			accepted[event.index] = true
		}
		group := groupFor[event.index]
		if !stopped && groups[group].next < len(groups[group].indexes) {
			ready = append(ready, group)
		}
	}
	close(jobs)
	running.Wait()
	for i, ok := range accepted {
		if ok && i == result.prefix {
			result.prefix++
		}
		if failures[i] != nil {
			result.failed = true
			if result.failure == nil {
				result.failure = failures[i]
			}
		}
	}
	result.err = errors.Join(stopError, observationContextError(work))
	if result.err == nil && result.budgetEnded {
		result.err = errCycleBudget
	}
	return result
}
