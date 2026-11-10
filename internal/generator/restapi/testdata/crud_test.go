// Package check runs the generated CRUD handlers of the test service over HTTP on the command and the query runtime
// (ST-SUP-RC20-OC37), against a local PostgreSQL server (STEGO_TEST_POSTGRES_DSN) and a local SpiceDB container
// (STEGO_TEST_SPICEDB), with the runtime library wired as the service wires it.
package check

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/query"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	"example.com/crud/out/api"
	"example.com/crud/out/slots"
)

// tablesSQL is the migration 001_initial of the PostgreSQL adapter for the declaration (ST-SUP-RC01-OC41).
//
//go:embed tables.sql
var tablesSQL string

const (
	issuer   = "https://issuer.authn.test/realms/r1"
	audience = "crud"
	password = "crud-check"
	schema   = `definition user {}

definition order {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}

definition customer {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}

definition todo {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}
`
)

var tables = []string{"orders", "customers", "todos"}

// env is the running service: its database with the runtime role, its runtimes and its HTTP server.
type env struct {
	t        *testing.T
	ctx      context.Context
	admin    *sql.DB
	server   *httptest.Server
	verifier *authn.Verifier
	key      *authntest.Key
	spice    *grpc.ClientConn
	// orderGate and customerGate are the decisions of the gate fills of orders.before_create and
	// customers.validate: nil, or a nil result, accepts.
	mu           sync.Mutex
	orderGate    func(*slots.BeforeCreateRequest) *slots.SlotResult
	customerGate func(*slots.ValidateRequest) *slots.SlotResult
	// orderAfter is the fill of orders.after_create: it records each call, and fails for the sku after-fails.
	afterCalls []string
}

var (
	shared  *env
	setupMu sync.Mutex
)

// service starts the service once for the test binary.
func service(t *testing.T) *env {
	t.Helper()
	setupMu.Lock()
	defer setupMu.Unlock()
	if shared != nil {
		shared.t = t
		return shared
	}
	dsn, endpoint := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB")
	if dsn == "" || endpoint == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN and STEGO_TEST_SPICEDB: a missing result is not a pass")
	}
	e := &env{t: t, ctx: context.Background()}
	e.start(dsn, endpoint, os.Getenv("STEGO_TEST_SPICEDB_KEY"))
	shared = e
	return e
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// start makes a database of its own with the runtime, outbox worker and relay roles, the runtime tables and the
// entity tables with row-level security, writes the schema to SpiceDB, and serves the generated handlers behind the
// verifier on the command and the query runtime.
func (e *env) start(dsn, endpoint, spiceKey string) {
	t := e.t
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	tag := hex.EncodeToString(suffix)
	name := "crud_" + tag
	roles := [3]string{"crud_rt_" + tag, "crud_worker_" + tag, "crud_relay_" + tag}
	_, err = root.ExecContext(e.ctx, "CREATE DATABASE "+name)
	must(t, err)
	for _, role := range roles {
		_, err := root.ExecContext(e.ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"'")
		must(t, err)
	}
	u, err := url.Parse(dsn)
	must(t, err)
	u.Path = "/" + name
	e.admin, err = sql.Open("pgx", u.String())
	must(t, err)
	queueDDL, err := outbox.Migration(roles[0], roles[1])
	must(t, err)
	authzDDL, err := authz.Migration("authz", roles[0], roles[2], roles[1])
	must(t, err)
	auditDDL, err := audit.DDL(audit.Config{RuntimeRole: roles[0], AnchorPeriod: time.Hour, RetentionMonths: 12})
	must(t, err)
	setup := []string{
		queueDDL,
		"CREATE SCHEMA authz",
		"GRANT USAGE ON SCHEMA authz TO " + roles[0] + ", " + roles[1] + ", " + roles[2],
		authzDDL,
		auditDDL,
		idempotency.Migration,
		"GRANT SELECT, INSERT, UPDATE ON idempotency_keys TO " + roles[0],
		tablesSQL,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON " + strings.Join(tables, ", ") + " TO " + roles[0],
		"GRANT " + roles[0] + " TO " + roles[1],
	}
	for _, table := range tables {
		setup = append(setup,
			"ALTER TABLE "+table+" ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE "+table+" FORCE ROW LEVEL SECURITY",
			"CREATE POLICY "+table+"_tenant ON "+table+" USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))")
	}
	for _, statement := range setup {
		if _, err := e.admin.ExecContext(e.ctx, statement); err != nil {
			t.Fatalf("%.80s: %v", statement, err)
		}
	}
	u.User = url.UserPassword(roles[0], password)
	runtimeDSN := u.String()
	pool, err := tenant.Open(e.ctx, tenant.Config{InsecureLoopback: true, DSN: runtimeDSN})
	must(t, err)
	auditDB, err := tenant.Open(e.ctx, tenant.Config{InsecureLoopback: true, DSN: runtimeDSN})
	must(t, err)
	maintainAudit(t, e.ctx, pool)
	queue, err := outbox.New(pool, outbox.Options{})
	must(t, err)
	writer, err := audit.NewWriter(auditOutbox{q: queue}, nil)
	must(t, err)

	if spiceKey == "" {
		spiceKey = "crud-check"
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(spiceKey)))
	must(t, err)
	e.spice = conn
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	for {
		_, err = v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: schema})
		if err == nil || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	must(t, err)
	provider, err := authz.NewSpiceDB(conn)
	must(t, err)
	az, err := authz.New(authz.Config{Provider: provider, DB: pool, Schema: "authz", Outbox: queue})
	must(t, err)

	settings := audit.Settings{Level: audit.LevelChange}
	probes := probe.NewRecorder(nil)
	commands, err := command.New(e.ctx, command.Config{DB: pool, AuditDB: auditDB, Outbox: queue, Authz: az, Audit: writer,
		AuditSettings: settings, Service: "crud", ServiceVersion: "1.0.0", DeclarationVersion: "d1",
		Entities: api.Entities(), Probes: probes})
	must(t, err)
	cursorKey := make([]byte, query.MinCursorKey)
	_, _ = rand.Read(cursorKey)
	queries, err := query.New(query.Config{DB: pool, AuditDB: auditDB, Audit: writer, AuditSettings: settings, Authz: az,
		Service: "crud", CursorKey: cursorKey})
	must(t, err)
	rt := &api.Runtime{Commands: commands, Queries: queries, Probes: probes}

	e.key = authntest.NewKey(t, "k1")
	keys, err := authn.ParseKeys(authntest.Document(e.key.JWK()))
	must(t, err)
	e.verifier, err = authn.New(authn.Settings{Issuer: issuer, Audience: audience, TenantClaim: "tenant", ProblemCode: "CRUD", Keys: keys})
	must(t, err)

	orders := api.NewOrdersHandler(rt, slots.NewBeforeCreateChain(true, orderGate{e}), slots.NewAfterCreateGate(orderAfter{e}))
	customers := api.NewCustomersHandler(rt, slots.NewValidateGate(customerGate{e}))
	todos := api.NewTodosHandler(rt)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/orders", orders.Create)
	mux.HandleFunc("GET /api/v1/orders/{id}", orders.Read)
	mux.HandleFunc("GET /api/v1/orders", orders.List)
	mux.HandleFunc("PATCH /api/v1/orders/{id}", orders.Patch)
	mux.HandleFunc("DELETE /api/v1/orders/{id}", orders.Delete)
	mux.HandleFunc("POST /api/v1/customers", customers.Create)
	mux.HandleFunc("GET /api/v1/customers/{id}", customers.Read)
	mux.HandleFunc("GET /api/v1/customers", customers.List)
	mux.HandleFunc("PUT /api/v1/customers/{id}", customers.Update)
	mux.HandleFunc("DELETE /api/v1/customers/{id}", customers.Delete)
	mux.HandleFunc("POST /api/v1/todos", todos.Create)
	mux.HandleFunc("GET /api/v1/todos/{id}", todos.Read)
	mux.HandleFunc("GET /api/v1/todos", todos.List)
	mux.HandleFunc("PATCH /api/v1/todos/{id}", todos.Patch)
	mux.HandleFunc("PUT /api/v1/todos/{id}", todos.Update)
	mux.HandleFunc("DELETE /api/v1/todos/{id}", todos.Delete)
	e.server = httptest.NewServer(e.verifier.Middleware(mux))
}

// orderGate is the fill of the short-circuit chain of orders.before_create.
type orderGate struct{ e *env }

func (g orderGate) Evaluate(_ context.Context, req *slots.BeforeCreateRequest) (*slots.SlotResult, error) {
	g.e.mu.Lock()
	decide := g.e.orderGate
	g.e.mu.Unlock()
	if decide != nil {
		if result := decide(req); result != nil {
			return result, nil
		}
	}
	return &slots.SlotResult{Ok: true}, nil
}

// orderAfter is the fill of orders.after_create.
type orderAfter struct{ e *env }

func (g orderAfter) Evaluate(_ context.Context, req *slots.AfterCreateRequest) (*slots.SlotResult, error) {
	g.e.mu.Lock()
	defer g.e.mu.Unlock()
	g.e.afterCalls = append(g.e.afterCalls, req.PersistedFields["sku"])
	if req.PersistedFields["sku"] == "after-fails" {
		return nil, errors.New("after slot failed: secret internal text")
	}
	return &slots.SlotResult{Ok: true}, nil
}

// afterCallsOf counts the calls of orders.after_create for the sku.
func (e *env) afterCallsOf(sku string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, s := range e.afterCalls {
		if s == sku {
			n++
		}
	}
	return n
}

// customerGate is the gate fill of customers.validate.
type customerGate struct{ e *env }

func (g customerGate) Evaluate(_ context.Context, req *slots.ValidateRequest) (*slots.SlotResult, error) {
	g.e.mu.Lock()
	decide := g.e.customerGate
	g.e.mu.Unlock()
	if decide != nil {
		if result := decide(req); result != nil {
			return result, nil
		}
	}
	return &slots.SlotResult{Ok: true}, nil
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// auditOutbox is the audit writer's outbox: it enqueues each sink message on the queue.
type auditOutbox struct{ q *outbox.Queue }

func (o auditOutbox) Enqueue(ctx context.Context, tx audit.Tx, _, destination, key string, payload []byte) error {
	sqlTx, ok := tx.(*sql.Tx)
	if !ok {
		return errors.New("the audit outbox needs a *sql.Tx")
	}
	return o.q.Enqueue(ctx, sqlTx, outbox.Message{ID: uuid.New(), Destination: destination, ResourceKey: key,
		Kind: "audit.event", Payload: payload, Idempotent: true, MustLand: true})
}

// maintainAudit makes the audit partitions of this month and the next, as the controller's maintenance does.
func maintainAudit(t *testing.T, ctx context.Context, pool *sql.DB) {
	t.Helper()
	for range 64 {
		var more bool
		err := func() error {
			tx, err := pool.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := tx.QueryRowContext(ctx, `SELECT step_left FROM `+audit.MaintainFunction+`()`).Scan(&more); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			t.Fatalf("audit_maintain: %v", err)
		}
		if !more {
			return
		}
	}
	t.Fatal("audit_maintain did not finish")
}

// caller is one verified caller of a tenant, with a token.
type caller struct {
	tenant    tenant.Tenant
	principal authz.Principal
	token     string
}

// as mints a token for subject in tenantID and verifies it.
func (e *env) as(subject, tenantID string) caller {
	t := e.t
	t.Helper()
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := authntest.Mint(t, authntest.Header("k1"), claims, e.key)
	id, err := e.verifier.Verify(e.ctx, token)
	must(t, err)
	c := caller{token: token}
	c.tenant, err = tenant.New(tenant.Claim(id))
	must(t, err)
	c.principal, err = authz.Person("user", id)
	must(t, err)
	return c
}

// newTenant gives a tenant ID that no other check uses.
func newTenant() string { return "t-" + uuid.NewString()[:8] }

// own writes the owner relation of the caller on a resource of the caller's tenant in SpiceDB.
func (e *env) own(c caller, resourceType, id string) { e.relate(c, "owner", resourceType, id) }

// relate writes the relation of the caller on a resource of the caller's tenant in SpiceDB.
func (e *env) relate(c caller, relation, resourceType, id string) {
	t := e.t
	t.Helper()
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(t, err)
	_, err = v1.NewPermissionsServiceClient(e.spice).WriteRelationships(e.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.principal.Subject().ID}}}}}})
	must(t, err)
}

// reply is one HTTP answer.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// object gives the JSON object of the body.
func (r reply) object(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("the body %s is not a JSON object: %v", r.body, err)
	}
	return v
}

// member gives the string member name of a JSON object body.
func (r reply) member(t *testing.T, name string) string {
	t.Helper()
	s, _ := r.object(t)[name].(string)
	return s
}

// send sends one request with the caller's token and, when key is not empty, the idempotency key.
func (e *env) send(c caller, method, path, key, body string) reply {
	t := e.t
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, e.server.URL+path, reader)
	must(t, err)
	request.Header.Set("Authorization", "Bearer "+c.token)
	if key != "" {
		request.Header.Set("Idempotency-Key", `"`+key+`"`)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	must(t, err)
	return reply{status: response.StatusCode, header: response.Header, body: data}
}

// create creates one resource of the collection with a new key and gives its ID; the caller owns it.
func (e *env) create(c caller, collection, resourceType, body string) string {
	t := e.t
	t.Helper()
	created := e.send(c, http.MethodPost, "/api/v1/"+collection, "k-"+uuid.NewString(), body)
	if created.status != http.StatusCreated {
		t.Fatalf("the create of %s answered %d %s, want 201", collection, created.status, created.body)
	}
	id := created.member(t, "id")
	e.own(c, resourceType, id)
	return id
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.admin.QueryRowContext(e.ctx, query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestCrud(t *testing.T) {
	t.Run("ST-SUP-RC20-OC37-T01 A collection create runs as a command", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		key := "create-" + uuid.NewString()
		created := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"s-1","qty":2}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		if n := e.count(`SELECT count(*) FROM orders WHERE id = $1::uuid AND tenant_id = $2 AND sku = 's-1' AND qty = 2`, id, a.tenant.ID()); n != 1 {
			t.Errorf("%d order rows with the caller's tenant, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE id = $1::uuid`, id); n != 1 {
			t.Errorf("%d order rows of the ID in any tenant, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'create' AND resource_type = 'order' AND resource_id = $2 AND outcome = 'ok' AND idempotency_key = $3`, a.tenant.ID(), id, key); n != 1 {
			t.Errorf("%d ok audit rows of the create, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1 AND principal_id = $2 AND key = $3 AND status = 'completed' AND response_code = 201`, a.tenant.ID(), a.principal.Subject().String(), key); n != 1 {
			t.Errorf("%d completed key rows, want 1", n)
		}
		// The order row, its audit row and its key row are of one transaction: they have one xmin.
		if n := e.count(`SELECT count(DISTINCT x) FROM (SELECT xmin::text AS x FROM orders WHERE id = $1::uuid UNION ALL SELECT xmin::text FROM audit_event WHERE resource_id = $2 AND action = 'create' UNION ALL SELECT xmin::text FROM idempotency_keys WHERE key = $3) s`, id, id, key); n != 1 {
			t.Errorf("the order row, its audit row and its key row are of %d transactions, want 1", n)
		}
		replay := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"s-1","qty":2}`)
		if replay.status != http.StatusCreated || string(replay.body) != string(created.body) || replay.header.Get(idempotency.ReplayedHeader) != "true" {
			t.Errorf("the replay answered %d %s (replayed %q), want the stored 201 %s", replay.status, replay.body, replay.header.Get(idempotency.ReplayedHeader), created.body)
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE tenant_id = $1`, a.tenant.ID()); n != 1 {
			t.Errorf("%d order rows after the replay, want 1", n)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T02 A refusing before slot stops the command before any write", func(t *testing.T) {
		e := service(t)
		e.mu.Lock()
		e.orderGate = func(req *slots.BeforeCreateRequest) *slots.SlotResult {
			if req.Input.Fields["sku"] == "refused" {
				return &slots.SlotResult{Ok: false, StatusCode: http.StatusUnprocessableEntity, ErrorMessage: "the sku is refused"}
			}
			return nil
		}
		e.mu.Unlock()
		defer func() { e.mu.Lock(); e.orderGate = nil; e.mu.Unlock() }()
		a := e.as("u-1", newTenant())
		key := "refused-" + uuid.NewString()
		refused := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"refused","qty":1}`)
		if refused.status != http.StatusUnprocessableEntity || !strings.HasPrefix(refused.header.Get("Content-Type"), "application/problem+json") {
			t.Fatalf("the refused create answered %d %q %s, want the slot's 422 as a problem", refused.status, refused.header.Get("Content-Type"), refused.body)
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE tenant_id = $1`, a.tenant.ID()); n != 0 {
			t.Errorf("%d order rows after the refusal, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1 AND key = $2`, a.tenant.ID(), key); n != 0 {
			t.Errorf("%d key rows after the refusal, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1`, a.tenant.ID()); n != 1 {
			t.Errorf("%d audit rows of the tenant, want only the denied row", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'create' AND outcome = 'denied' AND idempotency_key = $2`, a.tenant.ID(), key); n != 1 {
			t.Errorf("%d denied audit rows of the create, want 1", n)
		}
		accepted := e.send(a, http.MethodPost, "/api/v1/orders", "accepted-"+uuid.NewString(), `{"sku":"accepted","qty":1}`)
		if accepted.status != http.StatusCreated {
			t.Errorf("a create that the slot accepts answered %d %s, want 201", accepted.status, accepted.body)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T03 A write that the caller may not make, or of a missing row, is 403 and changes nothing", func(t *testing.T) {
		e := service(t)
		tenantID := newTenant()
		owner, other := e.as("owner", tenantID), e.as("other", tenantID)
		order := e.create(owner, "orders", "order", `{"sku":"own","qty":1}`)
		customer := e.create(owner, "customers", "customer", `{"name":"Eve","api_token":"tok","plan":"free"}`)
		todo := e.create(owner, "todos", "todo", `{"title":"keep"}`)
		// The other caller may view each row but not edit it, so only the write's own permission refuses the write.
		e.relate(other, "viewer", "order", order)
		e.relate(other, "viewer", "customer", customer)
		e.relate(other, "viewer", "todo", todo)
		writes := []struct{ method, path, key, body string }{
			{http.MethodPatch, "/api/v1/orders/" + order, "k-" + uuid.NewString(), `{"qty":9}`},
			{http.MethodDelete, "/api/v1/orders/" + order, "", ""},
			{http.MethodPut, "/api/v1/customers/" + customer, "", `{"name":"Eve","api_token":"tok","plan":"pro"}`},
			{http.MethodDelete, "/api/v1/customers/" + customer, "", ""},
			{http.MethodPatch, "/api/v1/todos/" + todo, "k-" + uuid.NewString(), `{"title":"changed"}`},
			{http.MethodPut, "/api/v1/todos/" + todo, "", `{"title":"changed"}`},
			{http.MethodDelete, "/api/v1/todos/" + todo, "", ""},
		}
		for _, w := range writes {
			if r := e.send(other, w.method, w.path, w.key, w.body); r.status != http.StatusForbidden {
				t.Errorf("%s %s as a caller who may not edit answered %d %s, want 403", w.method, w.path, r.status, r.body)
			}
		}
		for _, w := range writes {
			missing := w.path[:strings.LastIndex(w.path, "/")+1] + uuid.NewString()
			if r := e.send(owner, w.method, missing, w.key+"-m", w.body); r.status != http.StatusForbidden {
				t.Errorf("%s of a missing row answered %d %s, want 403", w.method, r.status, r.body)
			}
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE id = $1::uuid AND qty = 1`, order); n != 1 {
			t.Errorf("the order changed: %d rows as created, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM customers WHERE id = $1::uuid AND plan = 'free'`, customer); n != 1 {
			t.Errorf("the customer changed: %d rows as created, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM todos WHERE id = $1::uuid AND title = 'keep'`, todo); n != 1 {
			t.Errorf("the todo changed: %d rows as created, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND outcome = 'ok' AND action IN ('update', 'patch', 'delete')`, tenantID); n != 0 {
			t.Errorf("%d ok audit rows of a refused write, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND actor_id = $2 AND outcome = 'denied'`, tenantID, other.principal.Subject().String()); n != 7 {
			t.Errorf("%d denied audit rows of the refused writes, want 7", n)
		}
		// A deleted row whose owner tuple stays is allowed by the decision and then not found: it is 403 too, so the
		// answer does not tell that the row once existed.
		gone := e.create(owner, "orders", "order", `{"sku":"gone","qty":1}`)
		if r := e.send(owner, http.MethodDelete, "/api/v1/orders/"+gone, "", ""); r.status != http.StatusNoContent {
			t.Fatalf("the owner's delete answered %d %s, want 204", r.status, r.body)
		}
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			if r := e.send(owner, method, "/api/v1/orders/"+gone, "k-"+uuid.NewString(), `{"qty":2}`); r.status != http.StatusForbidden {
				t.Errorf("a %s of a deleted row answered %d %s, want 403", method, r.status, r.body)
			}
		}
		if r := e.send(owner, http.MethodPatch, "/api/v1/orders/"+order, "k-"+uuid.NewString(), `{"qty":5}`); r.status != http.StatusOK {
			t.Errorf("the owner's patch answered %d %s, want 200", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T05 A before slot that halts writes nothing and fires no after slot", func(t *testing.T) {
		e := service(t)
		e.mu.Lock()
		e.orderGate = func(req *slots.BeforeCreateRequest) *slots.SlotResult {
			if req.Input.Fields["sku"] == "halted" {
				return &slots.SlotResult{Ok: true, Halt: true, StatusCode: http.StatusNoContent}
			}
			return nil
		}
		e.mu.Unlock()
		defer func() { e.mu.Lock(); e.orderGate = nil; e.mu.Unlock() }()
		a := e.as("u-1", newTenant())
		key := "halt-" + uuid.NewString()
		before := e.afterCallsOf("")
		for i := 0; i < 2; i++ {
			r := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"halted","qty":1}`)
			if r.status != http.StatusNoContent || r.header.Get("Stego-Slot-Halted") != "" {
				t.Errorf("the halted create (call %d) answered %d with the halt mark %q, want 204 with no mark", i+1, r.status, r.header.Get("Stego-Slot-Halted"))
			}
		}
		if n := e.afterCallsOf("halted") + e.afterCallsOf("") - before; n != 0 {
			t.Errorf("the after slot ran %d times for a halted create and its replay, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE tenant_id = $1`, a.tenant.ID()); n != 0 {
			t.Errorf("%d order rows after the halt, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND outcome = 'ok'`, a.tenant.ID()); n != 0 {
			t.Errorf("%d ok audit rows after the halt, want 0", n)
		}
		e.create(a, "orders", "order", `{"sku":"written","qty":1}`)
		if n := e.afterCallsOf("written"); n != 1 {
			t.Errorf("the after slot ran %d times for a create that wrote, want 1", n)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T06 A failed after slot answers the committed response with no error text", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		key := "after-" + uuid.NewString()
		r := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"after-fails","qty":1}`)
		if r.status != http.StatusCreated || strings.Contains(string(r.body), "secret internal text") {
			t.Fatalf("the create whose after slot fails answered %d %s, want the committed 201 with no error text", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE tenant_id = $1 AND sku = 'after-fails'`, a.tenant.ID()); n != 1 {
			t.Errorf("%d order rows, want the committed one", n)
		}
		if replay := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"after-fails","qty":1}`); replay.status != r.status || string(replay.body) != string(r.body) {
			t.Errorf("the replay answered %d %s, want the same answer %d %s", replay.status, replay.body, r.status, r.body)
		}
		if api.AfterSlotFailures() < 1 {
			t.Error("the failed after slot is not counted")
		}
	})

	// tenantRoutes sends every route as the caller of tenant t1 with tenant t2 in the header, query or body.
	tenantRoutes := func(e *env, a caller, decorate func(*http.Request)) {
		t := e.t
		t.Helper()
		send := func(method, path, key, body string) reply {
			var reader io.Reader
			if body != "" {
				reader = strings.NewReader(body)
			}
			request, err := http.NewRequest(method, e.server.URL+path, reader)
			must(t, err)
			request.Header.Set("Authorization", "Bearer "+a.token)
			if key != "" {
				request.Header.Set("Idempotency-Key", `"`+key+`"`)
			}
			decorate(request)
			response, err := http.DefaultClient.Do(request)
			must(t, err)
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			must(t, err)
			return reply{status: response.StatusCode, header: response.Header, body: data}
		}
		for _, collection := range []struct{ name, resourceType, body, change, method string }{
			{"orders", "order", `{"sku":"t","qty":1}`, `{"qty":2}`, http.MethodPatch},
			{"customers", "customer", `{"name":"T","api_token":"x","plan":"free"}`, `{"name":"T","api_token":"x","plan":"pro"}`, http.MethodPut},
			{"todos", "todo", `{"title":"t"}`, `{"title":"u"}`, http.MethodPatch},
		} {
			created := send(http.MethodPost, "/api/v1/"+collection.name, "k-"+uuid.NewString(), collection.body)
			if created.status != http.StatusCreated {
				t.Fatalf("the create of %s answered %d %s", collection.name, created.status, created.body)
			}
			id := created.member(t, "id")
			e.own(a, collection.resourceType, id)
			for _, r := range []reply{
				send(http.MethodGet, "/api/v1/"+collection.name+"/"+id, "", ""),
				send(http.MethodGet, "/api/v1/"+collection.name, "", ""),
				send(collection.method, "/api/v1/"+collection.name+"/"+id, "k-"+uuid.NewString(), collection.change),
				send(http.MethodDelete, "/api/v1/"+collection.name+"/"+id, "", ""),
			} {
				if r.status >= 300 {
					t.Errorf("a route of %s answered %d %s, want it served for the caller's tenant", collection.name, r.status, r.body)
				}
			}
		}
		if n := e.count(`SELECT (SELECT count(*) FROM orders WHERE tenant_id = 't2') + (SELECT count(*) FROM customers WHERE tenant_id = 't2') + (SELECT count(*) FROM todos WHERE tenant_id = 't2')`); n != 0 {
			t.Errorf("%d rows with tenant t2, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 't2'`); n != 0 {
			t.Errorf("%d audit rows with tenant t2, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND outcome = 'ok' AND action IN ('create', 'patch', 'update', 'delete')`, a.tenant.ID()); n != 9 {
			t.Errorf("%d ok audit rows with the caller's tenant, want 9", n)
		}
	}

	t.Run("ST-SUP-RC20-OC35-T01 Every route takes the tenant from the verified identity", func(t *testing.T) {
		e := service(t)
		other := e.as("u-1", newTenant())
		orderOfT2 := e.create(other, "orders", "order", `{"sku":"t2","qty":1}`)
		a := e.as("u-1", newTenant())
		tenantRoutes(e, a, func(*http.Request) {})
		// A row of another tenant is not reached by its ID: the same subject in tenant t1 may not see or change it.
		if r := e.send(a, http.MethodGet, "/api/v1/orders/"+orderOfT2, "", ""); r.status != http.StatusNotFound {
			t.Errorf("a read of another tenant's row answered %d %s, want 404", r.status, r.body)
		}
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			if r := e.send(a, method, "/api/v1/orders/"+orderOfT2, "k-"+uuid.NewString(), `{"qty":7}`); r.status != http.StatusForbidden {
				t.Errorf("a %s of another tenant's row answered %d %s, want 403", method, r.status, r.body)
			}
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE id = $1::uuid AND tenant_id = $2 AND qty = 1`, orderOfT2, other.tenant.ID()); n != 1 {
			t.Errorf("the row of tenant t2 changed: %d rows as created, want 1", n)
		}
	})

	t.Run("ST-SUP-RC20-OC35-T02 A tenant in a header is not accepted", func(t *testing.T) {
		e := service(t)
		tenantRoutes(e, e.as("u-1", newTenant()), func(r *http.Request) {
			r.Header.Set("X-Tenant-ID", "t2")
			r.Header.Set("Tenant", "t2")
		})
	})

	t.Run("ST-SUP-RC20-OC35-T03 A tenant in a query parameter is not accepted", func(t *testing.T) {
		e := service(t)
		tenantRoutes(e, e.as("u-1", newTenant()), func(r *http.Request) {
			q := r.URL.Query()
			q.Set("tenant", "t2")
			q.Set("tenant_id", "t2")
			r.URL.RawQuery = q.Encode()
		})
	})

	t.Run("ST-SUP-RC20-OC35-T04 A tenant in a body field is not accepted", func(t *testing.T) {
		e := service(t)
		tenantRoutes(e, e.as("u-1", newTenant()), func(r *http.Request) {
			if r.Body == nil || r.Method == http.MethodDelete || r.Method == http.MethodGet {
				return
			}
			data, err := io.ReadAll(r.Body)
			must(t, err)
			var body map[string]any
			must(t, json.Unmarshal(data, &body))
			body["tenant"], body["tenant_id"] = "t2", "t2"
			data, err = json.Marshal(body)
			must(t, err)
			r.Body = io.NopCloser(strings.NewReader(string(data)))
			r.ContentLength = int64(len(data))
		})
	})

	t.Run("ST-SUP-RC30-OC04-T01 The key row commits with the write", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		key := "todo-" + uuid.NewString()
		created := e.send(a, http.MethodPost, "/api/v1/todos", key, `{"title":"write the check"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		if n := e.count(`SELECT count(*) FROM todos WHERE tenant_id = $1 AND title = 'write the check'`, a.tenant.ID()); n != 1 {
			t.Errorf("%d Todo rows, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1 AND principal_id = $2 AND key = $3 AND status = 'completed'`, a.tenant.ID(), a.principal.Subject().String(), key); n != 1 {
			t.Errorf("%d completed key rows scoped by the tenant, the principal and the key, want 1", n)
		}
	})

	t.Run("ST-SUP-RC30-OC04-T03 The same key from another tenant or principal is a new key", func(t *testing.T) {
		e := service(t)
		tenantA, tenantB := newTenant(), newTenant()
		key := "shared-" + uuid.NewString()
		title := "t-" + uuid.NewString()
		body := `{"title":"` + title + `"}`
		for _, c := range []caller{e.as("p1", tenantA), e.as("p2", tenantA), e.as("p1", tenantB)} {
			r := e.send(c, http.MethodPost, "/api/v1/todos", key, body)
			if r.status != http.StatusCreated || r.header.Get(idempotency.ReplayedHeader) != "" {
				t.Errorf("the create as %s in %s answered %d (replayed %q), want a new 201", c.principal.Subject(), c.tenant.ID(), r.status, r.header.Get(idempotency.ReplayedHeader))
			}
		}
		if n := e.count(`SELECT count(*) FROM todos WHERE title = $1`, title); n != 3 {
			t.Errorf("%d Todo rows, want 3", n)
		}
		if n := e.count(`SELECT count(*) FROM idempotency_keys WHERE key = $1`, key); n != 3 {
			t.Errorf("%d key rows, want 3", n)
		}
	})

	t.Run("ST-SUP-RC28-OC02-T02 A committed mutation leaves exactly one audit row", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		id := e.create(a, "customers", "customer", `{"name":"Ada","api_token":"tok-1","plan":"free"}`)
		if r := e.send(a, http.MethodPut, "/api/v1/customers/"+id, "", `{"name":"Ada","api_token":"tok-2","plan":"pro"}`); r.status != http.StatusOK {
			t.Fatalf("the update answered %d %s, want 200", r.status, r.body)
		}
		if r := e.send(a, http.MethodDelete, "/api/v1/customers/"+id, "", ""); r.status != http.StatusNoContent {
			t.Fatalf("the delete answered %d %s, want 204", r.status, r.body)
		}
		for _, action := range []string{"create", "update", "delete"} {
			if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND resource_id = $2 AND action = $3 AND outcome = 'ok'`, a.tenant.ID(), id, action); n != 1 {
				t.Errorf("%d ok audit rows with the action %s, want 1", n, action)
			}
			if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND resource_id = $2 AND action = $3`, a.tenant.ID(), id, action); n != 1 {
				t.Errorf("%d audit rows with the action %s, want 1", n, action)
			}
		}
	})

	t.Run("ST-SUP-RC28-OC03-T01 A denied mutation writes one audit row with outcome denied after the rollback", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		id := e.create(a, "customers", "customer", `{"name":"Bea","api_token":"tok-1","plan":"free"}`)
		e.mu.Lock()
		e.customerGate = func(req *slots.ValidateRequest) *slots.SlotResult {
			if req.Input.Fields["plan"] == "denied" {
				return &slots.SlotResult{Ok: false, StatusCode: http.StatusForbidden, ErrorMessage: "the plan is denied"}
			}
			return nil
		}
		e.mu.Unlock()
		defer func() { e.mu.Lock(); e.customerGate = nil; e.mu.Unlock() }()
		r := e.send(a, http.MethodPut, "/api/v1/customers/"+id, "", `{"name":"Bea","api_token":"tok-2","plan":"denied"}`)
		if r.status != http.StatusForbidden {
			t.Fatalf("the denied update answered %d %s, want 403", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM customers WHERE id = $1::uuid AND plan = 'free'`, id); n != 1 {
			t.Errorf("the change was not rolled back: %d rows with the old plan, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND resource_id = $2 AND action = 'update'`, a.tenant.ID(), id); n != 1 {
			t.Errorf("%d audit rows of the update, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND resource_id = $2 AND action = 'update' AND outcome = 'denied' AND error_code = 'slot_refused'`, a.tenant.ID(), id); n != 1 {
			t.Errorf("%d denied audit rows with the code slot_refused, want 1", n)
		}
	})

	t.Run("ST-SUP-RC28-OC04-T01 A single read of an entity with a pii field writes one read audit row", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		id := e.create(a, "customers", "customer", `{"name":"Cy","api_token":"tok-1","plan":"free"}`)
		before := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read'`, a.tenant.ID())
		if r := e.send(a, http.MethodGet, "/api/v1/customers/"+id, "", ""); r.status != http.StatusOK {
			t.Fatalf("the read answered %d %s, want 200", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read'`, a.tenant.ID()) - before; n != 1 {
			t.Errorf("%d new read audit rows, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read' AND resource_type = 'customer' AND resource_id = $2`, a.tenant.ID(), id); n != 1 {
			t.Errorf("%d read audit rows of the customer, want 1", n)
		}
		order := e.create(a, "orders", "order", `{"sku":"s-1","qty":1}`)
		if r := e.send(a, http.MethodGet, "/api/v1/orders/"+order, "", ""); r.status != http.StatusOK {
			t.Fatalf("the read of the order answered %d %s, want 200", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read' AND resource_id = $2`, a.tenant.ID(), order); n != 0 {
			t.Errorf("%d read audit rows of an entity with no pii field, want 0", n)
		}
	})

	t.Run("ST-SUP-RC28-OC04-T03 A list that returns a pii field writes one read audit row per returned resource, at most one page", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		for i := 0; i < 5; i++ {
			e.create(a, "customers", "customer", `{"name":"N","api_token":"tok","plan":"free"}`)
		}
		ids := func(r reply) []string {
			items, _ := r.object(t)["items"].([]any)
			var out []string
			for _, item := range items {
				m, _ := item.(map[string]any)
				id, _ := m["id"].(string)
				out = append(out, id)
			}
			return out
		}
		reads := func() int {
			return e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read'`, a.tenant.ID())
		}
		start := reads()
		first := e.send(a, http.MethodGet, "/api/v1/customers?pageSize=2", "", "")
		if first.status != http.StatusOK || len(ids(first)) != 2 {
			t.Fatalf("the first list answered %d %s, want 200 with two items", first.status, first.body)
		}
		if n := reads() - start; n != 2 {
			t.Errorf("%d read rows for the first list, want 2", n)
		}
		next := first.member(t, "next")
		if next == "" {
			t.Fatalf("the first list gives no cursor of the next page: %s", first.body)
		}
		second := e.send(a, http.MethodGet, "/api/v1/customers?pageSize=2&cursor="+url.QueryEscape(next), "", "")
		if second.status != http.StatusOK || len(ids(second)) != 2 {
			t.Fatalf("the second list answered %d %s, want 200 with two items", second.status, second.body)
		}
		if n := reads() - start; n != 4 {
			t.Errorf("%d read rows for both lists, want 4", n)
		}
		returned := append(ids(first), ids(second)...)
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read' AND resource_type = 'customer' AND resource_id = ANY($2)`, a.tenant.ID(), returned); n != 4 {
			t.Errorf("%d read rows name a returned customer, want 4", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read' AND NOT (resource_id = ANY($2))`, a.tenant.ID(), returned); n != 0 {
			t.Errorf("%d read rows name a resource that no list returned, or the collection", n)
		}
	})

	t.Run("ST-SUP-RC28-OC01-T03 A secret field reaches neither before, after nor the stored row", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		first, second := "secret-"+uuid.NewString(), "secret-"+uuid.NewString()
		id := e.create(a, "customers", "customer", `{"name":"Dee","api_token":"`+first+`","plan":"free"}`)
		r := e.send(a, http.MethodPut, "/api/v1/customers/"+id, "", `{"name":"Dee","api_token":"`+second+`","plan":"pro"}`)
		if r.status != http.StatusOK {
			t.Fatalf("the update answered %d %s, want 200", r.status, r.body)
		}
		if strings.Contains(string(r.body), second) {
			t.Errorf("the response holds the secret value: %s", r.body)
		}
		rows, err := e.admin.QueryContext(e.ctx, `SELECT row_to_json(a)::text, coalesce(before::text, ''), coalesce(after::text, ''), coalesce(patch::text, '') FROM audit_event a WHERE tenant_id = $1`, a.tenant.ID())
		must(t, err)
		defer rows.Close()
		n := 0
		for rows.Next() {
			var row, before, after, patch string
			must(t, rows.Scan(&row, &before, &after, &patch))
			n++
			for _, value := range []string{first, second} {
				if strings.Contains(row, value) {
					t.Errorf("an audit row holds the secret value: %s", row)
				}
			}
			for name, state := range map[string]string{"before": before, "after": after, "patch": patch} {
				if strings.Contains(state, "api_token") {
					t.Errorf("the %s of an audit row holds the key api_token: %s", name, state)
				}
			}
		}
		must(t, rows.Err())
		if n < 2 {
			t.Errorf("%d audit rows of the tenant, want the create and the update rows", n)
		}
		var stored string
		must(t, e.admin.QueryRowContext(e.ctx, `SELECT api_token FROM customers WHERE id = $1::uuid`, id).Scan(&stored))
		if stored != second {
			t.Errorf("the source row holds %q, want the secret it was given", stored)
		}
	})
}
