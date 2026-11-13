package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/audit/auditsink"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/modes"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// The fixed names of the connection settings of the worker mode (ST-SUP-RP14-LC04), each with its file form.
const (
	// WorkerDatabaseURLSetting is the connection string of the worker role, the only role that may claim outbox
	// messages (ST-SUP-RP01-LC19), and WorkerDatabaseURLFileSetting a file that holds it.
	WorkerDatabaseURLSetting     = "STEGO_WORKER_DATABASE_URL"
	WorkerDatabaseURLFileSetting = "STEGO_WORKER_DATABASE_URL_FILE"
	// RelayDatabaseURLSetting is the connection string of the relay login role, which raises the revocation floor
	// (ST-SUP-RP06-LC13), and RelayDatabaseURLFileSetting a file that holds it.
	RelayDatabaseURLSetting     = "STEGO_RELAY_DATABASE_URL"
	RelayDatabaseURLFileSetting = "STEGO_RELAY_DATABASE_URL_FILE"
)

// The names of the components that Start runs, as the readiness and the errors name them.
const (
	OutboxWorkerName = "outbox-worker"
	RelayWatchName   = "authz-relay-watch"
	ControllerName   = "controller-lease"
	AnchorJobName    = "audit-anchor"
	MaintainJobName  = "audit-maintain"
)

// ControllerLease is the name of the controller lease, under which the jobs of the controller mode run: one replica
// holds it at a time (ST-SUP-RP09-LC13). The migration job makes its row (ST-SUP-RP14-LC03).
const ControllerLease = "stego.controller"

// MaintainInterval is the interval of the audit maintenance loop, which the declaration does not state: each run
// makes the partitions that are missing and drops at most one partition per call (ST-SUP-RP05-LC10).
const MaintainInterval = time.Hour

// DefaultDrainBound is the drain bound of the worker mode when the start configuration gives none: the time that
// the claimed messages have to finish at shutdown (ST-SUP-RP09-LC06).
const DefaultDrainBound = 20 * time.Second

// WorkerSettings are the connection settings of the worker mode, which ReadWorkerSettings reads.
type WorkerSettings struct {
	// WorkerDatabaseURL is the connection string of the worker role.
	WorkerDatabaseURL string
	// RelayDatabaseURL is the connection string of the relay login role, or empty for a service with no relay.
	RelayDatabaseURL string
	// AuditSink are the settings of the system that the audit-sink port binds: an HTTPS endpoint, or the default
	// table in the service database (ST-SUP-RP05-LC15).
	AuditSink auditsink.Settings
}

// ReadWorkerSettings reads the connection settings of the worker mode from the process environment by their fixed
// names (ST-SUP-RP14-LC04). relay tells whether the service has the authorization relay, whose connection string is
// then required too. A missing or malformed setting is refused with an error that names it and not its value.
func ReadWorkerSettings(relay bool) (WorkerSettings, error) {
	var s WorkerSettings
	var err error
	if s.WorkerDatabaseURL, err = connectionString(WorkerDatabaseURLSetting, WorkerDatabaseURLFileSetting); err != nil {
		return WorkerSettings{}, err
	}
	if relay {
		if s.RelayDatabaseURL, err = connectionString(RelayDatabaseURLSetting, RelayDatabaseURLFileSetting); err != nil {
			return WorkerSettings{}, err
		}
	}
	if s.AuditSink, err = auditsink.ReadSettings(); err != nil {
		return WorkerSettings{}, err
	}
	return s, nil
}

// WorkerParts are what varies with the declaration for the components of the worker and controller modes, which
// the generated code gives in Parts: the constructors of the service's component packages and the declared values.
type WorkerParts struct {
	// NewWorker gives the outbox worker on queue with config, with the destinations of the service's components
	// registered on it, such as the audit sink on sink, the system of the worker settings (ST-SUP-RC18-OC30).
	NewWorker func(queue *outbox.Queue, config outbox.WorkerConfig, sink audit.Sink) (*outbox.Worker, error)
	// NewRelay builds the authorization relay on the relay role's pool and registers it on the worker as the
	// destination authz (ST-SUP-RP06-LC06). Nil: the service has no relay.
	NewRelay func(ctx context.Context, db *sql.DB, relationships authz.Relationships, worker *outbox.Worker,
		meter metric.MeterProvider, logger otellog.LoggerProvider) (*authz.Relay, error)
	// WatchInterval is the interval of the relay's late-revocation watch (ST-SUP-RP06-LC14).
	WatchInterval time.Duration
	// AuditJobs gives the audit anchor job and the maintenance loop (ST-SUP-RC28-OC08). Nil: the service has no
	// audit jobs.
	AuditJobs func(lease audit.LeaseOf, tenants audit.TenantsOf, db *sql.DB, outbox audit.Enqueuer) (*audit.Anchorer, *audit.Maintainer, error)
	// AnchorPeriod is the declared anchor period of the audit-log component, the interval of the anchor job.
	AnchorPeriod time.Duration
	// RetentionMonths is the declared retention of the audit-log component, whose value the source of audit_maintain
	// holds, which the schema manifest check compares (ST-SUP-RP14-LC06).
	RetentionMonths int
	// TenantSchema is the schema of the tenant list stego_tenants, whose tenants the audit jobs visit
	// (ST-SUP-RP04-LC07).
	TenantSchema string
}

// StartConfig is how one process runs: the modes it runs, the HTTP server of the api mode, and the bounds of the
// shutdown.
type StartConfig struct {
	// Selection is the modes the process runs, as modes.Parse gives them.
	Selection modes.Selection
	// Workers are the connection settings of the worker mode; Start reads them only when the selection runs it.
	Workers WorkerSettings
	// Server is the HTTP server of the api mode, which the shutdown drains. Nil: the api mode has nothing to drain.
	Server *http.Server
	// PropagationDelay is the wait of the api mode's shutdown after readiness fails.
	PropagationDelay time.Duration
	// DrainBound is the time the claimed outbox messages have to finish at shutdown; zero is DefaultDrainBound.
	DrainBound time.Duration
	// Holder names this process as a holder of the controller lease; empty is the host name with a random suffix.
	Holder string
	// LeaseSettings are the settings of the controller lease; the zero value is the lease runtime's defaults.
	LeaseSettings lease.Settings
	// WorkerConfig are the limits of the outbox worker; the zero value is outbox.DefaultWorkerConfig.
	WorkerConfig *outbox.WorkerConfig
}

// component is one started component and its state.
type component struct {
	name    string
	mode    modes.Mode
	running atomic.Bool
	failed  atomic.Bool
}

// Workers are the components that Start started for the selected modes: the outbox worker with its destinations
// and the relay's watch in the worker mode, and the controller lease with the periodic audit jobs in the controller
// mode. Shutdown stops them once.
type Workers struct {
	selection  modes.Selection
	readiness  *modes.Readiness
	sequencer  *modes.Sequencer
	components []*component
	wg         sync.WaitGroup
	cancel     context.CancelFunc

	workerDB, relayDB *sql.DB
	worker            *outbox.Worker
	relay             *authz.Relay
	periodic          *lease.Periodic
	anchorer          *audit.Anchorer
	maintainer        *audit.Maintainer

	// controller ends the controller lease loop, which releases the lease; controllerDone is closed when it returned.
	controller     context.CancelFunc
	controllerDone chan struct{}
	held           atomic.Pointer[lease.Held]

	once        sync.Once
	shutdownErr error
}

// Start starts the components of the selected modes (ST-SUP-RP14-LC04), each in its own goroutine, and returns once
// they run. In the worker mode it opens the pool of the worker role through tenant.Open, builds the outbox worker on
// it with the service's destinations, and the relay on the relay role's pool with its watch; in the controller mode
// it holds the controller lease on the runtime role's pool and runs the audit anchor job at the anchor period and
// the maintenance loop at MaintainInterval under it, so two replicas never run the same job. The api mode starts no
// component: the generated code serves its HTTP server on Readiness().Listener. A selection that runs the worker
// mode without the worker role's connection string is refused, naming the setting. A failed step stops what Start
// started and closes what it opened.
func (svc *Service) Start(ctx context.Context, c StartConfig) (_ *Workers, err error) {
	if len(c.Selection) == 0 {
		return nil, errors.New("service: Start needs the selected modes")
	}
	for _, m := range c.Selection {
		if m != modes.API && m != modes.Worker && m != modes.Controller && m != modes.All {
			return nil, fmt.Errorf("service: unknown mode %q", m)
		}
	}
	w := &Workers{selection: c.Selection}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w.cancel = cancel
	defer func() {
		if err != nil {
			cancel()
			w.wg.Wait()
			w.closePools()
		}
	}()
	registry := modes.NewRegistry()
	var workerSet, controllerSet []modes.Component
	if c.Selection.Has(modes.Worker) {
		if workerSet, err = svc.startWorker(ctx, c, w); err != nil {
			return nil, err
		}
	}
	if c.Selection.Has(modes.Controller) {
		if controllerSet, err = svc.startController(c, w); err != nil {
			return nil, err
		}
	}
	if err := registry.Register(modes.Worker, workerSet...); err != nil {
		return nil, err
	}
	if err := registry.Register(modes.Controller, controllerSet...); err != nil {
		return nil, err
	}
	w.readiness = modes.NewReadiness(svc.pool)
	w.readiness.WatchQueue(controllerQueue{w: w})
	drain := c.DrainBound
	if drain <= 0 {
		drain = DefaultDrainBound
	}
	w.sequencer = &modes.Sequencer{Readiness: w.readiness,
		API: &modes.APIShutdown{Server: c.Server, PropagationDelay: c.PropagationDelay}}
	if w.worker != nil {
		w.sequencer.Worker = &modes.WorkerShutdown{StopClaiming: w.worker.StopClaiming, Finish: w.worker.Finish, DrainBound: drain}
	}
	if w.controllerDone != nil {
		w.sequencer.Controller = &modes.ControllerShutdown{Release: w.releaseController}
	}
	for _, comp := range registry.Components(c.Selection) {
		state := w.track(comp.Name)
		w.wg.Add(1)
		go func(comp modes.Component) {
			defer w.wg.Done()
			defer state.running.Store(false)
			if err := comp.Run(runCtx); err != nil && runCtx.Err() == nil {
				state.failed.Store(true)
			}
		}(comp)
	}
	return w, nil
}

// track gives the state of the component name and marks it running.
func (w *Workers) track(name string) *component {
	for _, c := range w.components {
		if c.name == name {
			c.running.Store(true)
			return c
		}
	}
	c := &component{name: name}
	c.running.Store(true)
	w.components = append(w.components, c)
	return c
}

// startWorker opens the worker role's pool and builds the outbox worker, the relay and its watch.
func (svc *Service) startWorker(ctx context.Context, c StartConfig, w *Workers) ([]modes.Component, error) {
	p := svc.parts.Workers
	if p.NewWorker == nil {
		return nil, errors.New("service: the worker mode needs the outbox worker of the parts")
	}
	if c.Workers.WorkerDatabaseURL == "" {
		return nil, settingError(WorkerDatabaseURLSetting+" or "+WorkerDatabaseURLFileSetting, "must be set for the worker mode")
	}
	if p.NewRelay != nil && c.Workers.RelayDatabaseURL == "" {
		return nil, settingError(RelayDatabaseURLSetting+" or "+RelayDatabaseURLFileSetting, "must be set for the worker mode of a service with the relay")
	}
	config := outbox.DefaultWorkerConfig()
	if c.WorkerConfig != nil {
		config = *c.WorkerConfig
	}
	config.Meter, config.Logger = svc.telemetry.MeterProvider(), svc.telemetry.LoggerProvider()
	cfg, err := svc.parts.TenantConfig(c.Workers.WorkerDatabaseURL)
	if err != nil {
		return nil, err
	}
	// The worker role is the one role that runs the outbox relay functions (ST-SUP-RP01-LC19).
	cfg.Claims = true
	if w.workerDB, err = tenant.Open(ctx, cfg); err != nil {
		return nil, err
	}
	// Each step in flight holds one connection, and the claims and the purge timer one more each.
	if limit := w.workerDB.Stats().MaxOpenConnections; limit > 0 && limit < config.Concurrency+2 {
		w.workerDB.SetMaxOpenConns(config.Concurrency + 2)
	}
	queue, err := svc.parts.NewQueue(w.workerDB)
	if err != nil {
		return nil, err
	}
	sink, err := auditsink.New(c.Workers.AuditSink, w.workerDB)
	if err != nil {
		return nil, err
	}
	if w.worker, err = p.NewWorker(queue, config, sink); err != nil {
		return nil, err
	}
	set := []modes.Component{{Name: OutboxWorkerName, Run: w.worker.Run}}
	if p.NewRelay != nil {
		if p.WatchInterval <= 0 {
			return nil, errors.New("service: the relay's watch needs a positive interval")
		}
		relayCfg, err := svc.parts.TenantConfig(c.Workers.RelayDatabaseURL)
		if err != nil {
			return nil, err
		}
		if w.relayDB, err = tenant.Open(ctx, relayCfg); err != nil {
			return nil, err
		}
		if w.relay, err = p.NewRelay(ctx, w.relayDB, svc.provider, w.worker, svc.telemetry.MeterProvider(), svc.telemetry.LoggerProvider()); err != nil {
			return nil, err
		}
		worker, relay, interval := w.worker, w.relay, p.WatchInterval
		set = append(set, modes.Component{Name: RelayWatchName, Run: func(ctx context.Context) error {
			return relay.Watch(ctx, interval, func(ctx context.Context) (time.Duration, error) {
				return worker.OldestAge(ctx, authz.DestinationName)
			})
		}})
	}
	return set, nil
}

// startController builds the audit jobs and the controller lease loop that runs them.
func (svc *Service) startController(c StartConfig, w *Workers) ([]modes.Component, error) {
	p := svc.parts.Workers
	if p.AuditJobs == nil {
		return nil, nil
	}
	if p.AnchorPeriod <= 0 || p.TenantSchema == "" {
		return nil, errors.New("service: the audit jobs need the anchor period and the schema of the tenant list")
	}
	holder := c.Holder
	if holder == "" {
		holder = defaultHolder()
	}
	client, err := lease.New(svc.pool, ControllerLease, holder, c.LeaseSettings)
	if err != nil {
		return nil, err
	}
	enqueuer, err := svc.parts.AuditQueue(svc.queue)
	if err != nil {
		return nil, err
	}
	leaseOf := func() *lease.Held { return w.held.Load() }
	schema, pool := p.TenantSchema, svc.pool
	tenants := func(ctx context.Context) ([]tenant.Tenant, error) { return tenant.List(ctx, pool, schema) }
	if w.anchorer, w.maintainer, err = p.AuditJobs(leaseOf, tenants, svc.pool, enqueuer); err != nil {
		return nil, err
	}
	anchorer, maintainer := w.anchorer, w.maintainer
	if w.periodic, err = lease.NewPeriodic(lease.PeriodicConfig{Lease: leaseOf, Retry: client.RetryEvery(), Jobs: []lease.Job{
		{Name: AnchorJobName, Interval: p.AnchorPeriod, Run: func(ctx context.Context, _ *lease.Held) error {
			return anchorer.Tick(ctx, time.Now())
		}},
		{Name: MaintainJobName, Interval: MaintainInterval, Run: func(ctx context.Context, _ *lease.Held) error {
			_, err := maintainer.Run(ctx)
			return err
		}},
	}}); err != nil {
		return nil, err
	}
	controllerCtx, stop := context.WithCancel(context.Background())
	w.controller, w.controllerDone = stop, make(chan struct{})
	hold := func(ctx context.Context) error {
		// The lease loop ends with the controller's own context, which the controller shutdown ends, or with ctx.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-controllerCtx.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
		defer close(w.controllerDone)
		for ctx.Err() == nil {
			_ = client.Hold(ctx, func(ctx context.Context, h *lease.Held) error {
				w.held.Store(h)
				defer w.held.CompareAndSwap(h, nil)
				select {
				case <-ctx.Done():
				case <-h.Done():
				}
				return nil
			})
			select {
			case <-ctx.Done():
			case <-time.After(client.RetryEvery()):
			}
		}
		return nil
	}
	return []modes.Component{{Name: ControllerName, Run: hold}, {Name: lease.PeriodicName, Run: w.periodic.Run}}, nil
}

// releaseController ends the controller lease loop, which releases the lease, and waits for it.
func (w *Workers) releaseController(ctx context.Context) error {
	w.controller()
	select {
	case <-w.controllerDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// defaultHolder is the host name with a random suffix, so two processes on one host are two holders.
func defaultHolder() string {
	host, _ := os.Hostname()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return host + "-" + hex.EncodeToString(suffix)
}

// controllerQueue is the readiness of the controller mode's components: ready while the controller lease loop and
// the periodic jobs run and every job, the anchor job and the maintenance loop are healthy, and always ready when
// the service has no controller job.
type controllerQueue struct{ w *Workers }

func (q controllerQueue) QueueReady() bool { return q.w.controllerReason() == "" }

// controllerReason is why the controller mode's components are not ready, or empty.
func (w *Workers) controllerReason() string {
	if w.periodic == nil {
		return ""
	}
	if reason := w.stopped(ControllerName, lease.PeriodicName); reason != "" {
		return reason
	}
	var unhealthy []string
	if ok, _ := w.periodic.Health(); !ok {
		unhealthy = append(unhealthy, "a periodic job failed its last run")
	}
	if ok, _ := w.anchorer.Health(); !ok {
		unhealthy = append(unhealthy, "the "+AnchorJobName+" job has a boundary that is due and not anchored")
	}
	if ok, _ := w.maintainer.Health(); !ok {
		unhealthy = append(unhealthy, "the "+MaintainJobName+" job found no partition of the next month")
	}
	return strings.Join(unhealthy, "; ")
}

// stopped names each of the components names that does not run.
func (w *Workers) stopped(names ...string) string {
	var out []string
	for _, c := range w.components {
		for _, name := range names {
			if c.name == name && !c.running.Load() {
				out = append(out, "the component "+c.name+" stopped")
			}
		}
	}
	return strings.Join(out, "; ")
}

// Readiness is the readiness of the modes, whose Listener the HTTP server of the api mode serves on.
func (w *Workers) Readiness() *modes.Readiness { return w.readiness }

// Ready answers the readiness of each selected mode (ST-SUP-RP09-LC05) and names each mode that is not ready and
// why: the worker mode is ready while the worker role's pool reaches the database and the outbox worker and the
// relay's watch run; the controller mode while the controller lease loop and the periodic jobs run and every job is
// healthy, whether or not this replica holds the lease. The reason names components and jobs, never a tenant.
func (w *Workers) Ready(ctx context.Context) error {
	var errs []error
	for _, m := range w.selection.Modes() {
		if err := w.readiness.Ready(ctx, m); err != nil {
			if m == modes.Controller && w.periodic != nil && w.readiness != nil {
				if reason := w.controllerReason(); reason != "" {
					err = fmt.Errorf("%s: %w: %s", m, modes.ErrNotReady, reason)
				}
			}
			errs = append(errs, err)
			continue
		}
		if m == modes.Worker && w.workerDB != nil {
			if err := w.workerDB.PingContext(ctx); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w: the worker role's pool does not reach the database", m, modes.ErrNotReady))
			} else if reason := w.stopped(OutboxWorkerName, RelayWatchName); reason != "" {
				errs = append(errs, fmt.Errorf("%s: %w: %s", m, modes.ErrNotReady, reason))
			}
		}
	}
	return errors.Join(errs...)
}

// Handler serves the probes of the process: /readyz answers 200 when every selected mode is ready and 503 with the
// reasons of Ready when not; /livez answers 200 while the process serves, and no worker state fails it.
func (w *Workers) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := w.Ready(req.Context()); err != nil {
			rw.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintln(rw, err)
			return
		}
		_, _ = fmt.Fprintln(rw, "ready")
	})
	mux.HandleFunc("GET /livez", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(rw, "live")
	})
	return mux
}

// Worker is the outbox worker of the worker mode, or nil when the selection does not run it.
func (w *Workers) Worker() *outbox.Worker { return w.worker }

// Relay is the authorization relay of the worker mode, or nil.
func (w *Workers) Relay() *authz.Relay { return w.relay }

// Periodic is the periodic job component of the controller mode, or nil.
func (w *Workers) Periodic() *lease.Periodic { return w.periodic }

// Anchorer and Maintainer are the audit jobs of the controller mode, or nil.
func (w *Workers) Anchorer() *audit.Anchorer     { return w.anchorer }
func (w *Workers) Maintainer() *audit.Maintainer { return w.maintainer }

// Holds tells whether this process holds the controller lease now.
func (w *Workers) Holds() bool {
	h := w.held.Load()
	return h != nil && h.Err() == nil
}

// Shutdown stops the started components once (ST-SUP-RP14-LC04): it makes every mode not ready, then runs the
// shutdown of each selected mode in its own order (ST-SUP-RP09-LC06): the api mode waits the propagation delay and
// drains the HTTP server; the worker mode stops claiming and lets the claimed messages finish within the drain
// bound; the controller mode releases the controller lease after its running jobs end. Then it ends every
// component, waits for each to return and closes the worker and relay pools. A later call returns the result of the
// first and stops nothing again.
func (w *Workers) Shutdown(ctx context.Context) error {
	w.once.Do(func() {
		err := w.sequencer.Shutdown(ctx, w.selection)
		w.cancel()
		w.wg.Wait()
		w.closePools()
		w.shutdownErr = err
	})
	return w.shutdownErr
}

func (w *Workers) closePools() {
	for _, db := range []*sql.DB{w.relayDB, w.workerDB} {
		if db != nil {
			_ = db.Close()
		}
	}
}
