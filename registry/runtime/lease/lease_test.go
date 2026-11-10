package lease

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
)

// The integration tests of this package run against the local PostgreSQL server of pgtest: the server that
// STEGO_TEST_POSTGRES_DSN names, else a container that the test binary starts and removes (ST-STD-LOCAL-R04).
// Each test makes its own database on that server and drops it at the end.

// TestMain gives the PostgreSQL tests of the package their local server (ST-STD-LOCAL-R04).
func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// statement is one statement that the recorder read, with the connection that sent it.
type statement struct {
	conn *pgx.Conn
	sql  string
	args []any
}

// recorder reads every statement that a connection of the database sends.
type recorder struct {
	mu         sync.Mutex
	statements []statement
}

func (r *recorder) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	var args []any
	for _, arg := range data.Args {
		if _, ok := arg.(pgx.QueryResultFormatsByOID); !ok {
			args = append(args, arg)
		}
	}
	r.statements = append(r.statements, statement{conn: conn, sql: data.SQL, args: args})
	return ctx
}

func (r *recorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *recorder) all() []statement {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]statement(nil), r.statements...)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = nil
}

// database is one test database: db sends through the recorder, other is a second session with no recorder.
type database struct {
	db    *sql.DB
	other *sql.DB
	rec   *recorder
}

func openDatabase(t *testing.T) *database {
	t.Helper()
	ctx := context.Background()
	config, err := pgx.ParseConfig(pgtest.Database(t, "lease"))
	if err != nil {
		t.Fatal(err)
	}
	plain := config.Copy()
	rec := &recorder{}
	config.Tracer = rec
	d := &database{db: stdlib.OpenDB(*config), other: stdlib.OpenDB(*plain), rec: rec}
	t.Cleanup(func() { _ = d.db.Close(); _ = d.other.Close() })
	for _, ddl := range []string{LeasesTable, "CREATE SCHEMA stego_schema", BackfillsTable} {
		if _, err := d.other.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func (d *database) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := d.other.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// leaseRow reads the lease row from the second session.
func (d *database) leaseRow(t *testing.T, name string) (holder string, token int64, expires time.Time) {
	t.Helper()
	err := d.other.QueryRowContext(context.Background(), "SELECT holder, token, CASE WHEN isfinite(expires_at) THEN expires_at ELSE 'epoch' END FROM leases WHERE name=$1", name).Scan(&holder, &token, &expires)
	if err != nil {
		t.Fatal(err)
	}
	return holder, token, expires
}

// expire moves expires_at of the lease into the past from the second session, as the database clock would.
func (d *database) expire(t *testing.T, name string) {
	t.Helper()
	d.exec(t, "UPDATE leases SET expires_at=now()-interval '1 second' WHERE name=$1", name)
}

// fakeClock is a process clock that moves only when the test advances it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
}

// waitWaiters waits on the real clock until n timers wait on the fake clock.
func (c *fakeClock) waitWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		count := len(c.waiters)
		c.mu.Unlock()
		if count >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %d timers wait on the fake clock", n)
}

func newClient(t *testing.T, db DB, name, holder string, settings Settings) *Client {
	t.Helper()
	c, err := New(db, name, holder, settings)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func acquire(t *testing.T, c *Client) *Held {
	t.Helper()
	h, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire as %s: %v", c.Holder(), err)
	}
	return h
}

// leaseStatements is the recorded statements that name the leases table.
func leaseStatements(statements []statement) []statement {
	var out []statement
	for _, s := range statements {
		if strings.Contains(s.sql, "leases") {
			out = append(out, s)
		}
	}
	return out
}

// noDB is a DB for the unit tests that need no database.
type noDB struct{}

func (noDB) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("no database")
}
func (noDB) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }
func (noDB) BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error) {
	return nil, errors.New("no database")
}

func TestLeaseIsOneRow(t *testing.T) {
	t.Run("ST-SUP-RP09-LC02-T01 Acquire and Renew each run their own declared UPDATE", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		c := newClient(t, d.db, "stego.controller", "h1", Settings{})
		d.rec.reset()
		h := acquire(t, c)
		if err := h.Renew(ctx); err != nil {
			t.Fatal(err)
		}
		wantAcquire := "UPDATE leases SET holder=$1, expires_at=now()+ttl, token=token+1 WHERE name=$2 AND (holder=$1 OR expires_at<now()) RETURNING token"
		wantRenew := "UPDATE leases SET expires_at=now()+ttl, token=token+1 WHERE name=$2 AND holder=$1 AND token=$3 AND expires_at>=now() RETURNING token"
		sent := d.rec.all()
		if len(sent) != 2 {
			t.Fatalf("Acquire and Renew sent %d statements, want 2: %v", len(sent), sent)
		}
		if got := strings.Replace(sent[0].sql, "make_interval(secs => $3)", "ttl", 1); got != wantAcquire {
			t.Errorf("Acquire sent %q, want %q with ttl as $3", sent[0].sql, wantAcquire)
		}
		if a := sent[0].args; len(a) != 3 || a[0] != "h1" || a[1] != "stego.controller" || a[2] != 15.0 {
			t.Errorf("Acquire binds %v, want h1, stego.controller and the lease of 15 s", a)
		}
		if got := strings.Replace(sent[1].sql, "make_interval(secs => $4)", "ttl", 1); got != wantRenew {
			t.Errorf("Renew sent %q, want %q with ttl as $4", sent[1].sql, wantRenew)
		}
		if a := sent[1].args; len(a) != 4 || a[0] != "h1" || a[1] != "stego.controller" || a[2] != h.Token()-1 || a[3] != 15.0 {
			t.Errorf("Renew binds %v, want h1, stego.controller, the token it held and the lease of 15 s", a)
		}
	})

	t.Run("ST-SUP-RP09-LC02-T02 Each acquire and renew increments the token", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		_, n, _ := d.leaseRow(t, "stego.controller")
		h1 := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{TTL: time.Second, Renew: 500 * time.Millisecond}))
		tokens := []int64{h1.Token()}
		for range 2 {
			if err := h1.Renew(ctx); err != nil {
				t.Fatal(err)
			}
			tokens = append(tokens, h1.Token())
		}
		d.expire(t, "stego.controller")
		h2 := acquire(t, newClient(t, d.db, "stego.controller", "h2", Settings{}))
		tokens = append(tokens, h2.Token())
		for i, token := range tokens {
			if token != n+int64(i)+1 {
				t.Fatalf("the tokens are %v, want %d, %d, %d and %d", tokens, n+1, n+2, n+3, n+4)
			}
		}
	})

	t.Run("ST-SUP-RP09-LC02-T03 Another holder cannot acquire an unexpired lease", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		h1 := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{}))
		h2, err := newClient(t, d.db, "stego.controller", "h2", Settings{}).Acquire(ctx)
		if !errors.Is(err, ErrTaken) || h2 != nil {
			t.Fatalf("h2 got %v, %v, want no token and ErrTaken", h2, err)
		}
		holder, token, _ := d.leaseRow(t, "stego.controller")
		if holder != "h1" || token != h1.Token() {
			t.Fatalf("the row holds %s with token %d, want h1 with %d", holder, token, h1.Token())
		}
	})

	t.Run("ST-SUP-RP09-LC02-T06 A renew after a takeover and a release takes nothing", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		a := acquire(t, newClient(t, d.db, "stego.controller", "a", Settings{}))
		d.expire(t, "stego.controller")
		b := acquire(t, newClient(t, d.db, "stego.controller", "b", Settings{}))
		if err := b.Release(ctx); err != nil {
			t.Fatal(err)
		}
		holder, token, expires := d.leaseRow(t, "stego.controller")
		if err := a.Renew(ctx); !errors.Is(err, ErrLost) {
			t.Fatalf("the renew of A gave %v, want ErrLost and no token", err)
		}
		if h, k, e := d.leaseRow(t, "stego.controller"); h != holder || k != token || !e.Equal(expires) || e.After(time.Now()) {
			t.Fatalf("after the renew the row is %s, %d, %s, want B's release left as it is: %s, %d, %s", h, k, e, holder, token, expires)
		}
		if _, err := newClient(t, d.db, "stego.controller", "c", Settings{}).Acquire(ctx); err != nil {
			t.Fatalf("C could not acquire the lease at once: %v", err)
		}
	})

	t.Run("ST-SUP-RP09-LC02-T07 A renew after the lease expired takes nothing", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		a := acquire(t, newClient(t, d.db, "stego.controller", "a", Settings{}))
		d.expire(t, "stego.controller")
		holder, token, expires := d.leaseRow(t, "stego.controller")
		if err := a.Renew(ctx); !errors.Is(err, ErrLost) {
			t.Fatalf("the renew after the expiry gave %v, want ErrLost and no token", err)
		}
		if h, k, e := d.leaseRow(t, "stego.controller"); h != holder || k != token || !e.Equal(expires) {
			t.Fatalf("after the renew the row is %s, %d, %s, want it unchanged: %s, %d, %s", h, k, e, holder, token, expires)
		}
	})

	t.Run("ST-SUP-RP09-LC02-T04 The lease defaults are 15 s, 10 s and 2 s", func(t *testing.T) {
		c := newClient(t, noDB{}, "stego.controller", "h1", Settings{})
		if c.TTL() != 15*time.Second || c.RenewEvery() != 10*time.Second || c.RetryEvery() != 2*time.Second {
			t.Fatalf("the defaults are %s, %s and %s, want 15s, 10s and 2s", c.TTL(), c.RenewEvery(), c.RetryEvery())
		}
	})

	t.Run("ST-SUP-RP09-LC02-T05 The lease uses no session-scoped advisory lock", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		h := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{}))
		if err := h.Renew(ctx); err != nil {
			t.Fatal(err)
		}
		var locks int
		if err := d.other.QueryRowContext(ctx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory'").Scan(&locks); err != nil {
			t.Fatal(err)
		}
		if locks != 0 {
			t.Fatalf("pg_locks holds %d advisory locks between two renews, want none", locks)
		}
		if err := h.Renew(ctx); err != nil {
			t.Fatal(err)
		}
		sources, err := filepath.Glob("*.go")
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range sources {
			if strings.HasSuffix(source, "_test.go") {
				continue
			}
			text, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			for _, function := range []string{"pg_advisory_lock", "pg_advisory_lock_shared", "pg_try_advisory_lock", "pg_try_advisory_lock_shared"} {
				if strings.Contains(string(text), function) {
					t.Errorf("%s calls %s", source, function)
				}
			}
		}
	})
}

func TestWriteCarriesToken(t *testing.T) {
	writes := "CREATE TABLE writes (v text NOT NULL, token bigint NOT NULL)"
	insert := func(v string) func(context.Context, *sql.Tx, int64) error {
		return func(ctx context.Context, tx *sql.Tx, token int64) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO writes (v, token) VALUES ($1, $2)", v, token)
			return err
		}
	}
	count := func(t *testing.T, d *database) int {
		t.Helper()
		var n int
		if err := d.other.QueryRowContext(context.Background(), "SELECT count(*) FROM writes").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("ST-SUP-RP09-LC03-T01 A write under the lease carries the holder's token", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, writes)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		h := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{}))
		d.rec.reset()
		if err := h.WithToken(ctx, insert("one")); err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, s := range d.rec.all() {
			if strings.HasPrefix(s.sql, "INSERT INTO writes") {
				found = len(s.args) == 2 && s.args[1] == h.Token()
			}
		}
		if !found {
			t.Fatalf("no write bound the holder's token %d: %v", h.Token(), d.rec.all())
		}
		var token int64
		if err := d.other.QueryRowContext(ctx, "SELECT token FROM writes WHERE v='one'").Scan(&token); err != nil || token != h.Token() {
			t.Fatalf("the write holds token %d (%v), want %d", token, err, h.Token())
		}
	})

	t.Run("ST-SUP-RP09-LC03-T02 A write fails with the fenced error when the lease row holds a higher token", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, writes)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		h1 := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{}))
		d.expire(t, "stego.controller")
		acquire(t, newClient(t, d.db, "stego.controller", "h2", Settings{}))
		err := h1.WithToken(ctx, insert("stale"))
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("the write of h1 gave %v, want ErrFenced", err)
		}
		if n := count(t, d); n != 0 {
			t.Fatalf("%d writes applied, want none", n)
		}
	})

	t.Run("ST-SUP-RP09-LC03-T05 A write under the lease reads the holder's state while a renew waits for the row", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, writes)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		h := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{}))
		before := h.Token()
		inWrite := make(chan struct{})
		renewWaits := make(chan struct{})
		renewed := make(chan error, 1)
		var reads []time.Duration
		var nested int64
		var nestedErr error
		go func() {
			<-inWrite
			go func() { renewed <- h.Renew(ctx) }()
			// The renew's UPDATE waits for the lease row that the write holds FOR SHARE.
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				var waiting int
				if err := d.other.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'").Scan(&waiting); err != nil || waiting > 0 {
					break
				}
			}
			close(renewWaits)
		}()
		err := h.WithToken(ctx, func(ctx context.Context, tx *sql.Tx, token int64) error {
			close(inWrite)
			<-renewWaits
			start := time.Now()
			_ = h.Token()
			reads = append(reads, time.Since(start))
			start = time.Now()
			_ = h.Expires()
			reads = append(reads, time.Since(start))
			start = time.Now()
			nested, nestedErr = h.Fence(ctx, tx)
			reads = append(reads, time.Since(start))
			return insert("during the renew")(ctx, tx, token)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := within(t, renewed, "the renew"); err != nil {
			t.Fatalf("the renew gave %v", err)
		}
		for i, took := range reads {
			if took > time.Second {
				t.Fatalf("read %d in the write waited %s for the renew, want no wait", i, took)
			}
		}
		if nestedErr != nil || nested != before {
			t.Fatalf("the nested fence gave %d, %v, want the token %d", nested, nestedErr, before)
		}
		if h.Token() != before+1 {
			t.Fatalf("after the renew the token is %d, want %d", h.Token(), before+1)
		}
	})

	t.Run("ST-SUP-RP09-LC03-T03 The holder's loop stops on the fenced error", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, writes)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		clock := newFakeClock()
		c := newClient(t, d.db, "stego.controller", "h1", Settings{Clock: clock})
		first := make(chan struct{})
		takenOver := make(chan struct{})
		var after []error
		result := make(chan error, 1)
		go func() {
			result <- c.Hold(ctx, func(ctx context.Context, h *Held) error {
				if err := h.WithToken(ctx, insert("first")); err != nil {
					return err
				}
				close(first)
				<-takenOver
				for range 3 {
					after = append(after, h.WithToken(ctx, insert("after")))
				}
				return nil
			})
		}()
		within(t, first, "the first write")
		d.expire(t, "stego.controller")
		acquire(t, newClient(t, d.db, "stego.controller", "h2", Settings{}))
		close(takenOver)
		select {
		case err := <-result:
			if !errors.Is(err, ErrFenced) {
				t.Fatalf("the loop returned %v, want ErrFenced", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the loop did not stop")
		}
		if !errors.Is(after[0], ErrFenced) || !errors.Is(after[1], ErrStopped) || !errors.Is(after[2], ErrStopped) {
			t.Fatalf("the writes after the takeover gave %v, want ErrFenced and then ErrStopped", after)
		}
		if n := count(t, d); n != 1 {
			t.Fatalf("%d writes applied, want only the first", n)
		}
	})
}

// holdUntilCancel holds the lease with a fake clock and a work function that waits for its context. It returns the
// fake clock, the held lease, the channel of the clock time at the cancel and the result of Hold.
func holdUntilCancel(t *testing.T, d *database, onCancel func(h *Held)) (*fakeClock, *Held, chan time.Time, chan error) {
	t.Helper()
	ctx := context.Background()
	if err := Declare(ctx, d.db, "stego.controller"); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	c := newClient(t, d.db, "stego.controller", "h1", Settings{Clock: clock})
	held := make(chan *Held, 1)
	cancelled := make(chan time.Time, 1)
	result := make(chan error, 1)
	go func() {
		result <- c.Hold(ctx, func(ctx context.Context, h *Held) error {
			held <- h
			<-ctx.Done()
			cancelled <- clock.Now()
			if onCancel != nil {
				onCancel(h)
			}
			return ctx.Err()
		})
	}()
	h := within(t, held, "the held lease")
	clock.waitWaiters(t, 1)
	return clock, h, cancelled, result
}

func TestHolderStops(t *testing.T) {
	t.Run("ST-SUP-RP09-LC04-T01 A holder whose renew fails stops within one renew interval and before the lease expires", func(t *testing.T) {
		d := openDatabase(t)
		clock, h, cancelled, result := holdUntilCancel(t, d, nil)
		_, _, expires := d.leaseRow(t, "stego.controller")
		d.exec(t, "ALTER TABLE leases RENAME TO leases_refused")
		failure := clock.Now().Add(h.RenewEvery())
		clock.Advance(h.RenewEvery())
		select {
		case at := <-cancelled:
			if at.Sub(failure) > h.RenewEvery() {
				t.Fatalf("the work was cancelled %s after the failed renew, more than one renew interval", at.Sub(failure))
			}
			if !at.Before(expires) {
				t.Fatalf("the work was cancelled at %s, not before expires_at %s", at, expires)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the work was not cancelled")
		}
		if err := within(t, result, "the end of Hold"); !errors.Is(err, ErrLost) {
			t.Fatalf("Hold returned %v, want ErrLost", err)
		}
	})

	t.Run("ST-SUP-RP09-LC04-T02 A holder whose renew returns another token stops", func(t *testing.T) {
		d := openDatabase(t)
		clock, h, cancelled, result := holdUntilCancel(t, d, nil)
		_, _, expires := d.leaseRow(t, "stego.controller")
		d.exec(t, "UPDATE leases SET token=token+5 WHERE name='stego.controller'")
		renewAt := clock.Now().Add(h.RenewEvery())
		clock.Advance(h.RenewEvery())
		select {
		case at := <-cancelled:
			if at.Sub(renewAt) > h.RenewEvery() || !at.Before(expires) {
				t.Fatalf("the work was cancelled at %s, want within one renew interval of %s and before %s", at, renewAt, expires)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the work was not cancelled")
		}
		if err := within(t, result, "the end of Hold"); !errors.Is(err, ErrLost) {
			t.Fatalf("Hold returned %v, want ErrLost", err)
		}
	})

	t.Run("ST-SUP-RP09-LC04-T03 A holder does not act on the lease after it stopped", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, "CREATE TABLE writes (v text NOT NULL, token bigint NOT NULL)")
		late := make(chan error, 1)
		clock, h, _, result := holdUntilCancel(t, d, func(h *Held) {
			late <- h.WithToken(context.Background(), func(ctx context.Context, tx *sql.Tx, token int64) error {
				_, err := tx.ExecContext(ctx, "INSERT INTO writes (v, token) VALUES ('late', $1)", token)
				return err
			})
		})
		d.exec(t, "UPDATE leases SET token=token+5 WHERE name='stego.controller'")
		clock.Advance(h.RenewEvery())
		if err := within(t, late, "the write after the stop"); !errors.Is(err, ErrStopped) {
			t.Fatalf("the write after the stop gave %v, want ErrStopped", err)
		}
		within(t, result, "the end of Hold")
		var n int
		if err := d.other.QueryRowContext(context.Background(), "SELECT count(*) FROM writes").Scan(&n); err != nil || n != 0 {
			t.Fatalf("%d writes applied (%v), want none", n, err)
		}
	})
}

// within receives from ch, and fails the test when nothing comes within 10 seconds.
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
