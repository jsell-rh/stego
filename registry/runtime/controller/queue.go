package controller

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ErrOverflow reports a full queue: a FIFO watch that receives more items than its capacity, or a key that finds no
// free slot (ST-SUP-RP02-LC02).
var ErrOverflow = errors.New("controller watch queue exceeded its limit")

// ErrWatch reports a watch that returned no receiver.
var ErrWatch = errors.New("controller watch did not return a receiver")

// ErrSourceStopped reports an observer that returned without an error while its context was still active.
var ErrSourceStopped = errors.New("controller observation stopped")

// ErrKey reports a key that is empty, longer than 1,024 bytes or not UTF-8. Such a key takes no slot.
var ErrKey = errors.New("controller key must contain 1 to 1024 UTF-8 bytes")

// ErrReconcileResult reports a recheck delay outside zero or 1 ms to 1 h.
var ErrReconcileResult = errors.New("controller recheck delay is outside its limits")

// Source opens a live subscription before Scan starts. The returned receiver must stop when the supplied context is
// canceled. Scan must enumerate retained work, including failed work and missed deletions. It can block in emit.
// Sources must bound each item and each external request. They must not change an item after they emit it.
// Callbacks must return when their context ends.
type Source[T any] struct {
	Watch func(context.Context) (func() (T, error), error)
	Scan  func(context.Context, func(T) error) error
}

// Options apply to one FIFO controller. One worker processes items in order. Repeated runs of a domain action must be
// safe. This runtime does not provide leases, distributed exclusion, durable acknowledgments, or exactly-once writes.
type Options struct {
	// QueueCapacity bounds the waiting buffer. Telemetry capacity includes the one active action. A scan can
	// retain one additional item while it waits.
	QueueCapacity    int
	ResyncInterval   time.Duration
	ReconcileTimeout time.Duration
	ReconnectDelay   time.Duration
	// Terminal must identify errors that must stop the process, including denied credentials. It receives errors
	// from all source and action callbacks.
	Terminal func(error) bool
	// Observe is optional. It runs on the main worker and must return promptly. It receives errors, which can
	// contain private data. Do not log them without a domain policy. The runtime itself does not log resource data
	// or errors.
	Observe func(Event)
}

// Event is one notice of the controller to the caller's Observe callback.
type Event struct {
	Phase string
	Err   error
}

func (o Options) validate() error {
	if o.QueueCapacity < 1 || o.QueueCapacity > 65536 {
		return errors.New("controller queue capacity must be between 1 and 65536")
	}
	if o.ResyncInterval < time.Millisecond || o.ResyncInterval > 24*time.Hour {
		return errors.New("controller resync interval is outside its limits")
	}
	if o.ReconcileTimeout < time.Millisecond || o.ReconcileTimeout > time.Hour {
		return errors.New("controller action timeout is outside its limits")
	}
	if o.ReconnectDelay < time.Millisecond || o.ReconnectDelay > time.Minute {
		return errors.New("controller reconnect delay is outside its limits")
	}
	if o.Terminal == nil {
		return errors.New("controller requires a terminal error policy")
	}
	return nil
}

func (o Options) observe(phase string, err error) {
	if o.Observe != nil {
		o.Observe(Event{Phase: phase, Err: err})
	}
}

// Run repeats a complete scan after each reconnect. A full watch queue stops the session and causes recovery; it
// cannot silently discard work. Ordinary action errors are repaired by the next retained-state scan. Cancellation
// joins both source workers before Run returns or opens another watch. Each item reaches the action as the source
// emitted it, never merged with another (ST-SUP-RP02-LC01).
func Run[T any](ctx context.Context, source Source[T], reconcile func(context.Context, T) error, options Options) error {
	if ctx == nil || source.Watch == nil || source.Scan == nil || reconcile == nil {
		return errors.New("controller requires context, source, and action")
	}
	if err := options.validate(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	ctx, closeTelemetry, err := startControllerTelemetry(ctx)
	if err != nil {
		return err
	}
	defer closeTelemetry()
	for ctx.Err() == nil {
		err, finish := controllerWork(ctx, "watch", func(operation context.Context) error { return session(operation, source, reconcile, options) })
		if ctx.Err() != nil {
			finish(false)
			return nil
		}
		terminal := err != nil && (errors.Is(err, ErrWatch) || errors.Is(err, ErrTelemetryInUse) || options.Terminal(err))
		finish(!terminal)
		if terminal {
			return err
		}
		controllerNotice(ctx, "reconnect")
		options.observe("reconnect", err)
		timer := time.NewTimer(options.ReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func session[T any](parent context.Context, source Source[T], reconcile func(context.Context, T) error, options Options) error {
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	queue := make(chan T, options.QueueCapacity)
	var active atomic.Int32
	var waiting atomic.Int64
	var ready atomic.Bool
	detach, err := attachControllerQueue(ctx, func() QueueMetrics {
		return QueueMetrics{Capacity: cap(queue) + 1, Queued: len(queue), Active: int(active.Load()), Waiting: waiting.Load(), Ready: ready.Load() && ctx.Err() == nil}
	})
	if err != nil {
		cancel()
		return err
	}
	defer func() { cancel(); workers.Wait(); detach() }()
	// The same operation limit bounds subscription setup. Stop its timer after setup, so a healthy live stream can
	// remain open across many scans.
	setup := time.AfterFunc(options.ReconcileTimeout, cancel)
	receive, err := source.Watch(ctx)
	setup.Stop()
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if receive == nil {
		return ErrWatch
	}
	ready.Store(true)
	controllerNotice(ctx, "watch_started")
	options.observe("watch_started", nil)
	failures := make(chan error, 2)
	scanned := make(chan struct{}, 1)
	// Record failures before canceling. Cancellation interrupts an active action and prevents an old session from
	// writing while the next session starts.
	fail := func(err error) { failures <- err; cancel() }
	workers.Go(func() {
		for ctx.Err() == nil {
			item, err := receive()
			if err != nil {
				fail(err)
				return
			}
			if ctx.Err() != nil {
				return
			}
			select {
			case queue <- item:
			case <-ctx.Done():
				return
			default:
				fail(ErrOverflow)
				return
			}
		}
	})
	workers.Go(func() {
		emit := func(item T) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			waiting.Add(1)
			defer waiting.Add(-1)
			select {
			case queue <- item:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		for ctx.Err() == nil {
			err, finish := controllerWork(ctx, "scan", func(operation context.Context) error { return source.Scan(operation, emit) })
			// A scan failure ends the session. Only the joined watch session owns the reconnect decision; the
			// terminal policy stays on the main worker.
			finish(false)
			if err != nil {
				fail(err)
				return
			}
			select {
			case scanned <- struct{}{}:
			case <-ctx.Done():
				return
			}
			// The interval begins after a completed scan. A slow scan is not canceled by its resync interval, and
			// two scans never run at the same time.
			timer := time.NewTimer(options.ResyncInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	})
	for {
		// Prefer cancellation to queued items. No new action starts after a known source failure. Source
		// callbacks may still be stopping at this point.
		if ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-scanned:
			options.observe("scan_completed", nil)
		case item := <-queue:
			if ctx.Err() != nil {
				break
			}
			operation, stop := context.WithTimeout(ctx, options.ReconcileTimeout)
			err, finish := controllerWork(operation, "reconcile", func(work context.Context) error {
				active.Store(1)
				defer active.Store(0)
				return reconcile(work, item)
			})
			stop()
			terminal := err != nil && options.Terminal(err)
			finish(err != nil && ctx.Err() == nil && !terminal)
			if terminal {
				return err
			}
			if err != nil {
				options.observe("reconcile_failed", err)
			}
		}
	}
	// A concurrent permission failure must not be hidden by a stream cancellation.
	cancel()
	workers.Wait()
	var result []error
	for len(failures) != 0 {
		err := <-failures
		if options.Terminal(err) {
			return err
		}
		result = append(result, err)
	}
	if len(result) != 0 {
		return errors.Join(result...)
	}
	return ctx.Err()
}

// ReconcileResult requests another read of authoritative state. Zero means this pass needs no scheduled recheck.
// RecheckAfter must be zero or 1ms..1h. A nonzero result is not a readiness or completion claim. An action error or
// context failure takes precedence and discards the result.
type ReconcileResult struct{ RecheckAfter time.Duration }

func (r ReconcileResult) validate() error {
	if r.RecheckAfter != 0 && (r.RecheckAfter < time.Millisecond || r.RecheckAfter > time.Hour) {
		return ErrReconcileResult
	}
	return nil
}

// KeyedSource supplies invalidation keys, not authoritative resource payloads. Observe must establish a complete
// baseline before SetReady(true). It must call SetReady(false) before an incomplete replacement. It owns watch
// reconnects. Observe must propagate Add errors. Both callbacks must stop on cancellation. Scan emits keys from
// current authoritative state, including zero-value state.
type KeyedSource[K ~string] struct {
	Observe func(context.Context, *KeySink[K]) error
	Scan    func(context.Context, func(K) error) error
}

// KeyedOptions are the bounds of one keyed controller. Validation refuses each value outside its bounds before any
// source opens (ST-SUP-RP02-LC02).
type KeyedOptions struct {
	// Metrics is optional and must belong to one active controller.
	Metrics *Metrics
	// Cleanup is optional. With Metrics enabled, it runs independently of scans after each ResyncInterval, with
	// Timeout applied to each read.
	Cleanup  func(context.Context) (CleanupSample, error)
	Capacity int
	// Workers bounds concurrent actions for distinct keys. Zero selects one.
	Workers        int
	ResyncInterval time.Duration
	Timeout        time.Duration
	RetryMin       time.Duration
	RetryMax       time.Duration
	Terminal       func(error) bool
	// Observe calls are serialized. The callback must return promptly and apply a domain policy before it logs any
	// private error details.
	Observe func(Event)
}

func (o KeyedOptions) validate() error {
	if o.Capacity < 1 || o.Capacity > 65536 {
		return errors.New("controller key capacity must be between 1 and 65536")
	}
	if o.Workers < 0 || o.Workers > 64 || o.Workers > o.Capacity {
		return errors.New("controller workers must be between 1 and the smaller of capacity and 64; zero selects one")
	}
	if o.ResyncInterval < time.Millisecond || o.ResyncInterval > 24*time.Hour {
		return errors.New("controller key resync is outside its limits")
	}
	if o.Timeout < time.Millisecond || o.Timeout > time.Hour {
		return errors.New("controller key timeout is outside its limits")
	}
	if o.RetryMin < time.Millisecond || o.RetryMax < o.RetryMin || o.RetryMax > time.Hour {
		return errors.New("controller key retry delay is outside its limits")
	}
	if o.Terminal == nil {
		return errors.New("controller requires a terminal error policy")
	}
	return nil
}

// KeySink combines repeated invalidations for a key. A key changed during an action receives another pass. Changes
// cannot bypass a failed key's delay. Capacity includes queued, delayed, and active keys. Methods are safe for
// concurrent callbacks. A sink must not be retained after Observe returns.
type KeySink[K ~string] struct{ queue *keyQueue[K] }

// Add queues the key, or marks it changed when it is already queued or active. It never waits: a full queue answers
// ErrOverflow and keeps every admitted key (ST-SUP-RP02-LC11).
func (s *KeySink[K]) Add(key K) error { return s.queue.add(key) }

// AddWait applies backpressure until capacity is available or ctx ends. It does not discard another key or reset
// that key's retry delay. Waiting callers pass one at a time, so each producer holds at most one key outside the
// queue (ST-SUP-RP02-LC02). A paused or fully failing queue can keep it waiting.
func (s *KeySink[K]) AddWait(ctx context.Context, key K) error { return s.queue.addWait(ctx, key) }

// SetReady opens or closes the queue for new takes.
func (s *KeySink[K]) SetReady(ready bool) { s.queue.setReady(ready) }

type keyEntry[K ~string] struct {
	key    K
	due    time.Time
	order  uint64
	dirty  bool
	active bool
	delay  time.Duration
}

type keyHeap[K ~string] []*keyEntry[K]

func (h keyHeap[K]) Len() int { return len(h) }
func (h keyHeap[K]) Less(i, j int) bool {
	if h[i].due.Equal(h[j].due) {
		return h[i].order < h[j].order
	}
	return h[i].due.Before(h[j].due)
}
func (h keyHeap[K]) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *keyHeap[K]) Push(v any)   { *h = append(*h, v.(*keyEntry[K])) }
func (h *keyHeap[K]) Pop() any {
	old := *h
	n := len(old) - 1
	item := old[n]
	old[n] = nil
	*h = old[:n]
	return item
}

type keyQueue[K ~string] struct {
	mu        sync.Mutex
	ready     bool
	capacity  int
	next      uint64
	entries   map[K]*keyEntry[K]
	pending   keyHeap[K]
	changed   chan struct{}
	admission chan struct{}
	retrying  int
	waiting   atomic.Int64
	// The scan loop owns this schedule. Reconnect touches it only after join.
	scan scanSchedule
}

func newKeyQueue[K ~string](capacity int) *keyQueue[K] {
	return &keyQueue[K]{capacity: capacity, entries: make(map[K]*keyEntry[K]), changed: make(chan struct{}), admission: make(chan struct{}, 1)}
}

func (q *keyQueue[K]) metrics() QueueMetrics {
	q.mu.Lock()
	defer q.mu.Unlock()
	return QueueMetrics{Capacity: q.capacity, Queued: len(q.pending), Active: len(q.entries) - len(q.pending), Retrying: q.retrying, Waiting: q.waiting.Load(), Ready: q.ready}
}

func (q *keyQueue[K]) notify() { close(q.changed); q.changed = make(chan struct{}) }

func (q *keyQueue[K]) setReady(ready bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ready != ready {
		q.ready = ready
		q.notify()
	}
}

func validKey[K ~string](key K) bool {
	return len(key) != 0 && len(key) <= 1024 && utf8.ValidString(string(key))
}

func (q *keyQueue[K]) add(key K) error {
	if !validKey(key) {
		return ErrKey
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.addLocked(key)
}

func (q *keyQueue[K]) addLocked(key K) error {
	if entry := q.entries[key]; entry != nil {
		entry.dirty = true
		return nil
	}
	if len(q.entries) >= q.capacity {
		return ErrOverflow
	}
	q.next++
	entry := &keyEntry[K]{key: key, dirty: true, order: q.next, due: time.Now()}
	q.entries[key] = entry
	heap.Push(&q.pending, entry)
	q.notify()
	return nil
}

func (q *keyQueue[K]) addWait(ctx context.Context, key K) error {
	if ctx == nil {
		return errors.New("controller admission requires a context")
	}
	if !validKey(key) {
		return ErrKey
	}
	q.waiting.Add(1)
	defer q.waiting.Add(-1)
	// Serialize the scan and watch emitters while they wait for a slot. Waiting emitters retain one key each outside
	// the admitted queue.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case q.admission <- struct{}{}:
	}
	defer func() { <-q.admission }()
	for {
		q.mu.Lock()
		if err := ctx.Err(); err != nil {
			q.mu.Unlock()
			return err
		}
		changed := q.changed
		err := q.addLocked(key)
		q.mu.Unlock()
		if !errors.Is(err, ErrOverflow) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (q *keyQueue[K]) waitReady(ctx context.Context) error {
	for ctx.Err() == nil {
		q.mu.Lock()
		ready, changed := q.ready, q.changed
		q.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
	return ctx.Err()
}

func (q *keyQueue[K]) take(ctx context.Context) (K, error) {
	var zero K
	for ctx.Err() == nil {
		q.mu.Lock()
		changed := q.changed
		delay := time.Duration(-1)
		if q.ready && len(q.pending) > 0 {
			entry := q.pending[0]
			delay = time.Until(entry.due)
			if delay <= 0 {
				heap.Pop(&q.pending)
				if entry.delay > 0 {
					q.retrying--
				}
				entry.active = true
				entry.dirty = false
				q.mu.Unlock()
				return entry.key, nil
			}
		}
		q.mu.Unlock()
		if delay < 0 {
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-changed:
			}
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
	return zero, ctx.Err()
}

// nextDelay is the capped exponential backoff: the minimum first, then double, then the maximum.
func nextDelay(current, minimum, maximum time.Duration) time.Duration {
	if current < minimum {
		return minimum
	}
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}

func (q *keyQueue[K]) finish(key K, failed bool, minimum, maximum time.Duration) {
	q.finishResult(key, failed, 0, minimum, maximum)
}

func (q *keyQueue[K]) finishResult(key K, failed bool, recheck, minimum, maximum time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry := q.entries[key]
	if entry == nil || !entry.active {
		panic("controller finished an inactive key")
	}
	entry.active = false
	if failed {
		q.retrying++
		entry.delay = nextDelay(entry.delay, minimum, maximum)
		entry.due = time.Now().Add(entry.delay)
	} else if recheck > 0 {
		entry.delay = 0
		entry.due = time.Now().Add(recheck)
	} else {
		entry.delay = 0
		entry.due = time.Now()
		if !entry.dirty {
			delete(q.entries, key)
			q.notify()
			return
		}
	}
	q.next++
	entry.order = q.next
	heap.Push(&q.pending, entry)
	q.notify()
}

// scanSchedule retains failed inventory backoff across watch sessions. A successful scan permits immediate
// discovery after reconnect, and clears the backoff (ST-SUP-RP02-LC10).
type scanSchedule struct {
	due    time.Time
	delay  time.Duration
	active bool
}

func (s *scanSchedule) wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delay := time.Until(s.due)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *scanSchedule) finish(failed bool, now time.Time, options KeyedOptions) {
	s.active = false
	if failed {
		s.delay = nextDelay(s.delay, options.RetryMin, options.RetryMax)
		s.due = now.Add(s.delay)
	} else {
		s.delay = 0
		s.due = now.Add(options.ResyncInterval)
	}
}

func (s *scanSchedule) reconnect(now time.Time, minimum, maximum time.Duration) {
	if s.active {
		s.active = false
		s.delay = nextDelay(s.delay, minimum, maximum)
		s.due = now.Add(s.delay)
	} else if s.delay == 0 {
		s.due = time.Time{}
	}
}

// restart is called only after all callbacks from the old session have joined. Keep due times for queued failures.
// An interrupted action has no confirmed outcome, so retain it with the next delay. No key leaves the capacity
// bound (ST-SUP-RP02-LC04).
func (q *keyQueue[K]) restart(minimum, maximum time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ready = false
	now := time.Now()
	q.scan.reconnect(now, minimum, maximum)
	for _, entry := range q.entries {
		if !entry.active {
			continue
		}
		entry.active = false
		entry.dirty = true
		entry.delay = nextDelay(entry.delay, minimum, maximum)
		entry.due = now.Add(entry.delay)
		q.retrying++
		heap.Push(&q.pending, entry)
	}
	q.notify()
}

// RunKeyed owns bounded workers, the observer lifecycle, periodic scans, and a bounded queue. Only one action for
// each key can run at a time in this call. Different keys can run concurrently. The action must read current state
// and must support concurrent calls when Workers exceeds one. The runtime retries failures with a capped
// exponential delay. It does not retry terminal errors. The caller must make repeated actions safe. A pause prevents
// new takes; it cannot retract an external write already in progress. No distributed exclusion is supplied here.
func RunKeyed[K ~string](parent context.Context, source KeyedSource[K], reconcile func(context.Context, K) error, options KeyedOptions) error {
	if reconcile == nil {
		return errors.New("keyed controller requires an action")
	}
	return RunKeyedWithResult(parent, source, func(ctx context.Context, key K) (ReconcileResult, error) {
		return ReconcileResult{}, reconcile(ctx, key)
	}, options)
}

// RunKeyedWithResult also schedules expected incomplete work without a failure. Rechecks stay inside queue capacity
// and release the worker while waiting. Events and reconnects do not shorten a scheduled delay. A successful pending
// action resets prior error backoff; any action error keeps the error schedule. Invalid results stop the
// controller. A process restart requires a fresh scan.
func RunKeyedWithResult[K ~string](parent context.Context, source KeyedSource[K], reconcile func(context.Context, K) (ReconcileResult, error), options KeyedOptions) error {
	if parent == nil || source.Observe == nil || source.Scan == nil || reconcile == nil {
		return errors.New("keyed controller requires context, source, and action")
	}
	if err := options.validate(); err != nil {
		return err
	}
	if parent.Err() != nil {
		return nil
	}
	parent, closeTelemetry, err := startControllerTelemetry(parent)
	if err != nil {
		return err
	}
	defer closeTelemetry()
	q := newKeyQueue[K](options.Capacity)
	detach, err := attachControllerQueue(parent, q.metrics)
	if err != nil {
		return err
	}
	defer detach()
	detachMetrics, err := attachMetrics(options, q)
	if err != nil {
		return err
	}
	defer detachMetrics()
	return runKeyedWithResult(parent, source, reconcile, options, q)
}

// attachMetrics attaches the queue to the collector for the whole run, across watch sessions and reconnects, and
// gives the function that detaches it, which resets the gauges (ST-SUP-RP02-LC12).
func attachMetrics[K ~string](options KeyedOptions, q *keyQueue[K]) (func(), error) {
	if err := options.Metrics.attach(q.metrics); err != nil {
		return nil, err
	}
	options.Metrics.enableCleanup(options.Cleanup != nil)
	return options.Metrics.detach, nil
}

// runKeyed runs one keyed session on a queue that a caller made, with the collector attached for the session.
func runKeyed[K ~string](parent context.Context, source KeyedSource[K], reconcile func(context.Context, K) error, options KeyedOptions, q *keyQueue[K]) error {
	detach, err := attachMetrics(options, q)
	if err != nil {
		return err
	}
	defer detach()
	return runKeyedWithResult(parent, source, func(ctx context.Context, key K) (ReconcileResult, error) {
		return ReconcileResult{}, reconcile(ctx, key)
	}, options, q)
}

func runKeyedWithResult[K ~string](parent context.Context, source KeyedSource[K], reconcile func(context.Context, K) (ReconcileResult, error), options KeyedOptions, q *keyQueue[K]) error {
	if parent.Err() != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	var notices sync.Mutex
	notice := func(phase string, err error) {
		controllerNotice(ctx, phase)
		if options.Observe != nil {
			notices.Lock()
			defer notices.Unlock()
			options.Observe(Event{Phase: phase, Err: err})
		}
	}
	count := options.Workers
	if count == 0 {
		count = 1
	}
	failures := make(chan error, count+3)
	fail := func(err error) { failures <- err; cancel() }
	if options.Metrics != nil && options.Cleanup != nil {
		workers.Go(func() { runCleanup(ctx, options, notice, fail) })
	}
	workers.Go(func() {
		err := source.Observe(ctx, &KeySink[K]{queue: q})
		if err == nil {
			err = ErrSourceStopped
		}
		fail(err)
	})
	workers.Go(func() {
		for ctx.Err() == nil {
			if err := q.scan.wait(ctx); err != nil {
				return
			}
			if err := q.waitReady(ctx); err != nil {
				return
			}
			q.scan.active = true
			err, finish := controllerWork(ctx, "scan", func(operation context.Context) error {
				return source.Scan(operation, func(key K) error { return q.addWait(operation, key) })
			})
			terminal := err != nil && (options.Terminal(err) || errors.Is(err, ErrOverflow) || errors.Is(err, ErrKey))
			finish(err != nil && !terminal && ctx.Err() == nil)
			if ctx.Err() == nil {
				options.Metrics.scan(err != nil)
			}
			if terminal {
				fail(err)
				return
			}
			if ctx.Err() != nil {
				return
			}
			q.scan.finish(err != nil, time.Now(), options)
			if err != nil {
				notice("scan_failed", err)
			} else {
				notice("scan_completed", nil)
			}
		}
	})
	for i := 0; i < count; i++ {
		workers.Go(func() { keyWorker(ctx, ctx, q, reconcile, options, notice, fail) })
	}
	<-ctx.Done()
	cancel()
	workers.Wait()
	if parent.Err() != nil {
		return nil
	}
	var result []error
	for len(failures) > 0 {
		err := <-failures
		if options.Terminal(err) {
			return err
		}
		result = append(result, err)
	}
	return errors.Join(result...)
}

// keyWorker takes keys from the queue until take ends or an action fails terminally. Each action runs under run and
// its own timeout, so an end of take stops new takes and lets a running action finish (ST-SUP-RP02-LC10). In a
// session both contexts are the same.
func keyWorker[K ~string](take, run context.Context, q *keyQueue[K], reconcile func(context.Context, K) (ReconcileResult, error), options KeyedOptions, notice func(string, error), fail func(error)) {
	ctx := run
	for take.Err() == nil {
		key, err := q.take(take)
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		operation, stop := context.WithTimeout(ctx, options.Timeout)
		started := time.Now()
		result, err, finish := controllerResultWork(operation, func(operation context.Context) (ReconcileResult, error) { return reconcile(operation, key) })
		stop()
		terminal := err != nil && (errors.Is(err, ErrReconcileResult) || options.Terminal(err))
		outcome := outcomeSuccess
		if result.RecheckAfter > 0 {
			outcome = outcomePending
		}
		if err != nil {
			outcome = outcomeFailure
			if errors.Is(err, context.DeadlineExceeded) {
				outcome = outcomeTimeout
			} else if errors.Is(err, context.Canceled) {
				outcome = outcomeCanceled
			}
		}
		// Cancellation ends this queue; it does not schedule another attempt.
		retry := false
		duration := time.Since(started)
		if !terminal && ctx.Err() == nil {
			q.finishResult(key, err != nil, result.RecheckAfter, options.RetryMin, options.RetryMax)
			retry = err != nil
		}
		options.Metrics.action(outcome, duration, retry)
		finish(retry)
		if terminal {
			fail(err)
			return
		}
		if ctx.Err() != nil || take.Err() != nil {
			return
		}
		if err != nil {
			notice("reconcile_failed", err)
		} else if result.RecheckAfter > 0 {
			notice("reconcile_pending", nil)
		}
	}
}

// KeyedWatchOptions are the keyed options and the delay before a watch reconnects.
type KeyedWatchOptions struct {
	KeyedOptions
	ReconnectDelay time.Duration
}

// RunKeyedWatch accepts invalidation keys from a watch and retained-state scan. Every action must read
// authoritative state. The subscription opens before either scans or actions start. A failed watch cancels and joins
// all session callbacks before reconnecting. Each connection requests a new scan. Failed or interrupted scans retain
// their capped retry delay; successful scans permit immediate discovery. Scans and watch delivery wait for queue
// capacity. They retain one pending key each and stop on cancellation. Invalid keys and terminal errors stop the
// controller. Reconnects preserve retry delays and pending keys, and interrupted actions receive the next capped
// delay (ST-SUP-RP02-LC04). A process restart still loses the queue and its retry history. This function supplies
// process-local exclusion, not distributed fencing.
func RunKeyedWatch[K ~string](ctx context.Context, source Source[K], reconcile func(context.Context, K) error, options KeyedWatchOptions) error {
	return RunKeyedWatches(ctx, []Source[K]{source}, reconcile, options)
}

// RunKeyedWatches shares one bounded queue across 1..16 subscriptions. All subscriptions open before scans or
// actions start. Each receiver can retain one pending key. A failed subscription cancels and joins all receivers
// before reconnecting. Every source must scan its retained obligations. Scans run in source order and share the
// scan deadline. Keys must identify their resource kind when different sources can emit the same resource ID.
func RunKeyedWatches[K ~string](ctx context.Context, sources []Source[K], reconcile func(context.Context, K) error, options KeyedWatchOptions) error {
	if reconcile == nil {
		return errors.New("keyed watches require an action")
	}
	return RunKeyedWatchesWithResult(ctx, sources, func(ctx context.Context, key K) (ReconcileResult, error) {
		return ReconcileResult{}, reconcile(ctx, key)
	}, options)
}

// RunKeyedWatchWithResult adds explicit pending results to one watch source.
func RunKeyedWatchWithResult[K ~string](ctx context.Context, source Source[K], reconcile func(context.Context, K) (ReconcileResult, error), options KeyedWatchOptions) error {
	return RunKeyedWatchesWithResult(ctx, []Source[K]{source}, reconcile, options)
}

// RunKeyedWatchesWithResult retains the source and lifecycle rules above and the pending result rules of
// RunKeyedWithResult. Watch events cannot bypass either a pending recheck delay or a failed action's retry delay.
func RunKeyedWatchesWithResult[K ~string](ctx context.Context, sources []Source[K], reconcile func(context.Context, K) (ReconcileResult, error), options KeyedWatchOptions) error {
	if ctx == nil || reconcile == nil || len(sources) < 1 || len(sources) > 16 {
		return errors.New("keyed watches require context, action, and 1..16 sources")
	}
	sources = append([]Source[K](nil), sources...)
	for _, source := range sources {
		if source.Watch == nil || source.Scan == nil {
			return errors.New("keyed watch requires a source and scan")
		}
	}
	if err := options.KeyedOptions.validate(); err != nil {
		return err
	}
	if options.ReconnectDelay < time.Millisecond || options.ReconnectDelay > time.Minute {
		return errors.New("controller reconnect delay is outside its limits")
	}
	if ctx.Err() != nil {
		return nil
	}
	ctx, closeTelemetry, err := startControllerTelemetry(ctx)
	if err != nil {
		return err
	}
	defer closeTelemetry()
	q := newKeyQueue[K](options.Capacity)
	detach, err := attachControllerQueue(ctx, q.metrics)
	if err != nil {
		return err
	}
	defer detach()
	detachMetrics, err := attachMetrics(options.KeyedOptions, q)
	if err != nil {
		return err
	}
	defer detachMetrics()
	for ctx.Err() == nil {
		err, finish := controllerWork(ctx, "watch", func(operation context.Context) error {
			return keyedWatchSession(operation, sources, reconcile, options, q)
		})
		if ctx.Err() != nil {
			finish(false)
			return nil
		}
		terminal := err != nil && (errors.Is(err, ErrWatch) || errors.Is(err, ErrKey) || errors.Is(err, ErrTelemetryInUse) || errors.Is(err, ErrMetricsInUse) || errors.Is(err, ErrMetricsContract) || errors.Is(err, ErrReconcileResult) || options.Terminal(err))
		finish(ctx.Err() == nil && !terminal)
		if terminal {
			return err
		}
		q.restart(options.RetryMin, options.RetryMax)
		options.Metrics.reconnect()
		controllerNotice(ctx, "reconnect")
		if options.Observe != nil {
			options.Observe(Event{Phase: "reconnect", Err: err})
		}
		if err := drainDuringReconnect(ctx, q, reconcile, options); err != nil {
			return err
		}
	}
	return nil
}

// drainDuringReconnect runs the keys already queued while the watch waits for its reconnect delay, so a failed
// connection attempt does not delay them (ST-SUP-RP02-LC10). At the end of the delay it takes no new key, lets each
// running action finish under the parent and its own timeout, records it as any action, and joins the workers before
// the next session opens. It returns a terminal action error.
func drainDuringReconnect[K ~string](parent context.Context, q *keyQueue[K], reconcile func(context.Context, K) (ReconcileResult, error), options KeyedWatchOptions) error {
	run, stop := context.WithCancel(parent)
	defer stop()
	take, stopTaking := context.WithTimeout(run, options.ReconnectDelay)
	defer stopTaking()
	count := max(1, options.Workers)
	failures := make(chan error, count)
	fail := func(err error) { failures <- err; stop() }
	var notices sync.Mutex
	notice := func(phase string, err error) {
		controllerNotice(parent, phase)
		if options.Observe != nil {
			notices.Lock()
			defer notices.Unlock()
			options.Observe(Event{Phase: phase, Err: err})
		}
	}
	var workers sync.WaitGroup
	q.setReady(true)
	for range count {
		workers.Go(func() { keyWorker(take, run, q, reconcile, options.KeyedOptions, notice, fail) })
	}
	<-take.Done()
	workers.Wait()
	q.restart(options.RetryMin, options.RetryMax)
	select {
	case err := <-failures:
		return err
	default:
		return nil
	}
}

func keyedWatchSession[K ~string](parent context.Context, sources []Source[K], reconcile func(context.Context, K) (ReconcileResult, error), options KeyedWatchOptions, q *keyQueue[K]) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	setup := time.AfterFunc(options.Timeout, cancel)
	receives := make([]func() (K, error), 0, len(sources))
	for _, source := range sources {
		receive, err := source.Watch(ctx)
		if err != nil {
			setup.Stop()
			return err
		}
		if ctx.Err() != nil {
			setup.Stop()
			return ctx.Err()
		}
		if receive == nil {
			setup.Stop()
			return ErrWatch
		}
		receives = append(receives, receive)
	}
	setup.Stop()
	controllerNotice(ctx, "watch_started")
	if options.Observe != nil {
		options.Observe(Event{Phase: "watch_started"})
	}
	return runKeyedWithResult(parent, KeyedSource[K]{
		Observe: func(operation context.Context, sink *KeySink[K]) error {
			stop := context.AfterFunc(operation, cancel)
			defer stop()
			var readers sync.WaitGroup
			failures := make(chan error, len(receives))
			defer func() { cancel(); readers.Wait() }()
			sink.SetReady(true)
			for _, receive := range receives {
				readers.Go(func() {
					for ctx.Err() == nil {
						key, err := receive()
						if err == nil && ctx.Err() == nil {
							err = sink.AddWait(ctx, key)
						}
						if err != nil {
							failures <- err
							cancel()
							return
						}
					}
				})
			}
			<-ctx.Done()
			readers.Wait()
			if operation.Err() != nil {
				return operation.Err()
			}
			select {
			case err := <-failures:
				return err
			default:
				return ctx.Err()
			}
		},
		Scan: func(ctx context.Context, emit func(K) error) error {
			for _, source := range sources {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := source.Scan(ctx, emit); err != nil {
					return err
				}
			}
			return nil
		},
	}, reconcile, options.KeyedOptions, q)
}
