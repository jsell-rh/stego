package modes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jsell-rh/stego/registry/runtime/controller"
	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
	"github.com/jsell-rh/stego/registry/runtime/lease"
)

// The integration tests of this file run against the local PostgreSQL server of pgtest: the server that
// STEGO_TEST_POSTGRES_DSN names, else a container that the test binary starts and removes (ST-STD-LOCAL-R04).
// Each test makes its own database on that server and drops it at the end.

// TestMain gives the PostgreSQL tests of the package their local server (ST-STD-LOCAL-R04).
func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// recorder reads every statement that a connection of the database sends, in order.
type recorder struct {
	mu         sync.Mutex
	statements []recorded
}

type recorded struct {
	sql  string
	args []any
}

func (r *recorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	var args []any
	for _, arg := range data.Args {
		if _, ok := arg.(pgx.QueryResultFormatsByOID); !ok {
			args = append(args, arg)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, recorded{sql: data.SQL, args: args})
	return ctx
}

func (r *recorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.statements...)
}

// database is one test database. The tests connect to it through proxy, which can stop as a stopped server
// would; other is a second session that goes around the proxy.
type database struct {
	db    *sql.DB
	other *sql.DB
	proxy *proxy
	rec   *recorder
}

func openDatabase(t *testing.T) *database {
	t.Helper()
	config, err := pgx.ParseConfig(pgtest.Database(t, "modes"))
	if err != nil {
		t.Fatal(err)
	}
	plain := config.Copy()
	p := startProxy(t, net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))))
	config.Host, config.Port = "127.0.0.1", p.port()
	rec := &recorder{}
	config.Tracer = rec
	d := &database{db: stdlib.OpenDB(*config), other: stdlib.OpenDB(*plain), proxy: p, rec: rec}
	t.Cleanup(func() { _ = d.db.Close(); _ = d.other.Close() })
	for _, ddl := range []string{lease.LeasesTable, "CREATE SCHEMA stego_schema", lease.BackfillsTable} {
		d.exec(t, ddl)
	}
	return d
}

func (d *database) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := d.other.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// proxy forwards TCP connections to the database server until it stops; then it closes every connection and
// accepts none, as a stopped server does for its clients.
type proxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
}

func startProxy(t *testing.T, target string) *proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln, target: target}
	t.Cleanup(p.stop)
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, client, server)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
			go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
		}
	}()
	return p
}

func (p *proxy) port() uint16 { return uint16(p.ln.Addr().(*net.TCPAddr).Port) }

func (p *proxy) stop() {
	_ = p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
}

// fakeClock is a clock that moves only when the test advances it; it records each wait, and calls onWait in the
// call that starts the wait, before anything can follow it.
type fakeClock struct {
	mu      sync.Mutex
	waits   []time.Duration
	waiters []chan time.Time
	onWait  func(d time.Duration)
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.onWait != nil {
		c.onWait(d)
	}
	ch := make(chan time.Time, 1)
	c.waits = append(c.waits, d)
	c.waiters = append(c.waiters, ch)
	return ch
}

// advance fires every wait, once one waits.
func (c *fakeClock) advance(t *testing.T) []time.Duration {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.waiters) > 0 {
			for _, ch := range c.waiters {
				ch <- time.Now()
			}
			c.waiters = nil
			waits := append([]time.Duration(nil), c.waits...)
			c.mu.Unlock()
			return waits
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("nothing waits on the fake clock")
	return nil
}

// steps records the shutdown steps in order.
type steps struct {
	mu    sync.Mutex
	names []string
}

func (s *steps) add(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, name)
}

// addAfter adds name, and before it first adds the step that check gives when nothing is recorded yet. The lock
// is held across the check and the adds, so two sequences that record at the same time keep their order.
func (s *steps) addAfter(check func() string, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.names) == 0 {
		if first := check(); first != "" {
			s.names = append(s.names, first)
		}
	}
	s.names = append(s.names, name)
}

func (s *steps) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

// pingOK is a database that is always reachable, for the tests that need no PostgreSQL.
type pingOK struct{}

// readyQueue is a controller queue that runs and is ready. ST-SUP-RP09-LC05-T05 uses the real controller queue.
type readyQueue struct{}

func (readyQueue) QueueReady() bool { return true }

func (pingOK) PingContext(context.Context) error { return nil }

// serve starts an HTTP server for the api mode on the readiness listener, with handler under /work.
func serve(t *testing.T, readiness *Readiness, handler http.HandlerFunc) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", readiness.Handler(Selection{All}))
	if handler != nil {
		mux.HandleFunc("/work", handler)
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(readiness.Listener(ln)) }()
	t.Cleanup(func() { _ = server.Close() })
	return server, "http://" + ln.Addr().String()
}

// sigterm sends SIGTERM to the test process, which the channel of signal.Notify receives, and runs the sequencer.
func sigterm(t *testing.T, sequencer *Sequencer, selection Selection) chan error {
	t.Helper()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(signals) })
	result := make(chan error, 1)
	go func() { result <- sequencer.Run(context.Background(), selection, signals) }()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	return result
}

func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not come within 10 s", what)
		panic("unreachable")
	}
}

func get(t *testing.T, url string) int {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

func TestModeReadiness(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP09-LC05-T01 The api mode is ready only when the HTTP server accepts requests", func(t *testing.T) {
		readiness := NewReadiness(pingOK{})
		if err := readiness.Ready(ctx, API); !errors.Is(err, ErrNotReady) {
			t.Fatalf("before the server listens, api gave %v, want not ready", err)
		}
		_, url := serve(t, readiness, nil)
		if code := get(t, url+"/livez"); code != http.StatusOK {
			t.Fatalf("the server answered %d", code)
		}
		if err := readiness.Ready(ctx, API); err != nil {
			t.Fatalf("after the server accepted a request, api gave %v, want ready", err)
		}
	})

	t.Run("ST-SUP-RP09-LC05-T02 The worker mode is ready only when the database is reachable", func(t *testing.T) {
		d := openDatabase(t)
		readiness := NewReadiness(d.db)
		if err := readiness.Ready(ctx, Worker); err != nil {
			t.Fatalf("with the database reachable, worker gave %v, want ready", err)
		}
		d.proxy.stop()
		if err := readiness.Ready(ctx, Worker); !errors.Is(err, ErrNotReady) {
			t.Fatalf("with the database stopped, worker gave %v, want not ready", err)
		}
	})

	t.Run("ST-SUP-RP09-LC05-T03 The controller mode is ready when the database is reachable, with or without the lease", func(t *testing.T) {
		d := openDatabase(t)
		if err := lease.Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		readiness := NewReadiness(d.db)
		readiness.WatchQueue(readyQueue{})
		other, err := lease.New(d.other, "stego.controller", "other", lease.Settings{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := other.Acquire(ctx); err != nil {
			t.Fatal(err)
		}
		if err := readiness.Ready(ctx, Controller); err != nil {
			t.Fatalf("while another holder has the lease, controller gave %v, want ready", err)
		}
		d.exec(t, "UPDATE leases SET expires_at=now()-interval '1 second'")
		self, err := lease.New(d.db, "stego.controller", "self", lease.Settings{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := self.Acquire(ctx); err != nil {
			t.Fatal(err)
		}
		if err := readiness.Ready(ctx, Controller); err != nil {
			t.Fatalf("while it holds the lease, controller gave %v, want ready", err)
		}
		d.proxy.stop()
		if err := readiness.Ready(ctx, Controller); !errors.Is(err, ErrNotReady) {
			t.Fatalf("with the database stopped, controller gave %v, want not ready", err)
		}
	})

	t.Run("ST-SUP-RP09-LC05-T05 A controller whose watch is down is not ready", func(t *testing.T) {
		d := openDatabase(t)
		readiness := NewReadiness(d.db)
		if err := readiness.Ready(ctx, Controller); !errors.Is(err, ErrNotReady) {
			t.Fatalf("with no watched queue and the database reachable, controller gave %v, want not ready", err)
		}
		metrics := new(controller.Metrics)
		readiness.WatchQueue(metrics)
		if err := readiness.Ready(ctx, Controller); !errors.Is(err, ErrNotReady) {
			t.Fatalf("with no queue running, controller gave %v, want not ready", err)
		}
		drop, back := make(chan struct{}), make(chan struct{})
		var watches atomic.Int32
		source := controller.Source[string]{Watch: func(ctx context.Context) (func() (string, error), error) {
			switch watches.Add(1) {
			case 1:
				return func() (string, error) {
					select {
					case <-drop:
						return "", errors.New("watch down")
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}, nil
			default:
				select {
				case <-back:
				default:
					return nil, errors.New("connection refused")
				}
				return func() (string, error) { <-ctx.Done(); return "", ctx.Err() }, nil
			}
		}, Scan: func(context.Context, func(string) error) error { return nil }}
		run, stop := context.WithCancel(ctx)
		defer stop()
		done := make(chan error, 1)
		go func() {
			done <- controller.RunKeyedWatch(run, source, func(context.Context, string) error { return nil }, controller.KeyedWatchOptions{
				KeyedOptions:   controller.KeyedOptions{Metrics: metrics, Capacity: 4, ResyncInterval: time.Hour, Timeout: time.Second, RetryMin: time.Millisecond, RetryMax: 10 * time.Millisecond, Terminal: func(error) bool { return false }},
				ReconnectDelay: 5 * time.Millisecond,
			})
		}()
		await := func(ready bool) {
			t.Helper()
			deadline := time.Now().Add(5 * time.Second)
			for (readiness.Ready(ctx, Controller) == nil) != ready {
				if time.Now().After(deadline) {
					t.Fatalf("the controller mode did not become ready=%t", ready)
				}
				time.Sleep(time.Millisecond)
			}
		}
		await(true)
		close(drop)
		for watches.Load() < 3 {
			time.Sleep(time.Millisecond)
		}
		if err := readiness.Ready(ctx, Controller); !errors.Is(err, ErrNotReady) {
			t.Fatalf("with the watch down, controller gave %v, want not ready", err)
		}
		close(back)
		await(true)
		stop()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ST-SUP-RP09-LC05-T04 A replica reports not ready as the first step of shutdown", func(t *testing.T) {
		readiness := NewReadiness(pingOK{})
		readiness.WatchQueue(readyQueue{})
		server, url := serve(t, readiness, nil)
		if code := get(t, url+"/readyz"); code != http.StatusOK {
			t.Fatalf("before the shutdown, the replica answered %d, want ready", code)
		}
		recorded := &steps{}
		record := func(name string) {
			recorded.addAfter(func() string {
				if readiness.Check(ctx, Selection{All}) != nil {
					return "readiness false"
				}
				return "readiness true"
			}, name)
		}
		drained := make(chan struct{})
		server.RegisterOnShutdown(func() {
			record("drain")
			close(drained)
		})
		sequencer := &Sequencer{
			Readiness: readiness,
			API:       &APIShutdown{Server: server},
			Worker: &WorkerShutdown{
				StopClaiming: func() { record("claim stop") },
				Finish:       func(context.Context) error { record("finish"); return nil },
			},
			Controller: &ControllerShutdown{
				Flush:   func(context.Context) error { record("flush"); return nil },
				Release: func(context.Context) error { record("lease release"); return nil },
			},
		}
		if err := within(t, sigterm(t, sequencer, Selection{All}), "the shutdown"); err != nil {
			t.Fatal(err)
		}
		within(t, drained, "the drain")
		got := recorded.all()
		if len(got) < 2 || got[0] != "readiness false" {
			t.Fatalf("the steps are %v, want readiness false first", got)
		}
		for _, name := range []string{"drain", "claim stop", "lease release"} {
			if !slices.Contains(got[1:], name) {
				t.Fatalf("the steps are %v, want %s after readiness false", got, name)
			}
		}
	})
}

func TestModeShutdown(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP09-LC06-T01 The api mode fails readiness, waits the propagation delay and drains requests", func(t *testing.T) {
		readiness := NewReadiness(pingOK{})
		entered := make(chan struct{})
		release := make(chan struct{})
		server, url := serve(t, readiness, func(w http.ResponseWriter, _ *http.Request) {
			close(entered)
			<-release
			w.WriteHeader(http.StatusOK)
		})
		recorded := &steps{}
		drained := make(chan struct{})
		server.RegisterOnShutdown(func() {
			recorded.add("drain")
			close(release)
			close(drained)
		})
		response := make(chan int, 1)
		go func() {
			r, err := http.Get(url + "/work")
			if err != nil {
				response <- 0
				return
			}
			_ = r.Body.Close()
			response <- r.StatusCode
		}()
		within(t, entered, "the request")
		clock := &fakeClock{onWait: func(d time.Duration) {
			recorded.addAfter(func() string {
				if readiness.Ready(ctx, API) != nil {
					return "readiness failed"
				}
				return "readiness ready"
			}, fmt.Sprintf("wait %s", d))
		}}
		sequencer := &Sequencer{Readiness: readiness, Clock: clock, API: &APIShutdown{Server: server, PropagationDelay: 5 * time.Second}}
		result := sigterm(t, sequencer, Selection{API})
		clock.advance(t)
		if err := within(t, result, "the shutdown"); err != nil {
			t.Fatal(err)
		}
		within(t, drained, "the drain")
		if got := recorded.all(); !slices.Equal(got, []string{"readiness failed", "wait 5s", "drain"}) {
			t.Fatalf("the steps are %v, want readiness failed, wait 5s, drain", got)
		}
		if code := within(t, response, "the response"); code != http.StatusOK {
			t.Fatalf("the in-flight request was answered %d, want 200", code)
		}
	})

	t.Run("ST-SUP-RP09-LC06-T02 The worker mode stops claiming and finishes the claimed rows", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, "CREATE TABLE outbox (id bigint PRIMARY KEY, state text NOT NULL DEFAULT 'queued')")
		d.exec(t, "INSERT INTO outbox (id) VALUES (1), (2), (3)")
		w := &claimWorker{db: d.db, capacity: 2, release: make(chan struct{})}
		loop, stopLoop := context.WithCancel(ctx)
		defer stopLoop()
		go w.claim(loop)
		deadline := time.Now().Add(10 * time.Second)
		for w.held() < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if w.held() != 2 {
			t.Fatalf("the worker holds %d claimed rows, want 2", w.held())
		}
		sequencer := &Sequencer{Readiness: NewReadiness(d.db), Worker: &WorkerShutdown{StopClaiming: w.stopClaiming, Finish: w.finish, DrainBound: 5 * time.Second}}
		if err := within(t, sigterm(t, sequencer, Selection{Worker}), "the shutdown"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		states := map[int64]string{}
		rows, err := d.other.QueryContext(ctx, "SELECT id, state FROM outbox")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int64
			var state string
			if err := rows.Scan(&id, &state); err != nil {
				t.Fatal(err)
			}
			states[id] = state
		}
		rows.Close()
		if states[1] != "done" || states[2] != "done" || states[3] != "queued" {
			t.Fatalf("the rows are %v, want 1 and 2 done and 3 queued", states)
		}
		if n := w.claimsAfterStop.Load(); n != 0 {
			t.Fatalf("the worker sent %d claim statements after the signal, want none", n)
		}
	})

	t.Run("ST-SUP-RP09-LC06-T03 The controller mode flushes the checkpoint under the lease, then releases the lease", func(t *testing.T) {
		d := openDatabase(t)
		const change = "ST-CHG-01"
		d.exec(t, "CREATE TABLE items (id bigint PRIMARY KEY, old text NOT NULL, new_value bigint)")
		d.exec(t, "INSERT INTO items (id, old) SELECT n, n::text FROM generate_series(1, 250) AS n")
		d.exec(t, "INSERT INTO stego_schema.backfills (change_id) VALUES ($1)", change)
		if err := lease.Declare(ctx, d.db, "stego.backfill."+change); err != nil {
			t.Fatal(err)
		}
		inStep := make(chan struct{})
		flushing := make(chan struct{})
		var calls atomic.Int32
		step := func(ctx context.Context, tx *sql.Tx, after *lease.Key, limit int) (lease.Batch, error) {
			if calls.Add(1) == 2 {
				close(inStep)
				<-flushing
			}
			return moveItems(ctx, tx, after, limit)
		}
		backfill, err := lease.NewBackfill(change, step, lease.BackfillSettings{Key: lease.KeyInteger, BatchSize: 100})
		if err != nil {
			t.Fatal(err)
		}
		client, err := lease.New(d.db, "stego.backfill."+change, "h1", lease.Settings{})
		if err != nil {
			t.Fatal(err)
		}
		heldCh := make(chan *lease.Held, 1)
		holding := make(chan error, 1)
		go func() {
			holding <- client.Hold(ctx, func(ctx context.Context, h *lease.Held) error {
				heldCh <- h
				return backfill.Run(ctx, d.db, h)
			})
		}()
		held := within(t, heldCh, "the lease")
		var stopAt atomic.Int64
		within(t, inStep, "the unflushed batch")
		sequencer := &Sequencer{Readiness: NewReadiness(d.db), Controller: &ControllerShutdown{
			Flush: func(ctx context.Context) error {
				stopAt.Store(int64(len(d.rec.all())))
				close(flushing)
				return backfill.Flush(ctx)
			},
			Release: held.Release,
		}}
		if err := within(t, sigterm(t, sequencer, Selection{Controller}), "the shutdown"); err != nil {
			t.Fatal(err)
		}
		if err := within(t, holding, "the end of Hold"); err != nil {
			t.Fatal(err)
		}
		// After the stop, the batch that ran commits; then the flush must write the checkpoint again with the
		// holder's token, and only then may the lease be released.
		released, batchCommit, flushed := -1, -1, -1
		for i, s := range d.rec.all() {
			switch {
			case strings.HasPrefix(s.sql, "UPDATE stego_schema.backfills"):
				if released >= 0 {
					t.Fatalf("a checkpoint was written after the release: %s", s.sql)
				}
				if s.args[7] != held.Token() {
					t.Fatalf("a checkpoint carries the token %v, want the holder's %d", s.args[7], held.Token())
				}
				if batchCommit >= 0 && flushed < 0 {
					flushed = i
				}
			case s.sql == "commit" && i >= int(stopAt.Load()) && batchCommit < 0:
				batchCommit = i
			case strings.HasPrefix(s.sql, "UPDATE public.leases SET expires_at='-infinity'"):
				if released < 0 {
					released = i
				}
			}
		}
		if batchCommit < 0 || flushed < 0 || released < flushed {
			t.Fatalf("after the stop the batch committed at %d, the flush wrote the checkpoint at %d and the release came at %d; want the flush after the batch and the release after the flush", batchCommit, flushed, released)
		}
		var holder string
		var expired bool
		var lastKey []byte
		if err := d.other.QueryRowContext(ctx, "SELECT holder, expires_at < now() FROM leases").Scan(&holder, &expired); err != nil || holder != "h1" || !expired {
			t.Fatalf("the lease row names %s, expired %v (%v), want h1 released", holder, expired, err)
		}
		if err := d.other.QueryRowContext(ctx, "SELECT last_key FROM stego_schema.backfills").Scan(&lastKey); err != nil || string(lastKey) != "200" {
			t.Fatalf("the checkpoint is %v (%v), want the flushed batch's key 200", lastKey, err)
		}
	})

	t.Run("ST-SUP-RP09-LC06-T04 The sequencer runs the sequence of every selected mode", func(t *testing.T) {
		readiness := NewReadiness(pingOK{})
		server, _ := serve(t, readiness, nil)
		recorded := &steps{}
		drained := make(chan struct{})
		server.RegisterOnShutdown(func() {
			recorded.add("api drain")
			close(drained)
		})
		sequencer := &Sequencer{
			Readiness: readiness,
			API:       &APIShutdown{Server: server},
			Worker: &WorkerShutdown{
				StopClaiming: func() { recorded.add("worker claim stop") },
				Finish:       func(context.Context) error { recorded.add("worker finish"); return nil },
			},
			Controller: &ControllerShutdown{
				Flush:   func(context.Context) error { recorded.add("controller flush"); return nil },
				Release: func(context.Context) error { recorded.add("controller release"); return nil },
			},
		}
		if err := within(t, sigterm(t, sequencer, Selection{API, Worker}), "the shutdown"); err != nil {
			t.Fatal(err)
		}
		within(t, drained, "the drain")
		got := recorded.all()
		for _, name := range []string{"api drain", "worker claim stop", "worker finish"} {
			if !slices.Contains(got, name) {
				t.Fatalf("the steps are %v, want %s", got, name)
			}
		}
		for _, name := range got {
			if strings.HasPrefix(name, "controller") {
				t.Fatalf("the steps are %v, want no step of the controller sequence", got)
			}
		}
	})
}

// claimWorker is a stand-in for the outbox worker: it claims queued rows with SKIP LOCKED while it has capacity,
// and finishes each claimed row when the test releases it.
type claimWorker struct {
	db       *sql.DB
	capacity int
	release  chan struct{}

	mu              sync.Mutex
	stopped         bool
	inFlight        int
	wg              sync.WaitGroup
	claimsAfterStop atomic.Int32
}

func (w *claimWorker) held() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inFlight
}

func (w *claimWorker) claim(ctx context.Context) {
	for ctx.Err() == nil {
		w.mu.Lock()
		if w.stopped {
			w.mu.Unlock()
			return
		}
		if w.inFlight >= w.capacity {
			w.mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		var id int64
		err := w.db.QueryRowContext(ctx, "UPDATE outbox SET state='claimed' WHERE id = (SELECT id FROM outbox WHERE state='queued' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING id").Scan(&id)
		if w.stopped {
			w.claimsAfterStop.Add(1)
		}
		if err == nil {
			w.inFlight++
			w.wg.Add(1)
			go w.process(id)
		}
		w.mu.Unlock()
		if err != nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (w *claimWorker) process(id int64) {
	defer w.wg.Done()
	<-w.release
	_, _ = w.db.ExecContext(context.Background(), "UPDATE outbox SET state='done' WHERE id=$1", id)
}

func (w *claimWorker) stopClaiming() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
}

func (w *claimWorker) finish(ctx context.Context) error {
	close(w.release)
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// moveItems is the stand-in for a generated backfill step that copies old into new_value in key order.
func moveItems(ctx context.Context, tx *sql.Tx, after *lease.Key, limit int) (lease.Batch, error) {
	var arg any
	if after != nil {
		arg = after.Args()[0]
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM items WHERE ($1::bigint IS NULL OR id > $1::bigint) AND new_value IS NULL ORDER BY id LIMIT $2 FOR UPDATE", arg, limit)
	if err != nil {
		return lease.Batch{}, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return lease.Batch{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var batch lease.Batch
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE items SET new_value=old::bigint WHERE id=$1", id); err != nil {
			return lease.Batch{}, err
		}
		batch.Read++
		batch.Moved++
		batch.LastKey = lease.IntegerKey(id)
	}
	return batch, rows.Err()
}
