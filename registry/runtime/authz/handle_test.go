package authz

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"stego.dev/runtime/internal/pgtest"
	"stego.dev/runtime/internal/seal"
	"stego.dev/runtime/outbox"
	"stego.dev/runtime/tenant"
)

// The unit tests bind a handle to a real *sql.Tx of a database/sql driver of the test. The driver answers only the
// floor read of the handle, from the floor source that its data source name names.
type floorDriver struct{}

var (
	registerFloor sync.Once
	floorSources  sync.Map // data source name -> *floorSeq
)

func (floorDriver) Open(name string) (driver.Conn, error) {
	source, ok := floorSources.Load(name)
	if !ok {
		return nil, errors.New("no floor source")
	}
	return &floorConn{floor: source.(*floorSeq)}, nil
}

type floorConn struct{ floor *floorSeq }

func (c *floorConn) Prepare(query string) (driver.Stmt, error) {
	if !strings.Contains(query, ".authz_floor") {
		return nil, errors.New("the test driver answers only the floor read")
	}
	return floorStmt{floor: c.floor}, nil
}
func (c *floorConn) Close() error              { return nil }
func (c *floorConn) Begin() (driver.Tx, error) { return nopTx{}, nil }

type floorStmt struct{ floor *floorSeq }

func (floorStmt) Close() error                               { return nil }
func (floorStmt) NumInput() int                              { return -1 }
func (floorStmt) Exec([]driver.Value) (driver.Result, error) { return nil, errors.New("no exec") }
func (s floorStmt) Query([]driver.Value) (driver.Rows, error) {
	token, _ := s.floor.Current(context.Background())
	return &floorRows{value: token.Text()}, nil
}

type floorRows struct {
	value string
	done  bool
}

func (*floorRows) Columns() []string { return []string{"token"} }
func (*floorRows) Close() error      { return nil }
func (r *floorRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

type nopTx struct{}

func (nopTx) Commit() error   { return nil }
func (nopTx) Rollback() error { return nil }

func floorDB(t *testing.T, floor *floorSeq) *sql.DB {
	t.Helper()
	registerFloor.Do(func() { sql.Register("authz-floor", floorDriver{}) })
	name := uuid.NewString()
	floorSources.Store(name, floor)
	db, err := sql.Open("authz-floor", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(); floorSources.Delete(name) })
	return db
}

// floorTx opens a transaction of the test driver whose floor reads give the tokens of floor in turn.
func floorTx(t *testing.T, floor *floorSeq) *sql.Tx {
	t.Helper()
	tx, err := floorDB(t, floor).Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// fakeDB is a database of the test driver with a zero floor, for a part that needs a database it does not use.
func fakeDB(t *testing.T) *sql.DB { return floorDB(t, &floorSeq{}) }

func mustTenant(t *testing.T, id string) tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(tenant.Requested(tenant.ProvisionAction, id, tenantDecision{DecisionMark: seal.NewDecisionMark(id, "provision")}))
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

func newUnitRuntime(t *testing.T, provider Decision) *Runtime {
	t.Helper()
	queue, err := outbox.New(fakeDB(t), outbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(Config{Provider: provider, DB: fakeDB(t), Schema: "public", Outbox: queue})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// bindUnit binds a handle of the unit runtime rt for principal to a transaction whose floor reads give floor.
func bindUnit(t *testing.T, rt *Runtime, floor *floorSeq, principal Principal, allowed ...Relation) *Handle {
	t.Helper()
	if floor == nil {
		floor = &floorSeq{}
	}
	handle, err := rt.Bind(floorTx(t, floor), mustTenant(t, "t1"), principal, allowed)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

// fakeRelationships is a recording fake of the relationships port: it answers each tuple write with the next of its
// write tokens, w-1, w-2 and on, unless writes gives others, or, for a request that also TOUCHes the floor marker,
// with the next of heads when heads gives one. fail, when set, gives the error of the n-th write.
type fakeRelationships struct {
	mu      sync.Mutex
	touches [][]Tuple
	deletes [][]Filter
	writes  []Token
	heads   []Token
	fail    func(n int) error
	// failDelete, when set, is the error of every Delete.
	failDelete error
	// before, when set, runs before the n-th write, of the resource ID resource, is answered, outside the lock.
	before func(n int, resource string)
	events []string
	onHead func()
	given  map[string][]Token
	calls  int
	headN  int
	starts int
}

// begin counts one write of the resource ID resource and runs before for it.
func (f *fakeRelationships) begin(resource string) {
	f.mu.Lock()
	f.starts++
	n := f.starts
	f.events = append(f.events, "start "+itoa(n))
	before := f.before
	f.mu.Unlock()
	if before != nil {
		before(n, resource)
	}
}

func (f *fakeRelationships) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var events []string
	for _, e := range f.events {
		if e != "marker" {
			events = append(events, e)
		}
	}
	return events
}

func named(prefix string, n int) Token {
	token, err := NewToken(prefix + "-" + itoa(n))
	if err != nil {
		panic(err)
	}
	return token
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

func (f *fakeRelationships) answer() (Token, error) {
	f.calls++
	defer func() { f.events = append(f.events, "end "+itoa(f.calls)) }()
	if f.fail != nil {
		if err := f.fail(f.calls); err != nil {
			return Token{}, err
		}
	}
	if len(f.writes) == 0 {
		return named("w", f.calls), nil
	}
	token := f.writes[0]
	f.writes = f.writes[1:]
	return token, nil
}

func (f *fakeRelationships) Touch(ctx context.Context, tuples []Tuple, _ []Precondition) (Token, error) {
	if len(tuples) == 1 && tuples[0] == DefaultMarker {
		return f.mark(ctx)
	}
	f.begin(tuples[0].Resource.ID)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches = append(f.touches, tuples)
	return f.answer()
}

func (f *fakeRelationships) Delete(_ context.Context, filters []Filter, _ []Precondition) (Token, error) {
	f.begin(filters[0].ResourceID)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, filters)
	if f.failDelete != nil {
		f.calls++
		return Token{}, f.failDelete
	}
	return f.answer()
}

// Write records the tuple part of one request of touches and deletes and answers it with one token: the next of the
// marker tokens when the request holds the floor marker and heads gives one, otherwise the next write token. The
// event of a request with the marker is "write+marker".
func (f *fakeRelationships) Write(ctx context.Context, touches []Tuple, deletes []Filter, _ []Precondition) (Token, error) {
	var tuples []Tuple
	marker := false
	for _, tuple := range touches {
		if tuple == DefaultMarker {
			marker = true
			continue
		}
		tuples = append(tuples, tuple)
	}
	resource := ""
	switch {
	case len(tuples) > 0:
		resource = tuples[0].Resource.ID
	case len(deletes) > 0:
		resource = deletes[0].ResourceID
	}
	if resource == "" {
		return f.mark(ctx)
	}
	f.begin(resource)
	if marker && f.onHead != nil {
		f.onHead()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(tuples) > 0 {
		f.touches = append(f.touches, tuples)
	}
	if len(deletes) > 0 {
		f.deletes = append(f.deletes, deletes)
		if f.failDelete != nil {
			f.calls++
			return Token{}, f.failDelete
		}
	}
	if marker {
		f.events = append(f.events, "marker")
	}
	token, err := f.answer()
	if err == nil && marker && len(f.heads) > 0 {
		token = f.heads[0]
		f.heads = f.heads[1:]
	}
	if err == nil {
		if f.given == nil {
			f.given = map[string][]Token{}
		}
		f.given[resource] = append(f.given[resource], token)
	}
	return token, err
}

// tokensOf gives the tokens that the fake answered for writes of the resource ID resource.
func (f *fakeRelationships) tokensOf(resource string) []Token {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Token(nil), f.given[resource]...)
}

// mark answers a TOUCH of the floor marker alone, as Check sends it, with the next marker token.
func (f *fakeRelationships) mark(context.Context) (Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headN++
	if len(f.heads) == 0 {
		return named("h", f.headN), nil
	}
	token := f.heads[0]
	f.heads = f.heads[1:]
	return token, nil
}

// pgEnv is one database of the local PostgreSQL server of pgtest with the outbox runtime's queue and the authz
// tables, and a runtime role, an outbox worker role and an authz relay role of its own that are not superusers, so
// row-level security and the grants apply.
type pgEnv struct {
	admin    *sql.DB
	runtime  *sql.DB
	workerDB *sql.DB
	relayDB  *sql.DB
	schema   string
	roles    [3]string // runtime, worker, relay
	clock    *testClock
	queue    *outbox.Queue
	wqueue   *outbox.Queue
}

const rolePassword = "authz-test"

// testClock is a clock that the test advances; a nil one is the database clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newPG(t *testing.T) *pgEnv { return newPGClock(t, nil) }

// newPGClock is newPG with the queue clock clock, which may be nil for the database clock.
func newPGClock(t *testing.T, clock *testClock) *pgEnv {
	t.Helper()
	server := pgtest.DSN(t)
	ctx := context.Background()
	root, err := sql.Open("pgx", server)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	tag := hex.EncodeToString(suffix)
	roles := [3]string{"authz_rt_" + tag, "authz_worker_" + tag, "authz_relay_" + tag}
	for _, role := range roles {
		if _, err := root.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+rolePassword+"'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role) })
	}
	dsn := pgtest.Database(t, "authz")
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	t.Cleanup(func() {
		for _, role := range roles {
			_, _ = admin.ExecContext(context.Background(), "DROP OWNED BY "+role)
		}
	})
	queueDDL, err := outbox.Migration(roles[0], roles[1])
	if err != nil {
		t.Fatal(err)
	}
	schema := "authz"
	migration, err := Migration(schema, roles[0], roles[2], roles[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		queueDDL,
		"CREATE SCHEMA " + schema,
		"GRANT USAGE ON SCHEMA " + schema + " TO " + roles[0] + ", " + roles[1] + ", " + roles[2],
		migration,
		"CREATE TABLE " + schema + ".documents (tenant_id text NOT NULL, id text NOT NULL, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))",
		"ALTER TABLE " + schema + ".documents ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".documents FORCE ROW LEVEL SECURITY",
		"CREATE POLICY documents_tenant ON " + schema + ".documents USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
		"GRANT SELECT, INSERT ON " + schema + ".documents TO " + roles[0],
		"GRANT SELECT, UPDATE (authz_token) ON " + schema + ".documents TO " + roles[1],
	} {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%.80s: %v", statement, err)
		}
	}
	open := func(role string) *sql.DB {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, rolePassword)
		db, err := sql.Open("pgx", u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	env := &pgEnv{admin: admin, runtime: open(roles[0]), workerDB: open(roles[1]), relayDB: open(roles[2]),
		schema: schema, roles: roles, clock: clock}
	var queueClock outbox.Clock
	if clock != nil {
		queueClock = clock
	}
	if env.queue, err = outbox.New(env.runtime, outbox.Options{Clock: queueClock}); err != nil {
		t.Fatal(err)
	}
	if env.wqueue, err = outbox.New(env.workerDB, outbox.Options{Clock: queueClock}); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *pgEnv) runtimeOf(t *testing.T, provider Decision, clock Clock) *Runtime {
	t.Helper()
	// No Floor: the runtime builds the TableFloor of DB and Schema for Port.
	rt, err := New(Config{Provider: provider, DB: e.runtime, Schema: e.schema, Outbox: e.queue,
		Poll: 10 * time.Millisecond, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func (e *pgEnv) relay(t *testing.T, relationships Relationships) *Relay {
	t.Helper()
	relay, err := NewRelay(RelayConfig{Name: "authz-relay", Relationships: relationships, DB: e.relayDB, Schema: e.schema,
		Owners: map[string]Owner{"document": {Table: "documents", Key: "id"}}})
	if err != nil {
		t.Fatal(err)
	}
	return relay
}

// workerConfig is the configuration of the outbox worker of the tests: short polls and retries.
func workerConfig() outbox.WorkerConfig {
	config := outbox.DefaultWorkerConfig()
	config.PollInterval = 10 * time.Millisecond
	config.RetryMin = 10 * time.Millisecond
	config.RetryMax = 50 * time.Millisecond
	config.AttemptTimeout = 2 * time.Second
	config.LeaseDuration = 15 * time.Second
	config.PurgeEvery = time.Second
	return config
}

// newWorker builds the outbox worker of the worker role with the relay registered, and change, when given, changes
// its configuration first.
func (e *pgEnv) newWorker(t *testing.T, relay *Relay, change func(*outbox.WorkerConfig)) *outbox.Worker {
	t.Helper()
	config := workerConfig()
	if change != nil {
		change(&config)
	}
	worker, err := outbox.NewWorker(e.wqueue, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Register(relay.Destination()); err != nil {
		t.Fatal(err)
	}
	return worker
}

// run runs worker until the test ends.
func run(t *testing.T, worker *outbox.Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = worker.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// startWorker builds and runs the outbox worker with the relay registered.
func (e *pgEnv) startWorker(t *testing.T, relay *Relay) *outbox.Worker {
	t.Helper()
	worker := e.newWorker(t, relay, nil)
	run(t, worker)
	return worker
}

// begin opens a transaction of tenant id as the runtime role and binds a handle to it for principal.
func (e *pgEnv) begin(t *testing.T, rt *Runtime, id string, principal Principal, allowed ...Relation) (*sql.Tx, *Handle) {
	t.Helper()
	tx, err := tenant.Begin(context.Background(), e.runtime, mustTenant(t, id), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	handle, err := rt.Bind(tx, mustTenant(t, id), principal, allowed)
	if err != nil {
		t.Fatal(err)
	}
	return tx, handle
}

func (e *pgEnv) commit(t *testing.T, tx *sql.Tx, id string) {
	t.Helper()
	if err := tenant.Commit(context.Background(), tx, mustTenant(t, id)); err != nil {
		t.Fatal(err)
	}
}

func (e *pgEnv) scalar(t *testing.T, query string, args ...any) string {
	t.Helper()
	var value sql.NullString
	if err := e.admin.QueryRowContext(context.Background(), strings.ReplaceAll(query, "{s}", e.schema), args...).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return value.String
}

// ids gives the identities of the outbox messages, in enqueue order.
func (e *pgEnv) ids(t *testing.T) []uuid.UUID {
	t.Helper()
	rows, err := e.admin.QueryContext(context.Background(), "SELECT id FROM stego_outbox.messages ORDER BY sequence")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// state reads the dispatch state of the outbox message id.
func (e *pgEnv) state(t *testing.T, id uuid.UUID) string {
	t.Helper()
	return e.scalar(t, "SELECT state FROM stego_outbox.messages WHERE id = $1", id)
}

// waitState waits until the outbox message id is in state, for at most 10 seconds.
func (e *pgEnv) waitState(t *testing.T, id uuid.UUID, state outbox.State) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if e.state(t, id) == string(state) {
			return
		}
	}
	t.Fatalf("the message %s is %s, want %s", id, e.state(t, id), state)
}

// waitAll waits until every outbox message is in state.
func (e *pgEnv) waitAll(t *testing.T, state outbox.State) {
	t.Helper()
	for _, id := range e.ids(t) {
		e.waitState(t, id, state)
	}
}

// delivery reads the outbox message id as the worker claims it, for a test that runs one relay by hand.
func (e *pgEnv) delivery(t *testing.T, id uuid.UUID) outbox.Delivery {
	t.Helper()
	var d outbox.Delivery
	var tenantID string
	var body []byte
	if err := e.admin.QueryRowContext(context.Background(), "SELECT tenant_id, destination, resource_key, kind, payload, group_id "+
		"FROM stego_outbox.messages WHERE id = $1", id).Scan(&tenantID, &d.Destination, &d.ResourceKey, &d.Kind, &body, &d.Group); err != nil {
		t.Fatal(err)
	}
	d.ID, d.Payload, d.Tenant = id, body, mustTenant(t, tenantID)
	return d
}

// deliverByHand runs the relay's handler on the message id in a worker-role transaction of its tenant, as the
// worker does, and commits on success; it does not change the message's state.
func (e *pgEnv) deliverByHand(t *testing.T, relay *Relay, id uuid.UUID) error {
	t.Helper()
	ctx := context.Background()
	d := e.delivery(t, id)
	tx, err := tenant.Begin(ctx, e.workerDB, d.Tenant, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := relay.Deliver(ctx, tx, d); err != nil {
		return err
	}
	return tenant.Commit(ctx, tx, d.Tenant)
}

// fakeAuditor is the auditor of a worker that resolves: it writes nothing.
type fakeAuditor struct{}

func (fakeAuditor) Record(context.Context, *sql.Tx, outbox.AuditEntry) error { return nil }

// discard resolves the parked message id as discarded through the worker's Resolve, in a transaction of the
// message's tenant as the runtime role.
func (e *pgEnv) discard(t *testing.T, worker *outbox.Worker, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	d := e.delivery(t, id)
	tx, err := tenant.Begin(ctx, e.runtime, d.Tenant, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	resolved, err := worker.Resolve(ctx, tx, outbox.ResolveRequest{ID: id, Subject: "user:operator", Resolution: outbox.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.Commit(ctx, tx, d.Tenant); err != nil {
		t.Fatal(err)
	}
	resolved.Committed(ctx)
}

func mustPerson(t *testing.T, id string) Principal {
	t.Helper()
	p, err := Person("user", verifiedIdentity(t, id))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustSystem(t *testing.T, name string) Principal {
	t.Helper()
	p, err := System(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func rel(text string) Relation {
	r, err := ParseRelation(text)
	if err != nil {
		panic(err)
	}
	return r
}

func atLeast(t *testing.T, c Consistency) Token {
	t.Helper()
	token, ok := c.Token()
	if !ok {
		t.Fatalf("the consistency is %s, want at_least", c)
	}
	return token
}

// fixedClock is a clock that the test sets.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func TestHandleEvaluation(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP06-LC04-T02 A read of one row checks at least as fresh as the row's token", func(t *testing.T) {
		fake := &recorder{}
		handle := bindUnit(t, newUnitRuntime(t, fake), nil, mustPerson(t, "u-7"))
		rowToken, err := ParseToken(tok(17).Text())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Evaluate(ctx, "view", doc("1"), rowToken, nil); err != nil {
			t.Fatal(err)
		}
		if got := atLeast(t, fake.evaluations[0].Consistency); got != tok(17) {
			t.Fatalf("the check ran at least as fresh as %q, want t-17", got.Text())
		}
	})

	t.Run("ST-SUP-RP06-LC04-T03 Each page of a batch-filtered list runs at least as fresh as the floor at that page", func(t *testing.T) {
		fake := &recorder{token: func(n int) Token { return tok(2 + n) }} // t-3 at the first call
		floor := &floorSeq{tokens: []Token{tok(1), tok(1), tok(6)}}
		handle := bindUnit(t, newUnitRuntime(t, fake), floor, mustPerson(t, "u-7"))
		list := handle.List("view", nil)
		for page := 0; page < 3; page++ {
			if _, err := list.Page(ctx, []Row{{Resource: doc("a"), Token: tok(3)}, {Resource: doc("b")}}); err != nil {
				t.Fatal(err)
			}
		}
		if len(fake.batches) != 3 {
			t.Fatalf("the provider got %d batch calls, want 3", len(fake.batches))
		}
		for i, want := range []Token{tok(1), tok(1), tok(6)} {
			for _, evaluation := range fake.batches[i].Evaluations {
				if got := atLeast(t, evaluation.Consistency); got != want {
					t.Errorf("page %d ran at least as fresh as %q, want %q", i+1, got.Text(), want.Text())
				}
			}
		}
	})

	t.Run("ST-SUP-RP06-LC09-T02 The handle evaluates with the transaction's principal as subject", func(t *testing.T) {
		fake := &recorder{}
		handle := bindUnit(t, newUnitRuntime(t, fake), nil, mustPerson(t, "u-7"))
		if _, err := handle.Evaluate(ctx, "view", doc("1"), Token{}, nil); err != nil {
			t.Fatal(err)
		}
		if got, want := fake.evaluations[0].Subject, mustPerson(t, "u-7").Subject(); got != want {
			t.Fatalf("the subject is %s, want %s, the subject of user u-7", got, want)
		}
	})

	t.Run("ST-SUP-RP06-LC09-T03 The handle evaluates at the row token only while the floor is zero", func(t *testing.T) {
		fake := &recorder{}
		floor := &floorSeq{tokens: []Token{{}, {}, tok(9)}}
		handle := bindUnit(t, newUnitRuntime(t, fake), floor, mustPerson(t, "u-7"))
		for _, row := range []Token{tok(5), {}, tok(5)} {
			if _, err := handle.Evaluate(ctx, "view", doc("1"), row, nil); err != nil {
				t.Fatal(err)
			}
		}
		if got := atLeast(t, fake.evaluations[0].Consistency); got != tok(5) {
			t.Errorf("the first check ran at %q, want exactly the row token t-5", got.Text())
		}
		if !fake.evaluations[1].Consistency.IsFresh() {
			t.Errorf("the check of a row with no token ran %s, want fresh", fake.evaluations[1].Consistency)
		}
		if got := atLeast(t, fake.evaluations[2].Consistency); got != tok(9) {
			t.Errorf("the check under the floor t-9 ran at %q, want t-9", got.Text())
		}
	})

	t.Run("ST-SUP-RP06-LC10-T02 A system principal gets its service account subject", func(t *testing.T) {
		fake := &recorder{}
		handle := bindUnit(t, newUnitRuntime(t, fake), nil, mustSystem(t, "reconciler"))
		if _, err := handle.Evaluate(ctx, "view", doc("1"), Token{}, nil); err != nil {
			t.Fatal(err)
		}
		if got := fake.evaluations[0].Subject.String(); got != "service_account:reconciler" {
			t.Fatalf("the subject is %s, want service_account:reconciler", got)
		}
	})

	t.Run("ST-SUP-RP06-LC10-T03 The handle exposes no provider client, no relationships port and no bypass", func(t *testing.T) {
		handleType := reflect.TypeOf(&Handle{})
		if got := exportedFields(handleType.Elem()); len(got) != 0 {
			t.Fatalf("Handle has the exported fields %v", got)
		}
		forbidden := []reflect.Type{reflect.TypeOf((*Decision)(nil)).Elem(), reflect.TypeOf((*Relationships)(nil)).Elem(),
			reflect.TypeOf(&SpiceDB{}), reflect.TypeOf(&Port{}), reflect.TypeOf(&Runtime{}), reflect.TypeOf(&Relay{})}
		var holds func(reflect.Type, map[reflect.Type]bool) string
		holds = func(typ reflect.Type, seen map[reflect.Type]bool) string {
			if seen[typ] {
				return ""
			}
			seen[typ] = true
			for _, bad := range forbidden {
				if typ == bad || (bad.Kind() == reflect.Interface && typ.Kind() != reflect.Interface && typ.Implements(bad)) {
					return bad.String()
				}
			}
			switch typ.Kind() {
			case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
				return holds(typ.Elem(), seen)
			case reflect.Map:
				if found := holds(typ.Key(), seen); found != "" {
					return found
				}
				return holds(typ.Elem(), seen)
			case reflect.Struct:
				for i := 0; i < typ.NumField(); i++ {
					if typ.Field(i).IsExported() {
						if found := holds(typ.Field(i).Type, seen); found != "" {
							return found
						}
					}
				}
			case reflect.Func:
				for i := 0; i < typ.NumOut(); i++ {
					if found := holds(typ.Out(i), seen); found != "" {
						return found
					}
				}
			}
			return ""
		}
		if handleType.NumMethod() < 5 {
			t.Fatalf("Handle has %d methods", handleType.NumMethod())
		}
		for i := 0; i < handleType.NumMethod(); i++ {
			method := handleType.Method(i)
			lower := strings.ToLower(method.Name)
			if strings.Contains(lower, "bypass") || strings.Contains(lower, "skip") {
				t.Errorf("Handle has the method %s", method.Name)
			}
			for j := 0; j < method.Type.NumOut(); j++ {
				if found := holds(method.Type.Out(j), map[reflect.Type]bool{}); found != "" {
					t.Errorf("Handle.%s gives a value that is or holds %s", method.Name, found)
				}
			}
		}
	})

	t.Run("ST-SUP-RP06-LC11-T04 A list on a provider that does not search uses batch evaluation", func(t *testing.T) {
		fake := &recorder{allow: func(EvaluationRequest) bool { return true }}
		handle := bindUnit(t, newUnitRuntime(t, fake), nil, mustPerson(t, "u-7"))
		list := handle.List("view", nil)
		for range 2 {
			rows, err := list.Page(ctx, []Row{{Resource: doc("a")}, {Resource: doc("b")}})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 {
				t.Fatalf("the page gave %d rows, want 2", len(rows))
			}
		}
		if len(fake.batches) != 2 || len(fake.searchCalls) != 0 {
			t.Fatalf("the provider got %d batch calls and %d searches, want 2 and 0", len(fake.batches), len(fake.searchCalls))
		}
		if _, err := handle.Search(ctx, "view", "document", nil, Page{}); !errors.Is(err, ErrNoSearch) {
			t.Fatalf("a search on a provider that does not search gave %v", err)
		}
		if len(fake.searchCalls) != 0 {
			t.Fatal("the search reached a provider that does not search")
		}
	})

	t.Run("ST-SUP-RP06-LC13-T02 Every evaluation runs at least as fresh as the floor", func(t *testing.T) {
		fake := &recorder{}
		handle := bindUnit(t, newUnitRuntime(t, fake), fixedFloor(tok(9)), mustPerson(t, "u-7"))
		for _, row := range []Token{tok(5), tok(12)} {
			if _, err := handle.Evaluate(ctx, "view", doc("1"), row, nil); err != nil {
				t.Fatal(err)
			}
		}
		for i, evaluation := range fake.evaluations {
			if got := atLeast(t, evaluation.Consistency); got != tok(9) {
				t.Errorf("check %d ran at %q under the floor t-9, want t-9", i+1, got.Text())
			}
		}
	})

	t.Run("ST-SUP-RP06-LC13-T05 Batch evaluation, resource search and every list page run at least as fresh as the floor", func(t *testing.T) {
		fake := &recorder{token: func(int) Token { return tok(2) }, search: func(SearchRequest) SearchResult { return SearchResult{} }}
		handle := bindUnit(t, newUnitRuntime(t, fake), fixedFloor(tok(9)), mustPerson(t, "u-7"))
		if _, err := handle.EvaluateBatch(ctx, "view", []Row{{Resource: doc("a"), Token: tok(4)}}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Search(ctx, "view", "document", nil, Page{}); err != nil {
			t.Fatal(err)
		}
		list := handle.List("view", nil)
		for range 3 {
			if _, err := list.Page(ctx, []Row{{Resource: doc("a")}}); err != nil {
				t.Fatal(err)
			}
		}
		if len(fake.batches) != 4 || len(fake.searchCalls) != 1 {
			t.Fatalf("the provider got %d batch calls and %d searches", len(fake.batches), len(fake.searchCalls))
		}
		for i, batch := range fake.batches {
			for _, evaluation := range batch.Evaluations {
				if got := atLeast(t, evaluation.Consistency); got != tok(9) {
					t.Errorf("batch call %d ran at %q, want the floor t-9", i, got.Text())
				}
			}
		}
		if got := atLeast(t, fake.searchCalls[0].Consistency); got != tok(9) {
			t.Errorf("the search ran at %q, want t-9", got.Text())
		}
	})

	t.Run("ST-SUP-RP06-LC17-T01 Only an allow makes the tenant runtime's Decision, for the tenant it decided", func(t *testing.T) {
		allowed := mustPerson(t, "u-1").Subject()
		fake := &recorder{allow: func(r EvaluationRequest) bool { return r.Subject == allowed }}
		rt := newUnitRuntime(t, fake)
		platform := Resource{Type: "platform", ID: qualify("t1", "main")}
		decision, ok, err := bindUnit(t, rt, nil, mustPerson(t, "u-1")).DecideTenant(ctx, tenant.ProvisionAction, "ta", platform, nil)
		if err != nil || !ok || decision == nil {
			t.Fatalf("the allow gave %v, %t, %v", decision, ok, err)
		}
		if made, err := tenant.New(tenant.Requested(tenant.ProvisionAction, "ta", decision)); err != nil || made.ID() != "ta" {
			t.Fatalf("tenant.Requested of ta with the allow gave %v, %v", made, err)
		}
		if _, err := tenant.New(tenant.Requested(tenant.ProvisionAction, "tb", decision)); err == nil {
			t.Fatal("the allow for ta serves tb")
		}
		if _, err := tenant.New(tenant.Requested(tenant.RetireAction, "ta", decision)); err == nil {
			t.Fatal("the allow to provision ta serves a retire of ta")
		}
		denied, ok, err := bindUnit(t, rt, nil, mustPerson(t, "u-2")).DecideTenant(ctx, tenant.ProvisionAction, "ta", platform, nil)
		if err != nil || ok || denied != nil {
			t.Fatalf("the deny gave %v, %t, %v; want no decision", denied, ok, err)
		}
		// Types that embed a Decision and declare Allowed true, as a forger would. A package outside the runtime
		// module cannot declare the seal's unexported method either, so these are the types such a package can
		// build: none gives a tenant other than the one its genuine mark holds.
		for name, source := range map[string]tenant.Source{
			"the genuine allow embedded, for tb":    tenant.Requested(tenant.ProvisionAction, "tb", allowedForged{decision}),
			"the genuine allow embedded, to retire": tenant.Requested(tenant.RetireAction, "ta", allowedForged{decision}),
			"a nil embedded Decision":               tenant.Requested(tenant.ProvisionAction, "ta", allowedForged{}),
			"the zero value":                        tenant.Requested(tenant.ProvisionAction, "ta", tenantDecision{}),
			"no decision":                           tenant.Requested(tenant.ProvisionAction, "ta", nil),
		} {
			if made, err := tenant.New(source); err == nil {
				t.Errorf("%s gave the tenant %s", name, made.ID())
			}
		}
	})

	t.Run("ST-SUP-RP06-LC17-T02 DecideTenant decides the permission of the action it is given and binds the mark to it", func(t *testing.T) {
		fake := &recorder{allow: func(r EvaluationRequest) bool { return r.Action.Name == "provision" }}
		handle := bindUnit(t, newUnitRuntime(t, fake), nil, mustPerson(t, "u-1"))
		platform := Resource{Type: "platform", ID: qualify("t1", "main")}
		provision, ok, err := handle.DecideTenant(ctx, tenant.ProvisionAction, "ta", platform, nil)
		if err != nil || !ok {
			t.Fatalf("the provision gave %v, %t, %v", provision, ok, err)
		}
		retire, ok, err := handle.DecideTenant(ctx, tenant.RetireAction, "ta", platform, nil)
		if err != nil || ok || retire != nil {
			t.Fatalf("the retire gave %v, %t, %v; want no value", retire, ok, err)
		}
		if len(fake.evaluations) != 2 || fake.evaluations[0].Action.Name != "provision" || fake.evaluations[1].Action.Name != "retire" {
			t.Fatalf("the port got %+v, want the provision permission and then the retire permission", fake.evaluations)
		}
		tenantID, action, ok := seal.DecisionOf(provision)
		if !ok || tenantID != "ta" || action != "provision" {
			t.Fatalf("the mark holds %q, %q, %t; want ta and provision", tenantID, action, ok)
		}
		if _, _, err := handle.DecideTenant(ctx, "delete", "ta", platform, nil); err == nil {
			t.Fatal("an action other than provision or retire is decided")
		}
		// An allowed retire gives a mark of retire, which tenant.Requested takes only for a retire.
		permissive := bindUnit(t, newUnitRuntime(t, &recorder{allow: func(EvaluationRequest) bool { return true }}), nil, mustPerson(t, "u-1"))
		allowed, ok, err := permissive.DecideTenant(ctx, tenant.RetireAction, "ta", platform, nil)
		if err != nil || !ok {
			t.Fatalf("the allowed retire gave %v, %t, %v", allowed, ok, err)
		}
		if _, action, _ := seal.DecisionOf(allowed); action != "retire" {
			t.Fatalf("the mark of the retire holds the action %q, want retire", action)
		}
	})
}

// allowedForged is a type that embeds a Decision and declares Allowed true, as a forger would.
type allowedForged struct {
	tenant.Decision
}

func (allowedForged) Allowed() bool { return true }

func TestHandleWrites(t *testing.T) {
	ctx := context.Background()
	viewer := Tuple{Resource: doc("1"), Relation: "viewer", Subject: user("bob")}
	refused := Refused(errors.New("invalid argument"))

	t.Run("ST-SUP-RP06-LC09-T01 A handle is bound to its transaction and tenant", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		if _, err := handle.Relate(ctx, viewer); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		if got := env.scalar(t, "SELECT tenant_id FROM stego_outbox.messages"); got != "t1" {
			t.Fatalf("the outbox row carries the tenant %q, want t1", got)
		}
		if _, err := handle.Relate(ctx, viewer); err == nil {
			t.Fatal("Relate after the commit gave no error")
		}
	})

	t.Run("ST-SUP-RP06-LC09-T04 Relate enqueues a touch tuple in the transaction's outbox", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		if _, err := handle.Relate(ctx, viewer); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if n := env.scalar(t, "SELECT count(*) FROM stego_outbox.messages"); n != "0" {
			t.Fatalf("%s outbox rows after the rollback", n)
		}
		tx, handle = env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		if _, err := handle.Relate(ctx, viewer); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		if n := env.scalar(t, "SELECT count(*) FROM stego_outbox.messages WHERE destination = 'authz' AND resource_key = 'document:1'"); n != "1" {
			t.Fatalf("%s outbox rows for authz after the commit, want 1", n)
		}
		d := env.delivery(t, env.ids(t)[0])
		message, err := readPayload(d.Payload, d.Tenant)
		if err != nil || message.Operation != OperationTouch || message.Tuple.tuple() != viewer || d.Kind != "authz.touch" {
			t.Fatalf("the entry is %+v, %v; want a touch of the tuple", message, err)
		}
	})

	t.Run("ST-SUP-RP06-LC09-T05 Unrelate enqueues a delete tuple in the transaction's outbox", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		if _, err := handle.Unrelate(ctx, viewer); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		if n := env.scalar(t, "SELECT count(*) FROM stego_outbox.messages WHERE destination = 'authz'"); n != "1" {
			t.Fatalf("%s outbox rows for authz, want 1", n)
		}
		d := env.delivery(t, env.ids(t)[0])
		message, err := readPayload(d.Payload, d.Tenant)
		if err != nil || message.Operation != OperationDelete || message.Tuple.tuple() != viewer || d.Kind != "authz.delete" {
			t.Fatalf("the entry is %+v, %v; want a delete of the tuple", message, err)
		}
	})

	t.Run("ST-SUP-RP06-LC09-T06 Relate and Unrelate do not call the provider", func(t *testing.T) {
		env := newPG(t)
		fake := &recorder{}
		rt := env.runtimeOf(t, fake, nil)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		if _, err := handle.Relate(ctx, viewer); err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Unrelate(ctx, viewer); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		if n := fake.requests(); n != 0 {
			t.Fatalf("the provider got %d requests", n)
		}
		if n := len(env.ids(t)); n != 2 {
			t.Fatalf("the outbox got %d messages, want 2", n)
		}
	})

	t.Run("ST-SUP-RP06-LC09-T07 The handle reads the floor through its own transaction", func(t *testing.T) {
		env := newPG(t)
		env.runtime.SetMaxOpenConns(1)
		fake := &recorder{allow: func(EvaluationRequest) bool { return true }}
		rt := env.runtimeOf(t, fake, nil)
		_, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"))
		deadline, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, err := handle.Evaluate(deadline, "view", doc("1"), Token{}, nil); err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if _, err := handle.EvaluateBatch(deadline, "view", []Row{{Resource: doc("1")}}, nil); err != nil {
			t.Fatalf("EvaluateBatch: %v", err)
		}
		if _, err := handle.List("view", nil).Page(deadline, []Row{{Resource: doc("1")}}); err != nil {
			t.Fatalf("Page: %v", err)
		}
		if n := fake.requests(); n != 3 {
			t.Fatalf("the provider got %d requests, want 3", n)
		}
	})

	t.Run("ST-SUP-RP06-LC10-T01 A relation outside the allowed set is refused before anything is enqueued", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		_, err := handle.Relate(ctx, Tuple{Resource: doc("1"), Relation: "owner", Subject: user("bob")})
		if err == nil || !strings.Contains(err.Error(), "document#owner") {
			t.Fatalf("the relation owner gave %v, want an error that names document#owner", err)
		}
		env.commit(t, tx, "t1")
		if n := env.scalar(t, "SELECT count(*) FROM stego_outbox.messages"); n != "0" {
			t.Fatalf("%s outbox rows are written", n)
		}
		if n := env.scalar(t, "SELECT count(*) FROM {s}.authz_revocation_tuples"); n != "0" {
			t.Fatalf("%s tuple rows are written", n)
		}
	})

	t.Run("ST-SUP-RP06-LC12-T01 Wait returns the floor of the delivery once the relay delivers the delete", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		env.startWorker(t, env.relay(t, &fakeRelationships{writes: []Token{named("w", 42)}, heads: []Token{named("h", 42)}}))
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		revocation, err := handle.Unrelate(ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		status, err := handle.Wait(waitCtx, revocation)
		if err != nil {
			t.Fatal(err)
		}
		if status.State != RevocationComplete || status.Token != named("h", 42) || status.ID != revocation.ID() {
			t.Fatalf("Wait gave %+v, want complete with h-42", status)
		}
	})

	t.Run("ST-SUP-RP06-LC12-T02 Wait returns pending when its context ends before the delivery", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		revocation, err := handle.Unrelate(ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		status, err := handle.Wait(waitCtx, revocation)
		if err != nil || status.State != RevocationPending || status.ID != revocation.ID() || !status.Token.IsZero() {
			t.Fatalf("Wait gave %+v, %v; want pending with the identity and no token", status, err)
		}
		if got := env.state(t, env.ids(t)[0]); got != "pending" {
			t.Fatalf("the entry is %s, want still queued", got)
		}
	})

	t.Run("ST-SUP-RP06-LC12-T03 Wait answers parked for a parked revocation", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		fake := &fakeRelationships{failDelete: refused}
		env.startWorker(t, env.relay(t, fake))
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		revocation, err := handle.Unrelate(ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		status, err := handle.Wait(waitCtx, revocation)
		if err != nil || status.State != RevocationParked || status.ID != revocation.ID() || !status.Token.IsZero() {
			t.Fatalf("Wait gave %+v, %v; want parked with no token", status, err)
		}
		if got := env.state(t, env.ids(t)[0]); got != "parked" {
			t.Fatalf("the entry is %s, want parked", got)
		}
	})

	t.Run("ST-SUP-RP06-LC12-T04 Wait before the commit is refused", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		_, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		revocation, err := handle.Unrelate(ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		status, err := handle.Wait(ctx, revocation)
		if !errors.Is(err, ErrWaitBeforeCommit) || !status.Token.IsZero() {
			t.Fatalf("Wait in the open transaction gave %+v, %v", status, err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("Wait took %s before it refused", elapsed)
		}
	})

	t.Run("ST-SUP-RP06-LC12-T05 Every tuple write of a transaction joins one revocation", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"),
			rel("document#banned"), rel("document#viewer"), rel("document#owner"))
		first, err := handle.Relate(ctx, Tuple{Resource: doc("1"), Relation: "banned", Subject: user("bob")})
		if err != nil {
			t.Fatal(err)
		}
		second, err := handle.Unrelate(ctx, Tuple{Resource: doc("2"), Relation: "viewer", Subject: user("bob")})
		if err != nil {
			t.Fatal(err)
		}
		// The relationship mapper of a saved document enqueues its tuple through the same handle.
		mapper := func(h *Handle, id string) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO "+env.schema+".documents (tenant_id, id) VALUES ('t1', $1)", id); err != nil {
				return err
			}
			_, err := h.Relate(ctx, Tuple{Resource: doc(id), Relation: "owner", Subject: user("alice")})
			return err
		}
		if err := mapper(handle, "3"); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		if first.ID() != second.ID() || first.ID() == uuid.Nil {
			t.Fatalf("Relate gave %s and Unrelate gave %s, want one revocation", first.ID(), second.ID())
		}
		if n := env.scalar(t, "SELECT count(*) FROM stego_outbox.messages AS m JOIN {s}.authz_revocation_tuples AS t "+
			"ON t.message_id = m.id WHERE m.group_id = $1 AND t.revocation_id = $2", first.ID().String(), first.ID()); n != "3" {
			t.Fatalf("the revocation names %s of the three outbox entries", n)
		}
	})

	t.Run("ST-SUP-RP06-LC12-T06 Two handles on one transaction share one revocation", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		alice := mustPerson(t, "alice")
		var ids []uuid.UUID
		for run := 0; run < 2; run++ {
			tx, first := env.begin(t, rt, "t1", alice, rel("document#banned"))
			second, err := rt.Bind(tx, mustTenant(t, "t1"), alice, []Relation{rel("document#viewer")})
			if err != nil {
				t.Fatal(err)
			}
			a, err := first.Relate(ctx, Tuple{Resource: doc("1"), Relation: "banned", Subject: user("bob")})
			if err != nil {
				t.Fatal(err)
			}
			b, err := second.Unrelate(ctx, Tuple{Resource: doc("2"), Relation: "viewer", Subject: user("bob")})
			if err != nil {
				t.Fatal(err)
			}
			env.commit(t, tx, "t1")
			if a.ID() != b.ID() {
				t.Fatalf("run %d: the two handles gave %s and %s", run, a.ID(), b.ID())
			}
			if n := env.scalar(t, "SELECT count(*) FROM stego_outbox.messages WHERE group_id = $1", a.ID().String()); n != "2" {
				t.Fatalf("run %d: the revocation names %s outbox entries, want 2", run, n)
			}
			ids = append(ids, a.ID())
		}
		if ids[0] == ids[1] {
			t.Fatal("two transactions share one revocation")
		}
	})

	// twoTuples makes a revocation of a touch of document:1#banned@user:bob and a delete of
	// document:2#viewer@user:bob for alice in t1, and gives the identities of its two outbox entries.
	twoTuples := func(t *testing.T, env *pgEnv, rt *Runtime) (Revocation, []uuid.UUID) {
		t.Helper()
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#banned"), rel("document#viewer"))
		revocation, err := handle.Relate(ctx, Tuple{Resource: doc("1"), Relation: "banned", Subject: user("bob")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Unrelate(ctx, Tuple{Resource: doc("2"), Relation: "viewer", Subject: user("bob")}); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		return revocation, env.ids(t)
	}
	status := func(t *testing.T, rt *Runtime, revocation Revocation) RevocationStatus {
		t.Helper()
		s, err := rt.Status(ctx, mustTenant(t, "t1"), mustPerson(t, "alice"), revocation.ID())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	t.Run("ST-SUP-RP06-LC15-T01 A revocation of two tuples is complete only when both are done, with the floor of its last delivery", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		release := make(chan struct{})
		var releaseOnce sync.Once
		free := func() { releaseOnce.Do(func() { close(release) }) }
		// The write of document:2 waits for the release.
		fake := &fakeRelationships{heads: []Token{named("h", 5), named("h", 8)}, before: func(_ int, resource string) {
			if resource == doc("2").ID {
				<-release
			}
		}}
		revocation, ids := twoTuples(t, env, rt)
		if s := status(t, rt, revocation); s.State != RevocationPending {
			t.Fatalf("after the commit the state is %+v, want pending", s)
		}
		// The relay holds the floor lock while it writes, so the deliveries run one after another: one step at a
		// time, in enqueue order, and a long attempt timeout, so the held write is not given up and sent again.
		run(t, env.newWorker(t, env.relay(t, fake), func(c *outbox.WorkerConfig) {
			c.Concurrency = 1
			c.AttemptTimeout = 20 * time.Second
			c.LeaseDuration = 45 * time.Second
		}))
		t.Cleanup(free)
		env.waitState(t, ids[0], outbox.Done)
		if s := status(t, rt, revocation); s.State != RevocationPending || !s.Token.IsZero() {
			t.Fatalf("after the first delivery the state is %+v, want pending", s)
		}
		free()
		env.waitState(t, ids[1], outbox.Done)
		// The record keeps the floor of the last delivery: the written_at token of a write of document:2, the tuple
		// that became done last, whichever attempt of it landed.
		s := status(t, rt, revocation)
		last := fake.tokensOf(doc("2").ID)
		landed := false
		for _, token := range last {
			landed = landed || s.Token == token
		}
		if s.State != RevocationComplete || !landed || s.Token.Text() != env.scalar(t, "SELECT token FROM {s}.authz_revocations WHERE id = $1", revocation.ID()) {
			t.Fatalf("after the second delivery the state is %+v, want complete with a token of document:2's write %v", s, last)
		}
		for _, token := range fake.tokensOf(doc("1").ID) {
			if s.Token == token {
				t.Fatalf("the revocation holds the token %q of the first delivery", token.Text())
			}
		}
	})

	t.Run("ST-SUP-RP06-LC15-T02 Another principal or tenant gets not found for a revocation", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		alice := mustPerson(t, "alice")
		tx, handle := env.begin(t, rt, "t1", alice, rel("document#viewer"))
		revocation, err := handle.Relate(ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		if _, err := rt.Status(ctx, mustTenant(t, "t1"), alice, revocation.ID()); err != nil {
			t.Fatalf("the maker reads %v", err)
		}
		for name, read := range map[string]func() error{
			"user:bob in t1": func() error {
				_, err := rt.Status(ctx, mustTenant(t, "t1"), mustPerson(t, "bob"), revocation.ID())
				return err
			},
			"user:alice in t2":    func() error { _, err := rt.Status(ctx, mustTenant(t, "t2"), alice, revocation.ID()); return err },
			"an unknown identity": func() error { _, err := rt.Status(ctx, mustTenant(t, "t1"), alice, uuid.New()); return err },
		} {
			if err := read(); !errors.Is(err, ErrRevocationNotFound) {
				t.Errorf("%s got %v, want not found", name, err)
			}
		}
	})

	t.Run("ST-SUP-RP06-LC15-T03 A revocation with a parked entry reports parked", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		revocation, ids := twoTuples(t, env, rt)
		env.startWorker(t, env.relay(t, &fakeRelationships{failDelete: refused}))
		env.waitState(t, ids[0], outbox.Done)
		env.waitState(t, ids[1], outbox.Parked)
		if s := status(t, rt, revocation); s.State != RevocationParked || !s.Token.IsZero() {
			t.Fatalf("the state is %+v, want parked with no token", s)
		}
	})

	t.Run("ST-SUP-RP06-LC15-T04 A revocation with a discarded tuple reports failed", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{allow: func(EvaluationRequest) bool { return true }}, nil)
		release := make(chan struct{})
		var releaseOnce sync.Once
		free := func() { releaseOnce.Do(func() { close(release) }) }
		// The touch of document:1 waits for the release; the delete of document:2 is refused and parked.
		relay := env.relay(t, &fakeRelationships{failDelete: refused, before: func(_ int, resource string) {
			if resource == doc("1").ID {
				<-release
			}
		}})
		// The delete is enqueued first, and the worker takes one step at a time: the relay holds the floor lock while it
		// writes, so the held touch must come after the delete has parked.
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#banned"), rel("document#viewer"))
		revocation, err := handle.Unrelate(ctx, Tuple{Resource: doc("2"), Relation: "viewer", Subject: user("bob")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Relate(ctx, Tuple{Resource: doc("1"), Relation: "banned", Subject: user("bob")}); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		order := env.ids(t)
		ids := []uuid.UUID{order[1], order[0]} // the touch of document:1, then the delete of document:2
		worker := env.newWorker(t, relay, func(c *outbox.WorkerConfig) {
			c.Decider, c.Auditor, c.Concurrency = rt, fakeAuditor{}, 1
			c.AttemptTimeout, c.LeaseDuration = 20*time.Second, 45*time.Second
		})
		run(t, worker)
		t.Cleanup(free)
		env.waitState(t, ids[1], outbox.Parked)
		env.discard(t, worker, ids[1])
		if s := status(t, rt, revocation); s.State != RevocationFailed || !s.Token.IsZero() {
			t.Fatalf("the state is %+v, want failed with no token", s)
		}
		free()
		env.waitState(t, ids[0], outbox.Done)
		if s := status(t, rt, revocation); s.State != RevocationFailed || !s.Token.IsZero() {
			t.Fatalf("after the first tuple is done the state is %+v, want failed", s)
		}
	})

	t.Run("ST-SUP-RP06-LC18-T01 Status does not change when the outbox purges done tuples", func(t *testing.T) {
		clock := &testClock{t: time.Now()}
		env := newPGClock(t, clock)
		rt := env.runtimeOf(t, &recorder{allow: func(EvaluationRequest) bool { return true }}, nil)
		relay := env.relay(t, &fakeRelationships{})
		worker := env.newWorker(t, relay, func(c *outbox.WorkerConfig) {
			c.Decider = rt
			c.Auditor = fakeAuditor{}
			c.Retention = time.Hour
		})
		run(t, worker)
		revocation, ids := twoTuples(t, env, rt)
		env.waitAll(t, outbox.Done)
		before := status(t, rt, revocation)
		if before.State != RevocationComplete || before.Token.IsZero() {
			t.Fatalf("the state is %+v, want complete", before)
		}
		clock.add(2 * time.Hour)
		for deadline := time.Now().Add(10 * time.Second); env.scalar(t, "SELECT count(*) FROM stego_outbox.messages") != "0"; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the worker did not purge the done messages")
			}
		}
		if after := status(t, rt, revocation); after != before {
			t.Fatalf("after the purge of %v the state is %+v, want %+v", ids, after, before)
		}
		// A person discards the one tuple of another revocation, in a database whose provider refuses the delete.
		other := newPG(t)
		otherRT := other.runtimeOf(t, &recorder{allow: func(EvaluationRequest) bool { return true }}, nil)
		refusing := other.newWorker(t, other.relay(t, &fakeRelationships{failDelete: refused}),
			func(c *outbox.WorkerConfig) { c.Decider = otherRT; c.Auditor = fakeAuditor{} })
		run(t, refusing)
		tx, handle := other.begin(t, otherRT, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		discarded, err := handle.Unrelate(ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		other.commit(t, tx, "t1")
		id := other.ids(t)[0]
		other.waitState(t, id, outbox.Parked)
		other.discard(t, refusing, id)
		if s := status(t, otherRT, discarded); s.State != RevocationFailed {
			t.Fatalf("the discarded revocation is %+v, want failed", s)
		}
	})

	t.Run("ST-SUP-RP06-LC18-T02 A revocation record past its retention answers expired", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		env.startWorker(t, env.relay(t, &fakeRelationships{}))
		revocation, _ := twoTuples(t, env, rt)
		env.waitAll(t, outbox.Done)
		if s := status(t, rt, revocation); s.State != RevocationComplete {
			t.Fatalf("the state is %+v, want complete", s)
		}
		// Status reads no outbox row: the runtime role loses every right on the outbox table.
		if _, err := env.admin.ExecContext(ctx, "REVOKE ALL ON stego_outbox.messages FROM "+env.roles[0]); err != nil {
			t.Fatal(err)
		}
		later := env.runtimeOf(t, &recorder{}, fixedClock{t: time.Now().Add(31 * 24 * time.Hour)})
		if s := status(t, later, revocation); s.State != RevocationExpired {
			t.Fatalf("31 days later the state is %+v, want expired", s)
		}
		if _, err := New(Config{Provider: &recorder{}, DB: env.runtime, Schema: env.schema, Outbox: env.queue,
			Retention: 29 * 24 * time.Hour}); err == nil {
			t.Fatal("a retention of 29 days is accepted")
		}
	})

	t.Run("ST-SUP-RP06-LC21-T01 The runtime role cannot lower the floor or mark a revocation complete", func(t *testing.T) {
		if _, err := Migration("authz", "same_role", "same_role"); err == nil ||
			strings.Count(err.Error(), "same_role") != 2 {
			t.Fatalf("one role for both gave %v, want an error that names both roles", err)
		}
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		revocation, err := handle.Unrelate(ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		id := env.ids(t)[0]
		asRuntime := func(statement string, args ...any) error {
			tx, err := tenant.Begin(ctx, env.runtime, mustTenant(t, "t1"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
				return err
			}
			return tenant.Commit(ctx, tx, mustTenant(t, "t1"))
		}
		denied := func(name string, err error) {
			t.Helper()
			if err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("%s gave %v, want insufficient privilege", name, err)
			}
		}
		denied("the update of the floor", asRuntime("UPDATE authz.authz_floor SET token = 'old'"))
		denied("the insert with counts", asRuntime("INSERT INTO authz.authz_revocations (id, tenant_id, principal, tuple_count, done, token) "+
			"VALUES ($1, 't1', 'user:alice', 1, 1, 'forged')", uuid.New()))
		denied("the update of the done count", asRuntime("UPDATE authz.authz_revocations SET done = 1 WHERE id = $1", revocation.ID()))
		if err := asRuntime("SELECT authz.authz_revocation_state($1, $2, 'done')", id, revocation.ID()); err == nil {
			t.Fatal("the runtime role reported the state done")
		}
		if got := env.scalar(t, "SELECT done::text || '/' || token FROM {s}.authz_revocations WHERE id = $1", revocation.ID()); got != "0/" {
			t.Fatalf("the call for a pending message changed the record to %s", got)
		}
		// The relay's state hook counts and alerts on a change that the function did not count.
		relay, err := NewRelay(RelayConfig{Name: "authz-relay", Relationships: &fakeRelationships{}, DB: env.relayDB,
			Schema: env.schema, Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))})
		if err != nil {
			t.Fatal(err)
		}
		hookTx, err := tenant.Begin(ctx, env.workerDB, mustTenant(t, "t1"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := relay.Hook(ctx, hookTx, outbox.StateChange{ID: id, Group: revocation.ID().String(), Destination: DestinationName,
			Tenant: mustTenant(t, "t1"), State: outbox.Done}); err != nil {
			t.Fatal(err)
		}
		_ = hookTx.Rollback()
		if got := relay.UncountedChanges(); got != 1 {
			t.Fatalf("the hook counted %d uncounted changes, want 1", got)
		}
		env.startWorker(t, env.relay(t, &fakeRelationships{}))
		env.waitState(t, id, outbox.Done)
		if got := env.scalar(t, "SELECT done::text FROM {s}.authz_revocations WHERE id = $1", revocation.ID()); got != "1" {
			t.Fatalf("the hook of a done message counted %s, want 1", got)
		}
	})

	t.Run("ST-SUP-RP06-LC21-T03 Migration refuses overlapping roles", func(t *testing.T) {
		if _, err := Migration("authz", "authz_rt", "authz_worker", "authz_worker"); err == nil ||
			!strings.Contains(err.Error(), `"authz_worker"`) || strings.Count(err.Error(), "authz_worker") != 2 {
			t.Fatalf("a relay role equal to a hook role gave %v, want an error that names the two roles", err)
		}
		if _, err := Migration("authz", "authz_rt", "authz_relay", "authz_rt"); err == nil || strings.Count(err.Error(), "authz_rt") != 2 {
			t.Fatalf("a hook role equal to the runtime role gave %v, want an error that names the two roles", err)
		}
		env := newPG(t)
		// A runtime role that is a member of a hook role, or of stego_outbox_relay, is refused before anything is
		// created.
		for _, group := range []string{env.roles[1], "stego_outbox_relay"} {
			runtime := "authz_in_" + uuid.NewString()[:8]
			if _, err := env.admin.ExecContext(ctx, "CREATE ROLE "+runtime+" NOLOGIN IN ROLE "+group); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = env.admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+runtime) })
			text, err := Migration("refused_"+runtime, runtime, env.roles[2], env.roles[1])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.admin.ExecContext(ctx, "CREATE SCHEMA refused_"+runtime); err != nil {
				t.Fatal(err)
			}
			if _, err := env.admin.ExecContext(ctx, text); err == nil || !strings.Contains(err.Error(), runtime) || !strings.Contains(err.Error(), group) {
				t.Errorf("a runtime role that is a member of %s gave %v, want an error that names the two roles", group, err)
			}
			if got := env.scalar(t, "SELECT pg_catalog.to_regclass('refused_"+runtime+".authz_floor')::text"); got != "" {
				t.Errorf("the refused migration created %s", got)
			}
		}
		member := "authz_member_" + uuid.NewString()[:8]
		if _, err := env.admin.ExecContext(ctx, "CREATE ROLE "+member+" NOLOGIN IN ROLE "+env.roles[2]); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = env.admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+member) })
		migration, err := Migration("refused", member, env.roles[2], env.roles[1])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.admin.ExecContext(ctx, "CREATE SCHEMA refused"); err != nil {
			t.Fatal(err)
		}
		_, err = env.admin.ExecContext(ctx, migration)
		if err == nil || !strings.Contains(err.Error(), member) || !strings.Contains(err.Error(), env.roles[2]) {
			t.Fatalf("a runtime role that is a member of the relay role gave %v, want an error that names the two roles", err)
		}
		if got := env.scalar(t, "SELECT pg_catalog.to_regclass('refused.authz_floor')::text"); got != "" {
			t.Fatalf("the refused migration created %s", got)
		}
	})
	t.Run("ST-SUP-RP06-LC21-T04 Migration runs again on a database that it made, in one transaction", func(t *testing.T) {
		env := newPG(t)
		migration, err := Migration("authz", env.roles[0], env.roles[2], env.roles[1])
		if err != nil {
			t.Fatal(err)
		}
		shape := func() string {
			return env.scalar(t, `SELECT (SELECT count(*) FROM authz.authz_floor)::text || '/' ||
 (SELECT count(*) FROM pg_catalog.pg_policies WHERE schemaname = 'authz')::text || '/' ||
 (SELECT count(*) FROM pg_catalog.pg_trigger g JOIN pg_catalog.pg_class c ON c.oid = g.tgrelid
   JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'authz' AND NOT g.tgisinternal)::text || '/' ||
 (SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
   JOIN pg_catalog.pg_roles r ON r.oid = p.proowner WHERE n.nspname = 'authz' AND r.rolname = 'stego_outbox_relay')::text`)
		}
		before := shape()
		for run := 1; run <= 2; run++ {
			tx, err := env.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, migration); err != nil {
				_ = tx.Rollback()
				t.Fatalf("run %d of the migration on a database that it made: %v", run, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		if after := shape(); after != before || before != "1/3/1/2" {
			t.Errorf("floor rows, policies, triggers and relay functions are %s after two more runs and %s before, want 1/3/1/2 both: one floor row, the two revocation policies and the policy of the test table, the tuple check trigger and the two relay functions", after, before)
		}
	})
}
