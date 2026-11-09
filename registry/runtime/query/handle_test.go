package query

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"stego.dev/runtime/audit"
	"stego.dev/runtime/authz"
	"stego.dev/runtime/internal/pgtest"
	"stego.dev/runtime/internal/seal"
	"stego.dev/runtime/internal/staging"
	"stego.dev/runtime/outbox"
	"stego.dev/runtime/probe"
	"stego.dev/runtime/tenant"
)

func TestMain(m *testing.M) {
	code := pgtest.Main(m)
	stopSpiceDB()
	os.Exit(code)
}

// decided is the decision that tenant.Requested takes, made with the mark of the internal seal constructor, so a
// test can build the tenants it needs.
type decided struct{ seal.DecisionMark }

func mustTenant(t *testing.T, id string) tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(tenant.Requested(tenant.ProvisionAction, id, decided{seal.NewDecisionMark(id, "provision")}))
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

// mustPrincipal is the principal of the subject type:id, as the command runtime builds the staging principal of a
// claimed row; it gives the test a subject such as user:u-7 with no identity provider.
func mustPrincipal(t *testing.T, subject string) authz.Principal {
	t.Helper()
	p, err := authz.StagingPrincipal(staging.NewStaging(), subject)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeDecision is a recording decision port. decide gives the allow of each evaluation; search, when it is set,
// answers resource searches.
type fakeDecision struct {
	mu      sync.Mutex
	calls   []authz.EvaluationRequest
	batches [][]authz.EvaluationRequest
	decide  func(authz.EvaluationRequest) bool
	search  func(authz.SearchRequest) authz.SearchResult
	asked   []authz.SearchRequest
}

func allowAll(authz.EvaluationRequest) bool { return true }

func (f *fakeDecision) Evaluate(_ context.Context, r authz.EvaluationRequest) (authz.EvaluationResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, r)
	decide := f.decide
	f.mu.Unlock()
	return authz.EvaluationResult{Allowed: decide(r)}, nil
}

func (f *fakeDecision) EvaluateBatch(_ context.Context, b authz.BatchRequest) ([]authz.EvaluationResult, error) {
	f.mu.Lock()
	f.batches = append(f.batches, append([]authz.EvaluationRequest(nil), b.Evaluations...))
	f.calls = append(f.calls, b.Evaluations...)
	decide := f.decide
	f.mu.Unlock()
	results := make([]authz.EvaluationResult, len(b.Evaluations))
	for i, r := range b.Evaluations {
		results[i] = authz.EvaluationResult{Allowed: decide(r)}
	}
	return results, nil
}

func (f *fakeDecision) Search(_ context.Context, r authz.SearchRequest) (authz.SearchResult, error) {
	if f.search == nil {
		return authz.SearchResult{}, authz.ErrNoSearch
	}
	f.mu.Lock()
	f.asked = append(f.asked, r)
	f.mu.Unlock()
	return f.search(r), nil
}

func (f *fakeDecision) Searches() bool { return f.search != nil }

func (f *fakeDecision) recorded() []authz.EvaluationRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]authz.EvaluationRequest(nil), f.calls...)
}

// statements is a pgx tracer that records the SQL text and the arguments of each statement of a connection.
type statements struct {
	mu   sync.Mutex
	seen []statement
}

type statement struct {
	sql  string
	args []any
}

func (s *statements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	// The database/sql driver of pgx passes the result formats as a first argument; it is not a statement argument.
	var args []any
	for _, arg := range data.Args {
		if _, ok := arg.(pgx.QueryResultFormatsByOID); !ok {
			args = append(args, arg)
		}
	}
	s.mu.Lock()
	s.seen = append(s.seen, statement{sql: data.SQL, args: args})
	s.mu.Unlock()
	return ctx
}

func (s *statements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *statements) all() []statement {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]statement(nil), s.seen...)
}

// auditQueue is the audit writer's Enqueuer on the real outbox queue: each audit row becomes one outbox message in
// the same transaction.
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

const rolePassword = "query-test"

// env is one database of the local PostgreSQL server of pgtest with the outbox queue, the authz tables, the audit
// tables and three tenant-owned entity tables with row-level security, and a runtime role that is no superuser, so
// row-level security and the grants apply. The runtime role's pools come from tenant.Open.
type env struct {
	admin   *sql.DB
	dsn     string
	runtime string
	db      *sql.DB // tenant.Open, the store pool
	auditDB *sql.DB // tenant.Open, the audit connections
	queue   *outbox.Queue
	writer  *audit.Writer
}

func newEnv(t *testing.T) *env {
	t.Helper()
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
	roles := [3]string{"query_rt_" + tag, "query_worker_" + tag, "query_relay_" + tag}
	for _, role := range roles {
		if _, err := root.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+rolePassword+"'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role) })
	}
	dsn := pgtest.Database(t, "query")
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
	authzDDL, err := authz.Migration("authz", roles[0], roles[2], roles[1])
	if err != nil {
		t.Fatal(err)
	}
	auditDDL, err := audit.DDL(audit.Config{RuntimeRole: roles[0], RetentionMonths: 12, AnchorPeriod: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{queueDDL, "CREATE SCHEMA authz", "GRANT USAGE ON SCHEMA authz TO " + roles[0] + ", " + roles[1] + ", " + roles[2],
		authzDDL, auditDDL, "CREATE SCHEMA app", "GRANT USAGE ON SCHEMA app TO " + roles[0]}
	for _, table := range []string{
		"reservations (tenant_id text NOT NULL, id text NOT NULL, sku text NOT NULL, PRIMARY KEY (tenant_id, id))",
		"orders (tenant_id text NOT NULL, id text NOT NULL, total bigint NOT NULL, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))",
		"charges (tenant_id text NOT NULL, id text NOT NULL, card_last4 text NOT NULL, amount bigint NOT NULL, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))",
		"tickets (tenant_id text NOT NULL, id uuid NOT NULL, PRIMARY KEY (tenant_id, id))",
	} {
		name, _, _ := strings.Cut(table, " ")
		statements = append(statements, "CREATE TABLE app."+table,
			"ALTER TABLE app."+name+" ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE app."+name+" FORCE ROW LEVEL SECURITY",
			"CREATE POLICY "+name+"_tenant ON app."+name+" USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
			// INSERT is granted, so only the READ ONLY transaction stops a write.
			"GRANT SELECT, INSERT ON app."+name+" TO "+roles[0])
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
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(roles[0], rolePassword)
	e := &env{admin: admin, dsn: u.String(), runtime: roles[0]}
	e.db = e.open(t)
	e.auditDB = e.open(t)
	if e.queue, err = outbox.New(e.db, outbox.Options{}); err != nil {
		t.Fatal(err)
	}
	if e.writer, err = audit.NewWriter(auditQueue{queue: e.queue}, nil); err != nil {
		t.Fatal(err)
	}
	return e
}

// open opens a pool of the runtime role with tenant.Open.
func (e *env) open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := tenant.Open(context.Background(), tenant.Config{DSN: e.dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// traced opens a pool of the runtime role whose statements rec records. It is not a pool of tenant.Open, so a test
// gives it only to bind, as Run gives bind its transaction.
func (e *env) traced(t *testing.T, rec *statements) *sql.DB {
	t.Helper()
	cfg, err := pgx.ParseConfig(e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tracer = rec
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// runtimeOf is the query runtime of the env with the decision port provider.
func (e *env) runtimeOf(t *testing.T, provider authz.Decision) *Runtime {
	t.Helper()
	az, err := authz.New(authz.Config{Provider: provider, DB: e.db, Schema: "authz", Outbox: e.queue})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(Config{DB: e.db, AuditDB: e.auditDB, Audit: e.writer, AuditSettings: audit.Settings{Level: audit.LevelChange},
		Authz: az, Service: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// insert writes rows of a tenant as the admin, which row-level security does not hold.
func (e *env) insert(t *testing.T, statement string, args ...any) {
	t.Helper()
	if _, err := e.admin.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("%.60s: %v", statement, err)
	}
}

// The entities of the tests.
type Reservation struct{ ID, SKU string }
type Order struct {
	ID    string
	Total int64
}
type Charge struct {
	ID, CardLast4 string
	Amount        int64
}

func reservationEntity(t *testing.T) *Entity[Reservation] {
	t.Helper()
	e, err := NewEntity(Spec{Name: "reservation", Schema: "app", Table: "reservations", Key: "id", Columns: []string{"id", "sku"}},
		func(r Row) (Reservation, error) {
			id, err := r.Text("id")
			if err != nil {
				return Reservation{}, err
			}
			sku, err := r.Text("sku")
			return Reservation{ID: id, SKU: sku}, err
		})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func orderEntity(t *testing.T, search bool) *Entity[Order] {
	t.Helper()
	e, err := NewEntity(Spec{Name: "order", Schema: "app", Table: "orders", Key: "id", Columns: []string{"id", "total"},
		ResourceType: "order", ReadPermission: "view", ListPermission: "view", Search: search},
		func(r Row) (Order, error) {
			id, err := r.Text("id")
			if err != nil {
				return Order{}, err
			}
			total, err := r.Int("total")
			return Order{ID: id, Total: total}, err
		})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func chargeEntity(t *testing.T) *Entity[Charge] { return chargeEntityWith(t, false) }

// chargeEntityWith is the Charge entity, whose list uses the resource search when search is true.
func chargeEntityWith(t *testing.T, search bool) *Entity[Charge] {
	t.Helper()
	e, err := NewEntity(Spec{Name: "charge", Schema: "app", Table: "charges", Key: "id", Columns: []string{"id", "card_last4", "amount"},
		ResourceType: "charge", ReadPermission: "view", ListPermission: "view", Fields: audit.Fields{"card_last4": audit.PII},
		Search: search},
		func(r Row) (Charge, error) {
			id, err := r.Text("id")
			if err != nil {
				return Charge{}, err
			}
			card, err := r.Text("card_last4")
			if err != nil {
				return Charge{}, err
			}
			amount, err := r.Int("amount")
			return Charge{ID: id, CardLast4: card, Amount: amount}, err
		})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mustQuery(t *testing.T, name string, reads ...Readable) *Query {
	t.Helper()
	q, err := NewQuery(name, reads...)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func mustListQuery(t *testing.T, name string, lists Readable, permission string, reads ...Readable) *Query {
	t.Helper()
	q, err := NewListQuery(name, lists, permission, reads...)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// noProbe is the typed probe of a query that declares no observation.
type noProbe struct{}

func newNoProbe(*probe.Probe) noProbe { return noProbe{} }

// bound gives Run the tenant, the principal and a probe call of a recorder bound to them and the query name.
func bound(t *testing.T, tenantID, subject, queryName string) Bound[noProbe] {
	t.Helper()
	call := probe.NewRecorder(nil).Bind(context.Background(), tenantID, subject, queryName)
	return Bound[noProbe]{Tenant: mustTenant(t, tenantID), Principal: mustPrincipal(t, subject), Call: call, Probe: newNoProbe}
}

func TestHandle(t *testing.T) {
	t.Run("ST-SUP-RP11-LC01-T01 The query handle reports the tenant and principal it was bound with", func(t *testing.T) {
		e := newEnv(t)
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		q := mustQuery(t, "open-reservations", reservationEntity(t))
		var readOnly, isolation string
		_, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "open-reservations"), struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (struct{}, error) {
				if got := h.Tenant().ID(); got != "t1" {
					t.Errorf("the tenant is %q, want t1", got)
				}
				if got := h.Principal().Subject().String(); got != "user:u-7" {
					t.Errorf("the principal is %q, want user:u-7", got)
				}
				if err := h.core.tx.QueryRowContext(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
					return struct{}{}, err
				}
				return struct{}{}, h.core.tx.QueryRowContext(ctx, "SHOW transaction_read_only").Scan(&readOnly)
			})
		if err != nil {
			t.Fatal(err)
		}
		if readOnly != "on" {
			t.Errorf("transaction_read_only is %q, want on", readOnly)
		}
		if isolation != "read committed" {
			t.Errorf("transaction_isolation is %q, want read committed", isolation)
		}
	})

	t.Run("ST-SUP-RP11-LC02-T02 The query handle reaches only the entities the query reads", func(t *testing.T) {
		e := newEnv(t)
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		rec := &statements{}
		tx, err := tenant.Begin(context.Background(), e.traced(t, rec), mustTenant(t, "t1"), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		reservations, charges := reservationEntity(t), chargeEntity(t)
		h, err := bind(context.Background(), rt, tx, mustTenant(t, "t1"), mustPrincipal(t, "user:u-7"),
			mustQuery(t, "open-reservations", reservations), noProbe{})
		if err != nil {
			t.Fatal(err)
		}
		before := len(rec.all())
		if _, err := Repo(h, charges); !errors.Is(err, ErrNotRead) {
			t.Errorf("Repo of Charge gives %v, want ErrNotRead", err)
		}
		if got := rec.all()[before:]; len(got) != 0 {
			t.Errorf("Repo of Charge ran %d statements, want none: %v", len(got), got)
		}
		// A second declaration of the same table under the name reservation is not the declaration of the query.
		twin := reservationEntity(t)
		if _, err := Repo(h, twin); !errors.Is(err, ErrNotRead) {
			t.Errorf("Repo of a twin declaration gives %v, want ErrNotRead", err)
		}
		if _, err := Repo(h, reservations); err != nil {
			t.Errorf("Repo of the declared entity: %v", err)
		}
	})

	t.Run("ST-SUP-RP11-LC03-T01 A query fill cannot read a row of another tenant", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.reservations (tenant_id, id, sku) VALUES ('t2', 'r-1', 'sku-9')")
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		rec := &statements{}
		tx, err := tenant.Begin(context.Background(), e.traced(t, rec), mustTenant(t, "t1"), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		reservations := reservationEntity(t)
		h, err := bind(context.Background(), rt, tx, mustTenant(t, "t1"), mustPrincipal(t, "user:u-7"),
			mustQuery(t, "open-reservations", reservations), noProbe{})
		if err != nil {
			t.Fatal(err)
		}
		repo, err := Repo(h, reservations)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Get(context.Background(), "r-1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get of the row of t2 gives %v, want ErrNotFound", err)
		}
		list, err := repo.List(context.Background(), ListRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 0 {
			t.Errorf("the list holds %d rows of t2, want none", len(list.Items))
		}
		var setConfig, reads int
		for _, s := range rec.all() {
			if strings.Contains(s.sql, "set_config('app.tenant', $1, true)") && len(s.args) == 1 && s.args[0] == "t1" {
				setConfig++
			}
			if strings.Contains(s.sql, "FROM app.reservations") {
				reads++
				if !strings.Contains(s.sql, TenantColumn+" = $1") || len(s.args) == 0 || s.args[0] != "t1" {
					t.Errorf("the read %q with %v has no tenant predicate on t1", s.sql, s.args)
				}
			}
		}
		if setConfig != 1 {
			t.Errorf("%d statements set app.tenant to t1 with is_local true, want 1", setConfig)
		}
		if reads != 2 {
			t.Errorf("%d reads of app.reservations, want 2", reads)
		}
	})

	t.Run("ST-SUP-RP11-LC03-T01 A list of the tenant's rows pages a uuid key from its first row", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.tickets (tenant_id, id) VALUES ('t1', '00000000-0000-7000-8000-000000000001'), ('t1', '00000000-0000-7000-8000-000000000002'), ('t2', '00000000-0000-7000-8000-000000000003')")
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		tickets, err := NewEntity(Spec{Name: "ticket", Schema: "app", Table: "tickets", Key: "id", Columns: []string{"id"}},
			func(r Row) (string, error) { return r.Text("id") })
		if err != nil {
			t.Fatal(err)
		}
		ids, err := Run(context.Background(), rt, mustQuery(t, "tickets", tickets), bound(t, "t1", "user:u-7", "tickets"), struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[noProbe]) ([]string, error) {
				repo, err := Repo(h, tickets)
				if err != nil {
					return nil, err
				}
				var ids []string
				var after Cursor
				for {
					page, err := repo.List(ctx, ListRequest{Limit: 1, After: after})
					if err != nil {
						return nil, err
					}
					for _, item := range page.Items {
						ids = append(ids, item.Value())
					}
					if !page.More {
						return ids, nil
					}
					after = page.Next
				}
			})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(ids, ",") != "00000000-0000-7000-8000-000000000001,00000000-0000-7000-8000-000000000002" {
			t.Errorf("the pages hold %v, want the two tickets of t1", ids)
		}
	})

	t.Run("ST-SUP-RP11-LC04-T02 A write in the bound transaction fails in the database", func(t *testing.T) {
		e := newEnv(t)
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		q := mustQuery(t, "open-reservations", reservationEntity(t))
		var insertErr error
		_, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "open-reservations"), struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (struct{}, error) {
				_, insertErr = h.core.tx.ExecContext(ctx, "INSERT INTO app.reservations (tenant_id, id, sku) VALUES ('t1', 'r-9', 'x')")
				return struct{}{}, nil
			})
		var pgErr *pgconn.PgError
		if !errors.As(insertErr, &pgErr) || pgErr.Code != "25006" {
			t.Errorf("the INSERT gives %v, want the read-only transaction error 25006", insertErr)
		}
		_ = err
		var n int
		if err := e.admin.QueryRowContext(context.Background(), "SELECT count(*) FROM app.reservations").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("the table holds %d rows, want none", n)
		}
	})
}
