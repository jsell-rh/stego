package modes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Pinger is the part of *sql.DB that the readiness of the worker and controller modes reads.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// Staller is a backfill that the readiness of the controller mode reads, such as *lease.Backfill.
type Staller interface {
	Change() string
	Stalled() bool
}

// ErrNotReady is the error of Ready when a mode is not ready.
var ErrNotReady = errors.New("not ready")

// Readiness answers the readiness of each mode (ST-SUP-RP09-LC05).
type Readiness struct {
	db        Pinger
	accepting atomic.Bool
	stopping  atomic.Bool

	mu        sync.Mutex
	backfills []Staller
}

// NewReadiness returns the readiness that pings db for the worker and controller modes.
func NewReadiness(db Pinger) *Readiness {
	return &Readiness{db: db}
}

// Watch adds a backfill whose stall makes the controller mode not ready.
func (r *Readiness) Watch(backfill Staller) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backfills = append(r.backfills, backfill)
}

// Listener returns ln for the HTTP server of the api mode to serve on. The api mode is ready while the server
// accepts on it, and not ready once an accept fails, such as when the listener is closed.
func (r *Readiness) Listener(ln net.Listener) net.Listener {
	return &listener{Listener: ln, readiness: r}
}

type listener struct {
	net.Listener
	readiness *Readiness
}

func (l *listener) Accept() (net.Conn, error) {
	l.readiness.accepting.Store(true)
	conn, err := l.Listener.Accept()
	if err != nil {
		l.readiness.accepting.Store(false)
	}
	return conn, err
}

// Fail makes every mode not ready from now on. It is the first step of the shutdown.
func (r *Readiness) Fail() {
	r.stopping.Store(true)
}

// Ready answers the readiness of the mode m: api is ready when the HTTP server accepts requests; worker when the
// database is reachable; controller when the database is reachable, whether or not the replica holds the lease,
// and no backfill it watches is stalled. Every mode is not ready once the shutdown began.
func (r *Readiness) Ready(ctx context.Context, m Mode) error {
	if r.stopping.Load() {
		return fmt.Errorf("%s: %w: the replica is shutting down", m, ErrNotReady)
	}
	switch m {
	case API:
		if !r.accepting.Load() {
			return fmt.Errorf("%s: %w: the HTTP server does not accept requests", m, ErrNotReady)
		}
		return nil
	case Worker, Controller:
		if r.db == nil {
			return fmt.Errorf("%s: %w: no database", m, ErrNotReady)
		}
		if err := r.db.PingContext(ctx); err != nil {
			return fmt.Errorf("%s: %w: the database is not reachable: %w", m, ErrNotReady, err)
		}
		if m == Controller {
			r.mu.Lock()
			backfills := append([]Staller(nil), r.backfills...)
			r.mu.Unlock()
			for _, b := range backfills {
				if b.Stalled() {
					return fmt.Errorf("%s: %w: the backfill of %s is stalled", m, ErrNotReady, b.Change())
				}
			}
		}
		return nil
	case All:
		return r.Check(ctx, Selection{All})
	}
	return fmt.Errorf("modes: unknown mode %q", m)
}

// Check answers the readiness of every mode of the selection, and names each mode that is not ready.
func (r *Readiness) Check(ctx context.Context, selection Selection) error {
	var errs []error
	for _, m := range selection.Modes() {
		if err := r.Ready(ctx, m); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Handler serves the probes of the selection: /readyz answers 200 when every selected mode is ready and 503 with
// the reasons when not; /livez answers 200 while the process serves, and no backfill or lease state fails it.
func (r *Readiness) Handler(selection Selection) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := r.Check(req.Context(), selection); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintln(w, err)
			return
		}
		_, _ = fmt.Fprintln(w, "ready")
	})
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "live")
	})
	return mux
}

// Clock is the clock of the shutdown sequencer: the endpoint propagation delay runs on it.
type Clock interface {
	After(d time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// APIShutdown is the shutdown of the api mode: after readiness fails, wait the endpoint propagation delay, so the
// endpoints stop sending requests, then drain the in-flight requests of the server.
type APIShutdown struct {
	Server           *http.Server
	PropagationDelay time.Duration
}

// WorkerShutdown is the shutdown of the worker mode: stop claiming, then finish the claimed rows, waiting at most
// the drain bound when it is set.
type WorkerShutdown struct {
	StopClaiming func()
	Finish       func(ctx context.Context) error
	DrainBound   time.Duration
}

// ControllerShutdown is the shutdown of the controller mode: flush the checkpoint under the lease with the
// holder's token, then release the lease. A release that comes first would leave the flush with no valid token.
type ControllerShutdown struct {
	Flush   func(ctx context.Context) error
	Release func(ctx context.Context) error
}

// Sequencer runs the shutdown of each selected mode in its own order (ST-SUP-RP09-LC06). It owns no signal
// handler: the generated code passes the signal channel.
type Sequencer struct {
	Readiness  *Readiness
	Clock      Clock
	API        *APIShutdown
	Worker     *WorkerShutdown
	Controller *ControllerShutdown
}

// Run waits for a signal on signals, such as SIGTERM, or for ctx to end, and then runs Shutdown with a context
// that ctx's end does not cancel, so the shutdown can finish.
//
// The contexts of the components, such as the one Hold and Backfill.Run take, must not be the trigger of the
// shutdown: cancel them only after Run returns. A component context that ends first stops the backfill before
// its flush, and Hold then releases the lease with the checkpoint not flushed; Flush reports that as
// ErrNotFlushed.
func (s *Sequencer) Run(ctx context.Context, selection Selection, signals <-chan os.Signal) error {
	select {
	case <-signals:
	case <-ctx.Done():
	}
	return s.Shutdown(context.WithoutCancel(ctx), selection)
}

// Shutdown makes the replica not ready as its first step, then runs the sequence of every selected mode at the
// same time, each in its own order, and joins their errors. A mode with no sequence set has nothing to stop.
func (s *Sequencer) Shutdown(ctx context.Context, selection Selection) error {
	if s.Readiness != nil {
		s.Readiness.Fail()
	}
	clock := s.Clock
	if clock == nil {
		clock = systemClock{}
	}
	var sequences []func(context.Context) error
	if selection.Has(API) && s.API != nil {
		sequences = append(sequences, func(ctx context.Context) error { return s.API.run(ctx, clock) })
	}
	if selection.Has(Worker) && s.Worker != nil {
		sequences = append(sequences, s.Worker.run)
	}
	if selection.Has(Controller) && s.Controller != nil {
		sequences = append(sequences, s.Controller.run)
	}
	errs := make([]error, len(sequences))
	var wg sync.WaitGroup
	for i, sequence := range sequences {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = sequence(ctx)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (a *APIShutdown) run(ctx context.Context, clock Clock) error {
	if a.PropagationDelay > 0 {
		select {
		case <-clock.After(a.PropagationDelay):
		case <-ctx.Done():
			return fmt.Errorf("api: %w", ctx.Err())
		}
	}
	if a.Server == nil {
		return nil
	}
	if err := a.Server.Shutdown(ctx); err != nil {
		return fmt.Errorf("api: drain: %w", err)
	}
	return nil
}

func (w *WorkerShutdown) run(ctx context.Context) error {
	if w.StopClaiming != nil {
		w.StopClaiming()
	}
	if w.Finish == nil {
		return nil
	}
	if w.DrainBound > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.DrainBound)
		defer cancel()
	}
	if err := w.Finish(ctx); err != nil {
		return fmt.Errorf("worker: finish the claimed rows: %w", err)
	}
	return nil
}

func (c *ControllerShutdown) run(ctx context.Context) error {
	var errs []error
	if c.Flush != nil {
		if err := c.Flush(ctx); err != nil {
			errs = append(errs, fmt.Errorf("controller: flush the checkpoint: %w", err))
		}
	}
	if c.Release != nil {
		if err := c.Release(ctx); err != nil {
			errs = append(errs, fmt.Errorf("controller: release the lease: %w", err))
		}
	}
	return errors.Join(errs...)
}
