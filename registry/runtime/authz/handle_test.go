package authz

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"stego.dev/runtime/internal/seal"
	"stego.dev/runtime/tenant"
)

// The unit tests bind a handle to a real *sql.Tx of a database/sql driver of the test that holds no data: the
// handle's evaluation sends no statement.
type nopDriver struct{}

func (nopDriver) Open(string) (driver.Conn, error) { return nopConn{}, nil }

type nopConn struct{}

func (nopConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("the test driver runs no statement")
}
func (nopConn) Close() error              { return nil }
func (nopConn) Begin() (driver.Tx, error) { return nopTx{}, nil }

type nopTx struct{}

func (nopTx) Commit() error   { return nil }
func (nopTx) Rollback() error { return nil }

var registerNop sync.Once

func fakeDB(t *testing.T) *sql.DB {
	t.Helper()
	registerNop.Do(func() { sql.Register("authz-nop", nopDriver{}) })
	db, err := sql.Open("authz-nop", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func fakeTx(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := fakeDB(t).Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// allowedDecision is an allow of the tests, for tenant.Requested.
type allowedDecision struct{ seal.DecisionMark }

func (allowedDecision) Allowed() bool { return true }

func mustTenant(t *testing.T, id string) tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(tenant.Requested(id, allowedDecision{}))
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

// outboxRecorder records the messages that the handle enqueues.
type outboxRecorder struct {
	mu       sync.Mutex
	messages []Message
}

func (o *outboxRecorder) enqueue(_ context.Context, tx *sql.Tx, message Message) error {
	if tx == nil {
		return errors.New("no transaction")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.messages = append(o.messages, message)
	return nil
}

func (o *outboxRecorder) all() []Message {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Message(nil), o.messages...)
}

func noStates(context.Context, []uuid.UUID) (map[uuid.UUID]MessageState, error) {
	return map[uuid.UUID]MessageState{}, nil
}

func newUnitRuntime(t *testing.T, provider Decision, floor Floor) *Runtime {
	t.Helper()
	outbox := &outboxRecorder{}
	rt, err := New(Config{Provider: provider, Floor: floor, DB: fakeDB(t), Schema: "public", Enqueue: outbox.enqueue,
		States: noStates})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func bindUnit(t *testing.T, rt *Runtime, principal Principal, allowed ...Relation) *Handle {
	t.Helper()
	handle, err := rt.Bind(fakeTx(t), mustTenant(t, "t1"), principal, allowed)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

// fakeRelationships is a recording fake of the relationships port: it answers each write with the next of its
// tokens and the error that fail gives.
type fakeRelationships struct {
	mu      sync.Mutex
	touches [][]Tuple
	deletes [][]Filter
	tokens  []Token
	fail    func(n int) error
	calls   int
}

func (f *fakeRelationships) answer() (Token, error) {
	f.calls++
	if f.fail != nil {
		if err := f.fail(f.calls); err != nil {
			return Token{}, err
		}
	}
	if len(f.tokens) == 0 {
		return tok(f.calls), nil
	}
	token := f.tokens[0]
	f.tokens = f.tokens[1:]
	return token, nil
}

func (f *fakeRelationships) Touch(_ context.Context, tuples []Tuple, _ []Precondition) (Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches = append(f.touches, tuples)
	return f.answer()
}

func (f *fakeRelationships) Delete(_ context.Context, filters []Filter, _ []Precondition) (Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, filters)
	return f.answer()
}

// pgEnv is the schema of one test on the local PostgreSQL server that STEGO_TEST_POSTGRES_DSN names, a superuser
// URL such as postgres://postgres:secret@127.0.0.1:55476/postgres?sslmode=disable. The test runs as a role of its
// own that is not a superuser, so row-level security applies. Without the variable the test is skipped.
type pgEnv struct {
	admin  *sql.DB
	db     *sql.DB
	schema string
	outbox *outboxRecorder
}

func newPG(t *testing.T) *pgEnv {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "authz_" + hex.EncodeToString(suffix)
	migration, err := Migration(name, name, name)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE ROLE " + name + " LOGIN PASSWORD 'authz-test'",
		"CREATE SCHEMA " + name,
		"GRANT USAGE ON SCHEMA " + name + " TO " + name,
		migration,
		"CREATE TABLE " + name + ".documents (tenant_id text NOT NULL, id text NOT NULL, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))",
		"ALTER TABLE " + name + ".documents ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + name + ".documents FORCE ROW LEVEL SECURITY",
		"CREATE POLICY documents_tenant ON " + name + ".documents USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
		"GRANT SELECT, INSERT, UPDATE ON " + name + ".documents TO " + name,
	} {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(ctx, "DROP SCHEMA "+name+" CASCADE")
		_, _ = admin.ExecContext(ctx, "DROP OWNED BY "+name)
		_, _ = admin.ExecContext(ctx, "DROP ROLE "+name)
	})
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.User = name
	config.Password = "authz-test"
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = db.Close() })
	return &pgEnv{admin: admin, db: db, schema: name, outbox: &outboxRecorder{}}
}

func (e *pgEnv) runtime(t *testing.T, provider Decision, states MessageStates) *Runtime {
	t.Helper()
	floor, err := NewTableFloor(e.db, e.schema)
	if err != nil {
		t.Fatal(err)
	}
	if states == nil {
		states = noStates
	}
	rt, err := New(Config{Provider: provider, Floor: floor, DB: e.db, Schema: e.schema, Enqueue: e.outbox.enqueue,
		States: states, Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func (e *pgEnv) relay(t *testing.T, relationships Relationships) *Relay {
	t.Helper()
	relay, err := NewRelay(RelayConfig{Name: "authz-relay", Relationships: relationships, DB: e.db, Schema: e.schema,
		Owners: map[string]Owner{"document": {Table: "documents", Key: "id"}}, Permanent: markPermanent})
	if err != nil {
		t.Fatal(err)
	}
	return relay
}

// permanentMark stands for the outbox runtime's PermanentRefusal mark.
type permanentMark struct{ error }

func markPermanent(err error) error { return permanentMark{err} }

// begin opens a transaction of tenant id as the test role and binds a handle to it for principal.
func (e *pgEnv) begin(t *testing.T, rt *Runtime, id string, principal Principal, allowed ...Relation) (*sql.Tx, *Handle) {
	t.Helper()
	tx, err := tenant.Begin(context.Background(), e.db, mustTenant(t, id), nil)
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

// deliver hands the message to the relay as the outbox worker does for a claimed row of tenant id.
func deliver(t *testing.T, relay *Relay, id string, message Message) error {
	t.Helper()
	return relay.Deliver(context.Background(), Delivery{ID: message.ID, Tenant: mustTenant(t, id),
		ResourceKey: message.ResourceKey, Payload: message.Payload})
}

func mustPerson(t *testing.T, id string) Principal {
	t.Helper()
	p, err := Person("user", identity{userID: id})
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

func TestHandleEvaluation(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP06-LC04-T02 A read of one row checks at least as fresh as the row's token", func(t *testing.T) {
		fake := &recorder{}
		handle := bindUnit(t, newUnitRuntime(t, fake, nil), mustPerson(t, "u-7"))
		rowToken, err := ParseToken(tok(17).Text())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Evaluate(ctx, "view", doc("1"), rowToken, nil); err != nil {
			t.Fatal(err)
		}
		if got := atLeast(t, fake.evaluations[0].Consistency); got != tok(17) {
			t.Fatalf("the check ran at least as fresh as %v, want t-17", got)
		}
	})

	t.Run("ST-SUP-RP06-LC04-T03 A later page runs at the fresher of the first page's token and the floor", func(t *testing.T) {
		fake := &recorder{token: func(n int) Token { return tok(2 + n) }} // t-3 at the first call
		floor := &floorSeq{tokens: []Token{{}, tok(1), tok(6)}}
		handle := bindUnit(t, newUnitRuntime(t, fake, floor), mustPerson(t, "u-7"))
		list := handle.List("view", nil)
		for page := 0; page < 3; page++ {
			if _, err := list.Page(ctx, []Row{{Resource: doc("a")}, {Resource: doc("b")}}); err != nil {
				t.Fatal(err)
			}
		}
		if len(fake.batches) != 3 {
			t.Fatalf("the provider got %d batch calls, want 3", len(fake.batches))
		}
		for i, want := range map[int]Token{1: tok(3), 2: tok(6)} {
			for _, evaluation := range fake.batches[i].Evaluations {
				if got := atLeast(t, evaluation.Consistency); got != want {
					t.Errorf("page %d ran at least as fresh as %v, want %v", i+1, got, want)
				}
			}
		}
	})

	t.Run("ST-SUP-RP06-LC09-T02 The handle evaluates with the transaction's principal as subject", func(t *testing.T) {
		fake := &recorder{}
		handle := bindUnit(t, newUnitRuntime(t, fake, nil), mustPerson(t, "u-7"))
		if _, err := handle.Evaluate(ctx, "view", doc("1"), Token{}, nil); err != nil {
			t.Fatal(err)
		}
		if got := fake.evaluations[0].Subject.String(); got != "user:u-7" {
			t.Fatalf("the subject is %s, want user:u-7", got)
		}
	})

	t.Run("ST-SUP-RP06-LC09-T03 The handle evaluates at least as fresh as the row token", func(t *testing.T) {
		fake := &recorder{}
		handle := bindUnit(t, newUnitRuntime(t, fake, fixedFloor(Token{})), mustPerson(t, "u-7"))
		if _, err := handle.Evaluate(ctx, "view", doc("1"), tok(5), nil); err != nil {
			t.Fatal(err)
		}
		if got := atLeast(t, fake.evaluations[0].Consistency); got != tok(5) {
			t.Fatalf("the check ran at least as fresh as %v, want exactly t-5", got)
		}
	})

	t.Run("ST-SUP-RP06-LC10-T02 A system principal gets its service account subject", func(t *testing.T) {
		fake := &recorder{}
		handle := bindUnit(t, newUnitRuntime(t, fake, nil), mustSystem(t, "reconciler"))
		if _, err := handle.Evaluate(ctx, "view", doc("1"), Token{}, nil); err != nil {
			t.Fatal(err)
		}
		if got := fake.evaluations[0].Subject.String(); got != "service_account:reconciler" {
			t.Fatalf("the subject is %s, want service_account:reconciler", got)
		}
		if _, err := Person("service_account", identity{userID: "reconciler"}); err == nil {
			t.Fatal("a person is mapped to a service account subject")
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
		handle := bindUnit(t, newUnitRuntime(t, fake, nil), mustPerson(t, "u-7"))
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

	t.Run("ST-SUP-RP06-LC13-T02 An evaluation uses the fresher of the row token and the service floor", func(t *testing.T) {
		fake := &recorder{}
		handle := bindUnit(t, newUnitRuntime(t, fake, fixedFloor(tok(9))), mustPerson(t, "u-7"))
		for _, row := range []Token{tok(5), tok(12)} {
			if _, err := handle.Evaluate(ctx, "view", doc("1"), row, nil); err != nil {
				t.Fatal(err)
			}
		}
		if got := atLeast(t, fake.evaluations[0].Consistency); got != tok(9) {
			t.Errorf("the row t-5 under the floor t-9 ran at %v, want t-9", got)
		}
		if got := atLeast(t, fake.evaluations[1].Consistency); got != tok(12) {
			t.Errorf("the row t-12 under the floor t-9 ran at %v, want t-12", got)
		}
	})

	t.Run("ST-SUP-RP06-LC13-T05 Batch evaluation, resource search and every list page run at least as fresh as the floor", func(t *testing.T) {
		fake := &recorder{token: func(int) Token { return tok(2) }, search: func(SearchRequest) SearchResult { return SearchResult{} }}
		handle := bindUnit(t, newUnitRuntime(t, fake, fixedFloor(tok(9))), mustPerson(t, "u-7"))
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
				if got := atLeast(t, evaluation.Consistency); fresher(got, tok(9)) != got {
					t.Errorf("batch call %d ran at %v, older than the floor t-9", i, got)
				}
			}
		}
		if got := atLeast(t, fake.searchCalls[0].Consistency); got != tok(9) {
			t.Errorf("the search ran at %v, want t-9", got)
		}
	})

	t.Run("ST-SUP-RP06-LC17-T01 Only an allow makes the tenant runtime's Decision", func(t *testing.T) {
		fake := &recorder{allow: func(r EvaluationRequest) bool { return r.Subject.ID == "u-1" }}
		rt := newUnitRuntime(t, fake, nil)
		platform := Resource{Type: "platform", ID: "main"}
		decision, ok, err := bindUnit(t, rt, mustPerson(t, "u-1")).DecideTenant(ctx, "provision", platform, nil)
		if err != nil || !ok || decision == nil || !decision.Allowed() {
			t.Fatalf("the allow gave %v, %t, %v", decision, ok, err)
		}
		made, err := tenant.New(tenant.Requested("t9", decision))
		if err != nil || made.ID() != "t9" {
			t.Fatalf("tenant.Requested with the allow gave %v, %v", made, err)
		}
		decision, ok, err = bindUnit(t, rt, mustPerson(t, "u-2")).DecideTenant(ctx, "provision", platform, nil)
		if err != nil || ok || decision != nil {
			t.Fatalf("the deny gave %v, %t, %v; want no decision", decision, ok, err)
		}
		if len(fake.evaluations) != 2 {
			t.Fatalf("the provider got %d evaluations, want 2", len(fake.evaluations))
		}
		// A package outside the runtime module cannot make a Decision.
		src := "package outside\n\nimport \"stego.dev/runtime/tenant\"\n\ntype decision struct{}\n\n" +
			"func (decision) Allowed() bool { return true }\n\nfunc use() { _, _ = tenant.New(tenant.Requested(\"t9\", decision{})) }\n"
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "outside.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		config := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
		_, err = config.Check("example.com/outside", fset, []*ast.File{file}, nil)
		if err == nil || !strings.Contains(err.Error(), "sealedDecision") {
			t.Fatalf("the outside package gave %v, want an error for the missing seal method", err)
		}
	})
}

func TestHandleWrites(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP06-LC09-T06 Relate and Unrelate do not call the provider", func(t *testing.T) {
		env := newPG(t)
		fake := &recorder{}
		rt := env.runtime(t, fake, nil)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		tuple := Tuple{Resource: doc("1"), Relation: "viewer", Subject: user("bob")}
		if _, err := handle.Relate(ctx, tuple); err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Unrelate(ctx, tuple); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		if n := fake.requests(); n != 0 {
			t.Fatalf("the provider got %d requests", n)
		}
		if n := len(env.outbox.all()); n != 2 {
			t.Fatalf("the outbox got %d messages, want 2", n)
		}
	})

	t.Run("ST-SUP-RP06-LC10-T01 A relation outside the allowed set is refused before anything is enqueued", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtime(t, &recorder{}, nil)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		_, err := handle.Relate(ctx, Tuple{Resource: doc("1"), Relation: "owner", Subject: user("bob")})
		if err == nil || !strings.Contains(err.Error(), "document#owner") {
			t.Fatalf("the relation owner gave %v, want an error that names document#owner", err)
		}
		env.commit(t, tx, "t1")
		if n := len(env.outbox.all()); n != 0 {
			t.Fatalf("the outbox got %d messages", n)
		}
		if n := env.scalar(t, "SELECT count(*) FROM {s}.authz_revocation_tuples"); n != "0" {
			t.Fatalf("%s tuple rows are written", n)
		}
	})

	t.Run("ST-SUP-RP06-LC12-T01 Wait returns the written-at token once the relay delivers the delete", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtime(t, &recorder{}, nil)
		relay := env.relay(t, &fakeRelationships{tokens: []Token{tok(42)}})
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		revocation, err := handle.Unrelate(ctx, Tuple{Resource: doc("1"), Relation: "viewer", Subject: user("bob")})
		if err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		for _, message := range env.outbox.all() {
			if err := deliver(t, relay, "t1", message); err != nil {
				t.Fatal(err)
			}
		}
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		status, err := handle.Wait(waitCtx, revocation)
		if err != nil {
			t.Fatal(err)
		}
		if status.State != RevocationComplete || status.Token != tok(42) || status.ID != revocation.ID() {
			t.Fatalf("Wait gave %+v, want complete with t-42", status)
		}
	})

	t.Run("ST-SUP-RP06-LC12-T04 Wait before the commit is refused", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtime(t, &recorder{}, nil)
		_, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		revocation, err := handle.Unrelate(ctx, Tuple{Resource: doc("1"), Relation: "viewer", Subject: user("bob")})
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
		rt := env.runtime(t, &recorder{}, nil)
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
		messages := env.outbox.all()
		if len(messages) != 3 {
			t.Fatalf("the outbox got %d messages, want 3", len(messages))
		}
		rows, err := env.admin.QueryContext(ctx, "SELECT message_id FROM "+env.schema+
			".authz_revocation_tuples WHERE revocation_id = $1", first.ID())
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		named := map[uuid.UUID]bool{}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			named[id] = true
		}
		for _, message := range messages {
			if !named[message.ID] || message.Destination != DestinationName {
				t.Errorf("the revocation does not name the outbox entry %s for %s", message.ID, message.Destination)
			}
		}
		if len(named) != 3 {
			t.Fatalf("the revocation names %d entries, want 3", len(named))
		}
	})

	t.Run("ST-SUP-RP06-LC15-T01 A revocation of two tuples is complete only when both are done", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtime(t, &recorder{}, nil)
		relay := env.relay(t, &fakeRelationships{tokens: []Token{tok(5), tok(8)}})
		alice := mustPerson(t, "alice")
		tx, handle := env.begin(t, rt, "t1", alice, rel("document#banned"), rel("document#viewer"))
		revocation, err := handle.Relate(ctx, Tuple{Resource: doc("1"), Relation: "banned", Subject: user("bob")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Unrelate(ctx, Tuple{Resource: doc("2"), Relation: "viewer", Subject: user("bob")}); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		status := func() RevocationStatus {
			t.Helper()
			s, err := rt.Status(ctx, mustTenant(t, "t1"), alice, revocation.ID())
			if err != nil {
				t.Fatal(err)
			}
			return s
		}
		if s := status(); s.State != RevocationPending {
			t.Fatalf("after the commit the state is %+v, want pending", s)
		}
		messages := env.outbox.all()
		if err := deliver(t, relay, "t1", messages[0]); err != nil {
			t.Fatal(err)
		}
		if s := status(); s.State != RevocationPending || !s.Token.IsZero() {
			t.Fatalf("after the first delivery the state is %+v, want pending", s)
		}
		if err := deliver(t, relay, "t1", messages[1]); err != nil {
			t.Fatal(err)
		}
		if s := status(); s.State != RevocationComplete || s.Token != tok(8) {
			t.Fatalf("after the second delivery the state is %+v, want complete with t-8", s)
		}
	})

	t.Run("ST-SUP-RP06-LC15-T02 Another principal or tenant gets not found for a revocation", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtime(t, &recorder{}, nil)
		alice := mustPerson(t, "alice")
		tx, handle := env.begin(t, rt, "t1", alice, rel("document#viewer"))
		revocation, err := handle.Relate(ctx, Tuple{Resource: doc("1"), Relation: "viewer", Subject: user("bob")})
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
}
