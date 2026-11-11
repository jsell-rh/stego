package e2e

// This file runs inside the module of the generated test service shop (testdata/service.yaml): its query contract
// out/queries, its handlers out/queries/serve and its probes out/probes, as the generators emit them, and the fills of
// testdata/fills. It serves the handlers over HTTP with httptest, on the query runtime of github.com/jsell-rh/stego/registry/runtime, against
// the local PostgreSQL server of STEGO_TEST_POSTGRES_DSN and the local SpiceDB server of STEGO_TEST_SPICEDB_ENDPOINT,
// with the authentication runtime's verifier on a local key. A wire recorder between the store pool and PostgreSQL
// reads every statement of the query transactions, and a recorder around the SpiceDB provider reads every decision.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	getorder "example.com/shop/fills/get-order"
	getordertotal "example.com/shop/fills/get-order-total"
	listorders "example.com/shop/fills/list-orders"
	"example.com/shop/out/queries"
	"example.com/shop/out/queries/serve"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/query"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

//go:embed tables.sql
var tablesSQL string

//go:embed schema.zed
var schemaZed string

// secretToken is the value of the secret field api_token of every Order.
const secretToken = "tok-secret-1f0c"

const (
	issuer   = "https://issuer.shop.test"
	audience = "shop"
	password = "shop-test"
)

// The keys of the test rows: o1 to o4 are Orders of tenant t1, o5 an Order of tenant t2, c1 and c2 Charges of o1.
const (
	o1 = "00000000-0000-4000-8000-000000000001"
	o2 = "00000000-0000-4000-8000-000000000002"
	o3 = "00000000-0000-4000-8000-000000000003"
	o4 = "00000000-0000-4000-8000-000000000004"
	o5 = "00000000-0000-4000-8000-000000000005"
	o9 = "00000000-0000-4000-8000-000000000009" // no row
	c1 = "00000000-0000-4000-8000-0000000000c1"
	c2 = "00000000-0000-4000-8000-0000000000c2"
)

// wire is the statement recorder: a TCP proxy between the store pool and PostgreSQL that reads the SQL text of each
// simple query and each parse message that the pool sends.
type wire struct {
	mu         sync.Mutex
	statements []string
}

func (w *wire) record(text string) {
	w.mu.Lock()
	w.statements = append(w.statements, text)
	w.mu.Unlock()
}

// take gives the statements recorded since the last take and clears them.
func (w *wire) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.statements
	w.statements = nil
	return out
}

// serve proxies one connection: the startup message, then each typed frontend message.
func (w *wire) serve(client net.Conn, target string) {
	defer client.Close()
	server, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer server.Close()
	go func() { _, _ = io.Copy(client, server) }()
	in := bufio.NewReader(client)
	var size [4]byte
	if _, err := io.ReadFull(in, size[:]); err != nil {
		return
	}
	startup := make([]byte, binary.BigEndian.Uint32(size[:])-4)
	if _, err := io.ReadFull(in, startup); err != nil {
		return
	}
	if _, err := server.Write(append(size[:], startup...)); err != nil {
		return
	}
	for {
		head := make([]byte, 5)
		if _, err := io.ReadFull(in, head); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(head[1:])-4)
		if _, err := io.ReadFull(in, body); err != nil {
			return
		}
		switch head[0] {
		case 'Q':
			w.record(string(bytes.TrimRight(body, "\x00")))
		case 'P':
			if _, rest, ok := bytes.Cut(body, []byte{0}); ok {
				text, _, _ := bytes.Cut(rest, []byte{0})
				w.record(string(text))
			}
		}
		if _, err := server.Write(append(head, body...)); err != nil {
			return
		}
	}
}

// decisions is the recorder of the decision port: it passes each call to SpiceDB and keeps its request.
type decisions struct {
	inner authz.Decision
	mu    sync.Mutex
	evals []authz.EvaluationRequest
	batch []authz.BatchRequest
	finds []authz.SearchRequest
}

func (d *decisions) Evaluate(ctx context.Context, r authz.EvaluationRequest) (authz.EvaluationResult, error) {
	d.mu.Lock()
	d.evals = append(d.evals, r)
	d.mu.Unlock()
	return d.inner.Evaluate(ctx, r)
}

func (d *decisions) EvaluateBatch(ctx context.Context, r authz.BatchRequest) ([]authz.EvaluationResult, error) {
	d.mu.Lock()
	d.batch = append(d.batch, r)
	d.mu.Unlock()
	return d.inner.EvaluateBatch(ctx, r)
}

func (d *decisions) Search(ctx context.Context, r authz.SearchRequest) (authz.SearchResult, error) {
	d.mu.Lock()
	d.finds = append(d.finds, r)
	d.mu.Unlock()
	return d.inner.Search(ctx, r)
}

func (d *decisions) Searches() bool { return d.inner.Searches() }

// count gives the number of calls of each kind so far.
func (d *decisions) count() (int, int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.evals), len(d.batch), len(d.finds)
}

// auditQueue enqueues each read audit row in the outbox for the audit sink, in the row's own transaction.
type auditQueue struct{ queue *outbox.Queue }

func (a auditQueue) Enqueue(ctx context.Context, tx audit.Tx, _, destination, key string, payload []byte) error {
	sqlTx, ok := tx.(*sql.Tx)
	if !ok {
		return errors.New("the audit row is not in a *sql.Tx")
	}
	id, err := uuid.Parse(key)
	if err != nil {
		return err
	}
	return a.queue.Enqueue(ctx, sqlTx, outbox.Message{ID: id, Destination: destination, ResourceKey: key,
		Kind: "audit_event", Payload: payload, OutgoingKey: key})
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (bearer) RequireTransportSecurity() bool { return false }

// shop is the running test service.
type shop struct {
	// codes holds the code of the completion log of each request, in order.
	codesMu sync.Mutex
	codes   []string
	url     string
	admin   *sql.DB
	wire    *wire
	rec     *decisions
	key     *authntest.Key
	perms   v1.PermissionsServiceClient
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// resourceID is the tenant-qualified resource ID of the key in tenant, as the authorization runtime builds it
// (ST-SUP-RP06-LC22).
func resourceID(tenantID, key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(tenantID)) + "/" + base64.RawURLEncoding.EncodeToString([]byte(key))
}

// subject is the SpiceDB subject ID that the authorization runtime maps the user ID of a token of issuer to.
func subject(user string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(issuer)) + "|" + base64.RawURLEncoding.EncodeToString([]byte(user))
}

// start makes the database, the schema in SpiceDB, the rows and the relationships, and serves the handlers.
func start(t *testing.T) *shop {
	t.Helper()
	dsn, endpoint, spiceKey := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB_ENDPOINT"), os.Getenv("STEGO_TEST_SPICEDB_KEY")
	if dsn == "" || endpoint == "" || spiceKey == "" {
		t.Fatal("the service test needs STEGO_TEST_POSTGRES_DSN, STEGO_TEST_SPICEDB_ENDPOINT and STEGO_TEST_SPICEDB_KEY")
	}
	ctx := context.Background()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	tag := randomHex(t, 6)
	database := "shop_" + tag
	roles := [3]string{"shop_rt_" + tag, "shop_worker_" + tag, "shop_relay_" + tag}
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		if _, err := root.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"'"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)")
		for _, role := range roles {
			_, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	admin, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	queueDDL, err := outbox.Migration(roles[0], roles[1])
	if err != nil {
		t.Fatal(err)
	}
	authzDDL, err := authz.Migration("authz", roles[0], roles[2], roles[1])
	if err != nil {
		t.Fatal(err)
	}
	auditDDL, err := audit.DDL(audit.Config{RuntimeRole: roles[0], RetentionMonths: 12, AnchorPeriod: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{queueDDL, "CREATE SCHEMA authz", "GRANT USAGE ON SCHEMA authz TO " + strings.Join(roles[:], ", "),
		authzDDL, auditDDL, tablesSQL}
	for _, table := range []string{"orders", "charges"} {
		statements = append(statements,
			"ALTER TABLE public."+table+" ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE public."+table+" FORCE ROW LEVEL SECURITY",
			"CREATE POLICY "+table+"_tenant ON public."+table+" USING (tenant_id = current_setting('app.tenant', true))",
			// INSERT is granted too, so only the READ ONLY transaction stops a write.
			"GRANT SELECT, INSERT ON public."+table+" TO "+roles[0])
	}
	for _, statement := range statements {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%.80s: %v", statement, err)
		}
	}
	for more := true; more; {
		if err := admin.QueryRowContext(ctx, "SELECT bool_or(step_left) FROM "+audit.MaintainFunction+"()").Scan(&more); err != nil {
			t.Fatalf("audit_maintain: %v", err)
		}
	}
	for _, row := range [][]any{{"t1", o1, "open", 10}, {"t1", o2, "open", 20}, {"t1", o3, "paid", 30}, {"t1", o4, "open", 40}, {"t2", o5, "open", 50}} {
		if _, err := admin.ExecContext(ctx, "INSERT INTO public.orders (tenant_id, id, status, total, api_token) VALUES ($1, $2, $3, $4, $5)", append(row, secretToken)...); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range [][]any{{"t1", c1, o1, "4242", 7}, {"t1", c2, o1, "1881", 3}} {
		if _, err := admin.ExecContext(ctx, "INSERT INTO public.charges (tenant_id, id, order_id, card_last4, amount) VALUES ($1, $2, $3, $4, $5)", row...); err != nil {
			t.Fatal(err)
		}
	}

	// The store pool reaches PostgreSQL through the wire recorder; the audit connections do not.
	w := &wire{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go w.serve(conn, u.Host)
		}
	}()
	runtimeURL := *u
	runtimeURL.User = url.UserPassword(roles[0], password)
	direct := runtimeURL
	runtimeURL.Host = listener.Addr().String()
	q := runtimeURL.Query()
	q.Set("sslmode", "disable")
	runtimeURL.RawQuery = q.Encode()
	db, err := tenant.Open(ctx, tenant.Config{InsecureLoopback: true, DSN: runtimeURL.String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	auditDB, err := tenant.Open(ctx, tenant.Config{InsecureLoopback: true, DSN: direct.String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auditDB.Close() })
	queue, err := outbox.New(db, outbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := audit.NewWriter(auditQueue{queue: queue}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Each preshared key is a datastore of its own on the serve-testing server.
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearer(spiceKey+tag)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	schemas := v1.NewSchemaServiceClient(conn)
	for deadline := time.Now().Add(60 * time.Second); ; {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = schemas.WriteSchema(wctx, &v1.WriteSchemaRequest{Schema: schemaZed})
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("write the schema to SpiceDB: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	s := &shop{admin: admin, wire: w, key: authntest.NewKey(t, "k1"), perms: v1.NewPermissionsServiceClient(conn)}
	s.relate(t, "t1", "order", o1, "viewer", "u-7")
	s.relate(t, "t1", "order", o2, "auditor", "u-7")
	s.relate(t, "t1", "order", o3, "viewer", "u-7")
	s.relate(t, "t2", "order", o5, "viewer", "u-7")
	s.relate(t, "t1", "charge", c1, "viewer", "u-7")
	s.relate(t, "t1", "charge", c2, "viewer", "u-7")
	provider, err := authz.NewSpiceDB(conn)
	if err != nil {
		t.Fatal(err)
	}
	s.rec = &decisions{inner: provider}
	az, err := authz.New(authz.Config{Provider: s.rec, DB: db, Schema: "authz", Outbox: queue})
	if err != nil {
		t.Fatal(err)
	}
	cursorKey := make([]byte, query.MinCursorKey)
	if _, err := rand.Read(cursorKey); err != nil {
		t.Fatal(err)
	}
	rt, err := query.New(query.Config{DB: db, AuditDB: auditDB, Audit: writer, AuditSettings: audit.Settings{Level: audit.LevelChange},
		Authz: az, Service: "shop", CursorKey: cursorKey})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := authn.ParseKeys(authntest.Document(s.key.JWK()))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := authn.New(authn.Settings{Issuer: issuer, Audience: audience, TenantClaim: "tenant", ProblemCode: "SHOP", Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	handlers, err := serve.New(serve.Config{Runtime: rt, Verifier: verifier, Probes: probe.NewRecorder(nil), SubjectType: "user"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	handlers.Routes(mux)
	// The stream watch-orders has no HTTP helper in the runtime library; this test carries its messages as JSON lines
	// through the generated run of the stream, which decides each value it sends.
	mux.Handle("GET /api/shop/v1/orders/watch", verifier.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := handlers.BindWatchOrders(r.Context(), "")
		if err != nil {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		encoder := json.NewEncoder(w)
		if _, err := handlers.StreamWatchOrders(r.Context(), b, queries.WatchOrdersRequest{}, func(o queries.Order) error {
			return encoder.Encode(o)
		}); err != nil {
			_ = encoder.Encode(map[string]string{"error": "the stream failed"})
		}
	})))
	// The completion log of each request gets its fixed code through the context, as the telemetry runtime's request
	// wrapper gives it (ST-SUP-RP03-LC12).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.WithErrorCode(r.Context())
		mux.ServeHTTP(w, r.WithContext(ctx))
		code, _ := otel.ErrorCode(ctx)
		s.codesMu.Lock()
		s.codes = append(s.codes, code)
		s.codesMu.Unlock()
	}))
	t.Cleanup(server.Close)
	s.url = server.URL + "/api/shop/v1"
	s.wire.take()
	return s
}

// relate writes the relationship relation of user u to the resource key of tenant in SpiceDB.
func (s *shop) relate(t *testing.T, tenantID, resourceType, key, relation, user string) {
	t.Helper()
	s.write(t, v1.RelationshipUpdate_OPERATION_TOUCH, tenantID, resourceType, key, relation, user)
}

func (s *shop) write(t *testing.T, op v1.RelationshipUpdate_Operation, tenantID, resourceType, key, relation, user string) {
	t.Helper()
	id := resourceID(tenantID, key)
	_, err := s.perms.WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: op,
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: id},
			Relation: relation,
			Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: subject(user)}},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
}

// token mints a token of user in tenant that expires after life.
func (s *shop) token(t *testing.T, user, tenantID string, life time.Duration) string {
	t.Helper()
	now := time.Now()
	claims := authntest.Claims(issuer, audience, user, now.Add(-time.Minute))
	claims["exp"] = now.Add(life).Unix()
	claims["tenant"] = tenantID
	return authntest.Mint(t, authntest.Header(s.key.ID), claims, s.key)
}

// get sends a GET of path with the bearer token, when it is not empty, and gives the status and the body.
func (s *shop) get(t *testing.T, token, path string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, s.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

// snapshot gives the row count and a digest of the rows of every table of the database.
func (s *shop) snapshot(t *testing.T) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := s.admin.QueryContext(ctx, `SELECT table_schema, table_name FROM information_schema.tables
		WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema') ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, fmt.Sprintf("%q.%q", schema, name))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, table := range tables {
		var count int64
		var digest string
		if err := s.admin.QueryRowContext(ctx, "SELECT count(*), coalesce(md5(string_agg(t::text, ',' ORDER BY t::text)), '') FROM "+table+" AS t").
			Scan(&count, &digest); err != nil {
			t.Fatal(err)
		}
		out[table] = fmt.Sprintf("%d %s", count, digest)
	}
	return out
}

// changed gives the tables whose rows differ between two snapshots.
func changed(before, after map[string]string) []string {
	var out []string
	for table, value := range after {
		if before[table] != value {
			out = append(out, table)
		}
	}
	slices.Sort(out)
	return out
}

var (
	beginReadOnly = regexp.MustCompile(`(?i)^\s*begin\b.*\bread only\b`)
	setTenant     = regexp.MustCompile(`(?i)set_config\(\s*'app\.tenant'\s*,.*,\s*true\s*\)`)
	selectOrders  = regexp.MustCompile(`(?i)\bfrom public\.orders\b`)
)

func TestService(t *testing.T) {
	s := start(t)
	user := s.token(t, "u-7", "t1", 5*time.Minute)

	t.Run("ST-SUP-RC33-OC04-T01 The fill runs in one read-only transaction with the caller's tenant", func(t *testing.T) {
		calls := getorder.Calls()
		s.wire.take()
		status, body := s.get(t, user, "/orders/"+o1)
		if status != http.StatusOK {
			t.Fatalf("get-order of o1: %d %s, want 200", status, body)
		}
		var order queries.Order
		if err := json.Unmarshal(body, &order); err != nil || order.ID != o1 || order.Total != 10 {
			t.Fatalf("get-order of o1 answers %s (%v), want the Order o1", body, err)
		}
		if got := getorder.Calls() - calls; got != 1 {
			t.Fatalf("the fill ran %d times, want once", got)
		}
		statements := s.wire.take()
		begins, tenantAt, readAt := 0, -1, -1
		for i, text := range statements {
			if beginReadOnly.MatchString(text) {
				begins++
			}
			if setTenant.MatchString(text) && tenantAt < 0 {
				tenantAt = i
			}
			if selectOrders.MatchString(text) && readAt < 0 {
				readAt = i
			}
		}
		if begins != 1 || tenantAt < 0 || readAt < 0 || tenantAt > readAt {
			t.Fatalf("the statements are %q: want one BEGIN READ ONLY, and set_config of app.tenant with is_local true before the first read of orders", statements)
		}
		if !slices.ContainsFunc(statements, func(text string) bool { return strings.Contains(text, "t1") }) &&
			!slices.ContainsFunc(statements, func(text string) bool { return setTenant.MatchString(text) }) {
			t.Fatalf("no statement sets the tenant t1: %q", statements)
		}
	})

	t.Run("ST-SUP-RC33-OC01-T02 A secret field reaches no loaded value and no response", func(t *testing.T) {
		for _, path := range []string{"/orders/" + o1, "/orders", "/orders/" + o1 + "/total"} {
			status, body := s.get(t, user, path)
			if status != http.StatusOK {
				t.Fatalf("%s: %d %s, want 200", path, status, body)
			}
			if bytes.Contains(body, []byte(secretToken)) || bytes.Contains(body, []byte("api_token")) {
				t.Fatalf("%s answers %s, which holds the secret field api_token", path, body)
			}
		}
		s.wire.take()
		s.get(t, user, "/orders/"+o1)
		for _, text := range s.wire.take() {
			if strings.Contains(text, "api_token") {
				t.Fatalf("the query read the secret column: %s", text)
			}
		}
	})

	t.Run("ST-SUP-RC33-OC05-T03 A get query decides its declared permission only, before the fill", func(t *testing.T) {
		evals, _, _ := s.rec.count()
		// u-7 may browse o2 and may not view it: get-order-total declares browse.
		status, body := s.get(t, user, "/orders/"+o2+"/total")
		if status != http.StatusOK || !bytes.Contains(body, []byte(`"id":"`+o2+`"`)) {
			t.Fatalf("get-order-total of o2: %d %s, want 200 for o2", status, body)
		}
		s.rec.mu.Lock()
		first := s.rec.evals[evals]
		s.rec.mu.Unlock()
		if first.Action.Name != "browse" || first.Resource.Type != "order" {
			t.Fatalf("get-order-total decided %s on %s before the fill, want the declared permission browse on order", first.Action.Name, first.Resource.Type)
		}
		calls := getordertotal.Calls()
		if status, body := s.get(t, user, "/orders/"+o4+"/total"); status != http.StatusNotFound {
			t.Fatalf("get-order-total of o4, which u-7 may not browse: %d %s, want 404", status, body)
		}
		if got := getordertotal.Calls() - calls; got != 0 {
			t.Fatalf("the fill ran %d times for o4, want never", got)
		}
	})

	t.Run("ST-SUP-RC33-OC05-T04 A get query's declared permission reaches only the requested row", func(t *testing.T) {
		// The fill reads o2, which u-7 may browse and may not view, while it serves o1: its repository decides view.
		evals, _, _ := s.rec.count()
		status, body := s.get(t, user, "/orders/"+o1+"/total")
		if status != http.StatusOK || !bytes.Contains(body, []byte(`"peek":"not found"`)) {
			t.Fatalf("get-order-total of o1: %d %s, want 200 with the read of o2 not found", status, body)
		}
		s.rec.mu.Lock()
		var actions []string
		for _, e := range s.rec.evals[evals:] {
			actions = append(actions, e.Action.Name)
		}
		s.rec.mu.Unlock()
		if !slices.Equal(actions, []string{"browse", "view"}) {
			t.Fatalf("get-order-total decided %v, want browse on o1 before the fill and view on the fill's read of o2", actions)
		}
	})

	t.Run("ST-SUP-RC33-OC04-T02 An unauthenticated query never reaches the fill", func(t *testing.T) {
		calls := getorder.Calls()
		s.wire.take()
		expired := s.token(t, "u-7", "t1", -time.Minute)
		for name, token := range map[string]string{"no token": "", "an expired token": expired} {
			if status, body := s.get(t, token, "/orders/"+o1); status != http.StatusUnauthorized || bytes.Contains(body, []byte("total")) {
				t.Errorf("get-order with %s: %d %s, want 401 with no data", name, status, body)
			}
		}
		if statements := s.wire.take(); len(statements) != 0 {
			t.Errorf("the unauthenticated requests ran the statements %q, want none", statements)
		}
		if got := getorder.Calls() - calls; got != 0 {
			t.Errorf("the fill ran %d times, want never", got)
		}
	})

	t.Run("ST-SUP-RP11-LC17-T01 The generated get handler answers a denied row 404, as a missing row, and calls no fill", func(t *testing.T) {
		calls := getorder.Calls()
		evals, _, _ := s.rec.count()
		// o4 is a row the caller may not view, o9 a row that is not there, o5 a row of another tenant.
		var bodies [][]byte
		for _, key := range []string{o4, o9, o5} {
			status, body := s.get(t, user, "/orders/"+key)
			if status != http.StatusNotFound {
				t.Fatalf("get-order of %s: %d %s, want 404", key, status, body)
			}
			if bytes.Contains(body, []byte(key)) || bytes.Contains(body, []byte("total")) {
				t.Fatalf("the 404 of %s holds data of the row: %s", key, body)
			}
			bodies = append(bodies, body)
		}
		if !bytes.Equal(bodies[0], bodies[1]) || !bytes.Equal(bodies[1], bodies[2]) {
			t.Fatalf("the denied, the missing and the other tenant's row answer %q, want one answer", bodies)
		}
		if got := getorder.Calls() - calls; got != 0 {
			t.Fatalf("the fill ran %d times for rows the caller may not read, want never", got)
		}
		after, _, _ := s.rec.count()
		if after-evals != 3 {
			t.Fatalf("the three reads made %d decisions, want one each, also for the missing row", after-evals)
		}
		s.rec.mu.Lock()
		last := s.rec.evals[len(s.rec.evals)-3]
		s.rec.mu.Unlock()
		if last.Action.Name != "view" || last.Resource.Type != "order" {
			t.Fatalf("the decision before the fill is %s on %s, want the query's permission view on order", last.Action.Name, last.Resource.Type)
		}
		// The fill's declared refusal is answered with its status and its code through the generated lookup
		// queries.Declared (ST-SUP-RC33-OC07).
		if _, err := s.admin.ExecContext(context.Background(), `UPDATE orders SET status = 'lost' WHERE id = $1::uuid`, o3); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = s.admin.ExecContext(context.Background(), `UPDATE orders SET status = 'paid' WHERE id = $1::uuid`, o3)
		}()
		status, body := s.get(t, user, "/orders/"+o3)
		if status != http.StatusNotFound || !bytes.Contains(body, []byte(`"code":"order.unknown"`)) {
			t.Fatalf("the refusal order_unknown answered %d %s, want 404 with the code order.unknown", status, body)
		}
	})

	t.Run("ST-SUP-RC33-OC07-T01 A query fill's error text reaches no response, audit row or completion log", func(t *testing.T) {
		lastCode := func() string {
			s.codesMu.Lock()
			defer s.codesMu.Unlock()
			return s.codes[len(s.codes)-1]
		}
		var auditBefore int
		if err := s.admin.QueryRow("SELECT count(*) FROM audit_event").Scan(&auditBefore); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = s.admin.ExecContext(context.Background(), `UPDATE orders SET status = 'paid' WHERE id = $1::uuid`, o3)
		}()
		// The fill returns an error whose text holds an email and SQL text: 500 with the code internal only.
		if _, err := s.admin.ExecContext(context.Background(), `UPDATE orders SET status = 'broken' WHERE id = $1::uuid`, o3); err != nil {
			t.Fatal(err)
		}
		status, body := s.get(t, user, "/orders/"+o3)
		if status != http.StatusInternalServerError || !bytes.Contains(body, []byte(`"code":"internal"`)) || lastCode() != "internal" {
			t.Fatalf("the failed fill answered %d %s with the log code %q, want 500 and the code internal in both", status, body, lastCode())
		}
		for _, private := range []string{"a@example.com", "select 1"} {
			if bytes.Contains(body, []byte(private)) || strings.Contains(lastCode(), private) {
				t.Fatalf("the answer %s or the log code %q holds %q", body, lastCode(), private)
			}
		}
		// The declared refusal: 404 with the code order.unknown in the answer and the completion log.
		if _, err := s.admin.ExecContext(context.Background(), `UPDATE orders SET status = 'lost' WHERE id = $1::uuid`, o3); err != nil {
			t.Fatal(err)
		}
		status, body = s.get(t, user, "/orders/"+o3)
		if status != http.StatusNotFound || !bytes.Contains(body, []byte(`"code":"order.unknown"`)) || lastCode() != "order.unknown" {
			t.Fatalf("the refusal order_unknown answered %d %s with the log code %q, want 404 and the code order.unknown in both", status, body, lastCode())
		}
		// The read audit rows stay as the reads were: no row takes the code of the outcome, and none holds the
		// private text (ST-SUP-Q95, option (a)).
		var rows int
		if err := s.admin.QueryRow(`SELECT count(*) FROM audit_event WHERE error_code IN ('internal', 'order.unknown')
 OR to_jsonb(audit_event)::text LIKE '%a@example.com%' OR to_jsonb(audit_event)::text LIKE '%select 1%'`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		var auditAfter int
		if err := s.admin.QueryRow("SELECT count(*) FROM audit_event").Scan(&auditAfter); err != nil {
			t.Fatal(err)
		}
		if rows != 0 || auditAfter != auditBefore {
			t.Fatalf("%d audit rows hold the outcome code or the private text, and the queries wrote %d audit rows of an entity with no pii field, want none", rows, auditAfter-auditBefore)
		}
	})

	t.Run("ST-SUP-RC33-OC05-T01 A denied query answers 404 and does not call the fill", func(t *testing.T) {
		// The row o4 holds the token t5, and the revocation floor is a newer token that SpiceDB gave: the decision
		// before the fill runs at the floor, not at the row's token (ST-SUP-RP06-LC09).
		_, err := s.admin.ExecContext(context.Background(), `UPDATE orders SET authz_token = 't5' WHERE id = $1::uuid`, o4)
		if err != nil {
			t.Fatal(err)
		}
		written, err := s.perms.WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "order", ObjectId: "floor-mark"}, Relation: "viewer",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "nobody"}}}}}})
		if err != nil {
			t.Fatal(err)
		}
		floorText := written.GetWrittenAt().GetToken()
		if _, err := s.admin.ExecContext(context.Background(), `UPDATE authz.authz_floor SET token = $1`, floorText); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = s.admin.ExecContext(context.Background(), `UPDATE authz.authz_floor SET token = ''`) }()
		calls := getorder.Calls()
		evals, _, _ := s.rec.count()
		status, body := s.get(t, user, "/orders/"+o4)
		missingStatus, missingBody := s.get(t, user, "/orders/"+o9)
		if status != http.StatusNotFound || missingStatus != http.StatusNotFound || !bytes.Equal(body, missingBody) {
			t.Fatalf("the denied get-order answered %d %s and the missing one %d %s, want the same 404", status, body, missingStatus, missingBody)
		}
		if got := getorder.Calls() - calls; got != 0 {
			t.Fatalf("the fill ran %d times, want never", got)
		}
		s.rec.mu.Lock()
		decided := append([]authz.EvaluationRequest(nil), s.rec.evals[evals:]...)
		s.rec.mu.Unlock()
		floor, err := authz.ParseToken(floorText)
		if err != nil {
			t.Fatal(err)
		}
		if len(decided) != 2 || decided[0].Action.Name != "view" || decided[0].Resource.Type != "order" ||
			decided[0].Resource.ID != resourceID("t1", o4) || decided[0].Consistency != authz.AtLeast(floor) {
			t.Fatalf("the decisions were %+v, want view on order:o4 at least as fresh as the floor, and one for the missing row", decided)
		}
	})

	t.Run("ST-SUP-RC33-OC05-T02 A list query is decided row by row on its declared permission", func(t *testing.T) {
		evals, batches, finds := s.rec.count()
		var before [3]int
		listorders.OnEnter(func() { before[0], before[1], before[2] = s.rec.count() })
		defer listorders.OnEnter(nil)
		var ids []string
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatal("the list does not end")
			}
			path := "/orders?limit=1"
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			status, body := s.get(t, user, path)
			if status != http.StatusOK {
				t.Fatalf("list-orders: %d %s, want 200", status, body)
			}
			var page struct {
				Items []queries.Order `json:"items"`
				Next  string          `json:"next"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) > 1 {
				t.Fatalf("a page of limit 1 holds %d Orders", len(page.Items))
			}
			for _, o := range page.Items {
				ids = append(ids, o.ID)
			}
			if pages == 0 && (before[0] != evals || before[1] != batches || before[2] != finds) {
				t.Fatalf("the handler decided before the fill of a list query: %v, want %d %d %d", before, evals, batches, finds)
			}
			if page.Next == "" {
				break
			}
			if strings.Contains(page.Next, o1) || strings.Contains(page.Next, "00000000") {
				t.Fatalf("the cursor %s shows a key", page.Next)
			}
			cursor = page.Next
		}
		if !slices.Equal(ids, []string{o1, o3}) {
			t.Fatalf("the pages hold %v, want only the Orders o1 and o3, which the caller may view", ids)
		}
		s.rec.mu.Lock()
		defer s.rec.mu.Unlock()
		if len(s.rec.batch) == batches {
			t.Fatal("the list made no batch evaluation")
		}
		for _, b := range s.rec.batch[batches:] {
			for _, item := range b.Evaluations {
				if item.Action.Name != "view" {
					t.Fatalf("the list decided %s, want the query's declared permission view", item.Action.Name)
				}
			}
		}
	})

	t.Run("ST-SUP-RP11-LC15-T01 The generated list handler refuses a changed cursor and the cursor of another principal", func(t *testing.T) {
		status, body := s.get(t, user, "/orders?limit=1")
		var page struct {
			Next string `json:"next"`
		}
		if err := json.Unmarshal(body, &page); status != http.StatusOK || err != nil || page.Next == "" {
			t.Fatalf("list-orders: %d %s, want a page with a next cursor", status, body)
		}
		raw := []byte(page.Next)
		raw[len(raw)/2] ^= 1
		other := s.token(t, "u-8", "t1", 5*time.Minute)
		for name, request := range map[string]struct{ token, cursor string }{
			"a changed cursor":                          {user, string(raw)},
			"the cursor of another principal":           {other, page.Next},
			"the cursor of another tenant":              {s.token(t, "u-7", "t2", 5*time.Minute), page.Next},
			"the cursor of list-orders on list-charges": {user, page.Next},
		} {
			path := "/orders?limit=1&cursor=" + url.QueryEscape(request.cursor)
			if strings.Contains(name, "list-charges") {
				path = "/charges?limit=1&cursor=" + url.QueryEscape(request.cursor)
			}
			if status, body := s.get(t, request.token, path); status != http.StatusBadRequest || bytes.Contains(body, []byte("items")) {
				t.Errorf("%s: %d %s, want 400 with no rows", name, status, body)
			}
		}
		if status, body := s.get(t, user, "/orders?limit=1&cursor="+url.QueryEscape(page.Next)); status != http.StatusOK {
			t.Errorf("the unchanged cursor: %d %s, want the next page", status, body)
		}
	})

	t.Run("ST-SUP-RC33-OC06-T01 A query leaves the tables unchanged", func(t *testing.T) {
		before := s.snapshot(t)
		if status, body := s.get(t, user, "/orders/"+o1); status != http.StatusOK {
			t.Fatalf("get-order: %d %s", status, body)
		}
		if status, body := s.get(t, user, "/orders"); status != http.StatusOK {
			t.Fatalf("list-orders: %d %s", status, body)
		}
		if tables := changed(before, s.snapshot(t)); len(tables) != 0 {
			t.Fatalf("the queries of Orders, which hold no pii field, changed %v, want no table changed", tables)
		}
	})

	t.Run("ST-SUP-RC33-OC06-T02 A query of personal data writes only its read audit rows and their sink entries", func(t *testing.T) {
		before := s.snapshot(t)
		var audits, entries int
		count := func() (int, int) {
			var a, e int
			if err := s.admin.QueryRow("SELECT count(*) FROM audit_event WHERE action = 'read' AND operation = 'list-charges'").Scan(&a); err != nil {
				t.Fatal(err)
			}
			if err := s.admin.QueryRow("SELECT count(*) FROM stego_outbox.messages WHERE kind = 'audit_event'").Scan(&e); err != nil {
				t.Fatal(err)
			}
			return a, e
		}
		audits, entries = count()
		status, body := s.get(t, user, "/charges")
		if status != http.StatusOK || !bytes.Contains(body, []byte(c1)) || !bytes.Contains(body, []byte(c2)) {
			t.Fatalf("list-charges: %d %s, want the two Charges", status, body)
		}
		a, e := count()
		if a-audits != 2 || e-entries != 2 {
			t.Fatalf("the read of two Charges wrote %d read audit rows and %d sink entries, want two of each", a-audits, e-entries)
		}
		var rows int
		if err := s.admin.QueryRow("SELECT count(*) FROM audit_event WHERE operation = 'list-charges' AND resource_id IN ($1, $2) AND before::text NOT LIKE '%4242%' AND after::text NOT LIKE '%4242%' AND after::text NOT LIKE '%1881%'", c1, c2).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 2 {
			t.Fatalf("%d of the read audit rows name the Charges with card_last4 redacted, want 2", rows)
		}
		for _, table := range changed(before, s.snapshot(t)) {
			// The audit rows are the partitions of audit_event and the head of each tenant's hash chain.
			if !strings.HasPrefix(table, `"public"."audit_`) && table != `"stego_outbox"."messages"` {
				t.Errorf("the read of personal data changed %s, want only the audit rows and their sink entries", table)
			}
		}
	})

	t.Run("ST-SUP-RP11-LC13-T01 The generated stream sends only the Orders the caller may view", func(t *testing.T) {
		status, body := s.get(t, user, "/orders/watch")
		if status != http.StatusOK {
			t.Fatalf("watch-orders: %d %s", status, body)
		}
		var ids []string
		for _, line := range bytes.Split(bytes.TrimSpace(body), []byte("\n")) {
			var o queries.Order
			if err := json.Unmarshal(line, &o); err != nil || o.ID == "" {
				t.Fatalf("the stream sent %s, want an Order", line)
			}
			ids = append(ids, o.ID)
		}
		// The caller may browse o1, o2 and o3, so the fill lists and sends all three; it may view o1 and o3 only.
		if !slices.Equal(ids, []string{o1, o3}) {
			t.Fatalf("the stream sent %v, want o1 and o3 only, with o2 dropped", ids)
		}
		// After a revocation of view on o3 the stream drops o3 too.
		s.write(t, v1.RelationshipUpdate_OPERATION_DELETE, "t1", "order", o3, "viewer", "u-7")
		s.relate(t, "t1", "order", o3, "auditor", "u-7")
		_, body = s.get(t, user, "/orders/watch")
		if bytes.Contains(body, []byte(o3)) || !bytes.Contains(body, []byte(o1)) {
			t.Fatalf("after the revocation the stream sent %s, want o1 only", body)
		}
	})
}
