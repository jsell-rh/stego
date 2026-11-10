package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/probe"
)

// The outcome of one completed action, in the order of the action counters.
const (
	outcomeSuccess = iota
	outcomeFailure
	outcomeTimeout
	outcomeCanceled
	outcomePending
)

// ErrMetricsContract reports a cleanup summary that contradicts itself. It stops the controller (ST-SUP-RP02-LC18).
var ErrMetricsContract = errors.New("invalid controller cleanup summary")

// ErrMetricsInUse reports a collector that another running controller holds (ST-SUP-RP02-LC12).
var ErrMetricsInUse = errors.New("controller metrics already have an active queue")

// ErrTelemetryInUse reports a sixty-fifth queue in one telemetry runtime (ST-SUP-RP02-LC14).
var ErrTelemetryInUse = errors.New("controller telemetry queue limit reached")

// ErrRunAborted reports a Monitor callback that panicked or exited without return. It never includes or wraps the
// panic value.
var ErrRunAborted = errors.New("controller run callback aborted before return")

var errControllerPanic = errors.New("controller callback did not return")

// QueueLimit is the most queues that one telemetry runtime holds at a time.
const QueueLimit = 64

// Metrics is the collector of one active keyed controller. It retains counters across watch reconnects and resets
// its gauges when the queue stops. Its zero value is ready for use. Do not copy it after use. It retains no resource
// keys, payloads, errors, or user-selected labels, and its size does not grow with the resource count
// (ST-SUP-RP02-LC12).
type Metrics struct {
	mu                               sync.Mutex
	queue                            func() QueueMetrics
	actions                          [5]uint64
	retries                          uint64
	scans                            [2]uint64
	reconnects                       uint64
	durations                        [10]uint64
	seconds                          float64
	cleanupEnabled, cleanupAvailable bool
	cleanupPending                   int64
	cleanupOldest, cleanupSampleAt   time.Time
	cleanupReads                     [2]uint64
}

// QueueMetrics counts admitted keys. Retrying is a subset of Queued and includes retries whose delay has elapsed.
// Waiting counts AddWait callers, including callers waiting for the serialized admission path.
type QueueMetrics struct {
	Capacity, Queued, Active, Retrying int
	Waiting                            int64
	Ready                              bool
}

// MetricsSnapshot is one consistent read of a collector.
type MetricsSnapshot struct {
	Running                                                                                          bool
	Queue                                                                                            QueueMetrics
	Succeeded, Failed, TimedOut, Canceled, Pending, Retries, ScansSucceeded, ScansFailed, Reconnects uint64
	DurationBuckets                                                                                  [10]uint64
	DurationSeconds                                                                                  float64
	CleanupEnabled, CleanupAvailable                                                                 bool
	CleanupPending                                                                                   int64
	CleanupOldest, CleanupSampleAt                                                                   time.Time
	CleanupReadsSucceeded, CleanupReadsFailed                                                        uint64
}

// durationBounds are the fixed upper bounds, in seconds, of the action duration histogram.
var durationBounds = [10]float64{0.001, 0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60}

func (m *Metrics) attach(queue func() QueueMetrics) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.queue != nil {
		return ErrMetricsInUse
	}
	m.queue = queue
	return nil
}

func (m *Metrics) detach() {
	if m != nil {
		m.mu.Lock()
		m.queue = nil
		m.cleanupAvailable = false
		m.mu.Unlock()
	}
}

// Snapshot reads the counters and, while a queue runs, its gauges.
func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	m.mu.Lock()
	s := MetricsSnapshot{Succeeded: m.actions[outcomeSuccess], Failed: m.actions[outcomeFailure], TimedOut: m.actions[outcomeTimeout], Canceled: m.actions[outcomeCanceled], Pending: m.actions[outcomePending], Retries: m.retries, ScansSucceeded: m.scans[0], ScansFailed: m.scans[1], Reconnects: m.reconnects, DurationBuckets: m.durations, DurationSeconds: m.seconds}
	s.CleanupEnabled = m.cleanupEnabled
	s.CleanupAvailable = m.cleanupAvailable
	s.CleanupPending = m.cleanupPending
	s.CleanupOldest = m.cleanupOldest
	s.CleanupSampleAt = m.cleanupSampleAt
	s.CleanupReadsSucceeded = m.cleanupReads[0]
	s.CleanupReadsFailed = m.cleanupReads[1]
	queue := m.queue
	// Queue snapshots do not call Metrics. This lock keeps detach and a later attach from changing the run while its
	// queue is read.
	if queue != nil {
		s.Running = true
		s.Queue = queue()
	}
	m.mu.Unlock()
	return s
}

// QueueReady tells whether a queue is attached and running and its source permits new actions. The controller mode
// of the modes runtime reads it for its readiness (ST-SUP-RP09-LC05). A nil collector has no queue.
func (m *Metrics) QueueReady() bool {
	s := m.Snapshot()
	return s.Running && s.Queue.Ready
}

func (m *Metrics) action(outcome int, duration time.Duration, retry bool) {
	if m == nil {
		return
	}
	seconds := duration.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.actions[outcome]++
	m.seconds += seconds
	if retry {
		m.retries++
	}
	for i, limit := range durationBounds {
		if seconds <= limit {
			m.durations[i]++
		}
	}
}

func (m *Metrics) scan(failed bool) {
	if m != nil {
		m.mu.Lock()
		if failed {
			m.scans[1]++
		} else {
			m.scans[0]++
		}
		m.mu.Unlock()
	}
}

func (m *Metrics) reconnect() {
	if m != nil {
		m.mu.Lock()
		m.reconnects++
		m.mu.Unlock()
	}
}

// CleanupSample contains one authorized cleanup scope. OldestPending is the original deletion time, including work
// reopened after an earlier success. It must be nil exactly when Pending is zero. It carries no resource IDs.
type CleanupSample struct {
	Pending       int64
	OldestPending *time.Time
}

func (m *Metrics) enableCleanup(enabled bool) {
	if m != nil {
		m.mu.Lock()
		m.cleanupEnabled = enabled
		m.cleanupAvailable = false
		m.mu.Unlock()
	}
}

// recordCleanup keeps a good sample. A failed or malformed read keeps the last values but marks them unavailable
// and counts the failed read on its own counter (ST-SUP-RP02-LC18).
func (m *Metrics) recordCleanup(sample CleanupSample, err error) error {
	if err == nil && (sample.Pending < 0 || (sample.Pending == 0) != (sample.OldestPending == nil) || (sample.OldestPending != nil && sample.OldestPending.IsZero())) {
		err = ErrMetricsContract
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.cleanupAvailable = false
		m.cleanupReads[1]++
		return err
	}
	m.cleanupAvailable = true
	m.cleanupPending = sample.Pending
	m.cleanupOldest = time.Time{}
	if sample.OldestPending != nil {
		m.cleanupOldest = *sample.OldestPending
	}
	m.cleanupSampleAt = time.Now()
	m.cleanupReads[0]++
	return nil
}

// runCleanup reads the cleanup summary on its own worker, under the action timeout and at the resync interval, until
// ctx ends. A malformed sample or a terminal error stops the controller; any other failure is reported and sampled
// again (ST-SUP-RP02-LC18).
func runCleanup(ctx context.Context, options KeyedOptions, notice func(string, error), fail func(error)) {
	for ctx.Err() == nil {
		operation, stop := context.WithTimeout(ctx, options.Timeout)
		err, finish := controllerWork(operation, "cleanup", func(operation context.Context) error {
			sample, err := options.Cleanup(operation)
			if err == nil {
				err = operation.Err()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return options.Metrics.recordCleanup(sample, err)
		})
		stop()
		finish(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil && (errors.Is(err, ErrMetricsContract) || options.Terminal(err)) {
			fail(err)
			return
		}
		if err != nil {
			notice("metrics_failed", err)
		}
		timer := time.NewTimer(options.ResyncInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// ServeHTTP exports Prometheus text at /metrics with no-store and nosniff. It answers not found to any other path
// and method not allowed to any method but GET. Metric names, labels, and bucket limits are fixed: the only label is
// the outcome, besides the bucket bound of the histogram, and no application data is rendered (ST-SUP-RP02-LC12,
// ST-SUP-RP02-LC13). The cleanup gauges are part of the set only when a cleanup summary source is configured
// (ST-SUP-RP02-LC18).
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s := m.Snapshot()
	gauge := func(name, help string, value any) {
		fmt.Fprintf(w, "# HELP stego_controller_%s %s\n# TYPE stego_controller_%s gauge\nstego_controller_%s %v\n", name, help, name, name, value)
	}
	boolean := func(value bool) int {
		if value {
			return 1
		}
		return 0
	}
	gauge("running", "An active keyed queue is attached.", boolean(s.Running))
	gauge("ready", "The source permits new actions.", boolean(s.Queue.Ready))
	gauge("capacity", "Maximum admitted keys.", s.Queue.Capacity)
	gauge("queued_keys", "Admitted keys waiting for an action.", s.Queue.Queued)
	gauge("active_keys", "Keys with an active action.", s.Queue.Active)
	gauge("retry_keys", "Queued keys waiting for a retry.", s.Queue.Retrying)
	gauge("admission_waiters", "Calls in the waiting admission path.", s.Queue.Waiting)
	fmt.Fprint(w, "# HELP stego_controller_actions_total Completed actions by outcome.\n# TYPE stego_controller_actions_total counter\n")
	for i, label := range []string{"success", "failure", "timeout", "canceled", "pending"} {
		fmt.Fprintf(w, "stego_controller_actions_total{outcome=%q} %d\n", label, []uint64{s.Succeeded, s.Failed, s.TimedOut, s.Canceled, s.Pending}[i])
	}
	fmt.Fprintf(w, "# HELP stego_controller_retries_total Retries scheduled after failed actions.\n# TYPE stego_controller_retries_total counter\nstego_controller_retries_total %d\n", s.Retries)
	fmt.Fprint(w, "# HELP stego_controller_scans_total Completed scans by outcome.\n# TYPE stego_controller_scans_total counter\n")
	fmt.Fprintf(w, "stego_controller_scans_total{outcome=\"success\"} %d\nstego_controller_scans_total{outcome=\"failure\"} %d\n", s.ScansSucceeded, s.ScansFailed)
	fmt.Fprintf(w, "# HELP stego_controller_reconnects_total Watch reconnect attempts.\n# TYPE stego_controller_reconnects_total counter\nstego_controller_reconnects_total %d\n", s.Reconnects)
	fmt.Fprint(w, "# HELP stego_controller_action_duration_seconds Completed action duration including API and provider calls.\n# TYPE stego_controller_action_duration_seconds histogram\n")
	for i, limit := range durationBounds {
		fmt.Fprintf(w, "stego_controller_action_duration_seconds_bucket{le=\"%g\"} %d\n", limit, s.DurationBuckets[i])
	}
	total := s.Succeeded + s.Failed + s.TimedOut + s.Canceled + s.Pending
	fmt.Fprintf(w, "stego_controller_action_duration_seconds_bucket{le=\"+Inf\"} %d\nstego_controller_action_duration_seconds_count %d\nstego_controller_action_duration_seconds_sum %g\n", total, total, s.DurationSeconds)
	if !s.CleanupEnabled {
		return
	}
	gauge("cleanup_enabled", "A cleanup summary source is configured.", boolean(s.CleanupEnabled))
	gauge("cleanup_available", "The last cleanup read succeeded.", boolean(s.CleanupAvailable))
	gauge("cleanup_pending_resources", "Pending deleted resources in the authorized cleanup scope.", s.CleanupPending)
	timestamp := func(t time.Time) float64 {
		if t.IsZero() {
			return 0
		}
		return float64(t.Unix()) + float64(t.Nanosecond())/1e9
	}
	gauge("cleanup_oldest_pending_timestamp_seconds", "Original deletion time of the oldest pending resource, or zero.", timestamp(s.CleanupOldest))
	gauge("cleanup_last_success_timestamp_seconds", "Local time of the last successful cleanup read, or zero.", timestamp(s.CleanupSampleAt))
	fmt.Fprint(w, "# HELP stego_controller_cleanup_reads_total Completed cleanup reads by outcome.\n# TYPE stego_controller_cleanup_reads_total counter\n")
	fmt.Fprintf(w, "stego_controller_cleanup_reads_total{outcome=\"success\"} %d\nstego_controller_cleanup_reads_total{outcome=\"failure\"} %d\n", s.CleanupReadsSucceeded, s.CleanupReadsFailed)
}

// Monitor runs one controller callback with an optional loopback metrics endpoint. It starts the telemetry runtime
// before the callback and holds it for the callback's whole life, so that setup, nested controllers and deferred
// cleanup share one runtime (ST-SUP-RP02-LC14). A nil telemetry owner starts no telemetry. An empty address gives
// the callback a nil collector and opens no server. Otherwise the address must be a literal loopback IP and a port
// from 0 through 65535 (0 picks a free port), and a listener failure returns before the callback starts. The server registers on no
// default mux. When either the server or the callback exits, Monitor closes the listener, cancels the shared context
// and joins both before it returns the first error (ST-SUP-RP02-LC13). The callback's value and error return
// unchanged. A callback abort returns ErrRunAborted after its defers finish.
func Monitor[V any](parent context.Context, address string, telemetry *Telemetry, run func(context.Context, *Metrics) (V, error)) (V, error) {
	var zero V
	if parent == nil || run == nil {
		return zero, errors.New("controller monitoring requires context and action")
	}
	if parent.Err() != nil {
		return zero, nil
	}
	if address == "" {
		completed := make(chan runResult[V], 1)
		go completeControllerRun(parent, telemetry, nil, run, completed)
		result := <-completed
		return result.value, result.err
	}
	if err := monitorAddress(address); err != nil {
		return zero, err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return zero, err
	}
	return monitorListener(parent, listener, telemetry, run)
}

type runResult[V any] struct {
	value V
	err   error
}

func monitorAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("controller metrics require a loopback IP and port")
	}
	ip, err := netip.ParseAddr(host)
	number, portErr := strconv.Atoi(port)
	if err != nil || !ip.IsLoopback() || ip.Zone() != "" || portErr != nil || number < 0 || number > 65535 || strconv.Itoa(number) != port {
		return errors.New("controller metrics require a loopback IP and port")
	}
	return nil
}

// monitorServer is the metrics server with its fixed header, read, write and idle limits and the MaxHeaderBytes
// setting of 4096 (ST-SUP-RP02-LC13).
func monitorServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 4096}
}

func monitorListener[V any](parent context.Context, listener net.Listener, telemetry *Telemetry, run func(context.Context, *Metrics) (V, error)) (V, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	metrics := new(Metrics)
	server := monitorServer(metrics)
	served, completed := make(chan error, 1), make(chan runResult[V], 1)
	go func() { served <- server.Serve(listener) }()
	go completeControllerRun(ctx, telemetry, metrics, run, completed)
	var serveErr error
	var result runResult[V]
	var serverDone, actionDone, serverFirst bool
	select {
	case serveErr = <-served:
		serverDone, serverFirst = true, true
	case result = <-completed:
		actionDone = true
	case <-ctx.Done():
	}
	cancel()
	_ = server.Close()
	if !serverDone {
		serveErr = <-served
	}
	if !actionDone {
		result = <-completed
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	if serverFirst && serveErr != nil {
		return result.value, serveErr
	}
	if result.err != nil {
		return result.value, result.err
	}
	return result.value, serveErr
}

// completeControllerRun always reports a result. The return flag also handles a panic and runtime.Goexit. The panic
// value is not inspected, formatted, or wrapped.
func completeControllerRun[V any](ctx context.Context, telemetry *Telemetry, metrics *Metrics, run func(context.Context, *Metrics) (V, error), completed chan<- runResult[V]) {
	returned := false
	var result runResult[V]
	var closeTelemetry func(error)
	defer func() {
		if !returned {
			recover()
			result = runResult[V]{err: ErrRunAborted}
		}
		if closeTelemetry != nil {
			closeTelemetry(result.err)
		}
		completed <- result
	}()
	ctx, closeTelemetry, result.err = startControllerProcessTelemetry(ctx, telemetry)
	if result.err != nil {
		returned = true
		return
	}
	result.value, result.err = run(ctx, metrics)
	returned = true
}

// ProcessOptions are the addresses and the telemetry of one controller process.
type ProcessOptions struct {
	// MonitorAddress is the loopback address of the metrics endpoint, or empty for none.
	MonitorAddress string
	// HealthAddress is the loopback address of the health endpoint that a probe command asks.
	HealthAddress string
	// Telemetry is the owner of the process's telemetry runtime, or nil for none.
	Telemetry *Telemetry
}

// Process runs one controller process and returns its exit code. With the single argument --stego-probe=live or
// --stego-probe=ready it asks the health endpoint once and starts no telemetry and no callback
// (ST-SUP-RP02-LC14). With no argument it runs Monitor. Any other argument returns 2. It never prints an error.
func Process(ctx context.Context, args []string, options ProcessOptions, run func(context.Context, *Metrics) error) int {
	if ctx == nil || run == nil {
		return 2
	}
	if len(args) != 0 {
		if len(args) != 1 {
			return 2
		}
		var path string
		switch args[0] {
		case "--stego-probe=live":
			path = "/livez"
		case "--stego-probe=ready":
			path = "/readyz"
		default:
			return 2
		}
		if monitorAddress(options.HealthAddress) != nil {
			return 2
		}
		if probeHealth(ctx, options.HealthAddress, path) != nil {
			return 1
		}
		return 0
	}
	if options.MonitorAddress != "" && monitorAddress(options.MonitorAddress) != nil {
		return 2
	}
	_, err := Monitor(ctx, options.MonitorAddress, options.Telemetry, func(ctx context.Context, metrics *Metrics) (struct{}, error) {
		return struct{}{}, run(ctx, metrics)
	})
	if err != nil {
		return 1
	}
	return 0
}

// probeBodyLimit is the largest body that a health answer may have. The modes health handler answers "live" or
// "ready" and a newline.
const probeBodyLimit = 64

// providerCloseTimeout bounds one close of the provider set.
const providerCloseTimeout = 3 * time.Second

// probeHealth asks one health path with a one-second bound and no redirect. Any 200 answer with a body of at most
// probeBodyLimit bytes passes.
func probeHealth(parent context.Context, address, path string) error {
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, DisableKeepAlives: true, MaxResponseHeaderBytes: 4096, ResponseHeaderTimeout: time.Second, Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, probeBodyLimit+1))
	if err != nil || response.StatusCode != http.StatusOK || len(body) > probeBodyLimit {
		return errors.New("controller probe failed")
	}
	return nil
}

// Providers is the provider set of the telemetry runtime that the controller signals use. *otel.Runtime satisfies
// it. A nil provider leaves its signal off.
type Providers interface {
	// StartOperation starts an operation span of a registered name (ST-SUP-RP03-LC15).
	StartOperation(ctx context.Context, parent otel.Parent, name string) (context.Context, otel.Finish, error)
	MeterProvider() metric.MeterProvider
	LoggerProvider() otellog.LoggerProvider
	// Registered tells whether the runtime registered an operation span name. Start fails when a name of SpanNames
	// is missing, so no span is dropped in silence.
	Registered(name string) bool
	// CollectorSet tells whether a collector endpoint was set; without one the logs also go to standard error
	// (ST-SUP-RP02-LC20).
	CollectorSet() bool
	Shutdown(context.Context) error
}

// operations are the controller operations that have a span.
var operations = [...]string{"reconcile", "scan", "watch", "cleanup"}

// SpanNames are the operation span names of the controller, which the generated code registers when it starts the
// telemetry runtime: otel.New(service, controller.SpanNames()...).
func SpanNames() []string {
	names := make([]string, len(operations))
	for i, operation := range operations {
		names[i] = "controller." + operation
	}
	return names
}

// ServiceEvents sends the fixed service events of the probe runtime. *probe.Runtime and *probe.Recorder satisfy it.
type ServiceEvents interface {
	Service(context.Context, probe.ServiceEvent)
}

// Telemetry owns the one private provider set that overlapping controllers of one process share: the first user
// starts it, the last user closes it, and a new user waits for an ongoing bounded close (ST-SUP-RP02-LC08,
// ST-SUP-RP02-LC14). The generated code makes one owner per process and passes it to Monitor.
type Telemetry struct {
	start   func() (Providers, error)
	events  ServiceEvents
	stderr  io.Writer
	mu      sync.Mutex
	users   int
	current *signals
}

// NewTelemetry makes the owner. start starts the telemetry runtime, such as otel.New for the service. events may be
// nil, which sends no service event.
func NewTelemetry(start func() (Providers, error), events ServiceEvents) (*Telemetry, error) {
	if start == nil {
		return nil, errors.New("controller telemetry requires a start function")
	}
	return &Telemetry{start: start, events: events, stderr: os.Stderr}, nil
}

type telemetryKey struct{}

// operationKey marks a context that already records controller work, so a helper inside it records nothing more.
type operationKey struct{}

type ownership struct {
	owner   *Telemetry
	signals *signals
}

// Start takes one use of the shared provider set and returns a context that carries it, and the function that gives
// the use back. The release function is safe to call more than once; only its first call counts.
func (t *Telemetry) Start(ctx context.Context) (context.Context, func(), error) {
	if t == nil || ctx == nil {
		return ctx, nil, errors.New("controller telemetry requires an owner and a context")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current == nil {
		providers, err := t.start()
		if err != nil {
			return ctx, nil, err
		}
		if providers == nil {
			return ctx, nil, errors.New("controller telemetry start returned no providers")
		}
		current, err := newSignals(providers, t.stderr)
		if err == nil {
			for _, name := range SpanNames() {
				if !providers.Registered(name) {
					current.close()
					return ctx, nil, errors.New("controller: the telemetry runtime does not register the span name " + name + "; start it with otel.New(service, controller.SpanNames()...)")
				}
			}
		}
		if err != nil {
			closing, cancel := context.WithTimeout(context.Background(), providerCloseTimeout)
			_ = providers.Shutdown(closing)
			cancel()
			return ctx, nil, err
		}
		t.current = current
	}
	t.users++
	current := t.current
	var once sync.Once
	release := func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.users--
			if t.users == 0 {
				current.close()
				t.current = nil
			}
		})
	}
	return context.WithValue(ctx, telemetryKey{}, &ownership{owner: t, signals: current}), release, nil
}

func ownershipOf(ctx context.Context) *ownership {
	if ctx == nil {
		return nil
	}
	o, _ := ctx.Value(telemetryKey{}).(*ownership)
	return o
}

// startControllerTelemetry takes one more use of the provider set that ctx carries, so a controller keeps it open
// for its own run. A context with no telemetry starts none.
func startControllerTelemetry(ctx context.Context) (context.Context, func(), error) {
	o := ownershipOf(ctx)
	if o == nil {
		return ctx, func() {}, nil
	}
	return o.owner.Start(ctx)
}

// startControllerProcessTelemetry holds one use from before the callback until after it returns. The returned
// function sends one service.failed event for an error that is not a cancellation, and never formats the error.
func startControllerProcessTelemetry(ctx context.Context, telemetry *Telemetry) (context.Context, func(error), error) {
	if telemetry == nil {
		return ctx, func(error) {}, nil
	}
	ctx, release, err := telemetry.Start(ctx)
	if err != nil {
		return ctx, nil, err
	}
	return ctx, func(err error) {
		if err != nil && !errors.Is(err, context.Canceled) && telemetry.events != nil {
			telemetry.events.Service(ctx, probe.ServiceFailed)
		}
		release()
	}, nil
}

// signals are the controller instruments, logger and tracer of one started provider set, and its registered queues.
type signals struct {
	providers Providers
	closed    atomic.Bool
	mu        sync.Mutex
	queues    map[*queueRegistration]func() QueueMetrics
	logger    otellog.Logger
	// local writes each completion and failure log as one JSON line when no collector is set.
	local    *localLog
	duration metric.Float64Histogram
	retries  metric.Int64Counter
	events   metric.Int64Counter
}

// queueRegistration names one registered queue. It has a field, so that each registration has its own address.
type queueRegistration struct{ _ byte }

// instrumentScope is the instrumentation scope of every controller signal.
const instrumentScope = "stego/controller"

// queueGauges are the names of the queue gauges after stego.controller.queue.
var queueGauges = [...]string{"running", "capacity", "queued", "active", "retrying", "waiting", "ready"}

// newSignals creates the fixed stego.controller.* instruments with no configurable label (ST-SUP-RP02-LC15).
func newSignals(providers Providers, stderr io.Writer) (*signals, error) {
	s := &signals{providers: providers, queues: make(map[*queueRegistration]func() QueueMetrics)}
	if !providers.CollectorSet() && stderr != nil {
		s.local = newLocalLog(stderr)
	}
	if provider := providers.LoggerProvider(); provider != nil {
		s.logger = provider.Logger(instrumentScope)
	}
	provider := providers.MeterProvider()
	if provider == nil {
		return s, nil
	}
	meter := provider.Meter(instrumentScope)
	failed := errors.New("cannot create a controller metric")
	var err error
	if s.duration, err = meter.Float64Histogram("stego.controller.work.duration", metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(0.001, 0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 1800, 3600)); err != nil {
		return nil, failed
	}
	if s.retries, err = meter.Int64Counter("stego.controller.retries", metric.WithUnit("{retry}")); err != nil {
		return nil, failed
	}
	if s.events, err = meter.Int64Counter("stego.controller.events", metric.WithUnit("{event}")); err != nil {
		return nil, failed
	}
	gauges := make([]metric.Int64ObservableGauge, len(queueGauges))
	instruments := make([]metric.Observable, len(queueGauges))
	for i, name := range queueGauges {
		if gauges[i], err = meter.Int64ObservableGauge("stego.controller.queue." + name); err != nil {
			return nil, failed
		}
		instruments[i] = gauges[i]
	}
	if _, err = meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		var values [len(queueGauges)]int64
		for _, read := range s.queues {
			q := read()
			values[0]++
			values[1] += int64(q.Capacity)
			values[2] += int64(q.Queued)
			values[3] += int64(q.Active)
			values[4] += int64(q.Retrying)
			values[5] += q.Waiting
			if q.Ready {
				values[6]++
			}
		}
		for i, value := range values {
			observer.ObserveInt64(gauges[i], value)
		}
		return nil
	}, instruments...); err != nil {
		return nil, errors.New("cannot register the controller metrics")
	}
	return s, nil
}

func (s *signals) close() {
	s.closed.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), providerCloseTimeout)
	defer cancel()
	if s.local != nil {
		s.local.drain(ctx)
	}
	_ = s.providers.Shutdown(ctx)
}

// attach registers one queue until the returned function runs. A sixty-fifth queue is refused.
func (s *signals) attach(queue func() QueueMetrics) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if queue == nil || len(s.queues) >= QueueLimit {
		return nil, ErrTelemetryInUse
	}
	registration := &queueRegistration{}
	s.queues[registration] = queue
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.queues, registration)
			s.mu.Unlock()
		})
	}, nil
}

// attachControllerQueue registers the queue of one run with the telemetry that ctx carries.
func attachControllerQueue(ctx context.Context, read func() QueueMetrics) (func(), error) {
	o := ownershipOf(ctx)
	if o == nil {
		return func() {}, nil
	}
	return o.signals.attach(read)
}

// workResult is the fixed completion of one operation. Error takes precedence over Pending. Retry counts failed
// attempts only. Pending applies to reconcile only.
type workResult struct {
	err            error
	retry, pending bool
}

// begin starts one operation as its own trace root. The context keeps the parent's cancellation, deadline and
// values, so the runtime ownership stays; only the trace ancestry changes (ST-SUP-RP02-LC19). The returned function
// records one fixed outcome, one duration sample and one completion log, and ends the span. It records no error text,
// key or payload (ST-SUP-RP02-LC07, ST-SUP-RP02-LC15).
func (s *signals) begin(ctx context.Context, operation string) (context.Context, func(workResult)) {
	if s.closed.Load() {
		return ctx, func(workResult) {}
	}
	start := time.Now()
	ctx, span, err := s.providers.StartOperation(ctx, otel.Root, "controller."+operation)
	if err != nil {
		span = nil
	}
	ctx = context.WithValue(ctx, operationKey{}, struct{}{})
	var once sync.Once
	return ctx, func(result workResult) {
		once.Do(func() {
			err, retry := result.err, result.retry
			end := time.Now()
			outcome := "success"
			if result.pending && operation == "reconcile" {
				outcome = "pending"
			}
			if err != nil {
				outcome = "failure"
				if errors.Is(err, context.DeadlineExceeded) {
					outcome = "timeout"
				} else if errors.Is(err, context.Canceled) {
					outcome = "canceled"
				}
			}
			retry = retry && err != nil
			attrs := []attribute.KeyValue{attribute.String("operation", operation), attribute.String("outcome", outcome), attribute.Bool("retry", retry)}
			duration := end.Sub(start).Seconds()
			if s.duration != nil {
				s.duration.Record(ctx, duration, metric.WithAttributes(attrs...))
				if retry {
					s.retries.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", operation)))
				}
			}
			severity := otellog.SeverityInfo
			if err != nil && outcome != "canceled" {
				severity = otellog.SeverityWarn
				if !retry {
					severity = otellog.SeverityError
				}
			}
			s.emit(ctx, end, "controller.work.completed", "Controller work completed", severity, attrs, &duration)
			if span != nil {
				span(otel.Outcome(outcome), retry, err != nil && outcome != "canceled")
			}
		})
	}
}

// notice is one fixed runtime event. An unknown phase records nothing.
func (s *signals) notice(ctx context.Context, phase string) {
	var name, message string
	severity := otellog.SeverityWarn
	switch phase {
	case "watch_started":
		name, message, severity = "controller.watch.started", "Controller watch started", otellog.SeverityInfo
	case "reconnect":
		name, message = "controller.watch.reconnect", "Controller watch will reconnect"
	case "metrics_failed":
		name, message = "controller.cleanup.sample_failed", "Controller cleanup summary is unavailable"
	case "scan_failed":
		name, message = "controller.scan.failed", "Controller scan pass failed"
	case "scan_completed":
		name, message, severity = "controller.scan.completed", "Controller scan pass completed", otellog.SeverityInfo
	case "reconcile_failed":
		name, message = "controller.reconcile.failed", "Controller reconcile failed"
	case "reconcile_pending":
		name, message = "controller.reconcile.pending", "Controller reconcile is pending"
	default:
		return
	}
	if s.closed.Load() {
		return
	}
	if s.events != nil {
		s.events.Add(ctx, 1, metric.WithAttributes(attribute.String("event", name)))
	}
	s.emit(ctx, time.Now(), name, message, severity, nil, nil)
}

func (s *signals) emit(ctx context.Context, now time.Time, event, message string, severity otellog.Severity, attrs []attribute.KeyValue, duration *float64) {
	if s.closed.Load() {
		return
	}
	if s.local != nil && (duration != nil || severity != otellog.SeverityInfo) {
		s.local.write(ctx, now, event, message, severity, attrs, duration)
	}
	if s.logger == nil {
		return
	}
	var record otellog.Record
	record.SetTimestamp(now)
	record.SetEventName(event)
	record.SetBody(attribute.StringValue(message))
	record.SetSeverity(severity)
	record.SetSeverityText(severityText(severity))
	record.AddAttributes(attrs...)
	if duration != nil {
		record.AddAttributes(attribute.Float64("duration_seconds", *duration))
	}
	s.logger.Emit(ctx, record)
}

// LocalLogLimit is the most log lines that wait for standard error. A line beyond it is dropped and counted.
const LocalLogLimit = 256

// localLog writes the controller logs to standard error when no collector is set, one JSON line in one write each,
// with the fields of the completion log and no key, payload or error text (ST-SUP-RP02-LC20). A bounded queue and one
// writer keep a stalled standard error from blocking a worker: a line beyond the bound is dropped and counted, and
// the close drains the queue within the shutdown limit.
type localLog struct {
	out     io.Writer
	lines   chan []byte
	dropped atomic.Uint64
	mu      sync.Mutex
	closed  bool
	done    chan struct{}
}

func newLocalLog(out io.Writer) *localLog {
	l := &localLog{out: out, lines: make(chan []byte, LocalLogLimit), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for line := range l.lines {
			_, _ = l.out.Write(line)
		}
	}()
	return l
}

// put queues one line, or drops and counts it when the queue is full or closed.
func (l *localLog) put(line []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		l.dropped.Add(1)
		return
	}
	select {
	case l.lines <- line:
	default:
		l.dropped.Add(1)
	}
}

// drain closes the queue and waits for the writer until ctx ends. The lines a stalled writer cannot write in time
// are left behind.
func (l *localLog) drain(ctx context.Context) {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.lines)
	}
	l.mu.Unlock()
	select {
	case <-l.done:
	case <-ctx.Done():
	}
}

type localRecord struct {
	Time            string   `json:"time"`
	Severity        string   `json:"severity"`
	Event           string   `json:"event.name"`
	Message         string   `json:"message"`
	Operation       string   `json:"operation,omitempty"`
	Outcome         string   `json:"outcome,omitempty"`
	Retry           *bool    `json:"retry,omitempty"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
	TraceID         string   `json:"trace_id,omitempty"`
	SpanID          string   `json:"span_id,omitempty"`
}

func (l *localLog) write(ctx context.Context, now time.Time, event, message string, severity otellog.Severity, attrs []attribute.KeyValue, duration *float64) {
	record := localRecord{Time: now.UTC().Format(time.RFC3339Nano), Severity: severityText(severity), Event: event, Message: message, DurationSeconds: duration}
	for _, kv := range attrs {
		switch kv.Key {
		case "operation":
			record.Operation = kv.Value.AsString()
		case "outcome":
			record.Outcome = kv.Value.AsString()
		case "retry":
			retry := kv.Value.AsBool()
			record.Retry = &retry
		}
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		record.TraceID, record.SpanID = sc.TraceID().String(), sc.SpanID().String()
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	l.put(append(line, '\n'))
}

func severityText(severity otellog.Severity) string {
	switch severity {
	case otellog.SeverityWarn:
		return "WARN"
	case otellog.SeverityError:
		return "ERROR"
	}
	return "INFO"
}

func signalsOf(ctx context.Context) *signals {
	if o := ownershipOf(ctx); o != nil {
		return o.signals
	}
	return nil
}

// beginControllerWork starts one controller operation, or nothing when ctx carries no telemetry.
func beginControllerWork(ctx context.Context, operation string) (context.Context, func(workResult)) {
	s := signalsOf(ctx)
	if s == nil {
		return ctx, func(workResult) {}
	}
	return s.begin(ctx, operation)
}

// controllerWork runs one operation of a controller loop. A callback panic still propagates; its value cannot enter
// telemetry.
func controllerWork(ctx context.Context, operation string, work func(context.Context) error) (err error, finish func(bool)) {
	ctx, end := beginControllerWork(ctx, operation)
	completed := false
	defer func() {
		if !completed {
			end(workResult{err: errControllerPanic})
		}
	}()
	err = work(ctx)
	if err == nil {
		err = ctx.Err()
	}
	completed = true
	return err, func(retry bool) { end(workResult{err: err, retry: retry}) }
}

// controllerResultWork runs one reconcile with a result. Context and callback failures discard the result before
// scheduling or telemetry. A panic still propagates without its value.
func controllerResultWork(ctx context.Context, work func(context.Context) (ReconcileResult, error)) (result ReconcileResult, err error, finish func(bool)) {
	ctx, end := beginControllerWork(ctx, "reconcile")
	completed := false
	defer func() {
		if !completed {
			end(workResult{err: errControllerPanic})
		}
	}()
	result, err = work(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = result.validate()
	}
	if err != nil {
		result = ReconcileResult{}
	}
	completed = true
	return result, err, func(retry bool) { end(workResult{err: err, retry: retry, pending: result.RecheckAfter > 0}) }
}

// controllerHelperWork records one standalone helper operation, such as a scan. A helper inside an enclosing
// controller operation records nothing; that operation already covers the work. A panic still propagates without its
// value entering telemetry.
func controllerHelperWork(ctx context.Context, operation string, work func(context.Context) error) error {
	if ctx == nil {
		return work(ctx)
	}
	if _, active := ctx.Value(operationKey{}).(struct{}); active {
		return work(ctx)
	}
	ctx, end := beginControllerWork(ctx, operation)
	completed := false
	defer func() {
		if !completed {
			end(workResult{err: errControllerPanic})
		}
	}()
	err := work(ctx)
	completed = true
	end(workResult{err: err})
	return err
}

// controllerNotice sends one fixed runtime event to the telemetry that ctx carries.
func controllerNotice(ctx context.Context, phase string) {
	if s := signalsOf(ctx); s != nil {
		s.notice(ctx, phase)
	}
}
