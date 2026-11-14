package outbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// The integration tests of this package use the PostgreSQL server of pgtest: the local container that pgtest
// starts, unprivileged and pinned by digest, or the server that STEGO_TEST_POSTGRES_DSN names (ST-STD-LOCAL). Each
// test makes its own database and runtime role, applies Migration and drops both at the end. The tests reach
// PostgreSQL through pgx's database/sql driver (ST-STD-RUNTIME-R06).

// crashEnv names the mode of the helper process of the crash tests.
const crashEnv = "STEGO_OUTBOX_CRASH_MODE"

func TestMain(m *testing.M) {
	if mode := os.Getenv(crashEnv); mode != "" {
		crashHelper(mode)
		return
	}
	os.Exit(pgtest.Main(m))
}

// identity is a verified identity for the tests; only a package of the runtime module can make one.
type identity struct {
	seal.IdentityMark
	tenant string
}

func (i identity) TenantClaim() (string, bool) { return i.tenant, i.tenant != "" }

func testTenant(t testing.TB, id string) tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(tenant.Claim(identity{IdentityMark: seal.NewIdentityMark(id, true), tenant: id}))
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

// fakeClock is a clock that the test advances. It starts at a whole second, so the database keeps its times exactly.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Now().UTC().Truncate(time.Second).Add(time.Hour)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// admins maps the worker pool of each test database to its superuser pool, which the tests use to read and set up
// rows past row-level security, and requests maps it to the pool of the request-path runtime role.
var admins, requests sync.Map

// relays maps the worker pool of each test database to the relay role of that database.
var relays sync.Map

// relayRole gives the relay role of the test database of db.
func relayRole(t testing.TB, db *sql.DB) string {
	t.Helper()
	r, ok := relays.Load(db)
	if !ok {
		t.Fatal("the pool is not a runtime pool of testDatabase")
	}
	return r.(string)
}

// requestPool returns the pool of the request-path runtime role of the test database whose worker pool is db.
func requestPool(t testing.TB, db *sql.DB) *sql.DB {
	t.Helper()
	r, ok := requests.Load(db)
	if !ok {
		t.Fatal("the pool is not a worker pool of testDatabase")
	}
	return r.(*sql.DB)
}

// admin returns the superuser pool of the test database whose runtime pool is db.
func admin(t testing.TB, db *sql.DB) *sql.DB {
	t.Helper()
	a, ok := admins.Load(db)
	if !ok {
		t.Fatal("the pool is not a runtime pool of testDatabase")
	}
	return a.(*sql.DB)
}

func randomSuffix(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// testDatabase makes a database for one test on the server of pgtest, with a request-path runtime role and a worker
// role of its own, each no superuser, owning nothing and holding no BYPASSRLS. As superuser it applies Migration for
// the two roles and makes the application tables records, audit_rows and hook_rows. It returns the pool of the
// worker role, which every queue and worker of the test uses, and the DSN of the worker role; requestPool gives the
// pool of the runtime role. The roles and the database are dropped at the end.
func testDatabase(t testing.TB) (*sql.DB, string) {
	t.Helper()
	server := pgtest.DSN(t)
	root, err := sql.Open("pgx", server)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	suffix := randomSuffix(t)
	request, role, relay := "outbox_runtime_"+suffix, "outbox_worker_"+suffix, "outbox_"+suffix+RelaySuffix
	password := randomSuffix(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The migration makes the relay role of the test; it is dropped after the database, as the other roles are.
	t.Cleanup(func() {
		if _, err := root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+relay); err != nil {
			t.Errorf("drop the role %s: %v", relay, err)
		}
	})
	for _, r := range []string{request, role} {
		if _, err := root.ExecContext(ctx, "CREATE ROLE "+r+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"'"); err != nil {
			t.Fatal(err)
		}
		// Registered before the database, so it runs after the drop of the database.
		t.Cleanup(func() {
			if _, err := root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+r); err != nil {
				t.Errorf("drop the role %s: %v", r, err)
			}
		})
	}
	dsn := pgtest.Database(t, "outbox")
	adminDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	ddl, err := Migration(relay, request, role)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminDB.ExecContext(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := adminDB.ExecContext(ctx, `CREATE TABLE records (id text PRIMARY KEY);
		CREATE TABLE audit_rows (message_id uuid NOT NULL, tenant_id text NOT NULL, destination text NOT NULL,
			subject text NOT NULL, resolution text NOT NULL, at timestamptz NOT NULL);
		CREATE TABLE hook_rows (message_id uuid NOT NULL, group_id text NOT NULL, state text NOT NULL,
			tenant_id text NOT NULL);
		GRANT SELECT, INSERT ON records, audit_rows, hook_rows TO `+role+`, `+request); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(12)
	t.Cleanup(func() { _ = db.Close() })
	admins.Store(db, adminDB)
	t.Cleanup(func() { admins.Delete(db) })
	r := *u
	r.User = url.UserPassword(request, password)
	requestDB, err := sql.Open("pgx", r.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = requestDB.Close() })
	requests.Store(db, requestDB)
	t.Cleanup(func() { requests.Delete(db) })
	relays.Store(db, relay)
	t.Cleanup(func() { relays.Delete(db) })
	return db, u.String()
}

func message(destination, key string) Message {
	return Message{ID: uuid.New(), Destination: destination, ResourceKey: key, Kind: "record.created",
		Payload: []byte(`{"version":1}`)}
}

// enqueue writes messages in one transaction of tenant-a through tenant.Begin and tenant.Commit.
func enqueue(t testing.TB, db *sql.DB, q *Queue, messages ...Message) {
	t.Helper()
	enqueueAs(t, db, q, "tenant-a", messages...)
}

// enqueueAs writes messages in one transaction of the tenant tenantID.
func enqueueAs(t testing.TB, db *sql.DB, q *Queue, tenantID string, messages ...Message) {
	t.Helper()
	ctx := context.Background()
	tn := testTenant(t, tenantID)
	tx, err := tenant.Begin(ctx, db, tn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := q.Enqueue(ctx, tx, messages...); err != nil {
		t.Fatal(err)
	}
	if err := tenant.Commit(ctx, tx, tn); err != nil {
		t.Fatal(err)
	}
}

func count(t testing.TB, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := admin(t, db).QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// stored is one row of the queue table as the tests read it.
type stored struct {
	State       State
	Attempts    int64
	Conflicts   int64
	Lookups     int64
	Token       uuid.NullUUID
	OutgoingKey string
	FailureCode string
	ParkedFrom  sql.NullString
	AvailableAt time.Time
	Resolution  sql.NullString
	ResolvedBy  sql.NullString
	ResolvedAt  sql.NullTime
	TenantID    string
}

func readRow(t testing.TB, db *sql.DB, id uuid.UUID) stored {
	t.Helper()
	var s stored
	var state string
	if err := admin(t, db).QueryRow(`SELECT state, attempts, conflicts, lookups, lease_token, outgoing_key, failure_code,
		parked_from, available_at, resolution, resolved_by, resolved_at, tenant_id
		FROM stego_outbox.messages WHERE id = $1`, id).Scan(&state, &s.Attempts, &s.Conflicts, &s.Lookups, &s.Token,
		&s.OutgoingKey, &s.FailureCode, &s.ParkedFrom, &s.AvailableAt, &s.Resolution, &s.ResolvedBy, &s.ResolvedAt,
		&s.TenantID); err != nil {
		t.Fatal(err)
	}
	s.State = State(state)
	return s
}

// deadLetters lists the dead letters of tenant-a in a transaction of that tenant.
func deadLetters(t testing.TB, db *sql.DB, q *Queue, limit int) ([]DeadLetter, error) {
	t.Helper()
	ctx := context.Background()
	tn := testTenant(t, "tenant-a")
	tx, err := tenant.Begin(ctx, db, tn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	return q.DeadLetters(ctx, tx, limit)
}

func newQueue(t testing.TB, db *sql.DB, clock Clock) *Queue {
	t.Helper()
	q, err := New(db, Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestQueueTransaction(t *testing.T) {
	t.Run("ST-SUP-RP01-LC01-T01 The emitted queue commits the message with the application write", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		ctx := context.Background()
		tn := testTenant(t, "tenant-a")
		for _, commit := range []bool{false, true} {
			tx, err := tenant.Begin(ctx, db, tn, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec("INSERT INTO records VALUES ('one')"); err != nil {
				t.Fatal(err)
			}
			m := message("audit", "one")
			if err := q.Enqueue(ctx, tx, m); err != nil {
				t.Fatal(err)
			}
			if count(t, db, "SELECT count(*) FROM records") != 0 || count(t, db, "SELECT count(*) FROM stego_outbox.messages") != 0 {
				t.Fatal("data of an open transaction is visible")
			}
			want := 0
			if commit {
				err, want = tenant.Commit(ctx, tx, tn), 1
			} else {
				err = tx.Rollback()
			}
			if err != nil {
				t.Fatal(err)
			}
			if count(t, db, "SELECT count(*) FROM records") != want || count(t, db, "SELECT count(*) FROM stego_outbox.messages") != want {
				t.Fatal("the application row and the message did not commit together")
			}
			if commit {
				if row := readRow(t, db, m.ID); row.State != Pending || row.TenantID != "tenant-a" || row.OutgoingKey != m.ID.String() {
					t.Fatalf("the committed message is %+v", row)
				}
			}
		}
		// A transaction with no tenant writes no message, and a payload that is not valid sends no statement.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if err := q.Enqueue(ctx, tx, message("audit", "two")); err == nil {
			t.Fatal("a transaction with no tenant enqueued a message")
		}
		bad := message("audit", "three")
		bad.Payload = []byte(`{"a":1,"a":2}`)
		if err := q.Enqueue(ctx, nil, bad); err == nil || ValidateMessage(bad) == nil {
			t.Fatal("a payload with a duplicate member was accepted")
		}
		for _, payload := range []string{`{"a":"x\u0000y"}`, `[1]`, `{"a":1} {}`, "{\"a\":\"\xff\"}"} {
			bad.Payload = []byte(payload)
			if err := ValidateMessage(bad); err == nil {
				t.Fatalf("the payload %q was accepted", payload)
			}
		}
	})
}

func TestQueueClaim(t *testing.T) {
	t.Run("ST-SUP-RP01-LC02-T01 Concurrent workers claim distinct messages", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		for i := 0; i < 32; i++ {
			enqueue(t, db, q, message("audit", fmt.Sprint(i)))
		}
		var workers sync.WaitGroup
		results := make(chan []Delivery, 8)
		failures := make(chan error, 8)
		for i := 0; i < 8; i++ {
			workers.Go(func() {
				deliveries, err := q.Claim(context.Background(), 8, time.Minute)
				if err != nil {
					failures <- err
					return
				}
				results <- deliveries
			})
		}
		workers.Wait()
		close(results)
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		seen := map[uuid.UUID]bool{}
		for batch := range results {
			for _, d := range batch {
				if seen[d.ID] {
					t.Fatal("two workers claimed one message")
				}
				seen[d.ID] = true
				if d.Tenant.ID() != "tenant-a" || d.Receipt.State != Dispatching {
					t.Fatalf("the claim gave %+v", d)
				}
			}
		}
		if len(seen) != 32 {
			t.Fatalf("the workers claimed %d messages, want 32", len(seen))
		}
	})

	t.Run("ST-SUP-RP01-LC02-T02 An expired lease cannot acknowledge or retry a message", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		m := message("audit", "one")
		enqueue(t, db, q, m)
		first, err := q.Claim(context.Background(), 1, time.Second)
		if err != nil || len(first) != 1 {
			t.Fatalf("claim: %v %v", first, err)
		}
		clock.Advance(2 * time.Second)
		if ok, err := q.acknowledge(context.Background(), first[0].Receipt); ok || err != nil {
			t.Fatalf("an expired receipt acknowledged the message: %v %v", ok, err)
		}
		if ok, err := q.retry(context.Background(), first[0].Receipt, 0, "failure"); ok || err != nil {
			t.Fatalf("an expired receipt retried the message: %v %v", ok, err)
		}
		if row := readRow(t, db, m.ID); row.State != Dispatching || row.Token.UUID != first[0].Receipt.Token {
			t.Fatalf("the message changed: %+v", row)
		}
	})

	t.Run("ST-SUP-RP01-LC03-T01 Concurrent enqueues serialize before the sequence", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		ctx := context.Background()
		tn := testTenant(t, "tenant-a")
		a, b := message("audit", "one"), message("audit", "one")
		first, err := tenant.Begin(ctx, db, tn, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = first.Rollback() }()
		if err := q.Enqueue(ctx, first, a); err != nil {
			t.Fatal(err)
		}
		second, err := tenant.Begin(ctx, db, tn, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = second.Rollback() }()
		result := make(chan error, 1)
		go func() { result <- q.Enqueue(ctx, second, b) }()
		select {
		case err := <-result:
			t.Fatalf("the second enqueue passed an open predecessor: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if err := tenant.Commit(ctx, first, tn); err != nil {
			t.Fatal(err)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if err := tenant.Commit(ctx, second, tn); err != nil {
			t.Fatal(err)
		}
		deliveries, err := q.Claim(ctx, 32, time.Minute)
		if err != nil || len(deliveries) != 1 || deliveries[0].ID != a.ID {
			t.Fatalf("the commit order was lost: %v %v", deliveries, err)
		}
	})

	t.Run("ST-SUP-RP01-LC03-T02 Two workers keep the order of one key", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		first, second := message("audit", "one"), message("audit", "one")
		enqueue(t, db, q, first, second)
		var mu sync.Mutex
		var order []uuid.UUID
		entered, release := make(chan struct{}), make(chan struct{})
		handler := func(_ context.Context, _ *sql.Tx, d Delivery) error {
			if d.ID == first.ID {
				close(entered)
				<-release
			}
			mu.Lock()
			order = append(order, d.ID)
			mu.Unlock()
			return nil
		}
		var workers []*Worker
		for i := 0; i < 2; i++ {
			w := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 5 * time.Second })
			register(t, w, Destination{Name: "audit", Handler: handler})
			workers = append(workers, w)
			startWorker(t, w)
		}
		<-entered
		// Both workers poll while the first message is in flight; the second message of its key must wait.
		time.Sleep(200 * time.Millisecond)
		if row := readRow(t, db, second.ID); row.State != Pending || row.Attempts != 0 {
			t.Fatalf("the second message of the key was claimed while the first was in flight: %+v", row)
		}
		close(release)
		await(t, func() bool { return workers[0].Stats().Delivered+workers[1].Stats().Delivered == 2 })
		for _, w := range workers {
			w.StopClaiming()
		}
		mu.Lock()
		defer mu.Unlock()
		if len(order) != 2 || order[0] != first.ID || order[1] != second.ID {
			t.Fatalf("the delivery order is %v", order)
		}
	})

	t.Run("ST-SUP-RP01-LC03-T02 A retry holds only its own key", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		ctx := context.Background()
		a, b, other := message("audit", "one"), message("audit", "one"), message("audit", "two")
		enqueue(t, db, q, a, b, other)
		first, err := q.Claim(ctx, 32, time.Minute)
		if err != nil || len(first) != 2 {
			t.Fatalf("first claim: %v %v", first, err)
		}
		var head, second Delivery
		for _, d := range first {
			switch d.ID {
			case a.ID:
				head = d
			case other.ID:
				second = d
			default:
				t.Fatal("the successor was claimed before its predecessor")
			}
		}
		if ok, err := q.retry(ctx, head.Receipt, time.Hour, "sink-unavailable"); !ok || err != nil {
			t.Fatalf("retry: %v %v", ok, err)
		}
		if ok, err := q.acknowledge(ctx, second.Receipt); !ok || err != nil {
			t.Fatalf("the other key did not progress: %v %v", ok, err)
		}
		if next, err := q.Claim(ctx, 32, time.Minute); err != nil || len(next) != 0 {
			t.Fatalf("the retry did not hold its successor: %v %v", next, err)
		}
		clock.Advance(time.Hour)
		next, err := q.Claim(ctx, 32, time.Minute)
		if err != nil || len(next) != 1 || next[0].ID != a.ID || next[0].Attempts != 2 || string(next[0].Payload) != `{"version":1}` {
			t.Fatalf("the retry lost its identity or attempt count: %+v %v", next, err)
		}
		if ok, err := q.acknowledge(ctx, next[0].Receipt); !ok || err != nil {
			t.Fatalf("acknowledge: %v %v", ok, err)
		}
		next, err = q.Claim(ctx, 32, time.Minute)
		if err != nil || len(next) != 1 || next[0].ID != b.ID {
			t.Fatalf("the successor did not progress: %v %v", next, err)
		}
	})
}

// crashHelper is the helper process of the crash tests. It exits abruptly in the middle of its work.
func crashHelper(mode string) {
	ctx := context.Background()
	db, err := sql.Open("pgx", os.Getenv("STEGO_OUTBOX_CRASH_DSN"))
	if err != nil {
		os.Exit(2)
	}
	q, err := New(db, Options{})
	if err != nil {
		os.Exit(2)
	}
	tn, err := tenant.New(tenant.Claim(identity{IdentityMark: seal.NewIdentityMark("tenant-a", true), tenant: "tenant-a"}))
	if err != nil {
		os.Exit(2)
	}
	switch mode {
	case "transaction":
		tx, err := tenant.Begin(ctx, db, tn, nil)
		if err != nil {
			os.Exit(2)
		}
		if _, err := tx.Exec("INSERT INTO records VALUES ('crash')"); err != nil {
			os.Exit(2)
		}
		if err := q.Enqueue(ctx, tx, message("audit", "crash")); err != nil {
			os.Exit(2)
		}
		os.Exit(3)
	case "claim":
		deliveries, err := q.Claim(ctx, 1, 5*time.Second)
		if err != nil || len(deliveries) != 1 {
			os.Exit(2)
		}
		os.Exit(3)
	case "handler":
		config := DefaultWorkerConfig()
		config.Concurrency = 1
		w, err := NewWorker(q, config)
		if err != nil {
			os.Exit(2)
		}
		if err := w.Register(Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error {
			os.Exit(3)
			return nil
		}}); err != nil {
			os.Exit(2)
		}
		_, _ = w.cycle(ctx, ctx)
		os.Exit(2)
	}
	os.Exit(2)
}

// runCrash runs the helper process in mode and checks that it exited abruptly, as the mode asks.
func runCrash(t *testing.T, dsn, mode string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), crashEnv+"="+mode, "STEGO_OUTBOX_CRASH_DSN="+dsn)
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("the helper process in mode %s did not exit at its crash point: %v", mode, err)
	}
}

func TestQueueCrash(t *testing.T) {
	t.Run("ST-SUP-RP01-LC04-T03 A process crash commits nothing and releases its lease", func(t *testing.T) {
		db, dsn := testDatabase(t)
		runCrash(t, dsn, "transaction")
		if count(t, db, "SELECT count(*) FROM records") != 0 || count(t, db, "SELECT count(*) FROM stego_outbox.messages") != 0 {
			t.Fatal("an open transaction of a crashed process committed")
		}
		m := message("audit", "claimed")
		enqueue(t, db, newQueue(t, db, nil), m)
		runCrash(t, dsn, "claim")
		if row := readRow(t, db, m.ID); row.State != Dispatching || row.Attempts != 1 {
			t.Fatalf("the claim of the crashed process is not stored: %+v", row)
		}
		// The lease of the crashed process expires; the destination is idempotent, so the message is sent again.
		clock := newClock()
		q := newQueue(t, db, clock)
		w := newWorker(t, q, nil)
		var got []Delivery
		register(t, w, Destination{Name: "audit", Idempotent: true, Handler: func(_ context.Context, _ *sql.Tx, d Delivery) error {
			got = append(got, d)
			return nil
		}})
		for i := 0; i < 3; i++ {
			cycle(t, w)
		}
		if len(got) != 1 || got[0].ID != m.ID || got[0].Attempts != 2 {
			t.Fatalf("the message did not recover with a higher attempt count: %+v", got)
		}
		if row := readRow(t, db, m.ID); row.State != Done {
			t.Fatalf("the recovered message is %+v", row)
		}
	})
}

func TestQueueViews(t *testing.T) {
	t.Run("ST-SUP-RP01-LC06-T02 The blocked view holds one entry for each stuck key", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		ctx := context.Background()
		enqueue(t, db, q, message("audit", "stuck"), message("audit", "stuck"), message("audit", "moving"), message("audit", "other"))
		deliveries, err := q.Claim(ctx, 2, time.Minute)
		if err != nil || len(deliveries) != 2 {
			t.Fatalf("the claim did not lease the two oldest keys: %v %v", deliveries, err)
		}
		blocked, err := q.Blocked(ctx, MaxBatchSize)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, item := range blocked {
			if item.Destination != "audit" || item.BlockedSince.IsZero() || item.AvailableAt.IsZero() || item.State != Dispatching {
				t.Fatalf("the blocked view is not complete: %+v", item)
			}
			keys = append(keys, item.ResourceKey)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "moving,stuck" {
			t.Fatalf("the blocked view is not one entry for each claimed key: %v", keys)
		}
		if ok, err := q.retry(ctx, deliveries[0].Receipt, time.Minute, "delivery-failed"); !ok || err != nil {
			t.Fatalf("retry: %v %v", ok, err)
		}
		blocked, err = q.Blocked(ctx, MaxBatchSize)
		if err != nil || len(blocked) != 2 {
			t.Fatalf("the retried key is not blocked: %v %v", blocked, err)
		}
		found := ""
		for _, item := range blocked {
			if item.ResourceKey == deliveries[0].ResourceKey {
				found = item.FailureCode
			}
		}
		if found != "delivery-failed" {
			t.Fatalf("the blocked view lost the failure code: %+v", blocked)
		}
		clock.Advance(time.Minute)
		blocked, err = q.Blocked(ctx, MaxBatchSize)
		if err != nil || len(blocked) != 1 || blocked[0].ResourceKey != deliveries[1].ResourceKey {
			t.Fatalf("a ready key is still blocked: %+v %v", blocked, err)
		}
		for _, limit := range []int{0, MaxBatchSize + 1} {
			if _, err := q.Blocked(ctx, limit); err == nil {
				t.Fatalf("the blocked limit %d was accepted", limit)
			}
		}
	})
}

func TestQueuePurge(t *testing.T) {
	t.Run("ST-SUP-RP01-LC16-T01 Dead letters and purge keep the unexhausted messages", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		ctx := context.Background()
		doomed, fresh := message("audit", "doomed"), message("audit", "fresh")
		enqueue(t, db, q, doomed)
		w := newWorker(t, q, func(c *WorkerConfig) { c.MaxAttempts = 3 })
		register(t, w, Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error {
			return NotSent(errors.New("the sink is down"))
		}})
		for i := 0; i < 3; i++ {
			if n := cycle(t, w); n != 1 {
				t.Fatalf("cycle %d claimed %d messages", i, n)
			}
			clock.Advance(time.Hour)
		}
		enqueue(t, db, q, fresh)
		dead, err := deadLetters(t, db, q, MaxBatchSize)
		if err != nil || len(dead) != 1 || dead[0].ID != doomed.ID || dead[0].Attempts != 3 ||
			dead[0].ParkedFrom != Dispatching || dead[0].FailureCode != "attempts-exhausted" {
			t.Fatalf("the dead letters are %+v %v", dead, err)
		}
		removed, err := w.Purge(ctx, MaxBatchSize, time.Minute)
		if err != nil || removed != 1 {
			t.Fatalf("purge removed %d: %v", removed, err)
		}
		if count(t, db, "SELECT count(*) FROM stego_outbox.messages WHERE id = $1", fresh.ID) != 1 ||
			count(t, db, "SELECT count(*) FROM stego_outbox.messages") != 1 {
			t.Fatal("purge did not remove only the parked message")
		}
		if dead, err := deadLetters(t, db, q, MaxBatchSize); err != nil || len(dead) != 0 {
			t.Fatalf("dead letters stay after the purge: %v %v", dead, err)
		}
		for _, limit := range []int{0, MaxBatchSize + 1} {
			if _, err := w.Purge(ctx, limit, time.Minute); err == nil {
				t.Fatalf("the purge limit %d was accepted", limit)
			}
			if _, err := deadLetters(t, db, q, limit); err == nil {
				t.Fatalf("the dead letter limit %d was accepted", limit)
			}
		}
	})

	t.Run("ST-SUP-RP01-LC16-T02 Purge keeps a parked message whose outcome is unknown", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		ctx := context.Background()
		known, unknown := message("billing", "known"), message("billing", "unknown")
		enqueue(t, db, q, known, unknown)
		w := newWorker(t, q, func(c *WorkerConfig) { c.MaxAttempts = 1 })
		register(t, w, Destination{Name: "billing", Handler: func(_ context.Context, _ *sql.Tx, d Delivery) error {
			if d.ID == known.ID {
				return NotSent(errors.New("not sent"))
			}
			return errors.New("the connection broke")
		}})
		for i := 0; i < 4; i++ {
			cycle(t, w)
		}
		if a, b := readRow(t, db, known.ID), readRow(t, db, unknown.ID); a.State != Parked || b.State != Parked ||
			a.ParkedFrom.String != "dispatching" || b.ParkedFrom.String != "outcome_unknown" {
			t.Fatalf("the messages are %+v and %+v", a, b)
		}
		clock.Advance(time.Hour)
		removed, err := w.Purge(ctx, MaxBatchSize, time.Minute)
		if err != nil || removed != 1 {
			t.Fatalf("purge removed %d: %v", removed, err)
		}
		if count(t, db, "SELECT count(*) FROM stego_outbox.messages WHERE id = $1", known.ID) != 0 ||
			readRow(t, db, unknown.ID).State != Parked {
			t.Fatal("purge did not keep only the message whose outcome is unknown")
		}
	})

	t.Run("ST-SUP-RP01-LC16-T03 Purge keeps a parked message of a destination that must land", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		ctx := context.Background()
		m := message("authz", "document:1")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, Handler: func(context.Context, *sql.Tx, Delivery) error {
			return PermanentRefusal(errors.New("bad tuple"))
		}})
		cycle(t, w)
		clock.Advance(time.Hour)
		if removed, err := w.Purge(ctx, MaxBatchSize, time.Minute); err != nil || removed != 0 {
			t.Fatalf("purge removed %d: %v", removed, err)
		}
		dead, err := deadLetters(t, db, q, MaxBatchSize)
		if err != nil || len(dead) != 1 || dead[0].ID != m.ID {
			t.Fatalf("the must-land message is not listed as parked: %+v %v", dead, err)
		}
	})

	t.Run("ST-SUP-RP01-LC16-T04 Purge keeps a discarded message", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		ctx := context.Background()
		m := message("authz", "document:1")
		m.MustLand, m.Idempotent = true, true
		enqueue(t, db, q, m)
		decider := &fakeDecider{allow: true}
		w := newWorker(t, q, func(c *WorkerConfig) { c.Decider = decider; c.Auditor = tableAuditor{} })
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, Handler: func(context.Context, *sql.Tx, Delivery) error {
			return PermanentRefusal(errors.New("bad tuple"))
		}})
		cycle(t, w)
		resolve(t, db, w, m.ID, Discard)
		clock.Advance(time.Hour)
		if removed, err := w.Purge(ctx, MaxBatchSize, time.Minute); err != nil || removed != 0 {
			t.Fatalf("purge removed %d: %v", removed, err)
		}
		if row := readRow(t, db, m.ID); row.State != Discarded {
			t.Fatalf("the discarded message is %+v", row)
		}
	})
}

func TestQueueNotice(t *testing.T) {
	t.Run("ST-SUP-RP01-LC01-T03 No notice is sent when no live event source is declared", func(t *testing.T) {
		db, dsn := testDatabase(t)
		q := newQueue(t, db, nil)
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "LISTEN "+NoticeChannel); err != nil {
			t.Fatal(err)
		}
		m := message("audit", "one")
		enqueue(t, db, q, m)
		if readRow(t, db, m.ID).State != Pending {
			t.Fatal("the message was not written")
		}
		wait, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		if n, err := conn.WaitForNotification(wait); err == nil {
			t.Fatalf("a notice came with no live event source: %+v", n)
		}
	})
}

func TestQueueTenancy(t *testing.T) {
	t.Run("ST-SUP-RP01-LC19-T09 The runtime role cannot update a queue row", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		ctx := context.Background()
		m := message("billing", "charge:9")
		enqueueAs(t, db, q, "t1", m)
		w := resolvingWorker(t, q, &fakeDecider{allow: true})
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error {
			return PermanentRefusal(errors.New("400"))
		}})
		cycle(t, w)
		if readRow(t, db, m.ID).State != Parked {
			t.Fatal("the fixture message did not park")
		}
		request := requestPool(t, db)
		t1 := testTenant(t, "t1")
		tx, err := tenant.Begin(ctx, request, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE stego_outbox.messages SET state = 'done' WHERE id = $1`, m.ID)
		_ = tx.Rollback()
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the runtime role's UPDATE gave %v, want a permission error", err)
		}
		tx, err = tenant.Begin(ctx, request, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		resolved, err := w.Resolve(ctx, tx, ResolveRequest{ID: m.ID, Destination: m.Destination, Principal: person("user:ops-1"),
			Resolution: NotActed})
		if err != nil {
			t.Fatalf("Resolve as the runtime role: %v", err)
		}
		if err := tenant.Commit(ctx, tx, t1); err != nil {
			t.Fatal(err)
		}
		resolved.Committed(ctx)
		if got := readRow(t, db, m.ID); got.State != Pending || got.Resolution.String != "not_acted" {
			t.Fatalf("the resolved message is %+v, want pending through the relay function", got)
		}
		// Nor can the runtime role insert a row in another state than pending.
		itx, err := tenant.Begin(ctx, request, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = itx.ExecContext(ctx, `INSERT INTO stego_outbox.messages (id, tenant_id, destination, resource_key, kind, payload,
			outgoing_key, state, created_at, available_at) VALUES ($1, 't1', 'billing', 'x', 'k', '{}', 'k', 'done', now(), now())`, uuid.New())
		_ = itx.Rollback()
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the runtime role's INSERT of a done row gave %v, want a permission error", err)
		}
		// The worker role cannot update a row either.
		wtx, err := tenant.Begin(ctx, db, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = wtx.ExecContext(ctx, `UPDATE stego_outbox.messages SET state = 'done' WHERE id = $1`, m.ID)
		_ = wtx.Rollback()
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the worker role's UPDATE gave %v, want a permission error", err)
		}
		// Calls of the relay function resolve that the rules of ST-SUP-RP01-LC13 refuse, as the runtime role: a NULL
		// resolution, acted on a message recorded as idempotent, and discarded on one that need not land.
		idem := message("authz", "document:9")
		idem.MustLand, idem.Idempotent = true, true
		enqueueAs(t, db, q, "t1", idem)
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, Handler: func(context.Context, *sql.Tx, Delivery) error {
			return PermanentRefusal(errors.New("400"))
		}})
		cycle(t, w)
		cycle(t, w)
		if readRow(t, db, idem.ID).State != Parked || readRow(t, db, m.ID).State != Parked {
			t.Fatal("the fixture messages did not park")
		}
		call := func(id uuid.UUID, to, resolution any) error {
			tx, err := tenant.Begin(ctx, request, t1, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			var at time.Time
			var age float64
			return tx.QueryRowContext(ctx, `SELECT o_resolved_at, o_age_seconds FROM stego_outbox.resolve(NULL, $1, $2, $3, 'user:ops-1')`,
				id, to, resolution).Scan(&at, &age)
		}
		for name, c := range map[string]struct {
			id             uuid.UUID
			to, resolution any
		}{
			"a NULL resolution":                     {idem.ID, "done", nil},
			"acted on an idempotent row":            {idem.ID, "done", "acted"},
			"discarded on a row that need not land": {m.ID, "discarded", "discarded"},
		} {
			if err := call(c.id, c.to, c.resolution); err == nil {
				t.Errorf("%s was resolved", name)
			}
		}
		if readRow(t, db, idem.ID).State != Parked || readRow(t, db, m.ID).State != Parked {
			t.Fatal("a refused call changed a state")
		}
	})

	t.Run("ST-SUP-RP01-LC19-T01 Two tenants cannot see, write or resolve each other's messages", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		ctx := context.Background()
		one, two := message("billing", "charge:1"), message("billing", "charge:2")
		enqueueAs(t, db, q, "t1", one)
		enqueueAs(t, db, q, "t2", two)
		w := resolvingWorker(t, q, &fakeDecider{allow: true})
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error {
			return PermanentRefusal(errors.New("400"))
		}})
		cycle(t, w)
		cycle(t, w)
		before := readRow(t, db, one.ID)
		if before.State != Parked || readRow(t, db, two.ID).State != Parked {
			t.Fatal("the fixture messages did not park")
		}
		t2 := testTenant(t, "t2")
		tx, err := tenant.Begin(ctx, db, t2, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		dead, err := q.DeadLetters(ctx, tx, MaxBatchSize)
		if err != nil || len(dead) != 1 || dead[0].ID != two.ID {
			t.Fatalf("t2 lists the dead letters %+v %v", dead, err)
		}
		var ids []uuid.UUID
		rows, err := tx.QueryContext(ctx, `SELECT id FROM stego_outbox.messages`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) != 1 || ids[0] != two.ID {
			t.Fatalf("t2 reads the queue table rows %v", ids)
		}
		if _, err := w.Resolve(ctx, tx, ResolveRequest{ID: one.ID, Destination: one.Destination, Principal: person("user:ops-1"),
			Resolution: Acted}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("t2 resolving t1's message gave %v", err)
		}
		if _, err := tx.ExecContext(ctx, `SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO stego_outbox.messages (id, tenant_id, destination, resource_key,
			kind, payload, outgoing_key, created_at, available_at) VALUES ($1, 't1', 'billing', 'x', 'k', '{}', 'k', now(), now())`,
			uuid.New()); err == nil || !strings.Contains(err.Error(), "row-level security") {
			t.Fatalf("an insert of a t1 row in a t2 transaction gave %v", err)
		}
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
		if err := tenant.Commit(ctx, tx, t2); err != nil {
			t.Fatal(err)
		}
		if after := readRow(t, db, one.ID); after != before || auditRows(t, db, one.ID) != 0 {
			t.Fatalf("t1's message changed from %+v to %+v", before, after)
		}
	})

	t.Run("ST-SUP-RP01-LC19-T02 Only the relay functions span tenants, and they cannot be hijacked", func(t *testing.T) {
		db, dsn := testDatabase(t)
		a := admin(t, db)
		role := runtimeUser(t, dsn)
		rows, err := a.Query(`SELECT p.proname, p.prosecdef, pg_get_userbyid(p.proowner), coalesce(array_to_string(p.proconfig, ','), ''),
			has_function_privilege($1, p.oid, 'EXECUTE'), has_function_privilege('public', p.oid, 'EXECUTE')
			FROM pg_proc AS p JOIN pg_namespace AS n ON n.oid = p.pronamespace WHERE n.nspname = 'stego_outbox' ORDER BY 1`, role)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for rows.Next() {
			var name, owner, config string
			var definer, runtime, public bool
			if err := rows.Scan(&name, &definer, &owner, &config, &runtime, &public); err != nil {
				t.Fatal(err)
			}
			if !definer || owner != relayRole(t, db) || config != "search_path=pg_catalog, pg_temp" || !runtime || public {
				t.Fatalf("the function %s is definer %v, owner %s, config %q, runtime %v, public %v", name, definer, owner, config, runtime, public)
			}
			names = append(names, name)
		}
		rows.Close()
		if strings.Join(names, ",") != "blocked,claim,move,oldest,purge_done,purge_parked,release,resolve" {
			t.Fatalf("the relay functions are %v", names)
		}
		var relayCommands []string
		rows, err = a.Query(`SELECT cmd FROM pg_policies WHERE schemaname = 'stego_outbox' AND $1 = ANY(roles) ORDER BY cmd`, relayRole(t, db))
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var cmd string
			if err := rows.Scan(&cmd); err != nil {
				t.Fatal(err)
			}
			relayCommands = append(relayCommands, cmd)
		}
		rows.Close()
		if strings.Join(relayCommands, ",") != "DELETE,SELECT,UPDATE" {
			t.Fatalf("the relay policies are for %v", relayCommands)
		}
		var forced, deletes bool
		if err := a.QueryRow(`SELECT relforcerowsecurity, has_table_privilege($1, 'stego_outbox.messages', 'DELETE')
			FROM pg_class WHERE oid = 'stego_outbox.messages'::regclass`, role).Scan(&forced, &deletes); err != nil {
			t.Fatal(err)
		}
		if !forced || deletes {
			t.Fatalf("row-level security forced %v, runtime DELETE %v", forced, deletes)
		}
		// The runtime role puts decoys first on its search_path and in pg_temp, then claims.
		q := newQueue(t, db, nil)
		m := message("billing", "charge:1")
		enqueueAs(t, db, q, "t1", m)
		if _, err := a.Exec(`CREATE SCHEMA decoy AUTHORIZATION ` + role); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		for _, statement := range []string{
			`CREATE TABLE decoy.messages (id uuid)`,
			`CREATE FUNCTION decoy.clock_timestamp() RETURNS timestamptz LANGUAGE sql AS $$ SELECT timestamptz '2000-01-01' $$`,
			`CREATE FUNCTION decoy.jsonb_array_elements_text(jsonb) RETURNS SETOF text LANGUAGE sql AS $$ SELECT 'billing' $$`,
			`CREATE TEMP TABLE messages (id uuid)`,
			`SET search_path = decoy, pg_temp, public`,
		} {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		rows, err = conn.QueryContext(ctx, `SELECT r_tenant, r_id FROM stego_outbox.claim(NULL, 32, $1, 60000)`, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		var claimed []string
		for rows.Next() {
			var tid string
			var id uuid.UUID
			if err := rows.Scan(&tid, &id); err != nil {
				t.Fatal(err)
			}
			claimed = append(claimed, tid+"/"+id.String())
		}
		rows.Close()
		if len(claimed) != 1 || claimed[0] != "t1/"+m.ID.String() {
			t.Fatalf("the claim under decoys gave %v", claimed)
		}
	})
}

func TestQueueRetention(t *testing.T) {
	t.Run("ST-SUP-RP01-LC24-T01 Done messages older than the retention are purged in batches", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		var old, recent []Message
		for i := 0; i < 70; i++ {
			old = append(old, message("audit", fmt.Sprintf("old-%d", i)))
		}
		for i := 0; i < 5; i++ {
			recent = append(recent, message("audit", fmt.Sprintf("recent-%d", i)))
		}
		parked := message("audit", "parked")
		for i := 0; i < len(old); i += MaxBatchSize {
			enqueue(t, db, q, old[i:min(i+MaxBatchSize, len(old))]...)
		}
		enqueue(t, db, q, recent...)
		enqueue(t, db, q, parked)
		now := clock.Now()
		if _, err := admin(t, db).Exec(`UPDATE stego_outbox.messages SET state = 'done', done_at = $1
			WHERE resource_key LIKE 'old-%'`, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := admin(t, db).Exec(`UPDATE stego_outbox.messages SET state = 'done', done_at = $1
			WHERE resource_key LIKE 'recent-%'`, now.Add(-10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := admin(t, db).Exec(`UPDATE stego_outbox.messages SET state = 'parked', parked_from = 'dispatching',
			parked_at = $1 WHERE resource_key = 'parked'`, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		// The purge timer of the worker runs every second.
		w := newWorker(t, q, func(c *WorkerConfig) { c.Retention = time.Hour; c.PurgeEvery = time.Second })
		// Run starts the timer; the worker has no destination, and no message is due.
		done := make(chan error, 1)
		go func() { done <- w.Run(context.Background()) }()
		deadline := time.Now().Add(10 * time.Second)
		for w.Stats().PurgeBatches < 3 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		w.StopClaiming()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if s := w.Stats(); s.DonePurged != 70 || s.PurgeBatches != 3 {
			t.Fatalf("the purge timer removed %d in %d batches", s.DonePurged, s.PurgeBatches)
		}
		if n := count(t, db, "SELECT count(*) FROM stego_outbox.messages"); n != 6 {
			t.Fatalf("%d messages stay, want the 5 recent and the parked one", n)
		}
		if readRow(t, db, parked.ID).State != Parked {
			t.Fatal("the purge removed the parked message")
		}
		config := testConfig()
		config.Retention = 59 * time.Minute
		if _, err := NewWorker(q, config); err == nil {
			t.Fatal("a retention of 59 minutes was accepted")
		}
		if DefaultWorkerConfig().Retention != 7*24*time.Hour {
			t.Fatal("the default retention is not 7 days")
		}
	})
}

// relayCall runs one statement as the runtime role in a transaction of tenant t1 and returns its error.
func relayCall(t *testing.T, db *sql.DB, query string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	tx, err := tenant.Begin(ctx, db, testTenant(t, "t1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	rows.Close()
	return rows.Err()
}

func TestQueueRelayArguments(t *testing.T) {
	t.Run("ST-SUP-RP01-LC19-T05 Each relay function refuses arguments outside its bounds", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		mine, theirs := message("billing", "charge:1"), message("billing", "charge:2")
		enqueueAs(t, db, q, "t1", mine)
		enqueueAs(t, db, q, "t2", theirs)
		token := uuid.New()
		for _, c := range []struct {
			name  string
			query string
			args  []any
		}{
			{"a claim of 33", `SELECT * FROM stego_outbox.claim(NULL, 33, $1, 60000)`, []any{token}},
			{"a claim of 0", `SELECT * FROM stego_outbox.claim(NULL, 0, $1, 60000)`, []any{token}},
			{"a lease of 9e12 ms", `SELECT * FROM stego_outbox.claim(NULL, 32, $1, 9000000000000)`, []any{token}},
			{"a lease of 999 ms", `SELECT * FROM stego_outbox.claim(NULL, 32, $1, 999)`, []any{token}},
			{"a lease of 5 min 1 s", `SELECT * FROM stego_outbox.claim(NULL, 32, $1, 301000)`, []any{token}},
			{"a move of 2 attempts", `SELECT * FROM stego_outbox.move(NULL, $1, $2, 'dispatching', 'pending', 0, NULL, 2, false, false, '', 0)`, []any{mine.ID, token}},
			{"a claim with no token", `SELECT * FROM stego_outbox.claim(NULL, 32, NULL, 60000)`, nil},
			{"a move of 5 attempts", `SELECT * FROM stego_outbox.move(NULL, $1, $2, 'dispatching', 'pending', 0, NULL, 5, false, false, '', 0)`, []any{mine.ID, token}},
			{"a move of -2 attempts", `SELECT * FROM stego_outbox.move(NULL, $1, $2, 'dispatching', 'pending', 0, NULL, -2, false, false, '', 0)`, []any{mine.ID, token}},
			{"a delay of two days", `SELECT * FROM stego_outbox.move(NULL, $1, $2, 'dispatching', 'pending', 172800000, NULL, 0, false, false, '', 0)`, []any{mine.ID, token}},
			{"a negative delay", `SELECT * FROM stego_outbox.move(NULL, $1, $2, 'dispatching', 'pending', -1, NULL, 0, false, false, '', 0)`, []any{mine.ID, token}},
			{"a due time in 2312", `SELECT * FROM stego_outbox.move(NULL, $1, $2, 'outcome_unknown', 'reconciling', 0, '2312-01-01', 0, false, false, '', 0)`, []any{mine.ID, token}},
			{"a resend lease of 9e12 ms", `SELECT * FROM stego_outbox.move(NULL, $1, $2, 'outcome_unknown', 'dispatching', 0, NULL, 1, false, false, '', 9000000000000)`, []any{mine.ID, token}},
			{"a failure code with text", `SELECT * FROM stego_outbox.move(NULL, $1, $2, 'dispatching', 'pending', 0, NULL, 0, false, false, 'password=hunter2 x', 0)`, []any{mine.ID, token}},
			{"a release of 2 days", `SELECT stego_outbox.release(NULL, $1, $2, 172800000, '')`, []any{mine.ID, token}},
			{"a purge of a negative age", `SELECT stego_outbox.purge_parked(NULL, 32, -1, '["billing"]')`, nil},
			{"a purge of 33", `SELECT stego_outbox.purge_parked(NULL, 33, 0, '["billing"]')`, nil},
			{"a purge with no list", `SELECT stego_outbox.purge_parked(NULL, 32, 0, NULL)`, nil},
			{"a done purge of a minute", `SELECT stego_outbox.purge_done(NULL, 32, 60000)`, nil},
		} {
			if err := relayCall(t, db, c.query, c.args...); err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
		}
		// A claim of each tenant, then the probes with the claim's token and with others.
		claimed, err := q.Claim(context.Background(), 32, time.Minute)
		if err != nil || len(claimed) != 2 {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		var mineD, theirsD Delivery
		for _, d := range claimed {
			if d.ID == mine.ID {
				mineD = d
			} else {
				theirsD = d
			}
		}
		count := func(query string, args ...any) int {
			t.Helper()
			ctx := context.Background()
			tx, err := tenant.Begin(ctx, db, testTenant(t, "t1"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				n++
			}
			return n
		}
		// A token that is not the message's last claim learns nothing.
		if n := count(`SELECT * FROM stego_outbox.move(NULL, $1, $2, 'dispatching', 'pending', 0, NULL, 0, false, false, '', 0)`, mine.ID, uuid.New()); n != 0 {
			t.Fatalf("a wrong token got %d rows", n)
		}
		// In a transaction of t1, the move of t2's message with t2's own token changes nothing and says nothing.
		if n := count(`SELECT * FROM stego_outbox.move(NULL, $1, $2, 'dispatching', 'pending', 0, NULL, 0, false, false, '', 0)`, theirs.ID, theirsD.Receipt.Token); n != 0 {
			t.Fatalf("t1 moved or read t2's message: %d rows", n)
		}
		if readRow(t, db, theirs.ID).State != Dispatching {
			t.Fatal("t1 moved t2's message")
		}
		// A release of a dispatching message is refused: its next step is a send, not a wait. A release with a token
		// that does not match changes nothing.
		before := readRow(t, db, mine.ID)
		var released int64
		if err := db.QueryRow(`SELECT stego_outbox.release(NULL, $1, $2, 0, '')`, mine.ID, mineD.Receipt.Token).Scan(&released); err != nil || released != 0 {
			t.Fatalf("the release of a dispatching message gave %d, %v", released, err)
		}
		if err := db.QueryRow(`SELECT stego_outbox.release(NULL, $1, $2, 0, '')`, mine.ID, uuid.New()).Scan(&released); err != nil || released != 0 {
			t.Fatalf("the release with a wrong token gave %d, %v", released, err)
		}
		if readRow(t, db, mine.ID) != before {
			t.Fatal("a refused call changed the row")
		}
		// A release of a pending message is refused.
		pending := message("billing", "charge:9")
		enqueueAs(t, db, q, "t1", pending)
		if _, err := admin(t, db).Exec(`UPDATE stego_outbox.messages SET lease_token = $2, lease_until = now() + interval '1 minute'
			WHERE id = $1`, pending.ID, token); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT stego_outbox.release(NULL, $1, $2, 0, '')`, pending.ID, token).Scan(&released); err != nil || released != 0 {
			t.Fatalf("the release of a pending message gave %d, %v", released, err)
		}
		// A parked message enqueued as must land is kept by the purge, whatever list it is given.
		land := message("billing", "charge:3")
		land.MustLand = true
		enqueueAs(t, db, q, "t1", land)
		if _, err := admin(t, db).Exec(`UPDATE stego_outbox.messages SET state = 'parked', parked_from = 'dispatching',
			parked_at = now() - interval '1 day' WHERE id = $1`, land.ID); err != nil {
			t.Fatal(err)
		}
		var purged int64
		if err := db.QueryRow(`SELECT stego_outbox.purge_parked(NULL, 32, 0, '["billing"]')`).Scan(&purged); err != nil || purged != 0 {
			t.Fatalf("the purge removed %d must-land messages: %v", purged, err)
		}
	})

	t.Run("ST-SUP-RP01-LC08-T01 The transition table of the relay function move is the state machine's", func(t *testing.T) {
		db, _ := testDatabase(t)
		for _, from := range States() {
			for _, to := range States() {
				err := relayCall(t, db, `SELECT * FROM stego_outbox.move(NULL, $1, $2, $3, $4, 0, NULL, 0, false, false, '', 60000)`,
					uuid.New(), uuid.New(), string(from), string(to))
				// The person's moves from parked go through Resolve, not through the relay function.
				want := Transition(from, to) == nil && from != Parked
				if (err == nil) != want {
					t.Fatalf("move from %s to %s gave %v, want allowed %v", from, to, err, want)
				}
			}
		}
	})
}

func TestQueueRelayRoles(t *testing.T) {
	t.Run("ST-SUP-RP01-LC19-T04 Only the worker role can run the relay functions", func(t *testing.T) {
		db, dsn := testDatabase(t)
		request := requestPool(t, db)
		token := uuid.New()
		calls := []string{
			`SELECT * FROM stego_outbox.claim(NULL, 1, '` + token.String() + `', 60000)`,
			`SELECT * FROM stego_outbox.move(NULL, '` + uuid.NewString() + `', '` + token.String() + `', 'dispatching', 'pending', 0, NULL, 0, false, false, '', 0)`,
			`SELECT stego_outbox.release(NULL, '` + uuid.NewString() + `', '` + token.String() + `', 0, '')`,
			`SELECT stego_outbox.purge_parked(NULL, 1, 0, '[]')`,
			`SELECT stego_outbox.purge_done(NULL, 1, 3600000)`,
			`SELECT * FROM stego_outbox.blocked(NULL, 1)`,
			`SELECT * FROM stego_outbox.oldest(NULL)`,
		}
		for _, call := range calls {
			if err := relayCall(t, request, call); err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Fatalf("the runtime role ran %s: %v", call, err)
			}
			if err := relayCall(t, db, call); err != nil {
				t.Fatalf("the worker role could not run %s: %v", call, err)
			}
		}
		var request0 string
		if err := request.QueryRow(`SELECT current_user`).Scan(&request0); err != nil {
			t.Fatal(err)
		}
		rows, err := admin(t, db).Query(`SELECT p.proname, array_to_string(ARRAY(SELECT a.grantee::regrole::text FROM aclexplode(p.proacl) AS a
			WHERE a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner ORDER BY 1), ',')
			FROM pg_proc AS p JOIN pg_namespace AS n ON n.oid = p.pronamespace WHERE n.nspname = 'stego_outbox'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name, grantees string
			if err := rows.Scan(&name, &grantees); err != nil {
				t.Fatal(err)
			}
			want := runtimeUser(t, dsn)
			if name == "resolve" {
				// Resolve runs on the request path too, in a transaction of the message's tenant.
				want = strings.Join(sortedRoles(request0, want), ",")
			}
			if grantees != want {
				t.Fatalf("EXECUTE on %s goes to %q, want %q", name, grantees, want)
			}
		}
		rows.Close()
		var del, upd bool
		if err := admin(t, db).QueryRow(`SELECT has_table_privilege($1, 'stego_outbox.messages', 'DELETE'),
			has_table_privilege($1, 'stego_outbox.messages', 'TRUNCATE')`, request0).Scan(&del, &upd); err != nil || del || upd {
			t.Fatalf("the runtime role holds DELETE %v or TRUNCATE %v on the queue table: %v", del, upd, err)
		}
	})

	t.Run("ST-SUP-RP01-LC19-T06 The purge of parked messages refuses a negative age and keeps must-land messages", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		land, other := message("billing", "charge:1"), message("billing", "charge:2")
		land.MustLand = true
		enqueueAs(t, db, q, "t1", land, other)
		if _, err := admin(t, db).Exec(`UPDATE stego_outbox.messages SET state = 'parked', parked_from = 'dispatching',
			parked_at = now() - interval '400 days'`); err != nil {
			t.Fatal(err)
		}
		if err := relayCall(t, db, `SELECT stego_outbox.purge_parked(NULL, 32, -1000, '["billing"]')`); err == nil {
			t.Fatal("a purge of the age -1 s was accepted")
		}
		var purged int64
		if err := db.QueryRow(`SELECT stego_outbox.purge_parked(NULL, 32, 0, '["billing"]')`).Scan(&purged); err != nil || purged != 1 {
			t.Fatalf("the purge removed %d: %v", purged, err)
		}
		var mustLand bool
		if err := admin(t, db).QueryRow(`SELECT must_land FROM stego_outbox.messages WHERE id = $1`, land.ID).Scan(&mustLand); err != nil || !mustLand {
			t.Fatalf("the must-land message is gone or not marked: %v %v", mustLand, err)
		}
		if count(t, db, `SELECT count(*) FROM stego_outbox.messages WHERE id = $1`, other.ID) != 0 {
			t.Fatal("the other message was kept")
		}
	})

	t.Run("ST-SUP-RP01-LC19-T07 A move checks the tenant setting when it is set", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		ctx := context.Background()
		m := message("billing", "charge:1")
		enqueueAs(t, db, q, "t1", m)
		claimed, err := q.Claim(ctx, 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		r := claimed[0].Receipt
		move := func(tenantID string, to State) (bool, error) {
			t.Helper()
			var tx *sql.Tx
			if tenantID == "" {
				tx, err = db.BeginTx(ctx, nil)
			} else {
				tx, err = tenant.Begin(ctx, db, testTenant(t, tenantID), nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			receipt := r
			ok, _, err := q.move(ctx, tx, &receipt, to, change{failureCode: "x"})
			if err == nil && ok {
				err = tx.Commit()
			}
			return ok, err
		}
		before := readRow(t, db, m.ID)
		if ok, err := move("t2", Pending); ok || err != nil {
			t.Fatalf("the move in a t2 transaction gave %v, %v", ok, err)
		}
		if readRow(t, db, m.ID) != before {
			t.Fatal("the move in a t2 transaction changed the row")
		}
		if ok, err := move("t1", OutcomeUnknown); !ok || err != nil {
			t.Fatalf("the move in a t1 transaction gave %v, %v", ok, err)
		}
		// The worker's next claim takes the message in outcome unknown under a new lease; a move with no tenant set
		// is allowed.
		claimed, err = q.Claim(ctx, 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		r = claimed[0].Receipt
		if ok, err := move("", Parked); !ok || err != nil {
			t.Fatalf("the move with no tenant set gave %v, %v", ok, err)
		}
	})
}

func TestQueueMigration(t *testing.T) {
	t.Run("ST-SUP-RP01-LC19-T08 The migration leaves the relay role no CREATE on the schema", func(t *testing.T) {
		// The relay role is a lower-case name of the form <service role prefix>_outbox_relay, apart from the other
		// two roles.
		for _, roles := range [][3]string{{"stego_relay", "rt", "wk"}, {"outbox_relay", "rt", "wk"}, {"Shop_outbox_relay", "rt", "wk"},
			{"shop_outbox_relay'", "rt", "wk"}, {"x_outbox_relay", "x_outbox_relay", "wk"}, {"x_outbox_relay", "rt", "x_outbox_relay"}, {"x_outbox_relay", "rt", "rt"}} {
			if _, err := Migration(roles[0], roles[1], roles[2]); err == nil {
				t.Errorf("the roles %v are accepted", roles)
			}
		}
		f := newMigrationFixture(t)
		member, stranger := f.owner("outbox_owner", "CREATEROLE", true), f.owner("outbox_stranger", "CREATEROLE", false)
		if err := f.run(member); err != nil {
			t.Fatalf("the migration as a member of the relay role: %v", err)
		}
		err := f.run(stranger)
		if err == nil || !strings.Contains(err.Error(), "needs membership in "+f.relay+" WITH SET TRUE") {
			t.Fatalf("the migration as a role with no membership gave %v", err)
		}
	})
	t.Run("ST-SUP-RP01-LC19-T10 A migration owner without CREATEROLE runs the migration again", func(t *testing.T) {
		f := newMigrationFixture(t)
		owner := f.owner("outbox_plain", "NOCREATEROLE", true)
		var createRole bool
		if err := f.root.QueryRowContext(context.Background(),
			`SELECT rolcreaterole FROM pg_catalog.pg_roles WHERE rolname = $1`, owner).Scan(&createRole); err != nil {
			t.Fatal(err)
		}
		if createRole {
			t.Fatal("the owner has CREATEROLE")
		}
		if err := f.run(owner); err != nil {
			t.Fatalf("the migration as an owner without CREATEROLE: %v", err)
		}
		// A migration login that holds only SET membership in a NOLOGIN owner takes the owner with SET LOCAL ROLE,
		// as the migration job does; the text returns to the owner, not to the login, so it passes twice.
		ctx := context.Background()
		nologin := f.owner("outbox_group", "NOCREATEROLE", true)
		migrator := f.owner("outbox_login", "NOCREATEROLE", false)
		if _, err := f.root.ExecContext(ctx, "ALTER ROLE "+nologin+" NOLOGIN"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.root.ExecContext(ctx, "GRANT "+nologin+" TO "+migrator+" WITH SET TRUE, INHERIT FALSE"); err != nil {
			t.Fatal(err)
		}
		dsn := pgtest.Database(t, "outbox_login")
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.root.ExecContext(ctx, "ALTER DATABASE "+strings.TrimPrefix(u.Path, "/")+" OWNER TO "+nologin); err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(migrator, f.password)
		login, err := sql.Open("pgx", u.String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = login.Close() }()
		for run := 1; run <= 2; run++ {
			tx, err := login.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+nologin); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, f.ddl); err != nil {
				_ = tx.Rollback()
				t.Fatalf("run %d of the migration by a login with SET membership only in the owner: %v", run, err)
			}
			var role string
			if err := tx.QueryRowContext(ctx, "SELECT current_user").Scan(&role); err != nil || role != nologin {
				t.Fatalf("the text left the role %s, want the owner %s: %v", role, nologin, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		var create bool
		if err := login.QueryRowContext(ctx, "SELECT has_schema_privilege($1, 'stego_outbox', 'CREATE')", f.relay).Scan(&create); err != nil || create {
			t.Fatalf("the relay role keeps CREATE on stego_outbox: %v, %v", create, err)
		}
	})
}

func TestSharedRelayRole(t *testing.T) {
	t.Run("ST-SUP-RP01-LC19-T11 A queue whose relay functions belong to the shared role stego_outbox_relay is refused with the remedy, and passes after it", func(t *testing.T) {
		f := newMigrationFixture(t)
		ctx := context.Background()
		db, err := sql.Open("pgx", pgtest.Database(t, "outbox_shared"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		// The queue of a runtime before v0.31.0: a relay function of the shared role. The role is of the server,
		// so it is made only when it is missing, and dropped at the end.
		if _, err := f.root.ExecContext(ctx, `DO $$ BEGIN CREATE ROLE stego_outbox_relay NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $$`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = f.root.ExecContext(context.Background(), "DROP ROLE IF EXISTS stego_outbox_relay") })
		for _, statement := range []string{
			"CREATE SCHEMA stego_outbox",
			"CREATE FUNCTION stego_outbox.claim(timestamptz, integer, uuid, bigint) RETURNS integer LANGUAGE sql AS 'SELECT 1'",
			"ALTER FUNCTION stego_outbox.claim(timestamptz, integer, uuid, bigint) OWNER TO stego_outbox_relay",
		} {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		_, err = db.ExecContext(ctx, f.ddl)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55000" || !strings.Contains(pgErr.Message, "stego_outbox_relay") ||
			!strings.Contains(pgErr.Message, "DROP OWNED BY stego_outbox_relay") || !strings.Contains(pgErr.Message, "run the migration job again") {
			t.Fatalf("the migration on a queue of the shared relay role gave %v, want a refusal (55000) that names stego_outbox_relay and the remedy", err)
		}
		var made bool
		if err := db.QueryRowContext(ctx, "SELECT to_regclass('stego_outbox.messages') IS NOT NULL").Scan(&made); err != nil || made {
			t.Fatalf("the refused migration made the queue table (%v): %v", made, err)
		}
		// The remedy that the refusal names, and the migration passes.
		for _, statement := range []string{"DROP OWNED BY stego_outbox_relay", "DROP ROLE stego_outbox_relay"} {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		if _, err := db.ExecContext(ctx, f.ddl); err != nil {
			t.Fatalf("the migration after the remedy: %v", err)
		}
	})
}

// migrationFixture holds a server with the relay role of the test, a runtime role and a worker role, and the
// migration text for them.
type migrationFixture struct {
	t        *testing.T
	root     *sql.DB
	password string
	relay    string
	ddl      string
}

func newMigrationFixture(t *testing.T) *migrationFixture {
	t.Helper()
	root, err := sql.Open("pgx", pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	suffix := randomSuffix(t)
	runtime, worker, relay := "outbox_rt_"+suffix, "outbox_wk_"+suffix, "outbox_"+suffix+RelaySuffix
	ctx := context.Background()
	for _, r := range []string{runtime, worker} {
		if _, err := root.ExecContext(ctx, "CREATE ROLE "+r+" NOLOGIN"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+r) })
	}
	// The relay role of the test exists before the migration, as the tenancy role migration makes it.
	if _, err := root.ExecContext(ctx, "CREATE ROLE "+relay+" NOLOGIN NOINHERIT"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+relay) })
	ddl, err := Migration(relay, runtime, worker)
	if err != nil {
		t.Fatal(err)
	}
	return &migrationFixture{t: t, root: root, password: randomSuffix(t), relay: relay, ddl: ddl}
}

// owner makes a login role with the given attribute, not a superuser, and grants it SET membership in the relay
// role, with no INHERIT, when member is true.
func (f *migrationFixture) owner(prefix, attribute string, member bool) string {
	f.t.Helper()
	ctx := context.Background()
	r := prefix + "_" + randomSuffix(f.t)
	if _, err := f.root.ExecContext(ctx, "CREATE ROLE "+r+" LOGIN "+attribute+" NOSUPERUSER PASSWORD '"+f.password+"'"); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _, _ = f.root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+r) })
	if member {
		if _, err := f.root.ExecContext(ctx, `GRANT `+f.relay+` TO `+r+` WITH SET TRUE, INHERIT FALSE`); err != nil {
			f.t.Fatal(err)
		}
	}
	return r
}

// run gives a new database to owner, runs the migration in it as owner, checks that the relay role keeps no CREATE
// on the schema, and runs the migration a second time.
func (f *migrationFixture) run(owner string) error {
	t := f.t
	t.Helper()
	ctx := context.Background()
	dsn := pgtest.Database(t, "outbox_owner")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if _, err := f.root.ExecContext(ctx, "ALTER DATABASE "+name+" OWNER TO "+owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u.User = url.UserPassword(owner, f.password)
	ownerDB, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ownerDB.Close() }()
	if _, err := ownerDB.ExecContext(ctx, f.ddl); err != nil {
		return err
	}
	var create, public bool
	if err := ownerDB.QueryRowContext(ctx, `SELECT has_schema_privilege($1, 'stego_outbox', 'CREATE'), has_schema_privilege($1, 'public', 'CREATE')`,
		f.relay).Scan(&create, &public); err != nil {
		t.Fatal(err)
	}
	if create || public {
		t.Fatalf("the relay role keeps CREATE on the schema stego_outbox (%v) or has it on public (%v)", create, public)
	}
	// The text runs again with no error.
	_, err = ownerDB.ExecContext(ctx, f.ddl)
	return err
}

// sortedRoles gives the role names in the order of the regrole text sort of the EXECUTE grantee list.
func sortedRoles(roles ...string) []string {
	sort.Strings(roles)
	return roles
}
