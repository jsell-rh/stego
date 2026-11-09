package command

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"stego.dev/runtime/audit"
	"stego.dev/runtime/authz"
	"stego.dev/runtime/idempotency"
	"stego.dev/runtime/internal/pgtest"
	"stego.dev/runtime/internal/seal"
	"stego.dev/runtime/internal/staging"
	"stego.dev/runtime/lease"
	"stego.dev/runtime/outbox"
	"stego.dev/runtime/probe"
	"stego.dev/runtime/tenant"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// The test entities, as the generated code declares them: their Go types, and their tables with the tenant, the
// resource version and the authorization token on each row, with row-level security on the tenant setting.
type (
	order       struct{ ID, Status string }
	reservation struct{ ID, OrderID, SKU string }
	charge      struct {
		ID, CardLast4 string
		Amount        int64
	}
	customer     struct{ ID, Name, Email string }
	organization struct {
		ID          string
		MemberCount int64
	}
	user struct {
		ID, OrgID, Email string
		Admin            bool
		Rank             int64
	}
)

const entityDDL = `
CREATE TABLE orders (tenant_id text NOT NULL, id text NOT NULL, status text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
CREATE TABLE reservations (tenant_id text NOT NULL, id text NOT NULL, order_id text NOT NULL, sku text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
CREATE TABLE charges (tenant_id text NOT NULL, id text NOT NULL, amount bigint NOT NULL, card_last4 text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
CREATE TABLE customers (tenant_id text NOT NULL, id text NOT NULL, name text NOT NULL, email text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
CREATE TABLE organizations (tenant_id text NOT NULL, id text NOT NULL, member_count bigint NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
CREATE TABLE users (tenant_id text NOT NULL, id text NOT NULL, org_id text NOT NULL, email text NOT NULL,
  admin boolean NOT NULL DEFAULT false, rank bigint NOT NULL DEFAULT 0,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, email), FOREIGN KEY (tenant_id, org_id) REFERENCES organizations (tenant_id, id));
CREATE INDEX users_rank ON users (tenant_id, org_id, rank DESC) WHERE admin;
CREATE TABLE user_refs (tenant_id text NOT NULL, id text NOT NULL, user_email text NOT NULL,
  PRIMARY KEY (tenant_id, id), FOREIGN KEY (tenant_id, user_email) REFERENCES users (tenant_id, email));
`

var entityTables = []string{"orders", "reservations", "charges", "customers", "organizations", "users", "user_refs"}

const rolePassword = "command-test"

// env is one database of the local PostgreSQL server of pgtest with the tables of the outbox, authorization, audit,
// idempotency and lease runtimes and the entity tables, a runtime role, an outbox worker role and an authz relay role
// that are not superusers, so row-level security and the grants apply.
type env struct {
	t        *testing.T
	ctx      context.Context
	admin    *sql.DB // the superuser on the test database
	pool     *sql.DB // the main pool, from tenant.Open
	auditDB  *sql.DB // the audit connections, from tenant.Open
	workerDB *sql.DB // the outbox worker role
	dsn      func(role string) string
	roles    [3]string // runtime, worker, relay
	queue    *outbox.Queue
	wqueue   *outbox.Queue
	decision *decision
	authz    *authz.Runtime
	writer   *audit.Writer
	probes   *probe.Recorder
	rt       *Runtime

	statements *statements

	orders        *Entity[order]
	organizations *Entity[organization]
	users         *Entity[user]
	reservationE  *Entity[reservation]
	chargeE       *Entity[charge]
	customerE     *Entity[customer]
	admins        *List[user]
}

// statements records the statements that the handle sends, for the tests that read them.
type statements struct {
	mu   sync.Mutex
	list []string
}

func (s *statements) add(query string, _ []any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, query)
}

func (s *statements) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.list...)
}

func (s *statements) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = nil
}

// envOption changes the fixture before the runtime starts.
type envOption func(*envConfig)

type envConfig struct {
	queueClock outbox.Clock
	mainConns  int
	auditConns int
}

func newEnv(t *testing.T, options ...envOption) *env {
	t.Helper()
	cfg := envConfig{}
	for _, o := range options {
		o(&cfg)
	}
	ctx := context.Background()
	root, err := sql.Open("pgx", pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	tag := hex.EncodeToString(suffix)
	roles := [3]string{"cmd_rt_" + tag, "cmd_worker_" + tag, "cmd_relay_" + tag}
	for _, role := range roles {
		if _, err := root.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+rolePassword+"'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role) })
	}
	database := pgtest.Database(t, "command")
	admin, err := sql.Open("pgx", database)
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
	authzDDL, err := authz.Migration("authz", roles[0], roles[2], roles[1])
	if err != nil {
		t.Fatal(err)
	}
	auditDDL, err := audit.DDL(audit.Config{RuntimeRole: roles[0], AnchorPeriod: time.Hour, RetentionMonths: 12})
	if err != nil {
		t.Fatal(err)
	}
	grants := "GRANT SELECT, INSERT, UPDATE, DELETE ON " + strings.Join(entityTables, ", ") + " TO " + roles[0]
	setup := []string{
		queueDDL,
		"CREATE SCHEMA authz",
		"GRANT USAGE ON SCHEMA authz TO " + roles[0] + ", " + roles[1] + ", " + roles[2],
		authzDDL,
		auditDDL,
		idempotency.Migration,
		"GRANT SELECT, INSERT, UPDATE ON idempotency_keys TO " + roles[0],
		lease.LeasesTable,
		"GRANT SELECT, UPDATE ON leases TO " + roles[0],
		entityDDL,
		grants,
		// The outbox worker runs the consumers in its own transactions, with the runtime role's grants.
		"GRANT " + roles[0] + " TO " + roles[1],
	}
	for _, table := range entityTables {
		setup = append(setup,
			"ALTER TABLE "+table+" ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE "+table+" FORCE ROW LEVEL SECURITY",
			"CREATE POLICY "+table+"_tenant ON "+table+" USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))")
	}
	for _, statement := range setup {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%.80s: %v", statement, err)
		}
	}
	dsn := func(role string) string {
		u, err := url.Parse(database)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, rolePassword)
		return u.String()
	}
	e := &env{t: t, ctx: ctx, admin: admin, dsn: dsn, roles: roles, statements: &statements{}}
	e.pool = e.openPool(dsn(roles[0]))
	e.auditDB = e.openPool(dsn(roles[0]))
	if cfg.mainConns > 0 {
		e.pool.SetMaxOpenConns(cfg.mainConns)
	}
	if cfg.auditConns > 0 {
		e.auditDB.SetMaxOpenConns(cfg.auditConns)
	}
	e.workerDB, err = sql.Open("pgx", dsn(roles[1]))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.workerDB.Close() })
	e.maintainAudit()
	if e.queue, err = outbox.New(e.pool, outbox.Options{Clock: cfg.queueClock}); err != nil {
		t.Fatal(err)
	}
	if e.wqueue, err = outbox.New(e.workerDB, outbox.Options{Clock: cfg.queueClock}); err != nil {
		t.Fatal(err)
	}
	e.decision = &decision{}
	if e.authz, err = authz.New(authz.Config{Provider: e.decision, DB: e.pool, Schema: "authz", Outbox: e.queue}); err != nil {
		t.Fatal(err)
	}
	if e.writer, err = audit.NewWriter(auditOutbox{q: e.queue}, nil); err != nil {
		t.Fatal(err)
	}
	e.probes = probe.NewRecorder(nil)
	e.rt = e.runtime(nil)
	e.entities()
	return e
}

// openPool opens a pool of the runtime role with tenant.Open.
func (e *env) openPool(dsn string) *sql.DB {
	e.t.Helper()
	db, err := tenant.Open(e.ctx, tenant.Config{DSN: dsn})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = db.Close() })
	return db
}

// runtime starts the command runtime of the fixture; change, when given, changes its configuration first.
func (e *env) runtime(change func(*Config)) *Runtime {
	e.t.Helper()
	cfg := Config{DB: e.pool, AuditDB: e.auditDB, Authz: e.authz, Audit: e.writer, Outbox: e.queue, Probes: e.probes,
		Service: "orders", ServiceVersion: "1.0.0", DeclarationVersion: "d1"}
	if change != nil {
		change(&cfg)
	}
	rt, err := New(e.ctx, cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	rt.recorder = e.statements.add
	return rt
}

// maintainAudit makes the audit partitions of this month and the next, as the controller's maintenance does.
func (e *env) maintainAudit() {
	e.t.Helper()
	for range 64 {
		var more bool
		err := func() error {
			tx, err := e.pool.BeginTx(e.ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			rows, err := tx.QueryContext(e.ctx, `SELECT step_left FROM `+audit.MaintainFunction+`()`)
			if err != nil {
				return err
			}
			for rows.Next() {
				if err := rows.Scan(&more); err != nil {
					rows.Close()
					return err
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			e.t.Fatalf("audit_maintain: %v", err)
		}
		if !more {
			return
		}
	}
	e.t.Fatal("audit_maintain did not finish")
}

// auditOutbox is the audit writer's outbox in the tests: it enqueues each sink message on the queue.
type auditOutbox struct{ q *outbox.Queue }

func (o auditOutbox) Enqueue(ctx context.Context, tx audit.Tx, _, destination, key string, payload []byte) error {
	sqlTx, ok := tx.(*sql.Tx)
	if !ok {
		return errors.New("the audit outbox needs a *sql.Tx")
	}
	return o.q.Enqueue(ctx, sqlTx, outbox.Message{ID: uuid.New(), Destination: destination, ResourceKey: key,
		Kind: "audit.event", Payload: payload, Idempotent: true, MustLand: true})
}

// spec builds the repository declaration of a table as the generated code does: cols are the columns after id, in
// the order of fields.
func spec[T any](name, resourceType, table string, cols []string, scan func(Scanner) (T, Meta, error),
	values func(T) []any, state func(T) map[string]any) EntitySpec[T] {
	all := "id, " + strings.Join(cols, ", ") + ", version, authz_token"
	selectRow := "SELECT " + all + " FROM " + table + " WHERE tenant_id = $1 AND id = $2"
	params := make([]string, len(cols)+1)
	sets := make([]string, len(cols))
	for i := range params {
		params[i] = fmt.Sprintf("$%d", i+2)
	}
	for i, col := range cols {
		sets[i] = fmt.Sprintf("%s = $%d", col, i+5)
	}
	return EntitySpec[T]{
		Name: name, ResourceType: resourceType, ReadPermission: "view", ListPermission: "view",
		Select: selectRow, SelectForUpdate: selectRow + " FOR NO KEY UPDATE", SelectForDelete: selectRow + " FOR UPDATE",
		Insert: "INSERT INTO " + table + " (tenant_id, id, " + strings.Join(cols, ", ") + ") VALUES ($1, " +
			strings.Join(params, ", ") + ") RETURNING " + all,
		// $4 is the ID in the values, which an update does not change.
		Update: "UPDATE " + table + " SET " + strings.Join(sets, ", ") + ", version = version + 1 WHERE tenant_id = $1 AND id = $2 AND version = $3 AND $4::text = id RETURNING " + all,
		Delete: "DELETE FROM " + table + " WHERE tenant_id = $1 AND id = $2 AND version = $3",
		Scan:   scan, Values: values, State: state,
		KeyChanged: func(T, T) bool { return false },
		// The test types have no slice, map or pointer, so a copy of the struct is a deep copy.
		Clone:   func(v T) T { return v },
		ValidID: func(id string) bool { return id != "" },
	}
}

func (e *env) entities() {
	t := e.t
	t.Helper()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	var err error
	e.orders, err = NewEntity(spec("Order", "order", "orders", []string{"status"},
		func(s Scanner) (order, Meta, error) {
			var v order
			var m Meta
			err := s.Scan(&v.ID, &v.Status, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v order) []any { return []any{v.ID, v.Status} },
		func(v order) map[string]any { return map[string]any{"id": v.ID, "status": v.Status} }))
	must(err)
	e.reservationE, err = NewEntity(spec("Reservation", "", "reservations", []string{"order_id", "sku"},
		func(s Scanner) (reservation, Meta, error) {
			var v reservation
			var m Meta
			err := s.Scan(&v.ID, &v.OrderID, &v.SKU, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v reservation) []any { return []any{v.ID, v.OrderID, v.SKU} },
		func(v reservation) map[string]any {
			return map[string]any{"id": v.ID, "order_id": v.OrderID, "sku": v.SKU}
		}))
	must(err)
	chargeSpec := spec("Charge", "charge", "charges", []string{"amount", "card_last4"},
		func(s Scanner) (charge, Meta, error) {
			var v charge
			var m Meta
			err := s.Scan(&v.ID, &v.Amount, &v.CardLast4, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v charge) []any { return []any{v.ID, v.Amount, v.CardLast4} },
		func(v charge) map[string]any {
			return map[string]any{"id": v.ID, "amount": v.Amount, "card_last4": v.CardLast4}
		})
	chargeSpec.PII = true
	chargeSpec.Fields = audit.Fields{"card_last4": audit.PII}
	e.chargeE, err = NewEntity(chargeSpec)
	must(err)
	customerSpec := spec("Customer", "customer", "customers", []string{"name", "email"},
		func(s Scanner) (customer, Meta, error) {
			var v customer
			var m Meta
			err := s.Scan(&v.ID, &v.Name, &v.Email, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v customer) []any { return []any{v.ID, v.Name, v.Email} },
		func(v customer) map[string]any { return map[string]any{"id": v.ID, "name": v.Name, "email": v.Email} })
	customerSpec.PII = true
	customerSpec.Fields = audit.Fields{"email": audit.PII}
	e.customerE, err = NewEntity(customerSpec)
	must(err)
	e.organizations, err = NewEntity(spec("Organization", "organization", "organizations", []string{"member_count"},
		func(s Scanner) (organization, Meta, error) {
			var v organization
			var m Meta
			err := s.Scan(&v.ID, &v.MemberCount, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v organization) []any { return []any{v.ID, v.MemberCount} },
		func(v organization) map[string]any { return map[string]any{"id": v.ID, "member_count": v.MemberCount} }))
	must(err)
	userSpec := spec("User", "user", "users", []string{"org_id", "email", "admin", "rank"},
		func(s Scanner) (user, Meta, error) {
			var v user
			var m Meta
			err := s.Scan(&v.ID, &v.OrgID, &v.Email, &v.Admin, &v.Rank, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v user) []any { return []any{v.ID, v.OrgID, v.Email, v.Admin, v.Rank} },
		func(v user) map[string]any {
			return map[string]any{"id": v.ID, "org_id": v.OrgID, "email": v.Email, "admin": v.Admin, "rank": v.Rank}
		})
	userSpec.LockAtMost = 3
	userSpec.KeyChanged = func(before, after user) bool { return before.Email != after.Email || before.ID != after.ID }
	e.users, err = NewEntity(userSpec)
	must(err)
	all := "id, org_id, email, admin, rank, version, authz_token"
	e.admins, err = NewList(e.users, ListSpec{Name: "admins",
		Read: "SELECT " + all + " FROM users WHERE tenant_id = $1 AND org_id = $3 AND admin ORDER BY rank DESC LIMIT $2",
		Lock: "SELECT " + all + " FROM users WHERE tenant_id = $1 AND id = ANY($2) AND org_id = $3 AND admin ORDER BY id FOR NO KEY UPDATE"})
	must(err)
}

// exec runs statements as the superuser on the test database, which row-level security does not limit.
func (e *env) exec(statements ...string) {
	e.t.Helper()
	for _, statement := range statements {
		if _, err := e.admin.ExecContext(e.ctx, statement); err != nil {
			e.t.Fatalf("%s: %v", statement, err)
		}
	}
}

// count counts rows as the superuser.
func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.admin.QueryRowContext(e.ctx, query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// auditRows counts the audit rows of the tenant with the action, or of any action when action is empty.
func (e *env) auditRows(tenantID, action string) int {
	e.t.Helper()
	if action == "" {
		return e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1`, tenantID)
	}
	return e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = $2`, tenantID, action)
}

// outboxRows counts the outbox messages of the tenant for the destination.
func (e *env) outboxRows(tenantID, destination string) int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = $2`, tenantID, destination)
}

// tenantDecision is a decided Provision request, the one source of a tenant that a test may build.
type tenantDecision struct{ seal.DecisionMark }

func mustTenant(t testing.TB, id string) tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(tenant.Requested(tenant.ProvisionAction, id,
		tenantDecision{DecisionMark: seal.NewDecisionMark(id, "provision")}))
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

// person is the principal of a subject such as user:u-7, built as RunConsumer builds a staging principal.
func person(t testing.TB, subject string) authz.Principal {
	t.Helper()
	p, err := authz.StagingPrincipal(staging.NewStaging(), subject)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// decision is a recording fake of the decision port: it allows a request when allow says so, or every request when
// allow is nil, and keeps every request it got.
type decision struct {
	mu    sync.Mutex
	allow func(subject, action, resource string) bool
	calls []evaluation
	// after, when set, runs after each decision, outside the lock.
	after func(e evaluation)
}

// unqualified gives a resource as type:key, from its tenant-qualified ID of ST-SUP-RP06-LC22, and its tenant. An ID
// that is not qualified gives UNQUALIFIED and the raw ID, which no test expects.
func unqualified(r authz.Resource) string {
	tenantPart, keyPart, ok := strings.Cut(r.ID, "/")
	tenantID, err1 := base64.RawURLEncoding.DecodeString(tenantPart)
	key, err2 := base64.RawURLEncoding.DecodeString(keyPart)
	if !ok || err1 != nil || err2 != nil || len(tenantID) == 0 {
		return "UNQUALIFIED " + r.String()
	}
	if want, err := authz.ResourceID(mustTenant(nopTB{}, string(tenantID)), string(key)); err != nil || want != r.ID {
		return "UNQUALIFIED " + r.String()
	}
	return r.Type + ":" + string(key)
}

// nopTB is a testing.TB for helpers that cannot fail here.
type nopTB struct{ testing.TB }

func (nopTB) Helper()           {}
func (nopTB) Fatal(args ...any) { panic(fmt.Sprint(args...)) }

// evaluation is one request that the fake decision port got.
type evaluation struct {
	Subject, Action, Resource string
	Consistency               authz.Consistency
}

func (d *decision) setAllow(allow func(subject, action, resource string) bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allow = allow
}

func (d *decision) decide(request authz.EvaluationRequest) authz.EvaluationResult {
	d.mu.Lock()
	e := evaluation{Subject: request.Subject.String(), Action: request.Action.Name,
		Resource: unqualified(request.Resource), Consistency: request.Consistency}
	d.calls = append(d.calls, e)
	allowed := d.allow == nil || d.allow(e.Subject, e.Action, e.Resource)
	after := d.after
	d.mu.Unlock()
	if after != nil {
		after(e)
	}
	token, _ := authz.NewToken("checked")
	return authz.EvaluationResult{Allowed: allowed, Token: token}
}

func (d *decision) setAfter(after func(e evaluation)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.after = after
}

// deny returns an allow function that denies the resources named, such as user:u9, and allows every other.
func deny(resources ...string) func(subject, action, resource string) bool {
	return func(_, _, resource string) bool {
		for _, r := range resources {
			if r == resource {
				return false
			}
		}
		return true
	}
}

func (d *decision) Evaluate(_ context.Context, request authz.EvaluationRequest) (authz.EvaluationResult, error) {
	return d.decide(request), nil
}

func (d *decision) EvaluateBatch(_ context.Context, request authz.BatchRequest) ([]authz.EvaluationResult, error) {
	results := make([]authz.EvaluationResult, len(request.Evaluations))
	for i, evaluation := range request.Evaluations {
		results[i] = d.decide(evaluation)
	}
	return results, nil
}

func (d *decision) Search(context.Context, authz.SearchRequest) (authz.SearchResult, error) {
	return authz.SearchResult{}, authz.ErrNoSearch
}

func (d *decision) Searches() bool { return false }

// taken returns the requests the port got since the last call, and forgets them.
func (d *decision) taken() []evaluation {
	d.mu.Lock()
	defer d.mu.Unlock()
	calls := d.calls
	d.calls = nil
	return calls
}

// testProbe is the typed probe of the test commands and workers.
type testProbe struct{ p *probe.Probe }

func newTestProbe(p *probe.Probe) testProbe { return testProbe{p: p} }

// command makes the test command name, which writes writes, with change, when given, applied to its declaration.
func (e *env) command(name string, change func(*CommandSpec[testProbe]), writes ...Declared) *Command[testProbe] {
	e.t.Helper()
	spec := CommandSpec[testProbe]{Name: name, Writes: writes, Probe: newTestProbe,
		Destinations: []Destination{{Name: "billing", Idempotent: true}, {Name: "partner-replies", Idempotent: true,
			MustLand: true, Consumed: true}}}
	if change != nil {
		change(&spec)
	}
	cmd, err := NewCommand(spec)
	if err != nil {
		e.t.Fatal(err)
	}
	return cmd
}

// request is a command request of tenant tenantID and principal p with the inbound key key and body body.
func (e *env) request(tenantID string, p authz.Principal, key, body string) Request {
	e.t.Helper()
	fingerprint, err := idempotency.Fingerprint("POST", "/commands", []byte(body))
	if err != nil {
		e.t.Fatal(err)
	}
	return Request{Tenant: mustTenant(e.t, tenantID), Principal: p, Key: key, Fingerprint: fingerprint,
		Body: []byte(body), RequestID: "req-" + key}
}

// noBody decodes a request with nothing in it.
func noBody([]byte) (struct{}, error) { return struct{}{}, nil }

// ok is the response of a fill that succeeds.
var ok = idempotency.Response{Status: 200, Body: []byte(`{"ok":true}`)}

// run calls Run for the command with the fill, as the generated handler does.
func (e *env) run(cmd *Command[testProbe], req Request,
	fill func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error)) (Result, error) {
	return e.runOn(e.rt, cmd, req, fill)
}

// runOn is run on the runtime rt.
func (e *env) runOn(rt *Runtime, cmd *Command[testProbe], req Request,
	fill func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error)) (Result, error) {
	return Run(e.ctx, rt, cmd, req, noBody, func(ctx context.Context, _ struct{}, h *Handle[testProbe]) (idempotency.Response, error) {
		return fill(ctx, h)
	})
}

// bind opens a transaction of the tenant on the main pool and binds a command handle to it, as one attempt of Run
// does, for the tests of the handle alone. The test ends the transaction with end.
func (e *env) bind(tenantID string, p authz.Principal, cmd *Command[testProbe]) (h *Handle[testProbe], tx *sql.Tx, end func()) {
	e.t.Helper()
	return e.bindOn(e.pool, tenantID, p, cmd)
}

// bindOn is bind on the pool db.
func (e *env) bindOn(db *sql.DB, tenantID string, p authz.Principal, cmd *Command[testProbe]) (h *Handle[testProbe], tx *sql.Tx, end func()) {
	e.t.Helper()
	tn := mustTenant(e.t, tenantID)
	tx, err := tenant.Begin(e.ctx, db, tn, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		e.t.Fatal(err)
	}
	s := newState(e.ctx, e.rt, tx, tn, p, modeCommand, cmd.spec.Name, cmd.writes, cmd.destinations)
	s.lockTimeout = DefaultLockTimeout
	s.request = Request{Key: "k-bind", RequestID: "req-bind"}
	s.inbound = commandInbound("k-bind")
	if s.az, err = e.authz.Bind(tx, tn, p, cmd.spec.Relations); err != nil {
		e.t.Fatal(err)
	}
	call := e.probes.Bind(e.ctx, tenantID, p.Subject().String(), cmd.spec.Name)
	h = &Handle[testProbe]{st: s, probe: newTestProbe(call.Probe())}
	var once sync.Once
	end = func() { once.Do(func() { s.close(); call.Rollback(); _ = tx.Rollback() }) }
	e.t.Cleanup(end)
	return h, tx, end
}

func repo[T any](t testing.TB, h *Handle[testProbe], e *Entity[T]) *Repository[T] {
	t.Helper()
	r, err := Repo(h, e)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var u7 = func(t testing.TB) authz.Principal { return person(t, "user:u-7") }

func TestHandleBinding(t *testing.T) {
	e := newEnv(t)
	cmd := e.command("reserve-and-charge", nil, e.reservationE, e.chargeE, e.orders)

	t.Run("ST-SUP-RP10-LC01-T01 The handle reports the tenant and principal it was bound with", func(t *testing.T) {
		h, _, end := e.bind("t1", u7(t), cmd)
		defer end()
		if h.Tenant().ID() != "t1" || h.Principal().Subject().String() != "user:u-7" {
			t.Fatalf("the handle reports %s and %s", h.Tenant(), h.Principal().Subject())
		}
	})

	t.Run("ST-SUP-RP10-LC01-T03 A handle binds only to a READ COMMITTED transaction", func(t *testing.T) {
		e.exec("ALTER ROLE " + e.roles[0] + " SET default_transaction_isolation = 'repeatable read'")
		defer e.exec("ALTER ROLE " + e.roles[0] + " RESET default_transaction_isolation")
		pool := e.openPool(e.dsn(e.roles[0]))
		var level string
		if err := pool.QueryRowContext(e.ctx, `SELECT current_setting('default_transaction_isolation')`).Scan(&level); err != nil || level != "repeatable read" {
			t.Fatalf("the role's default isolation is %q (%v)", level, err)
		}
		rt := e.runtime(func(c *Config) { c.DB = pool })
		if _, err := e.runOn(rt, cmd, e.request("t1", u7(t), "k-lc01-rr", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-lc01-rr", OrderID: "o1", SKU: "s"}, nil)
			return err
		})); err != nil {
			t.Fatalf("Run on a role whose default is REPEATABLE READ gave %v", err)
		}
		tn := mustTenant(t, "t1")
		rr, err := tenant.Begin(e.ctx, e.pool, tn, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		defer rr.Rollback()
		if err := newState(e.ctx, e.rt, rr, tn, u7(t), modeCommand, "x", nil, nil).checkIsolation(e.ctx); !errors.Is(err, ErrInternal) {
			t.Fatalf("a transaction at REPEATABLE READ gave %v, want an internal error", err)
		}
	})

	t.Run("ST-SUP-RP10-LC02-T02 The handle reaches only the entities the command writes", func(t *testing.T) {
		writesReservation := e.command("reserve", nil, e.reservationE)
		h, _, end := e.bind("t1", u7(t), writesReservation)
		defer end()
		e.statements.reset()
		_, err := Repo(h, e.chargeE)
		if err == nil || !strings.Contains(err.Error(), "Charge") {
			t.Fatalf("the repository of Charge gave %v, want an error that names Charge", err)
		}
		if got := e.statements.all(); len(got) != 0 {
			t.Fatalf("the refused repository ran %q", got)
		}
	})

	t.Run("ST-SUP-RP10-LC03-T01 A fill's write lands in the bound transaction and tenant", func(t *testing.T) {
		h, tx, end := e.bind("t1", u7(t), cmd)
		r := repo(t, h, e.reservationE)
		if _, err := r.Create(e.ctx, reservation{ID: "r-lc03", OrderID: "o1", SKU: "sku-1"}, nil); err != nil {
			t.Fatal(err)
		}
		var tenantID string
		if err := tx.QueryRowContext(e.ctx, `SELECT tenant_id FROM reservations WHERE id = 'r-lc03'`).Scan(&tenantID); err != nil || tenantID != "t1" {
			t.Fatalf("inside the transaction the row has the tenant %q (%v), want t1", tenantID, err)
		}
		end()
		if n := e.count(`SELECT count(*) FROM reservations WHERE id = 'r-lc03'`); n != 0 {
			t.Fatalf("after the rollback %d rows stay", n)
		}
	})

	t.Run("ST-SUP-RP10-LC03-T02 A fill cannot read a row of another tenant", func(t *testing.T) {
		e.exec(`INSERT INTO reservations (tenant_id, id, order_id, sku) VALUES ('t2', 'r-t2', 'o1', 'sku-2')`)
		// A pool of one connection with no pooler guard: the connection that comes back after the transaction shows
		// whether the tenant setting was transaction-local.
		single, err := sql.Open("pgx", e.dsn(e.roles[0]))
		if err != nil {
			t.Fatal(err)
		}
		defer single.Close()
		single.SetMaxOpenConns(1)
		h, tx, end := e.bindOn(single, "t1", u7(t), cmd)
		e.statements.reset()
		_, err = repo(t, h, e.reservationE).Get(e.ctx, "r-t2")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("the read of t2's row gave %v, want not found", err)
		}
		got := e.statements.all()
		if len(got) != 1 || !strings.Contains(got[0], "tenant_id = $1") {
			t.Fatalf("the read sent %q, want one statement with the tenant predicate", got)
		}
		var setting string
		if err := tx.QueryRowContext(e.ctx, `SELECT current_setting('app.tenant')`).Scan(&setting); err != nil || setting != "t1" {
			t.Fatalf("the transaction holds the tenant %q (%v)", setting, err)
		}
		end()
		var after sql.NullString
		if err := single.QueryRowContext(e.ctx, `SELECT current_setting('app.tenant', true)`).Scan(&after); err != nil || after.String != "" {
			t.Fatalf("a connection of the pool holds the tenant %q after the transaction (%v)", after.String, err)
		}
	})
}

// seedOrg and seedUser insert rows as the superuser.
func (e *env) seedOrg(tenantID, id string, members int) {
	e.t.Helper()
	e.exec(fmt.Sprintf(`INSERT INTO organizations (tenant_id, id, member_count) VALUES ('%s', '%s', %d)`, tenantID, id, members))
}

func (e *env) seedUser(tenantID, id, org string, admin bool, rank, version int) {
	e.t.Helper()
	e.exec(fmt.Sprintf(`INSERT INTO users (tenant_id, id, org_id, email, admin, rank, version) VALUES ('%s', '%s', '%s', '%s@example.com', %t, %d, %d)`,
		tenantID, id, org, id, admin, rank, version))
}

// session is a transaction of its own as the superuser, such as session A or C of a test.
type session struct {
	t  testing.TB
	tx *sql.Tx
}

func (e *env) session() *session {
	e.t.Helper()
	tx, err := e.admin.BeginTx(e.ctx, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = tx.Rollback() })
	return &session{t: e.t, tx: tx}
}

// lockNoWait tries to lock the row of id in table with FOR NO KEY UPDATE NOWAIT, and gives the SQLSTATE of the
// error, or empty when the lock is taken.
func (c *session) lockNoWait(table, id string) string {
	c.t.Helper()
	if _, err := c.tx.Exec(`SAVEPOINT nowait`); err != nil {
		c.t.Fatal(err)
	}
	_, err := c.tx.Exec(`SELECT 1 FROM `+table+` WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, id)
	if err != nil {
		if _, err := c.tx.Exec(`ROLLBACK TO SAVEPOINT nowait`); err != nil {
			c.t.Fatal(err)
		}
		return sqlState(err)
	}
	if _, err := c.tx.Exec(`RELEASE SAVEPOINT nowait`); err != nil {
		c.t.Fatal(err)
	}
	return ""
}

func (c *session) exec(statement string, args ...any) {
	c.t.Helper()
	if _, err := c.tx.Exec(statement, args...); err != nil {
		c.t.Fatalf("%s: %v", statement, err)
	}
}

func (c *session) commit() {
	c.t.Helper()
	if err := c.tx.Commit(); err != nil {
		c.t.Fatal(err)
	}
}

// waitLockWaits waits until at least n sessions of the runtime role wait for a lock, and fails the test after 5 s.
func (e *env) waitLockWaits(n int) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.lockWaits() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("fewer than %d sessions of the runtime role wait for a lock", n)
}

func (e *env) lockWaits() int {
	return e.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND usename = $1
 AND wait_event_type = 'Lock'`, e.roles[0])
}

// deadlocks reads the deadlock count of the test database, after the statistics of every ended transaction are in.
func (e *env) deadlocks() int {
	e.t.Helper()
	e.exec(`SELECT pg_stat_force_next_flush()`)
	return e.count(`SELECT deadlocks::int FROM pg_stat_database WHERE datname = current_database()`)
}

func TestLockedReads(t *testing.T) {
	e := newEnv(t)

	t.Run("ST-SUP-RP10-LC23-T01 A second locked read waits for the first transaction and then sees its write", func(t *testing.T) {
		e.seedOrg("t1", "o1", 1)
		cmd := e.command("add-member", nil, e.organizations)
		locked, release := make(chan struct{}), make(chan struct{})
		first := make(chan error, 1)
		go func() {
			_, err := e.run(cmd, e.request("t1", u7(t), "k-first", `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, e.organizations)
				if err != nil {
					return idempotency.Response{}, err
				}
				org, err := r.GetForUpdate(ctx, "o1")
				if err != nil {
					return idempotency.Response{}, err
				}
				close(locked)
				<-release
				v := org.Value()
				v.MemberCount++
				if _, err := r.Update(ctx, org, v); err != nil {
					return idempotency.Response{}, err
				}
				return ok, nil
			})
			first <- err
		}()
		<-locked
		var seen Loaded[organization]
		second := make(chan error, 1)
		go func() {
			_, err := e.run(cmd, e.request("t1", u7(t), "k-second", `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, e.organizations)
				if err != nil {
					return idempotency.Response{}, err
				}
				seen, err = r.GetForUpdate(ctx, "o1")
				return ok, err
			})
			second <- err
		}()
		e.waitLockWaits(1)
		select {
		case err := <-second:
			t.Fatalf("the second GetForUpdate returned (%v) while the first transaction held the lock", err)
		default:
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		if versionOf(seen) != 2 || seen.Value().MemberCount != 2 {
			t.Fatalf("the second read saw version %d and %d members, want version 2 and 2 members",
				versionOf(seen), seen.Value().MemberCount)
		}
	})

	t.Run("ST-SUP-RP10-LC23-T02 A child insert does not block a parent lock, and two delete locks after child inserts deadlock as a conflict", func(t *testing.T) {
		e.seedOrg("t1", "o2", 0)
		for _, forDelete := range []bool{false, true} {
			name := "lock-for-update"
			if forDelete {
				name = "lock-for-delete"
			}
			cmd := e.command(name, func(s *CommandSpec[testProbe]) { s.RetryOnConflict = 1 }, e.organizations, e.users)
			before := e.deadlocks()
			var barrier sync.WaitGroup
			barrier.Add(2)
			results := make(chan Result, 2)
			errs := make(chan error, 2)
			for i := range 2 {
				go func() {
					calls := 0
					result, err := e.run(cmd, e.request("t1", u7(t), fmt.Sprintf("k-%s-%d", name, i), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
						calls++
						users, err := Repo(h, e.users)
						if err != nil {
							return idempotency.Response{}, err
						}
						orgs, err := Repo(h, e.organizations)
						if err != nil {
							return idempotency.Response{}, err
						}
						id := fmt.Sprintf("%s-%d-%d", name, i, calls)
						if _, err := users.Create(ctx, user{ID: id, OrgID: "o2", Email: id + "@example.com"}, nil); err != nil {
							return idempotency.Response{}, err
						}
						if calls == 1 {
							barrier.Done()
							barrier.Wait()
						}
						if forDelete {
							_, err = orgs.GetForDelete(ctx, "o2")
						} else {
							_, err = orgs.GetForUpdate(ctx, "o2")
						}
						return ok, err
					})
					results <- result
					errs <- err
				}()
			}
			attempts := 0
			for range 2 {
				if err := <-errs; err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				attempts += (<-results).Attempts
			}
			deadlocks := e.deadlocks() - before
			if forDelete && (deadlocks != 1 || attempts != 3) {
				t.Fatalf("with GetForDelete: %d deadlocks and %d attempts, want one 40P01 and one rerun", deadlocks, attempts)
			}
			if !forDelete && (deadlocks != 0 || attempts != 2) {
				t.Fatalf("with GetForUpdate: %d deadlocks and %d attempts, want none and no rerun", deadlocks, attempts)
			}
		}
		if n := e.count(`SELECT count(*) FROM users WHERE org_id = 'o2'`); n != 4 {
			t.Fatalf("%d users of o2 committed, want 4", n)
		}
	})

	t.Run("ST-SUP-RP10-LC23-T03 A denied row is not locked and not returned", func(t *testing.T) {
		e.seedOrg("t1", "o3", 0)
		e.seedUser("t1", "u9", "o3", false, 0, 1)
		e.seedUser("t1", "u8", "o3", false, 0, 1)
		e.decision.setAllow(deny("user:u9"))
		defer e.decision.setAllow(nil)
		cmd := e.command("lock-user", nil, e.users)
		h, _, end := e.bind("t1", u7(t), cmd)
		defer end()
		r := repo(t, h, e.users)
		got, err := r.GetForUpdate(e.ctx, "u9")
		if !errors.Is(err, ErrNotFound) || got != nil {
			t.Fatalf("GetForUpdate of the denied u9 gave %v, %v; want not found and no data", got, err)
		}
		c := e.session()
		if code := c.lockNoWait("users", "u9"); code != "" {
			t.Fatalf("session C could not lock u9 at once: %s", code)
		}
		// Between the read and the lock of u8, the relation goes and the row's token moves.
		var once sync.Once
		e.decision.setAfter(func(ev evaluation) {
			if ev.Resource != "user:u8" {
				return
			}
			once.Do(func() {
				e.decision.setAllow(deny("user:u9", "user:u8"))
				e.exec(`UPDATE users SET authz_token = 'moved' WHERE id = 'u8'`)
			})
		})
		defer e.decision.setAfter(nil)
		got, err = r.GetForUpdate(e.ctx, "u8")
		if !errors.Is(err, ErrNotFound) || got != nil {
			t.Fatalf("GetForUpdate of u8 after its relation went gave %v, %v; want not found", got, err)
		}
		u8 := 0
		for _, call := range e.decision.taken() {
			if call.Resource == "user:u8" {
				u8++
			}
		}
		if u8 != 2 {
			t.Fatalf("u8 was decided %d times, want 2: before the lock and again after its token moved", u8)
		}
		if code := c.lockNoWait("users", "u8"); code != "" {
			t.Fatalf("session C could not lock u8, which the handle denied, at once: %s", code)
		}
	})
}

func TestVersionedWrites(t *testing.T) {
	e := newEnv(t)
	e.seedOrg("t1", "o1", 0)

	t.Run("ST-SUP-RP10-LC24-T01 A write with a stale version writes nothing and returns the conflict", func(t *testing.T) {
		for _, verb := range []string{"update", "delete"} {
			id := "stale-" + verb
			e.seedUser("t1", id, "o1", false, 0, 3)
			cmd := e.command("rename-"+verb, nil, e.users)
			_, err := e.run(cmd, e.request("t1", u7(t), "k-"+id, `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, e.users)
				if err != nil {
					return idempotency.Response{}, err
				}
				u, err := r.Get(ctx, id)
				if err != nil || versionOf(u) != 3 {
					return idempotency.Response{}, fmt.Errorf("read %s at %v: %v", id, u, err)
				}
				e.exec(`UPDATE users SET rank = 40, version = 4 WHERE id = '` + id + `'`)
				if verb == "update" {
					v := u.Value()
					v.Rank = 7
					_, err = r.Update(ctx, u, v)
				} else {
					err = r.Delete(ctx, u)
				}
				if !errors.Is(err, ErrConflict) {
					return idempotency.Response{}, fmt.Errorf("the %s gave %v, want the conflict", verb, err)
				}
				return idempotency.Response{}, err
			})
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("the %s: Run gave %v, want the conflict", verb, err)
			}
			if n := e.count(`SELECT count(*) FROM users WHERE id = $1 AND version = 4 AND rank = 40`, id); n != 1 {
				t.Fatalf("the %s changed the row", verb)
			}
			if n := e.auditRows("t1", "rename-"+verb); n != 0 {
				t.Fatalf("the %s left %d audit rows", verb, n)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC24-T02 Two writes of one row in one transaction do not conflict", func(t *testing.T) {
		e.seedUser("t1", "twice", "o1", false, 0, 1)
		cmd := e.command("rank-twice", nil, e.users)
		_, err := e.run(cmd, e.request("t1", u7(t), "k-twice", `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, e.users)
			if err != nil {
				return idempotency.Response{}, err
			}
			u, err := r.Get(ctx, "twice")
			if err != nil {
				return idempotency.Response{}, err
			}
			v := u.Value()
			v.Rank = 1
			if u, err = r.Update(ctx, u, v); err != nil {
				return idempotency.Response{}, err
			}
			v.Rank = 2
			if _, err = r.Update(ctx, u, v); err != nil {
				return idempotency.Response{}, err
			}
			return ok, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if n := e.count(`SELECT count(*) FROM users WHERE id = 'twice' AND version = 3 AND rank = 2`); n != 1 {
			t.Fatal("the row did not move two versions")
		}
		if n := e.auditRows("t1", "rank-twice"); n != 2 {
			t.Fatalf("%d audit rows, want 2", n)
		}
	})

	t.Run("ST-SUP-RP10-LC24-T04 An update of a unique column needs the key-change lock", func(t *testing.T) {
		e.seedUser("t1", "keyed", "o1", false, 0, 1)
		for _, read := range []string{"GetForUpdate", "Get", "GetForDelete"} {
			cmd := e.command("change-email-"+strings.ToLower(read), nil, e.users)
			referenced := make(chan error, 1)
			_, err := e.run(cmd, e.request("t1", u7(t), "k-email-"+read, `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, e.users)
				if err != nil {
					return idempotency.Response{}, err
				}
				var u Loaded[user]
				switch read {
				case "GetForUpdate":
					u, err = r.GetForUpdate(ctx, "keyed")
				case "Get":
					u, err = r.Get(ctx, "keyed")
				default:
					u, err = r.GetForDelete(ctx, "keyed")
					// Another transaction inserts a row whose foreign key references the user by email.
					go func() {
						_, err := e.admin.ExecContext(e.ctx, `INSERT INTO user_refs (tenant_id, id, user_email) VALUES ('t1', 'ref-1', 'keyed@example.com')`)
						referenced <- err
					}()
					waitAdminLockWait(t, e)
				}
				if err != nil {
					return idempotency.Response{}, err
				}
				v := u.Value()
				v.Email = "new-" + read + "@example.com"
				if _, err := r.Update(ctx, u, v); err != nil {
					return idempotency.Response{}, err
				}
				return ok, nil
			})
			if read != "GetForDelete" {
				if !errors.Is(err, ErrInternal) || errors.Is(err, ErrConflict) {
					t.Fatalf("a key change from %s gave %v, want an internal error", read, err)
				}
				if n := e.count(`SELECT count(*) FROM users WHERE id = 'keyed' AND email = 'keyed@example.com' AND version = 1`); n != 1 {
					t.Fatalf("a key change from %s wrote the row", read)
				}
				continue
			}
			if err != nil {
				t.Fatalf("a key change from GetForDelete: %v", err)
			}
			if err := <-referenced; err == nil {
				t.Fatal("the insert that referenced the old email committed after the email changed")
			}
		}
		// A delete takes the lock of a key change: a value that GetForUpdate locked is refused.
		e.seedUser("t1", "deleted", "o1", false, 0, 1)
		_, err := e.run(e.command("delete-user", nil, e.users), e.request("t1", u7(t), "k-delete-locked", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, e.users)
			u, err := r.GetForUpdate(ctx, "deleted")
			if err != nil {
				return err
			}
			return r.Delete(ctx, u)
		}))
		if !errors.Is(err, ErrInternal) || e.count(`SELECT count(*) FROM users WHERE id = 'deleted'`) != 1 {
			t.Fatalf("a delete of a value that GetForUpdate locked gave %v", err)
		}
	})
}

// waitAdminLockWait waits until a session of the superuser waits for a lock.
func waitAdminLockWait(t *testing.T, e *env) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND usename = current_user
 AND wait_event_type = 'Lock'`) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the referencing insert did not wait for the handle's lock")
}

func ids(values []Loaded[user]) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = v.ID()
	}
	return out
}

func TestLockedLists(t *testing.T) {
	e := newEnv(t)
	cmd := e.command("check-admins", nil, e.users)

	t.Run("ST-SUP-RP10-LC27-T01 A locked list locks its rows in primary key order", func(t *testing.T) {
		e.seedOrg("t1", "o1", 3)
		for i, id := range []string{"u1", "u2", "u3"} {
			e.seedUser("t1", id, "o1", true, i+1, 1)
		}
		e.exec(`ANALYZE users`)
		a := e.session()
		a.exec(`SELECT 1 FROM users WHERE id = 'u3' FOR NO KEY UPDATE`)
		h, _, end := e.bind("t1", u7(t), cmd)
		defer end()
		r := repo(t, h, e.users)
		var got []Loaded[user]
		listed := make(chan error, 1)
		go func() {
			var err error
			got, err = r.ListForUpdate(e.ctx, e.admins, "o1")
			listed <- err
		}()
		e.waitLockWaits(1)
		c := e.session()
		for _, id := range []string{"u1", "u2"} {
			if code := c.lockNoWait("users", id); code != "55P03" {
				t.Fatalf("session C locked %s with %q while B waited on u3; want 55P03", id, code)
			}
		}
		// The plan of B's locking statement: LockRows on top, over its input in primary key order.
		var plan []map[string]any
		var raw []byte
		explain, err := tenant.Begin(e.ctx, e.pool, mustTenant(t, "t1"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := explain.QueryRowContext(e.ctx, `EXPLAIN (FORMAT JSON) `+e.admins.spec.Lock, "t1", []string{"u1", "u2", "u3"}, "o1").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		_ = explain.Rollback()
		if err := jsonUnmarshal(raw, &plan); err != nil {
			t.Fatal(err)
		}
		top := plan[0]["Plan"].(map[string]any)
		child := top["Plans"].([]any)[0].(map[string]any)
		ordered := child["Node Type"] == "Sort" && (fmt.Sprint(child["Sort Key"]) == "[id]" || fmt.Sprint(child["Sort Key"]) == "[users.id]") ||
			strings.HasPrefix(fmt.Sprint(child["Node Type"]), "Index") && child["Index Name"] == "users_pkey"
		if top["Node Type"] != "LockRows" || !ordered {
			t.Fatalf("the plan is %s over %v, want LockRows over input in primary key order", top["Node Type"], child)
		}
		c.commit()
		a.commit()
		if err := <-listed; err != nil {
			t.Fatal(err)
		}
		if strings.Join(ids(got), ",") != "u1,u2,u3" {
			t.Fatalf("B returned %v, want u1, u2 and u3 in that order", ids(got))
		}
	})

	t.Run("ST-SUP-RP10-LC27-T02 A locked list of more rows than its bound locks nothing and fails", func(t *testing.T) {
		e.seedOrg("t1", "o2", 4)
		for i, id := range []string{"v1", "v2", "v3"} {
			e.seedUser("t1", id, "o2", true, i+1, 1)
		}
		h, _, end := e.bind("t1", u7(t), cmd)
		got, err := repo(t, h, e.users).ListForUpdate(e.ctx, e.admins, "o2")
		end()
		if err != nil || len(got) != 3 {
			t.Fatalf("the list of three admins gave %v, %v", ids(got), err)
		}
		e.seedUser("t1", "v4", "o2", true, 4, 1)
		h, _, end = e.bind("t1", u7(t), cmd)
		defer end()
		got, err = repo(t, h, e.users).ListForUpdate(e.ctx, e.admins, "o2")
		if got != nil || !errors.Is(err, ErrInternal) || errors.Is(err, ErrConflict) {
			t.Fatalf("the list of four admins gave %v, %v; want no row and an internal error", ids(got), err)
		}
		c := e.session()
		for _, id := range []string{"v1", "v2", "v3", "v4"} {
			if code := c.lockNoWait("users", id); code != "" {
				t.Fatalf("session C could not lock %s at once: %s", id, code)
			}
		}
		if _, err := NewList(e.orders, ListSpec{Name: "open", Read: "SELECT 1 WHERE $1 = $1", Lock: "SELECT 1 WHERE $1 = $1 ORDER BY 1 FOR NO KEY UPDATE"}); err == nil {
			t.Fatal("a locked list of Order, which declares no Lock at most, was accepted")
		}
		unlocked, err := NewList(e.users, ListSpec{Name: "all", Read: e.admins.spec.Read})
		if err != nil {
			t.Fatal(err)
		}
		h2, _, end2 := e.bind("t1", u7(t), cmd)
		defer end2()
		if _, err := repo(t, h2, e.users).ListForUpdate(e.ctx, unlocked, "o2"); err == nil {
			t.Fatal("ListForUpdate ran a list with no Lock statement")
		}
	})

	t.Run("ST-SUP-RP10-LC27-T03 A locked list leaves out and does not lock a denied row", func(t *testing.T) {
		e.seedOrg("t1", "o3", 3)
		for i, id := range []string{"w1", "w2", "w3"} {
			e.seedUser("t1", id, "o3", true, i+1, 1)
		}
		e.decision.setAllow(deny("user:w3"))
		defer e.decision.setAllow(nil)
		h, _, end := e.bind("t1", u7(t), cmd)
		got, err := repo(t, h, e.users).ListForUpdate(e.ctx, e.admins, "o3")
		if err != nil || strings.Join(ids(got), ",") != "w1,w2" {
			t.Fatalf("the list gave %v, %v; want w1 and w2", ids(got), err)
		}
		c := e.session()
		if code := c.lockNoWait("users", "w3"); code != "" {
			t.Fatalf("session C could not lock the denied w3 at once: %s", code)
		}
		c.commit()
		end()
		// Between the first read and the lock of a second list, w2's relation goes and its token moves.
		var once sync.Once
		e.decision.setAfter(func(ev evaluation) {
			if ev.Resource != "user:w2" {
				return
			}
			once.Do(func() {
				e.decision.setAllow(deny("user:w3", "user:w2"))
				e.exec(`UPDATE users SET authz_token = 'moved' WHERE id = 'w2'`)
			})
		})
		defer e.decision.setAfter(nil)
		e.decision.taken()
		h, _, end = e.bind("t1", u7(t), cmd)
		defer end()
		got, err = repo(t, h, e.users).ListForUpdate(e.ctx, e.admins, "o3")
		if err != nil || strings.Join(ids(got), ",") != "w1" {
			t.Fatalf("the second list gave %v, %v; want only w1", ids(got), err)
		}
		w2 := 0
		for _, call := range e.decision.taken() {
			if call.Resource == "user:w2" {
				w2++
			}
		}
		if w2 != 2 {
			t.Fatalf("w2 was decided %d times, want 2", w2)
		}
		c = e.session()
		if code := c.lockNoWait("users", "w2"); code != "" {
			t.Fatalf("session C could not lock w2, which the list left out, at once: %s", code)
		}
	})
}

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

// versionOf is the resource version that a loaded value carries.
func versionOf[T any](l Loaded[T]) int64 { return viewOf(l).meta.Version }
