// Package check runs the generated command handlers of the test service over HTTP and gRPC, against a local
// PostgreSQL server (STEGO_TEST_POSTGRES_DSN) and a local SpiceDB container (STEGO_TEST_SPICEDB), with the runtime
// library wired as the service wires it.
package check

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	reservecharge "example.com/orders/fills/reserve-and-charge"
	commands "example.com/orders/out/commands"
	"example.com/orders/out/commands/handlers"
)

// tablesSQL is the migration 001_initial of the PostgreSQL adapter for the declaration (ST-SUP-RC01-OC41).
//
//go:embed tables.sql
var tablesSQL string

const (
	issuer   = "https://issuer.authn.test/realms/r1"
	audience = "orders"
	password = "command-check"
	schema   = `definition user {}

definition order {
	relation owner: user
	relation viewer: user
	relation holder: user
	permission reserve = owner
	permission view = owner + viewer
}

definition hold {
	relation owner: user
	relation creator: user
	relation order: order
	permission view = owner + creator
	permission reserve = owner + creator
}
`
)

var tables = []string{"orders", "reservations", "charges", "holds"}

// env is the running service: its database with the runtime role, its runtime and its HTTP server.
type env struct {
	t        *testing.T
	ctx      context.Context
	admin    *sql.DB
	rt       *command.Runtime
	server   *httptest.Server
	verifier *authn.Verifier
	key      *authntest.Key
	spice    *grpc.ClientConn
	decision *recorder
	calls    atomic.Int64
	rtConfig command.Config
	fills    handlers.Fills
	mux      *http.ServeMux
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
	e.database(dsn)
	e.authorization(endpoint, os.Getenv("STEGO_TEST_SPICEDB_KEY"))
	e.start()
	shared = e
	return e
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// database makes a database of its own with the runtime, outbox worker and relay roles, the runtime tables and
// the entity tables with row-level security on the tenant setting.
func (e *env) database(dsn string) {
	t := e.t
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	tag := hex.EncodeToString(suffix)
	name := "cmd_" + tag
	roles := [3]string{"cmd_rt_" + tag, "cmd_worker_" + tag, "cmd_relay_" + tag}
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
	// The outbox relay role of the test service, one role of the server, which the outbox migration makes.
	queueDDL, err := outbox.Migration("cmd_check"+outbox.RelaySuffix, roles[0], roles[1])
	must(t, err)
	authzDDL, err := authz.Migration("authz", roles[0], roles[2], "cmd_check"+outbox.RelaySuffix, roles[1])
	must(t, err)
	auditDDL, err := audit.DDL(audit.Config{RuntimeRole: roles[0], AnchorPeriod: time.Hour, RetentionMonths: 12})
	must(t, err)
	// The idempotency purge role of the test service, one role of the server, which the migration makes.
	keysDDL, err := idempotency.Migration("cmd_check_idempotency_purge", roles[0])
	must(t, err)
	setup := []string{
		queueDDL,
		"CREATE SCHEMA authz",
		"GRANT USAGE ON SCHEMA authz TO " + roles[0] + ", " + roles[1] + ", " + roles[2],
		authzDDL,
		auditDDL,
		keysDDL,
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
	e.rtConfig = command.Config{DB: pool, AuditDB: auditDB, Outbox: queue, Service: "orders", ServiceVersion: "1.0.0",
		DeclarationVersion: "d1", Entities: handlers.Entities(), Probes: probe.NewRecorder(nil)}
	writer, err := audit.NewWriter(auditOutbox{q: queue}, nil)
	must(t, err)
	e.rtConfig.Audit = writer
}

// authorization connects the decision port to the local SpiceDB container and writes the schema.
func (e *env) authorization(endpoint, key string) {
	t := e.t
	if key == "" {
		key = "command-check"
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(key)))
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
	e.decision = &recorder{inner: provider}
	az, err := authz.New(authz.Config{Provider: e.decision, DB: e.rtConfig.DB, Schema: "authz", Outbox: e.rtConfig.Outbox})
	must(t, err)
	e.rtConfig.Authz = az
}

// start starts the command runtime and the HTTP server with the generated handlers behind the verifier.
func (e *env) start() {
	t := e.t
	var err error
	e.rt, err = command.New(e.ctx, e.rtConfig)
	must(t, err)
	e.key = authntest.NewKey(t, "k1")
	keys, err := authn.ParseKeys(authntest.Document(e.key.JWK()))
	must(t, err)
	e.verifier, err = authn.New(authn.Settings{Issuer: issuer, Audience: audience, TenantClaim: "tenant", ProblemCode: "ORD", Keys: keys})
	must(t, err)
	mux := http.NewServeMux()
	fills := handlers.BoundFills()
	counted := fills.ReserveAndCharge
	fills.ReserveAndCharge = func(ctx context.Context, request *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
		e.calls.Add(1)
		return counted(ctx, request, h)
	}
	e.fills = fills
	must(t, handlers.Register(mux, e.rt, fills, handlers.Options{}))
	e.mux = mux
	e.server = httptest.NewServer(e.verifier.Middleware(mux))
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// recorder records each request of the decision port and passes it to SpiceDB.
type recorder struct {
	inner authz.Decision
	mu    sync.Mutex
	calls []authz.EvaluationRequest
}

func (r *recorder) Evaluate(ctx context.Context, request authz.EvaluationRequest) (authz.EvaluationResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, request)
	r.mu.Unlock()
	return r.inner.Evaluate(ctx, request)
}

func (r *recorder) EvaluateBatch(ctx context.Context, request authz.BatchRequest) ([]authz.EvaluationResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, request.Evaluations...)
	r.mu.Unlock()
	return r.inner.EvaluateBatch(ctx, request)
}

func (r *recorder) Search(ctx context.Context, request authz.SearchRequest) (authz.SearchResult, error) {
	return r.inner.Search(ctx, request)
}

func (r *recorder) Searches() bool { return r.inner.Searches() }

// taken gives the requests since the last call and forgets them.
func (r *recorder) taken() []authz.EvaluationRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := r.calls
	r.calls = nil
	return calls
}

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

// caller is one verified caller: user u-7 of a tenant, with a token.
type caller struct {
	tenant    tenant.Tenant
	principal authz.Principal
	token     string
	identity  authn.Identity
}

// as mints a token for subject in tenantID and verifies it.
func (e *env) as(subject, tenantID string, expires time.Time) caller {
	t := e.t
	t.Helper()
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
	claims["exp"] = expires.Unix()
	token := authntest.Mint(t, authntest.Header("k1"), claims, e.key)
	c := caller{token: token}
	if expires.Before(time.Now()) {
		return c
	}
	id, err := e.verifier.Verify(e.ctx, token)
	must(t, err)
	c.identity = id
	c.tenant, err = tenant.New(tenant.Claim(id))
	must(t, err)
	c.principal, err = authz.Person("user", id)
	must(t, err)
	return c
}

// subject is the SpiceDB subject ID of the caller.
func (c caller) subject() string { return c.principal.Subject().ID }

// order seeds an Order row of the caller's tenant, owned in SpiceDB by owner (the caller, or another subject when
// owner is given), and gives its ID.
func (e *env) order(c caller, owner ...string) string {
	return e.orderWith(c, "owner", owner...)
}

// orderWith seeds an Order row whose subject has the relation on it.
func (e *env) orderWith(c caller, relation string, owner ...string) string {
	t := e.t
	t.Helper()
	id := uuid.NewString()
	_, err := e.admin.ExecContext(e.ctx, `INSERT INTO orders (tenant_id, id, sku) VALUES ($1, $2, 'sku-1')`, c.tenant.ID(), id)
	must(t, err)
	subject := c.subject()
	if len(owner) > 0 {
		subject = owner[0]
	}
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(t, err)
	_, err = v1.NewPermissionsServiceClient(e.spice).WriteRelationships(e.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "order", ObjectId: resourceID}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: subject}}}}}})
	must(t, err)
	return id
}

// reply is one HTTP answer.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// post sends reserve-and-charge on the order with the token and the idempotency key, either of which may be empty.
func (e *env) post(token, key, orderID, body string) reply {
	t := e.t
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, e.server.URL+"/api/v1/orders/"+orderID+"/reservations", strings.NewReader(body))
	must(t, err)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", `"`+key+`"`)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	must(t, err)
	return reply{status: response.StatusCode, header: response.Header, body: data}
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.admin.QueryRowContext(e.ctx, query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// auditRows counts the audit rows of reserve-and-charge of the tenant with the outcome on the resource.
func (e *env) auditRows(c caller, outcome, resourceID string) int {
	return e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'reserve-and-charge' AND actor_id = $2 AND outcome = $3 AND (resource_id = $4 OR $4 = '')`,
		c.tenant.ID(), c.principal.Subject().String(), outcome, resourceID)
}

// rows counts the rows of the table of the order.
func (e *env) rows(table, orderID string) int {
	return e.count(`SELECT count(*) FROM `+table+` WHERE order_id = $1`, orderID)
}

func (e *env) keyRows(key string) int {
	return e.count(`SELECT count(*) FROM idempotency_keys WHERE key = $1`, key)
}

// holdTuple is one authz outbox message of a Hold: op relation@subject_type:subject_id, and its revocation.
type holdTuple struct {
	text, revocation string
}

// holdTuples reads the authz outbox messages of the declared relationships of the Hold row id of the caller's tenant,
// in enqueue order. The relation order of its reference to Order is proved by ST-SUP-RP10-LC05-T12 and left out.
func (e *env) holdTuples(c caller, id string) []holdTuple {
	t := e.t
	t.Helper()
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(t, err)
	rows, err := e.admin.QueryContext(e.ctx, `SELECT p->>'op', p->'tuple'->>'relation', p->'tuple'->>'subject_type', p->'tuple'->>'subject_id', coalesce(p->>'revocation', '')
 FROM (SELECT convert_from(payload, 'UTF8')::jsonb AS p, sequence FROM stego_outbox.messages WHERE destination = 'authz' AND tenant_id = $1) m
 WHERE p->'tuple'->>'resource_type' = 'hold' AND p->'tuple'->>'resource_id' = $2 AND p->'tuple'->>'relation' <> 'order' ORDER BY sequence`, c.tenant.ID(), resourceID)
	must(t, err)
	defer rows.Close()
	var out []holdTuple
	for rows.Next() {
		var op, relation, subjectType, subjectID, revocation string
		must(t, rows.Scan(&op, &relation, &subjectType, &subjectID, &revocation))
		out = append(out, holdTuple{text: op + " " + relation + "@" + subjectType + ":" + subjectID, revocation: revocation})
	}
	must(t, rows.Err())
	return out
}

// holdRepositories gives the loaded order, the parent of a Hold, and the Hold repository of the handle.
func (e *env) holdRepositories(ctx context.Context, h *commands.ReserveAndChargeHandle, order string) (command.Ref, *command.Repository[commands.Hold]) {
	orders, err := commands.OrderRepository(h)
	must(e.t, err)
	holds, err := commands.HoldRepository(h)
	must(e.t, err)
	parent, err := orders.Get(ctx, order)
	must(e.t, err)
	return parent, holds
}

// holdCreate creates, in one command, a Hold under the order with the owner, and gives its ID. The caller gets the
// relation creator on the new Hold in SpiceDB, as the relay would write it, so later commands may lock it.
func (e *env) holdCreate(t *testing.T, c caller, key, order, owner string) string {
	t.Helper()
	var id string
	e.behave(t, func(ctx context.Context, _ *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
		parent, holds := e.holdRepositories(ctx, h, order)
		hold, err := holds.Create(ctx, commands.Hold{OrderID: order, OwnerID: owner}, parent)
		if err != nil {
			return nil, err
		}
		id = hold.ID()
		return &commands.ReserveAndChargeResponse{}, nil
	})
	if got := e.post(c.token, key, order, body); got.status != http.StatusCreated {
		t.Fatalf("the create of a Hold answered %d %s, want 201", got.status, got.body)
	}
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(t, err)
	_, err = v1.NewPermissionsServiceClient(e.spice).WriteRelationships(e.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "hold", ObjectId: resourceID}, Relation: "creator",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject()}}}}}})
	must(t, err)
	return id
}

// holdUpdate sets, in one command, the owner of the Hold, and gives the answer.
func (e *env) holdUpdate(t *testing.T, c caller, key, order, id, owner string) reply {
	t.Helper()
	e.behave(t, func(ctx context.Context, _ *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
		_, holds := e.holdRepositories(ctx, h, order)
		from, err := holds.GetForUpdate(ctx, id)
		if err != nil {
			return nil, err
		}
		value := from.Value()
		value.OwnerID = owner
		if _, err := holds.Update(ctx, from, value); err != nil {
			return nil, err
		}
		return &commands.ReserveAndChargeResponse{}, nil
	})
	return e.post(c.token, key, order, body)
}

// holdDelete deletes the Hold in one command.
func (e *env) holdDelete(t *testing.T, c caller, key, order, id string) {
	t.Helper()
	e.behave(t, func(ctx context.Context, _ *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
		_, holds := e.holdRepositories(ctx, h, order)
		from, err := holds.GetForDelete(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := holds.Delete(ctx, from); err != nil {
			return nil, err
		}
		return &commands.ReserveAndChargeResponse{}, nil
	})
	if got := e.post(c.token, key, order, body); got.status != http.StatusCreated {
		t.Fatalf("the delete of a Hold answered %d %s, want 201", got.status, got.body)
	}
}

// behave sets what the fill does for one test, and the default again when it ends.
func (e *env) behave(t *testing.T, fill commands.ReserveAndChargeFill) {
	reservecharge.Behavior = fill
	e.calls.Store(0)
	t.Cleanup(func() { reservecharge.Behavior = reservecharge.ReserveAndCharge })
}

const body = `{"sku":"sku-1","qty":2,"amountCents":1250,"cardLast4":"4242"}`

func TestCommand(t *testing.T) {
	t.Run("ST-SUP-RC32-OC04-T01 The fill runs in one transaction with the caller's tenant", func(t *testing.T) {
		e := service(t)
		a, b := e.as("u-7", "tA", time.Now().Add(time.Hour)), e.as("u-7", "tB", time.Now().Add(time.Hour))
		ownA, ownB := uuid.NewString(), uuid.NewString()
		_, err := e.admin.ExecContext(e.ctx, `INSERT INTO reservations (tenant_id, id, order_id, sku, qty) VALUES ($1, $2, 'x', 's', 1), ($3, $4, 'x', 's', 1)`,
			a.tenant.ID(), ownA, b.tenant.ID(), ownB)
		must(t, err)
		var seenTenant string
		var foundOwn, foundOther error
		e.behave(t, func(ctx context.Context, request *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
			seenTenant = h.Tenant().ID()
			r, err := commands.ReservationRepository(h)
			if err != nil {
				return nil, err
			}
			_, foundOwn = r.Get(ctx, ownA)
			_, foundOther = r.Get(ctx, ownB)
			return &commands.ReserveAndChargeResponse{ReservationId: ownA}, nil
		})
		order := e.order(a)
		got := e.post(a.token, "oc04-t01", order, body)
		if got.status != http.StatusCreated {
			t.Fatalf("the answer is %d %s, want 201", got.status, got.body)
		}
		if e.calls.Load() != 1 {
			t.Errorf("the fill ran %d times, want once", e.calls.Load())
		}
		if seenTenant != "tA" {
			t.Errorf("the handle's tenant is %q, want tA", seenTenant)
		}
		// Row-level security shows the fill's first statement only the rows of tenant A: the tenant is set in
		// the transaction before the fill.
		if foundOwn != nil || !errors.Is(foundOther, command.ErrNotFound) {
			t.Errorf("the fill read its own tenant's row with %v and the other tenant's with %v; want found and not found", foundOwn, foundOther)
		}
		if e.keyRows("oc04-t01") != 1 {
			t.Error("the transaction did not commit the key row")
		}
	})

	t.Run("ST-SUP-RC32-OC04-T02 An unauthenticated command never reaches the fill", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		order := e.order(a)
		e.behave(t, reservecharge.ReserveAndCharge)
		expired := e.as("u-7", "tA", time.Now().Add(-time.Hour))
		for _, token := range []string{"", expired.token} {
			if got := e.post(token, "oc04-t02", order, body); got.status != http.StatusUnauthorized {
				t.Errorf("the answer is %d, want 401", got.status)
			}
		}
		// The generated handler verifies the identity itself too: served with no verifier in front, a request with
		// no verified identity gets 401.
		bare := httptest.NewServer(e.mux)
		defer bare.Close()
		request, err := http.NewRequest(http.MethodPost, bare.URL+"/api/v1/orders/"+order+"/reservations", strings.NewReader(body))
		must(t, err)
		request.Header.Set("Idempotency-Key", `"oc04-t02"`)
		response, err := http.DefaultClient.Do(request)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("the handler with no verified identity answered %d, want 401", response.StatusCode)
		}
		if e.calls.Load() != 0 || e.keyRows("oc04-t02") != 0 || e.rows("reservations", order) != 0 {
			t.Errorf("the fill ran %d times, or a transaction wrote a row", e.calls.Load())
		}
	})

	t.Run("ST-SUP-RC32-OC05-T01 A fill error leaves nothing behind", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		order := e.order(a)
		outboxBefore := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE destination = 'billing'`)
		e.behave(t, func(ctx context.Context, request *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
			if _, err := reservecharge.ReserveAndCharge(ctx, request, h); err != nil {
				return nil, err
			}
			return nil, errors.New("the charge failed")
		})
		if got := e.post(a.token, "oc05-t01", order, body); got.status != http.StatusInternalServerError {
			t.Fatalf("the answer is %d %s, want 500", got.status, got.body)
		}
		if n := e.rows("reservations", order) + e.rows("charges", order); n != 0 {
			t.Errorf("%d Reservation and Charge rows stayed", n)
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE destination = 'billing'`); n != outboxBefore {
			t.Errorf("the billing outbox has %d rows, want %d", n, outboxBefore)
		}
		if e.keyRows("oc05-t01") != 0 {
			t.Error("the key row stayed")
		}
		if e.auditRows(a, "failed", order) != 1 || e.auditRowsOK(a, order) != 0 {
			t.Errorf("the audit rows are not one failed row: failed %d, ok %d", e.auditRows(a, "failed", order), e.auditRowsOK(a, order))
		}
	})

	t.Run("ST-SUP-RC32-OC05-T02 A successful command commits its writes and effects together", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		order := e.order(a)
		e.behave(t, reservecharge.ReserveAndCharge)
		got := e.post(a.token, "oc05-t02", order, body)
		if got.status != http.StatusCreated {
			t.Fatalf("the answer is %d %s, want 201", got.status, got.body)
		}
		var reservation, charge string
		must(t, e.admin.QueryRowContext(e.ctx, `SELECT r.xmin::text, c.xmin::text FROM reservations r, charges c WHERE r.order_id = $1 AND c.order_id = $1`, order).Scan(&reservation, &charge))
		var key, effect string
		must(t, e.admin.QueryRowContext(e.ctx, `SELECT xmin::text FROM idempotency_keys WHERE key = 'oc05-t02'`).Scan(&key))
		must(t, e.admin.QueryRowContext(e.ctx, `SELECT xmin::text FROM stego_outbox.messages WHERE destination = 'billing' AND tenant_id = $1 ORDER BY created_at DESC LIMIT 1`, a.tenant.ID()).Scan(&effect))
		var audits []string
		rows, err := e.admin.QueryContext(e.ctx, `SELECT xmin::text FROM audit_event WHERE tenant_id = $1 AND action = 'reserve-and-charge' AND outcome = 'ok' AND resource_type IN ('Reservation', 'Charge') AND request_id = (SELECT request_id FROM audit_event WHERE resource_type = 'Charge' AND tenant_id = $1 ORDER BY occurred_at DESC LIMIT 1)`, a.tenant.ID())
		must(t, err)
		for rows.Next() {
			var x string
			must(t, rows.Scan(&x))
			audits = append(audits, x)
		}
		must(t, rows.Err())
		if len(audits) != 2 {
			t.Fatalf("the command left %d audit rows of its writes, want 2", len(audits))
		}
		for _, x := range append(audits, charge, key, effect) {
			if x != reservation {
				t.Errorf("the Reservation, the Charge, their audit rows, the outbox row and the key row are not in one commit: %s and %s", reservation, x)
			}
		}
	})

	t.Run("ST-SUP-RC29-OC05-T01 Create, update and delete each enqueue the declared tuples", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		order := e.order(a)
		hold := e.holdCreate(t, a, "rc29-oc05-t01-create", order, "u1")
		// An update that keeps the owner enqueues nothing; one that changes it is refused with 400 naming owner_id,
		// since the subject of a field-sourced relationship is set at create only (ST-SUP-Q118).
		if got := e.holdUpdate(t, a, "rc29-oc05-t01-update", order, hold, "u1"); got.status != http.StatusCreated {
			t.Fatalf("an update that keeps the owner answered %d %s, want 201", got.status, got.body)
		}
		if got := e.holdUpdate(t, a, "rc29-oc05-t01-move", order, hold, "u2"); got.status != http.StatusBadRequest ||
			!strings.Contains(string(got.body), `"code":"invalid"`) || !strings.Contains(string(got.body), `"field":"owner_id"`) {
			t.Fatalf("an update that moves the owner answered %d %s, want 400 invalid naming owner_id", got.status, got.body)
		}
		e.holdDelete(t, a, "rc29-oc05-t01-delete", order, hold)
		got := e.holdTuples(a, hold)
		// The delete clears each relation that the schema declares on hold by a filtered delete; the reference
		// relation order is left out of holdTuples.
		want := []string{"touch creator@user:" + a.subject(), "touch owner@user:u1", "delete_relation creator@:", "delete_relation owner@:"}
		if len(got) != len(want) {
			t.Fatalf("the authz outbox holds %v for the Hold, want %v", got, want)
		}
		for i := range want {
			if got[i].text != want[i] {
				t.Errorf("message %d is %s, want %s", i, got[i].text, want[i])
			}
		}
	})

	t.Run("ST-SUP-RC29-OC05-T02 The tuples commit and roll back with the row", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		order := e.order(a)
		failed := errors.New("the fill fails after the create")
		var created string
		e.behave(t, func(ctx context.Context, _ *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
			parent, holds := e.holdRepositories(ctx, h, order)
			hold, err := holds.Create(ctx, commands.Hold{OrderID: order, OwnerID: "u-oc05-t02"}, parent)
			if err != nil {
				return nil, err
			}
			created = hold.ID()
			return nil, failed
		})
		if got := e.post(a.token, "rc29-oc05-t02", order, body); got.status != http.StatusInternalServerError {
			t.Fatalf("the failed command answered %d %s, want 500", got.status, got.body)
		}
		if created == "" {
			t.Fatal("the fill created no Hold before it failed")
		}
		if n := e.count(`SELECT count(*) FROM holds WHERE id = $1`, created); n != 0 {
			t.Errorf("the rolled-back create left %d Hold rows", n)
		}
		if got := e.holdTuples(a, created); len(got) != 0 {
			t.Errorf("the rolled-back create left the authz messages %v", got)
		}
	})

	t.Run("ST-SUP-RC29-OC05-T03 The tuples of a row join the transaction's revocation", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		order := e.order(a)
		hold := e.holdCreate(t, a, "rc29-oc05-t03-create", order, "u1")
		e.holdDelete(t, a, "rc29-oc05-t03-delete", order, hold)
		got := e.holdTuples(a, hold)
		if len(got) < 3 {
			t.Fatalf("the authz outbox holds %v for the Hold, want the two touches of the create and the delete", got)
		}
		if got[0].revocation != got[1].revocation || got[0].revocation == "" {
			t.Errorf("the two touches of the create name the revocations %q and %q, want one", got[0].revocation, got[1].revocation)
		}
		for _, m := range got[2:] {
			if m.revocation != got[2].revocation || m.revocation == "" {
				t.Errorf("the messages of the delete name the revocations %q and %q, want one", got[2].revocation, m.revocation)
			}
		}
		if got[0].revocation == got[2].revocation {
			t.Error("the create and the delete name one revocation")
		}
	})

	t.Run("ST-SUP-RC32-OC06-T01 A denied command answers 404 and does not call the fill", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		order := e.order(a, "someone-else")
		// The row holds the token t5 and the revocation floor is a newer token: the decision runs at the floor, not at the row's
		// token, because Run decides a missing and an existing row at one consistency (ST-SUP-RP10-LC31).
		_, err := e.admin.ExecContext(e.ctx, `UPDATE orders SET authz_token = 't5' WHERE id = $1`, order)
		must(t, err)
		// The floor is a token that SpiceDB gave, of a write after the order's relationship.
		written, err := v1.NewPermissionsServiceClient(e.spice).WriteRelationships(e.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "order", ObjectId: "floor-mark"}, Relation: "owner",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "nobody"}}}}}})
		must(t, err)
		floorText := written.GetWrittenAt().GetToken()
		_, err = e.admin.ExecContext(e.ctx, `UPDATE authz.authz_floor SET token = $1`, floorText)
		must(t, err)
		defer func() { _, _ = e.admin.ExecContext(e.ctx, `UPDATE authz.authz_floor SET token = ''`) }()
		e.behave(t, reservecharge.ReserveAndCharge)
		e.decision.taken()
		got := e.post(a.token, "oc06-t01", order, body)
		if got.status != http.StatusNotFound {
			t.Fatalf("the answer is %d %s, want 404", got.status, got.body)
		}
		missing := e.post(a.token, "oc06-t01-missing", uuid.NewString(), body)
		if missing.status != http.StatusNotFound || string(missing.body) != string(got.body) {
			t.Fatalf("a missing order answered %d %s, want the same 404 as the deny %s", missing.status, missing.body, got.body)
		}
		if e.calls.Load() != 0 {
			t.Errorf("the fill ran %d times", e.calls.Load())
		}
		calls := e.decision.taken()
		want, err := authz.ResourceID(a.tenant, order)
		must(t, err)
		if len(calls) != 2 || calls[0].Action.Name != "reserve" || calls[0].Resource.Type != "order" || calls[0].Resource.ID != want ||
			calls[0].Subject != a.principal.Subject() {
			t.Fatalf("the decision port got %+v, want a call for reserve on order:%s and one for the missing order", calls, want)
		}
		floor, err := authz.ParseToken(floorText)
		must(t, err)
		for _, call := range calls {
			if call.Consistency != authz.AtLeast(floor) {
				t.Errorf("the decision ran at %+v, want at least the floor and not the row token t5", call.Consistency)
			}
		}
		if e.keyRows("oc06-t01") != 0 || e.auditRows(a, "denied", order) != 1 {
			t.Error("a deny wrote a key row, or no denied audit row")
		}
	})

	t.Run("ST-SUP-RC32-OC06-T02 A write under a parent that the principal may not act on is refused", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		own := e.order(a)
		// The caller may view the other order but has no reserve on it.
		other := e.orderWith(a, "viewer")
		var underOwn, underOther, noParent error
		e.behave(t, func(ctx context.Context, request *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
			orders, err := commands.OrderRepository(h)
			if err != nil {
				return nil, err
			}
			holds, err := commands.HoldRepository(h)
			if err != nil {
				return nil, err
			}
			x, err := orders.Get(ctx, own)
			if err != nil {
				return nil, err
			}
			_, underOwn = holds.Create(ctx, commands.Hold{OrderID: own}, x)
			y, err := orders.Get(ctx, other)
			if err != nil {
				return nil, err
			}
			_, underOther = holds.Create(ctx, commands.Hold{OrderID: other}, y)
			_, noParent = holds.Create(ctx, commands.Hold{OrderID: other}, nil)
			return &commands.ReserveAndChargeResponse{}, nil
		})
		e.post(a.token, "oc06-t02", own, body)
		if underOwn != nil {
			t.Errorf("the create under the decided order gave %v, want it written", underOwn)
		}
		if !errors.Is(underOther, command.ErrNotFound) {
			t.Errorf("the create under an order without reserve gave %v, want not found", underOther)
		}
		if noParent == nil {
			t.Error("a create that names the other order with no parent was written")
		}
		if n := e.count(`SELECT count(*) FROM holds WHERE order_id = $1`, other); n != 0 {
			t.Errorf("%d holds were written under the order without reserve", n)
		}
	})

	t.Run("ST-SUP-RC32-OC07-T01 A committed, a denied and a failed command each leave one command audit row", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		ok, denied, failed := e.order(a), e.order(a, "someone-else"), e.order(a)
		e.behave(t, reservecharge.ReserveAndCharge)
		if got := e.post(a.token, "oc07-ok", ok, body); got.status != http.StatusCreated {
			t.Fatalf("the allowed command answered %d %s", got.status, got.body)
		}
		if got := e.post(a.token, "oc07-denied", denied, body); got.status != http.StatusNotFound {
			t.Fatalf("the denied command answered %d", got.status)
		}
		reservecharge.Behavior = func(context.Context, *commands.ReserveAndChargeRequest, *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
			return nil, errors.New("no")
		}
		if got := e.post(a.token, "oc07-failed", failed, body); got.status != http.StatusInternalServerError {
			t.Fatalf("the failed command answered %d", got.status)
		}
		if n := e.auditRowsOK(a, ok); n < 1 {
			t.Errorf("the committed command left %d ok audit rows", n)
		}
		if e.auditRows(a, "denied", denied) != 1 || e.auditRows(a, "failed", failed) != 1 {
			t.Errorf("the denied and the failed command left %d and %d audit rows, want 1 each", e.auditRows(a, "denied", denied), e.auditRows(a, "failed", failed))
		}
	})

	t.Run("ST-SUP-RC32-OC08-T01 A retried command replays and does not call the fill again", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		order := e.order(a)
		e.behave(t, reservecharge.ReserveAndCharge)
		first := e.post(a.token, "k1-"+order, order, body)
		second := e.post(a.token, "k1-"+order, order, body)
		if first.status != http.StatusCreated || e.calls.Load() != 1 || e.rows("reservations", order) != 1 || e.rows("charges", order) != 1 {
			t.Fatalf("the first answer is %d, the fill ran %d times, %d Reservations", first.status, e.calls.Load(), e.rows("reservations", order))
		}
		if second.status != first.status || !bytes.Equal(second.body, first.body) || second.header.Get(idempotency.ReplayedHeader) != "true" {
			t.Errorf("the second answer %d %s with replayed %q is not the first %d %s", second.status, second.body,
				second.header.Get(idempotency.ReplayedHeader), first.status, first.body)
		}
		// Over gRPC, the same message with the same key has the same fingerprint, so it replays too.
		rpc, err := handlers.ReserveAndChargeRPC(e.rt, e.fills.ReserveAndCharge, handlers.Options{})
		must(t, err)
		ctx, err := e.verifier.Authenticate(e.ctx, "Bearer "+a.token)
		must(t, err)
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("idempotency-key", `"rpc-k1-`+order+`"`))
		e.calls.Store(0)
		message := &commands.ReserveAndChargeRequest{OrderId: order, Sku: "sku-1", Qty: 2, AmountCents: 1250, CardLast4: "4242"}
		firstRPC, err := rpc(ctx, message)
		must(t, err)
		secondRPC, err := rpc(ctx, proto.Clone(message).(*commands.ReserveAndChargeRequest))
		must(t, err)
		if e.calls.Load() != 1 || !proto.Equal(firstRPC, secondRPC) {
			t.Errorf("the gRPC retry ran the fill %d times, or answered %v after %v", e.calls.Load(), secondRPC, firstRPC)
		}
	})

	t.Run("ST-SUP-RC32-OC08-T02 A command without an Idempotency-Key gets 400 and does not call the fill", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		order := e.order(a)
		e.behave(t, reservecharge.ReserveAndCharge)
		got := e.post(a.token, "", order, body)
		if got.status != http.StatusBadRequest || got.header.Get("Content-Type") != "application/problem+json" || !bytes.Contains(got.body, []byte(idempotency.ProblemMissing)) {
			t.Fatalf("the answer is %d %s %s, want 400 with the problem %s", got.status, got.header.Get("Content-Type"), got.body, idempotency.ProblemMissing)
		}
		if e.calls.Load() != 0 || e.rows("reservations", order) != 0 {
			t.Error("the fill ran")
		}
	})

	t.Run("ST-SUP-RC32-OC09-T01 A command fill's error text reaches no response, audit row or completion log", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		first, second := e.order(a), e.order(a)
		e.behave(t, func(context.Context, *commands.ReserveAndChargeRequest, *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
			return nil, fmt.Errorf("the mail a@example.com failed on select 1")
		})
		internal := e.post(a.token, "oc09-a", first, body)
		reservecharge.Behavior = func(context.Context, *commands.ReserveAndChargeRequest, *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
			return nil, fmt.Errorf("short: %w", commands.ReserveAndChargeShortStock)
		}
		refused := e.post(a.token, "oc09-b", second, body)
		for _, c := range []struct {
			got    reply
			order  string
			status int
			code   string
		}{{internal, first, 500, "internal"}, {refused, second, 409, "reservation.short_stock"}} {
			var code, row string
			must(t, e.admin.QueryRowContext(e.ctx, `SELECT error_code, row_to_json(a)::text FROM audit_event a WHERE tenant_id = $1 AND resource_id = $2 AND outcome = 'failed'`, a.tenant.ID(), c.order).Scan(&code, &row))
			if c.got.status != c.status || !bytes.Contains(c.got.body, []byte(`"code":"`+c.code+`"`)) || code != c.code {
				t.Errorf("the answer %d %s and the audit code %q, want %d and %s", c.got.status, c.got.body, code, c.status, c.code)
			}
			for _, secret := range []string{"a@example.com", "select 1"} {
				if bytes.Contains(c.got.body, []byte(secret)) || strings.Contains(row, secret) {
					t.Errorf("%q reached the answer %s or the audit row %s", secret, c.got.body, row)
				}
			}
		}
	})

	t.Run("ST-SUP-RC32-OC10-T01 A command that loses a conflict gets 409 conflict, an audit row, and runs again on a retry with the same key", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		order := e.order(a)
		held := uuid.NewString()
		_, err := e.admin.ExecContext(e.ctx, `INSERT INTO reservations (tenant_id, id, order_id, sku, qty) VALUES ($1, $2, 'held', 's', 1)`, a.tenant.ID(), held)
		must(t, err)
		// The fill reads the held Reservation, another writer moves its version, and the fill's update meets the
		// conflict, on every call.
		e.behave(t, func(ctx context.Context, request *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
			r, err := commands.ReservationRepository(h)
			if err != nil {
				return nil, err
			}
			l, err := r.Get(ctx, held)
			if err != nil {
				return nil, err
			}
			if _, err := e.admin.ExecContext(context.Background(), `UPDATE reservations SET version = version + 1 WHERE id = $1`, held); err != nil {
				return nil, err
			}
			v := l.Value()
			v.Qty++
			if _, err := r.Update(ctx, l, v); err != nil {
				return nil, err
			}
			return &commands.ReserveAndChargeResponse{}, nil
		})
		got := e.post(a.token, "k1-"+order, order, body)
		if got.status != http.StatusConflict || !bytes.Contains(got.body, []byte(`"code":"conflict"`)) || bytes.Contains(got.body, []byte("version")) || bytes.Contains(got.body, []byte(held)) {
			t.Fatalf("the HTTP answer is %d %s, want 409 conflict with no version or row value", got.status, got.body)
		}
		if e.calls.Load() != 2 {
			t.Errorf("the HTTP command ran the fill %d times, want twice", e.calls.Load())
		}
		rpc, err := handlers.ReserveAndChargeRPC(e.rt, e.fills.ReserveAndCharge, handlers.Options{})
		must(t, err)
		ctx, err := e.verifier.Authenticate(e.ctx, "Bearer "+a.token)
		must(t, err)
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("idempotency-key", `"rpc-`+order+`"`))
		e.calls.Store(0)
		_, err = rpc(ctx, &commands.ReserveAndChargeRequest{OrderId: order, Sku: "sku-1", Qty: 2})
		if status.Code(err) != codes.Aborted {
			t.Errorf("the gRPC call ended with %v, want ABORTED", err)
		}
		if e.calls.Load() != 2 {
			t.Errorf("the gRPC command ran the fill %d times, want twice", e.calls.Load())
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND resource_id = $2 AND outcome = 'failed' AND error_code = 'conflict'`, a.tenant.ID(), order); n != 2 {
			t.Errorf("the conflicts left %d audit rows failed conflict, want 2", n)
		}
		// A call whose request is cancelled still leaves its audit row, written in its own short transaction.
		cancelled := e.order(a)
		gone, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := rpc(gone, &commands.ReserveAndChargeRequest{OrderId: cancelled, Sku: "sku-1", Qty: 2}); err == nil {
			t.Error("the cancelled call succeeded")
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND resource_id = $2 AND outcome = 'failed'`, a.tenant.ID(), cancelled); n != 1 {
			t.Errorf("the cancelled call left %d failed audit rows, want 1", n)
		}
		reservecharge.Behavior = reservecharge.ReserveAndCharge
		e.calls.Store(0)
		again := e.post(a.token, "k1-"+order, order, body)
		if again.status != http.StatusCreated || e.calls.Load() != 1 {
			t.Errorf("the retry with the same key answered %d %s and ran the fill %d times, want 201 and once", again.status, again.body, e.calls.Load())
		}
	})

	t.Run("ST-SUP-RC32-OC11-T01 HTTP and gRPC take one request message", func(t *testing.T) {
		e := service(t)
		a := e.as("u-7", "tA", time.Now().Add(time.Hour))
		order := e.order(a)
		var seen []string
		e.behave(t, func(ctx context.Context, request *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
			seen = append(seen, fmt.Sprintf("%s %s %d", proto.MessageName(request), request.GetOrderId(), request.GetQty()))
			return reservecharge.ReserveAndCharge(ctx, request, h)
		})
		if got := e.post(a.token, "oc11-http", order, body); got.status != http.StatusCreated {
			t.Fatalf("the HTTP answer is %d %s", got.status, got.body)
		}
		rpc, err := handlers.ReserveAndChargeRPC(e.rt, e.fills.ReserveAndCharge, handlers.Options{})
		must(t, err)
		ctx, err := e.verifier.Authenticate(e.ctx, "Bearer "+a.token)
		must(t, err)
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("idempotency-key", `"oc11-rpc"`))
		response, err := rpc(ctx, &commands.ReserveAndChargeRequest{OrderId: order, Sku: "sku-1", Qty: 2})
		if err != nil || response.GetChargeId() == "" {
			t.Fatalf("the gRPC call gave %v, %v", response, err)
		}
		want := "orders.v1.ReserveAndChargeRequest " + order + " 2"
		if len(seen) != 2 || seen[0] != want || seen[1] != want {
			t.Errorf("the fill got %q, want the message %q twice", seen, want)
		}
	})
}

// auditRowsOK counts the ok audit rows of reserve-and-charge of the caller's writes under the order.
func (e *env) auditRowsOK(c caller, orderID string) int {
	return e.count(`SELECT count(*) FROM audit_event a WHERE tenant_id = $1 AND action = 'reserve-and-charge' AND actor_id = $2 AND outcome = 'ok' AND resource_id IN (SELECT id::text FROM reservations WHERE order_id = $3 UNION SELECT id::text FROM charges WHERE order_id = $3)`,
		c.tenant.ID(), c.principal.Subject().String(), orderID)
}
