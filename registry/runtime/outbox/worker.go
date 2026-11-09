package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
)

// Handler delivers one message to its destination. tx is a transaction of the message's tenant, which the worker
// opened with tenant.Begin (ST-SUP-RP01-LC19): a consumer writes through it, and the worker moves the message to
// done in it and commits both together when the handler returns nil, or rolls it back otherwise. The handler must
// not commit or roll back tx. It must return only after the delivery succeeds or fails, must honor the deadline of
// ctx, and must send d.OutgoingKey as the idempotency key of every request. It marks an error with NotSent,
// TransientRefusal, PermanentRefusal or Conflict when it knows the outcome; an error with no mark is an outcome that
// is not known (ST-SUP-RP01-LC09). A panic is an outcome that is not known (ST-SUP-RP01-LC27).
type Handler func(ctx context.Context, tx *sql.Tx, d Delivery) error

// Destination is the registration of one destination on the worker (ST-SUP-RP01-LC14): its name, its handler,
// whether it is idempotent, whether its messages must land, and an optional status lookup with its settle time. A
// destination that does not say it is idempotent or must land counts as neither.
type Destination struct {
	Name       string
	Handler    Handler
	Idempotent bool
	MustLand   bool
	Lookup     Lookup
	SettleTime time.Duration
	// StateHook, when set, follows the state of the destination's messages in the transaction of each change
	// (ST-SUP-RP01-LC25).
	StateHook StateHook
}

// WorkerConfig holds the limits of the worker. NewWorker refuses a value outside its bounds (ST-SUP-RC18-OC16).
type WorkerConfig struct {
	Concurrency    int
	PollInterval   time.Duration
	AttemptTimeout time.Duration
	LeaseDuration  time.Duration
	RetryMin       time.Duration
	RetryMax       time.Duration
	// MaxAttempts is the attempts limit, 1 to 1000: a message whose attempts reach it is parked, unless its
	// destination must land (ST-SUP-RP01-LC12, ST-SUP-RP01-LC23). DefaultWorkerConfig gives DefaultMaxAttempts.
	MaxAttempts int64
	// LookupBound is the most status lookups of one message before it is parked (ST-SUP-RP01-LC11). Zero is
	// DefaultLookups, 10.
	LookupBound int
	// Meter gives the delivery delay, the oldest age and the park metric (ST-SUP-RP01-LC07, ST-SUP-RP01-LC12), such
	// as the MeterProvider of the otel runtime. Nil records no metric.
	Meter metric.MeterProvider
	// Logger gives the alert event of a parked message (ST-SUP-RP01-LC12). Nil emits no event.
	Logger otellog.LoggerProvider
	// Decider is the decision port that Resolve asks (ST-SUP-RP01-LC17). Nil refuses every resolution.
	Decider Decider
	// Auditor writes the audit row of a resolution (ST-SUP-RP01-LC13). Nil refuses every resolution.
	Auditor Auditor
	// Retention is how long a done message stays before the purge timer removes it, at least MinRetention
	// (ST-SUP-RP01-LC24). DefaultWorkerConfig gives DefaultRetention.
	Retention time.Duration
	// PurgeEvery is the period of the purge timer, 1 s to 1 h. DefaultWorkerConfig gives one minute.
	PurgeEvery time.Duration
}

// DefaultRetention is the retention of a done message when the configuration does not change it.
const DefaultRetention = 7 * 24 * time.Hour

// DefaultMaxAttempts is the attempts limit when the configuration gives none.
const DefaultMaxAttempts = 25

// DefaultWorkerConfig returns the default limits.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		Concurrency: 4, PollInterval: 250 * time.Millisecond,
		AttemptTimeout: 10 * time.Second, LeaseDuration: 30 * time.Second,
		RetryMin: time.Second, RetryMax: 5 * time.Minute,
		MaxAttempts: DefaultMaxAttempts, LookupBound: DefaultLookups,
		Retention: DefaultRetention, PurgeEvery: time.Minute,
	}
}

// WorkerStats are the counters of the worker. They hold no payload and no error text.
type WorkerStats struct {
	Delivered           uint64
	Retried             uint64
	DeliveryFailures    uint64
	DatabaseFailures    uint64
	LostLeases          uint64
	UnknownDestinations uint64
	UnknownOutcomes     uint64
	Conflicts           uint64
	Lookups             uint64
	Parked              uint64
	StateHookFailures   uint64
	DonePurged          uint64
	PurgeBatches        uint64
}

// Worker claims messages and takes each one's next step. It is a library value: the service builds it, registers
// its destinations and calls Run; it owns no main loop beyond Run.
type Worker struct {
	queue        *Queue
	config       WorkerConfig
	decider      Decider
	auditor      Auditor
	metrics      *metrics
	mu           sync.Mutex
	destinations map[string]Destination
	running      atomic.Bool
	started      atomic.Bool
	stop         chan struct{}
	stopOnce     sync.Once
	drain        chan struct{}
	drainOnce    sync.Once
	finished     chan struct{}

	delivered           atomic.Uint64
	retried             atomic.Uint64
	deliveryFailures    atomic.Uint64
	databaseFailures    atomic.Uint64
	lostLeases          atomic.Uint64
	unknownDestinations atomic.Uint64
	unknownOutcomes     atomic.Uint64
	conflicts           atomic.Uint64
	lookups             atomic.Uint64
	parked              atomic.Uint64
	hookFailures        atomic.Uint64
	donePurged          atomic.Uint64
	purgeBatches        atomic.Uint64
}

// NewWorker builds a worker on queue with config. It refuses a limit outside its bounds, and a pool of the queue
// that holds fewer than Concurrency + 2 connections: each step in flight holds one connection for the transaction of
// its tenant, the claims need one, and the purge timer one. A pool with no limit is accepted.
func NewWorker(queue *Queue, config WorkerConfig) (*Worker, error) {
	if queue == nil || queue.db == nil {
		return nil, errors.New("outbox: the worker needs a queue")
	}
	if config.LookupBound == 0 {
		config.LookupBound = DefaultLookups
	}
	if config.Concurrency < 1 || config.Concurrency > MaxBatchSize ||
		config.PollInterval < 10*time.Millisecond || config.PollInterval > time.Minute ||
		config.AttemptTimeout < 10*time.Millisecond || config.AttemptTimeout > time.Minute ||
		config.LeaseDuration < config.AttemptTimeout+2*operationTimeout+time.Second || config.LeaseDuration > 5*time.Minute ||
		config.RetryMin < 10*time.Millisecond || config.RetryMax < config.RetryMin || config.RetryMax > 24*time.Hour ||
		config.MaxAttempts < 1 || config.MaxAttempts > 1000 || config.LookupBound < 1 || config.LookupBound > 1000 ||
		config.Retention < MinRetention || config.PurgeEvery < time.Second || config.PurgeEvery > time.Hour {
		return nil, errors.New("outbox: a worker limit is out of its bounds")
	}
	// Each step in flight holds one connection for its tenant transaction, and the claim and the purge timer need
	// one more each.
	if limit := queue.db.Stats().MaxOpenConnections; limit > 0 && limit < config.Concurrency+2 {
		return nil, fmt.Errorf("outbox: the pool holds at most %d connections; the worker needs Concurrency + 2 = %d",
			limit, config.Concurrency+2)
	}
	w := &Worker{queue: queue, config: config, decider: config.Decider, auditor: config.Auditor,
		destinations: map[string]Destination{}, stop: make(chan struct{}), drain: make(chan struct{}),
		finished: make(chan struct{})}
	m, err := newMetrics(config.Meter, config.Logger, w)
	if err != nil {
		return nil, err
	}
	w.metrics = m
	return w, nil
}

// Register registers one destination on the worker before Run (ST-SUP-RP01-LC14). It keeps a copy of d. It refuses
// a name that is not a label, a nil handler, a name registered already, a settle time with no lookup, and a status
// lookup without a positive settle time. A destination that is neither idempotent nor has a status lookup is
// accepted: its unknown outcomes are parked.
func (w *Worker) Register(d Destination) error {
	if !labelPattern.MatchString(d.Name) || d.Handler == nil {
		return errors.New("outbox: a destination needs a label name and a handler")
	}
	if d.Lookup != nil && d.SettleTime <= 0 {
		return fmt.Errorf("outbox: the status lookup of %s needs a positive settle time", d.Name)
	}
	if d.Lookup == nil && d.SettleTime != 0 {
		return fmt.Errorf("outbox: %s has a settle time and no status lookup", d.Name)
	}
	if d.SettleTime > time.Hour {
		return fmt.Errorf("outbox: the settle time of %s is longer than one hour", d.Name)
	}
	if w.started.Load() {
		return errors.New("outbox: a destination is registered before Run")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.destinations[d.Name]; ok {
		return fmt.Errorf("outbox: %s is registered already", d.Name)
	}
	w.destinations[d.Name] = d
	return nil
}

// Registration is what the worker holds for one destination.
type Registration struct {
	Name       string
	Idempotent bool
	MustLand   bool
	HasLookup  bool
	SettleTime time.Duration
}

// Registrations lists the registered destinations.
func (w *Worker) Registrations() []Registration {
	w.mu.Lock()
	defer w.mu.Unlock()
	var all []Registration
	for _, d := range w.destinations {
		all = append(all, Registration{Name: d.Name, Idempotent: d.Idempotent, MustLand: d.MustLand,
			HasLookup: d.Lookup != nil, SettleTime: d.SettleTime})
	}
	return all
}

// Stats returns the counters.
func (w *Worker) Stats() WorkerStats {
	return WorkerStats{
		Delivered: w.delivered.Load(), Retried: w.retried.Load(),
		DeliveryFailures: w.deliveryFailures.Load(), DatabaseFailures: w.databaseFailures.Load(),
		LostLeases: w.lostLeases.Load(), UnknownDestinations: w.unknownDestinations.Load(),
		UnknownOutcomes: w.unknownOutcomes.Load(), Conflicts: w.conflicts.Load(), Lookups: w.lookups.Load(),
		Parked: w.parked.Load(), StateHookFailures: w.hookFailures.Load(), DonePurged: w.donePurged.Load(),
		PurgeBatches: w.purgeBatches.Load(),
	}
}

// Run claims and steps messages with at most Concurrency at one time (ST-SUP-RC18-OC17) until StopClaiming or the
// end of ctx. Claims run on a claim context that StopClaiming ends; handlers and lookups run on a separate work
// context that only the end of ctx or the drain bound of Finish ends, so StopClaiming lets every claimed message
// finish its step (ST-SUP-RP09-LC06). Run waits for every step that started, and a step that completed is still
// recorded (ST-SUP-RC18-OC23). A handler that ignores its context can hold up the end; no goroutine is left behind
// to hide that. Run returns nil after StopClaiming, the cancellation of ctx when ctx ended, and an error on a second
// run.
func (w *Worker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("outbox: the worker needs a context")
	}
	if !w.running.CompareAndSwap(false, true) {
		return errors.New("outbox: the worker is running already")
	}
	w.started.Store(true)
	defer close(w.finished)
	claimCtx, stopClaims := context.WithCancel(ctx)
	defer stopClaims()
	workCtx, stopWork := context.WithCancel(ctx)
	defer stopWork()
	go func() {
		select {
		case <-w.stop:
			stopClaims()
		case <-claimCtx.Done():
		}
	}()
	go func() {
		select {
		case <-w.drain:
			stopWork()
		case <-workCtx.Done():
		}
	}()
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		w.purgeTimer(claimCtx)
	}()
	for i := 0; i < w.config.Concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w.runOne(claimCtx, workCtx)
		}()
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// StopClaiming stops new claims; Run then returns once the claimed messages are finished. It is the stop step of
// the worker mode's shutdown (ST-SUP-RP09-LC06). It does not cancel a handler that runs.
func (w *Worker) StopClaiming() { w.stopOnce.Do(func() { close(w.stop) }) }

// Finish waits until Run returned. It is the finish step of the worker mode's shutdown, and the end of ctx is its
// drain bound: then Finish cancels the work context of the running handlers, waits a short time for Run to record
// their results, and returns the end of ctx. Finish also waits for a Run that has not started yet, so call it only
// for a worker whose Run is started.
func (w *Worker) Finish(ctx context.Context) error {
	select {
	case <-w.finished:
		return nil
	case <-ctx.Done():
	}
	w.drainOnce.Do(func() { close(w.drain) })
	timer := time.NewTimer(operationTimeout + time.Second)
	defer timer.Stop()
	select {
	case <-w.finished:
	case <-timer.C:
	}
	return ctx.Err()
}

func (w *Worker) runOne(claimCtx, workCtx context.Context) {
	var databaseAttempts int64
	for claimCtx.Err() == nil {
		n, err := w.cycle(claimCtx, workCtx)
		if err != nil {
			if claimCtx.Err() != nil {
				return
			}
			w.databaseFailures.Add(1)
			if databaseAttempts < 63 {
				databaseAttempts++
			}
			delay := jitterBackoff(w.config.PollInterval, max(5*time.Second, w.config.PollInterval), databaseAttempts)
			if !waitWorker(claimCtx, delay) {
				return
			}
			continue
		}
		databaseAttempts = 0
		if n == 0 && !waitWorker(claimCtx, w.config.PollInterval) {
			return
		}
	}
}

// purgeTimer purges done messages every PurgeEvery until ctx ends (ST-SUP-RP01-LC24).
func (w *Worker) purgeTimer(ctx context.Context) {
	ticker := time.NewTicker(w.config.PurgeEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, _, err := w.purgeTick(ctx); err != nil && ctx.Err() == nil {
				w.databaseFailures.Add(1)
			}
		}
	}
}

// purgeTick removes the done messages older than the retention, in batches of MaxBatchSize through the relay
// function purge_done, until a batch is not full or 1000 batches ran. It returns how many it removed and how many
// batches ran.
func (w *Worker) purgeTick(ctx context.Context) (int64, int, error) {
	var removed int64
	for batch := 1; batch <= 1000; batch++ {
		n, err := w.queue.purgeDone(ctx, MaxBatchSize, w.config.Retention)
		if err != nil {
			return removed, batch, err
		}
		removed += n
		w.donePurged.Add(uint64(n))
		w.purgeBatches.Add(1)
		if n < MaxBatchSize {
			return removed, batch, nil
		}
	}
	return removed, 1000, nil
}

// cycle claims at most one message on claimCtx and takes its next step on workCtx. It claims only when this worker
// can deliver, so a waiting batch never spends its lease before its step starts.
func (w *Worker) cycle(claimCtx, workCtx context.Context) (int, error) {
	deliveries, err := w.queue.Claim(claimCtx, 1, w.config.LeaseDuration)
	if err != nil {
		return 0, err
	}
	for _, d := range deliveries {
		w.step(workCtx, d)
	}
	return len(deliveries), nil
}

// Purge removes at most limit parked messages that were parked before olderThan (ST-SUP-RP01-LC16). It never removes
// a message that left outcome unknown or reconciling, a message of a destination registered as must land, or of a
// destination this worker has no registration for, until a person resolves it; and it never removes a discarded
// message.
func (w *Worker) Purge(ctx context.Context, limit int, olderThan time.Duration) (int64, error) {
	w.mu.Lock()
	var purgeable []string
	for name, d := range w.destinations {
		if !d.MustLand {
			purgeable = append(purgeable, name)
		}
	}
	w.mu.Unlock()
	return w.queue.purge(ctx, limit, olderThan, purgeable)
}

func (w *Worker) retryDelay(attempts int64) time.Duration {
	return jitterBackoff(w.config.RetryMin, w.config.RetryMax, attempts)
}

// unknownDelay is the wait of a message whose destination this worker has no registration for. It stays queued
// (ST-SUP-RP01-LC03); another worker of a newer release may hold the registration.
func (w *Worker) unknownDelay() time.Duration {
	return jitterBackoff(w.config.RetryMin, w.config.RetryMax, 4)
}

func jitterBackoff(minimum, maximum time.Duration, attempts int64) time.Duration {
	delay := minimum
	for i := int64(1); i < attempts && delay < maximum; i++ {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	// The jitter spreads the retries after a shared outage.
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(delay-half)+1))
}

func waitWorker(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// metrics are the instruments of ST-SUP-RP01-LC07 and ST-SUP-RP01-LC12.
type metrics struct {
	delay   metric.Float64Histogram
	parks   metric.Int64Counter
	hooks   metric.Int64Counter
	logger  otellog.Logger
	gauge   metric.Registration
	enabled bool
}

const instrumentation = "stego.dev/runtime/outbox"

func newMetrics(provider metric.MeterProvider, logs otellog.LoggerProvider, w *Worker) (*metrics, error) {
	m := &metrics{}
	if logs != nil {
		m.logger = logs.Logger(instrumentation)
	}
	if provider == nil {
		return m, nil
	}
	meter := provider.Meter(instrumentation)
	var err error
	m.delay, err = meter.Float64Histogram("outbox.delivery.delay", metric.WithUnit("s"),
		metric.WithDescription("The time from the enqueue of a message in the caller's transaction to done."))
	if err != nil {
		return nil, err
	}
	m.parks, err = meter.Int64Counter("outbox.parked",
		metric.WithDescription("The messages parked, by destination and the state they left."))
	if err != nil {
		return nil, err
	}
	m.hooks, err = meter.Int64Counter("outbox.hook.failures",
		metric.WithDescription("The state hook calls that failed or panicked, by destination."))
	if err != nil {
		return nil, err
	}
	oldest, err := meter.Float64ObservableGauge("outbox.oldest.age", metric.WithUnit("s"),
		metric.WithDescription("The age of the oldest message of each destination that is not done."))
	if err != nil {
		return nil, err
	}
	m.gauge, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		ages, err := w.oldestAges(ctx)
		if err != nil {
			return err
		}
		for destination, age := range ages {
			o.ObserveFloat64(oldest, age, metric.WithAttributes(attribute.String("destination", destination)))
		}
		return nil
	}, oldest)
	if err != nil {
		return nil, err
	}
	m.enabled = true
	return m, nil
}

// delivered records the delivery delay of one message that reached done, labelled by its destination only.
func (m *metrics) delivered(ctx context.Context, destination string, delay time.Duration) {
	if m == nil || !m.enabled {
		return
	}
	m.delay.Record(ctx, delay.Seconds(), metric.WithAttributes(attribute.String("destination", destination)))
}

// parked raises the one alert event and metric of a parked message, which name the destination and the state the
// message left.
func (m *metrics) parked(ctx context.Context, destination string, from State) {
	if m == nil {
		return
	}
	if m.enabled {
		m.parks.Add(ctx, 1, metric.WithAttributes(attribute.String("destination", destination),
			attribute.String("state", string(from))))
	}
	if m.logger != nil {
		var record otellog.Record
		record.SetEventName("outbox.parked")
		record.SetSeverity(otellog.SeverityWarn)
		record.SetSeverityText("WARN")
		record.SetBody(attribute.StringValue("an outbox message is parked and waits for a person"))
		record.AddAttributes(attribute.String("destination", destination), attribute.String("state", string(from)))
		m.logger.Emit(ctx, record)
	}
}

// hookFailed raises the alert event and metric of a state hook that failed, which name the destination.
func (m *metrics) hookFailed(ctx context.Context, destination string) {
	if m == nil {
		return
	}
	if m.enabled {
		m.hooks.Add(ctx, 1, metric.WithAttributes(attribute.String("destination", destination)))
	}
	if m.logger != nil {
		var record otellog.Record
		record.SetEventName("outbox.hook_failed")
		record.SetSeverity(otellog.SeverityError)
		record.SetSeverityText("ERROR")
		record.SetBody(attribute.StringValue("an outbox state hook failed, so the state change was rolled back"))
		record.AddAttributes(attribute.String("destination", destination))
		m.logger.Emit(ctx, record)
	}
}

// oldestAges reads the age of the oldest message of each destination that is not done, and gives 0 for each
// registered destination with no such message (ST-SUP-RP01-LC07). A discarded message is not waiting, so it does
// not count.
func (w *Worker) oldestAges(ctx context.Context) (map[string]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	ages := map[string]float64{}
	w.mu.Lock()
	for name := range w.destinations {
		ages[name] = 0
	}
	w.mu.Unlock()
	// The relay function oldest reads every tenant's messages (ST-SUP-RP01-LC19).
	rows, err := w.queue.db.QueryContext(ctx, `SELECT * FROM stego_outbox.oldest($1)`, w.queue.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var destination string
		var age float64
		if err := rows.Scan(&destination, &age); err != nil {
			return nil, err
		}
		ages[destination] = age
	}
	return ages, rows.Err()
}
