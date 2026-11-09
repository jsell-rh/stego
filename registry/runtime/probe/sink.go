package probe

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"stego.dev/runtime/otel"
)

// QueueLimit is the most records that the queue of one sink holds before it drops (ST-SUP-RP12-LC08).
const QueueLimit = 1024

// consoleSetting is the one setting of the probe runtime (ST-SUP-RP12-LC16).
const consoleSetting = "STEGO_PROBE_CONSOLE"

// The sink names of the counters domain.probe.dropped and domain.probe.failed (ST-SUP-RP12-LC19). The probe counts
// under its own name what it drops before any sink: values that do not match, emissions after the fill returned
// or after Close, and emissions beyond HoldLimit.
const (
	sinkProbe   = "probe"
	sinkOTLP    = "otlp"
	sinkConsole = "console"
)

// sink takes the records of one runtime. send must not block.
type sink interface {
	send(r *record)
}

// core is what a Runtime and a Recorder share: the sinks, the built-in declaration and the drop count of the probe.
type core struct {
	sinks    []sink
	builtins *Builtins
	closed   atomic.Bool
	dropped  atomic.Uint64
}

// send gives the record to each sink, or drops and counts it after Close.
func (c *core) send(r *record) {
	if c.closed.Load() {
		c.dropped.Add(1)
		return
	}
	for _, s := range c.sinks {
		s.send(r)
	}
}

// queue is the bounded queue of one sink: put drops and counts rather than blocks, and one goroutine writes the
// records in order.
type queue struct {
	records   chan *record
	done      chan struct{}
	mu        sync.RWMutex
	closed    bool
	abandoned atomic.Bool
	dropped   atomic.Uint64
	failed    atomic.Uint64
	stopOnce  sync.Once
}

func newQueue(write func(*record) error) *queue {
	q := &queue{records: make(chan *record, QueueLimit), done: make(chan struct{})}
	go q.run(write)
	return q
}

func (q *queue) put(r *record) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		q.dropped.Add(1)
		return
	}
	select {
	case q.records <- r:
	default:
		q.dropped.Add(1)
	}
}

// run writes each record. A write that fails or panics is counted, and a record left after the shutdown limit
// ended is counted as dropped.
func (q *queue) run(write func(*record) error) {
	defer close(q.done)
	for r := range q.records {
		if q.abandoned.Load() {
			q.dropped.Add(1)
			continue
		}
		if !safeWrite(write, r) {
			q.failed.Add(1)
		}
	}
}

func safeWrite(write func(*record) error, r *record) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return write(r) == nil
}

// drain closes the queue and waits until the goroutine wrote what it holds or ctx ends. It tells whether the queue
// drained.
func (q *queue) drain(ctx context.Context) bool {
	q.stopOnce.Do(func() {
		q.mu.Lock()
		q.closed = true
		close(q.records)
		q.mu.Unlock()
	})
	select {
	case <-q.done:
		return true
	case <-ctx.Done():
		q.abandoned.Store(true)
		return false
	}
}

// Runtime is the probe runtime of one service process: the OTLP sink through the telemetry runtime when it has a
// collector endpoint, and the console sink on standard output. Only the process environment at start chooses the
// sinks and the console format (ST-SUP-RP12-LC17).
type Runtime struct {
	core      *core
	telemetry *otel.Runtime
	otlp      *otlpSink
	console   *consoleSink
	closeOnce sync.Once
	closeErr  error
}

// Start starts the probe runtime on the telemetry runtime of the service. It reads STEGO_PROBE_CONSOLE: json or
// logfmt turns the console sink on for declared observations, and an absent, empty or off setting leaves it off for
// them; any other value fails with fixed text (ST-SUP-RP12-LC16). The built-in service events are on the console in
// every setting (ST-SUP-RP12-LC22). The OTLP sink is on when, and only when, the telemetry runtime has a collector
// endpoint, and it uses the telemetry runtime's providers only (ST-SUP-RP12-LC06, ST-SUP-RP12-LC14). builtins may be
// nil when the declaration has no worker and no open schema change.
func Start(telemetry *otel.Runtime, builtins *Builtins) (*Runtime, error) {
	return start(telemetry, builtins, os.Getenv(consoleSetting), os.Stdout)
}

// start is Start with the setting and the writer of standard output given.
func start(telemetry *otel.Runtime, builtins *Builtins, setting string, stdout io.Writer) (*Runtime, error) {
	if telemetry == nil {
		return nil, errors.New("probe: no telemetry runtime")
	}
	format, err := consoleFormat(setting)
	if err != nil {
		return nil, err
	}
	if builtins == nil {
		builtins = &Builtins{}
	}
	r := &Runtime{core: &core{builtins: builtins}, telemetry: telemetry}
	r.console = newConsoleSink(stdout, format, telemetry.Service(), telemetry.Instance())
	if telemetry.CollectorSet() {
		otlp, err := newOTLPSink(telemetry, r.core, r.console)
		if err != nil {
			r.console.queue.drain(context.Background())
			return nil, err
		}
		r.otlp = otlp
		r.core.sinks = append(r.core.sinks, otlp)
	}
	r.core.sinks = append(r.core.sinks, r.console)
	return r, nil
}

// consoleFormat reads the console setting. The error text is fixed and does not echo the value.
func consoleFormat(setting string) (format, error) {
	switch setting {
	case "", "off":
		return formatOff, nil
	case "json":
		return formatJSON, nil
	case "logfmt":
		return formatLogfmt, nil
	}
	return formatOff, errors.New("probe: STEGO_PROBE_CONSOLE must be json, logfmt or off")
}

// Bind binds a new call of a fill to the span of ctx, the tenant ID, the principal's opaque ID and the name of the
// command, query, worker, process method or slot binding. It reads nothing from ctx but the span.
func (r *Runtime) Bind(ctx context.Context, tenant, principal, operation string) *Call {
	return r.core.bind(ctx, tenant, principal, operation)
}

// Close stops the probe runtime and the telemetry runtime inside one shared shutdown limit: the earlier of the
// deadline of ctx and the telemetry runtime's ShutdownTimeout. The console sink writes what it holds and the OTLP
// sink hands what it holds to the providers while the telemetry runtime shuts down, with no budget of their own,
// also when standard output blocks (ST-SUP-RP12-LC12). An emission after Close is dropped and counted. It is safe
// to call more than once.
func (r *Runtime) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.core.closed.Store(true)
		ctx, cancel := context.WithTimeout(ctx, otel.ShutdownTimeout)
		defer cancel()
		var group sync.WaitGroup
		var drained atomic.Bool
		drained.Store(true)
		group.Go(func() {
			if !r.console.queue.drain(ctx) {
				drained.Store(false)
			}
		})
		var telemetryErr error
		group.Go(func() {
			if r.otlp != nil && !r.otlp.queue.drain(ctx) {
				drained.Store(false)
			}
			telemetryErr = r.telemetry.Shutdown(ctx)
		})
		group.Wait()
		switch {
		case telemetryErr != nil:
			r.closeErr = telemetryErr
		case !drained.Load():
			r.closeErr = errors.New("probe: a sink did not drain inside the shutdown limit")
		}
	})
	return r.closeErr
}

// Delivery is what the sinks of a runtime dropped and failed to write (ST-SUP-RP12-LC19).
type Delivery struct {
	// ProbeDropped counts what the probe dropped before any sink.
	ProbeDropped   uint64
	OTLPDropped    uint64
	OTLPFailed     uint64
	ConsoleDropped uint64
	ConsoleFailed  uint64
}

// Delivery reads the drop and failure counts of the runtime.
func (r *Runtime) Delivery() Delivery {
	d := Delivery{ProbeDropped: r.core.dropped.Load(), ConsoleDropped: r.console.queue.dropped.Load(), ConsoleFailed: r.console.queue.failed.Load()}
	if r.otlp != nil {
		d.OTLPDropped = r.otlp.queue.dropped.Load()
		d.OTLPFailed = r.otlp.queue.failed.Load()
	}
	return d
}
