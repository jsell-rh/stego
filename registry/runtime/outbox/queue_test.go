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
	_ "github.com/jackc/pgx/v5/stdlib"

	"stego.dev/runtime/internal/seal"
	"stego.dev/runtime/tenant"
)

// The integration tests of this package read STEGO_TEST_POSTGRES_DSN, the DSN of a local PostgreSQL 17 server that
// runs in an unprivileged container (ST-STD-LOCAL), and skip when it is not set. Each test makes its own database on
// that server, applies Migration and drops the database at the end. The tests reach PostgreSQL through pgx's
// database/sql driver (ST-STD-RUNTIME-R06).

// crashEnv names the mode of the helper process of the crash tests.
const crashEnv = "STEGO_OUTBOX_CRASH_MODE"

func TestMain(m *testing.M) {
	if mode := os.Getenv(crashEnv); mode != "" {
		crashHelper(mode)
		return
	}
	os.Exit(m.Run())
}

// identity is a verified identity for the tests; only a package of the runtime module can make one.
type identity struct {
	seal.IdentityMark
	tenant string
}

func (i identity) TenantClaim() (string, bool) { return i.tenant, i.tenant != "" }

func testTenant(t testing.TB, id string) tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(tenant.Claim(identity{tenant: id}))
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

func testDSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("STEGO_TEST_POSTGRES_DSN is not set; this test needs a local PostgreSQL server")
	}
	return dsn
}

// testDatabase makes a database for one test with the queue table and an application table records, and drops it
// at the end. It returns the database and its DSN.
func testDatabase(t testing.TB) (*sql.DB, string) {
	t.Helper()
	dsn := testDSN(t)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "outbox_" + hex.EncodeToString(suffix)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the test database: %v", err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("STEGO_TEST_POSTGRES_DSN must be a postgres:// URL: %v", err)
	}
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(12)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, Migration); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE records (id text PRIMARY KEY);
		CREATE TABLE audit_rows (message_id uuid NOT NULL, tenant_id text NOT NULL, destination text NOT NULL,
			subject text NOT NULL, resolution text NOT NULL, at timestamptz NOT NULL)`); err != nil {
		t.Fatal(err)
	}
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
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
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
	if err := db.QueryRow(`SELECT state, attempts, conflicts, lookups, lease_token, outgoing_key, failure_code,
		parked_from, available_at, resolution, resolved_by, resolved_at, tenant_id
		FROM stego_outbox.messages WHERE id = $1`, id).Scan(&state, &s.Attempts, &s.Conflicts, &s.Lookups, &s.Token,
		&s.OutgoingKey, &s.FailureCode, &s.ParkedFrom, &s.AvailableAt, &s.Resolution, &s.ResolvedBy, &s.ResolvedAt,
		&s.TenantID); err != nil {
		t.Fatal(err)
	}
	s.State = State(state)
	return s
}

func newQueue(t testing.TB, db *sql.DB, clock Clock) *Queue {
	t.Helper()
	q, err := New(db, clock)
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
		if ok, err := q.Acknowledge(context.Background(), first[0].Receipt); ok || err != nil {
			t.Fatalf("an expired receipt acknowledged the message: %v %v", ok, err)
		}
		if ok, err := q.Retry(context.Background(), first[0].Receipt, 0, "failure"); ok || err != nil {
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
		handler := func(_ context.Context, d Delivery) error {
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
		if ok, err := q.Retry(ctx, head.Receipt, time.Hour, "sink-unavailable"); !ok || err != nil {
			t.Fatalf("retry: %v %v", ok, err)
		}
		if ok, err := q.Acknowledge(ctx, second.Receipt); !ok || err != nil {
			t.Fatalf("the other key did not progress: %v %v", ok, err)
		}
		if next, err := q.Claim(ctx, 32, time.Minute); err != nil || len(next) != 0 {
			t.Fatalf("the retry did not hold its successor: %v %v", next, err)
		}
		clock.Advance(time.Hour)
		next, err := q.Claim(ctx, 32, time.Minute)
		if err != nil || len(next) != 1 || next[0].ID != a.ID || next[0].Attempts != 2 || string(next[0].Payload) != `{"version": 1}` {
			t.Fatalf("the retry lost its identity or attempt count: %+v %v", next, err)
		}
		if ok, err := q.Acknowledge(ctx, next[0].Receipt); !ok || err != nil {
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
	q, err := New(db, nil)
	if err != nil {
		os.Exit(2)
	}
	tn, err := tenant.New(tenant.Claim(identity{tenant: "tenant-a"}))
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
		if err := w.Register(Destination{Name: "billing", Handler: func(context.Context, Delivery) error {
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
		register(t, w, Destination{Name: "audit", Idempotent: true, Handler: func(_ context.Context, d Delivery) error {
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
		if ok, err := q.Retry(ctx, deliveries[0].Receipt, time.Minute, "delivery-failed"); !ok || err != nil {
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
		register(t, w, Destination{Name: "audit", Handler: func(context.Context, Delivery) error {
			return NotSent(errors.New("the sink is down"))
		}})
		for i := 0; i < 3; i++ {
			if n := cycle(t, w); n != 1 {
				t.Fatalf("cycle %d claimed %d messages", i, n)
			}
			clock.Advance(time.Hour)
		}
		enqueue(t, db, q, fresh)
		dead, err := q.DeadLetters(ctx, MaxBatchSize)
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
		if dead, err := q.DeadLetters(ctx, MaxBatchSize); err != nil || len(dead) != 0 {
			t.Fatalf("dead letters stay after the purge: %v %v", dead, err)
		}
		for _, limit := range []int{0, MaxBatchSize + 1} {
			if _, err := w.Purge(ctx, limit, time.Minute); err == nil {
				t.Fatalf("the purge limit %d was accepted", limit)
			}
			if _, err := q.DeadLetters(ctx, limit); err == nil {
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
		register(t, w, Destination{Name: "billing", Handler: func(_ context.Context, d Delivery) error {
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
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, Handler: func(context.Context, Delivery) error {
			return PermanentRefusal(errors.New("bad tuple"))
		}})
		cycle(t, w)
		clock.Advance(time.Hour)
		if removed, err := w.Purge(ctx, MaxBatchSize, time.Minute); err != nil || removed != 0 {
			t.Fatalf("purge removed %d: %v", removed, err)
		}
		dead, err := q.DeadLetters(ctx, MaxBatchSize)
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
		enqueue(t, db, q, m)
		decider := &fakeDecider{allow: true}
		w := newWorker(t, q, func(c *WorkerConfig) { c.Decider = decider; c.Auditor = tableAuditor{} })
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, Handler: func(context.Context, Delivery) error {
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
