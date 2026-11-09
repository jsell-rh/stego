package tenant

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// This file holds the tests of ST-SUP-RP04-LC01 and ST-SUP-RP04-LC02 and the helpers that the tests of the package
// share: the statement recorder, the database fake and the local PostgreSQL fixture.

// identity is a stand-in for the verified identity of the authentication component.
type identity struct{ claim string }

func (i identity) TenantClaim() (string, bool) { return i.claim, i.claim != "" }

// allow is a stand-in for a decision of the authorization port.
type allow bool

func (a allow) Allowed() bool { return bool(a) }

// mustTenant builds a Tenant from a column value, as List and the outbox worker do.
func mustTenant(t *testing.T, id string) Tenant {
	t.Helper()
	var column Column
	if err := column.Scan(id); err != nil {
		t.Fatal(err)
	}
	tenant, err := New(column)
	if err != nil {
		t.Fatal(err)
	}
	return tenant
}

// recorder records every statement that pgx sends on a connection: the client side statement log.
type recorder struct {
	mu         sync.Mutex
	statements []string
	args       [][]any
}

func (r *recorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, data.SQL)
	r.args = append(r.args, data.Args)
	return ctx
}

func (r *recorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements, r.args = nil, nil
}

func (r *recorder) log() ([]string, [][]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.statements...), append([][]any(nil), r.args...)
}

// fakeDB is a database fake that records each statement and each begin and serves none.
type fakeDB struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeDB) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeDB) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

var errFake = errors.New("the database fake serves no statement")

func (f *fakeDB) Connect(context.Context) (driver.Conn, error) { return fakeConn{f}, nil }
func (f *fakeDB) Driver() driver.Driver                        { return fakeDriver{f} }

type fakeDriver struct{ f *fakeDB }

func (d fakeDriver) Open(string) (driver.Conn, error) { return fakeConn(d), nil }

type fakeConn struct{ f *fakeDB }

func (c fakeConn) Prepare(query string) (driver.Stmt, error) {
	c.f.record(query)
	return nil, errFake
}
func (c fakeConn) Close() error { return nil }
func (c fakeConn) Begin() (driver.Tx, error) {
	c.f.record("BEGIN")
	return nil, errFake
}
func (c fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) { return c.Begin() }
func (c fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.f.record(query)
	return nil, errFake
}
func (c fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.f.record(query)
	return nil, errFake
}

func newFakeDB(t *testing.T) (*sql.DB, *fakeDB) {
	fake := &fakeDB{}
	db := sql.OpenDB(fake)
	t.Cleanup(func() { _ = db.Close() })
	return db, fake
}

// pgEnv is one test's own database on the local PostgreSQL server: an owner role that owns the schema, a runtime
// role, a tenant-owned table items with row-level security on tenant_id, and the tenant list of Migration.
type pgEnv struct {
	admin      *sql.DB
	adminURL   *url.URL
	name       string
	owner      string
	runtime    string
	db         *sql.DB // the test database as the superuser of the server, which bypasses row-level security
	ownerDB    *sql.DB
	runtimeDB  *sql.DB
	statements *recorder
}

const rolePassword = "tenant-test-password"

// newPG makes the database of one test. The server is the local container that STEGO_TEST_POSTGRES_DSN names, a
// superuser URL such as postgres://postgres:secret@127.0.0.1:55444/postgres?sslmode=disable; without it the test
// is skipped.
func newPG(t *testing.T) *pgEnv {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	adminURL, err := url.Parse(dsn)
	if err != nil || adminURL.Scheme == "" {
		t.Fatalf("STEGO_TEST_POSTGRES_DSN must be a postgres:// URL: %v", err)
	}
	ctx := context.Background()
	admin := openURL(t, adminURL, nil)
	suffix := randomSuffix(t)
	env := &pgEnv{admin: admin, adminURL: adminURL, name: "tenant_" + suffix, owner: "owner_" + suffix,
		runtime: "runtime_" + suffix, statements: &recorder{}}
	adminExec(t, admin,
		"CREATE ROLE "+env.owner+" LOGIN PASSWORD '"+rolePassword+"'",
		"CREATE ROLE "+env.runtime+" LOGIN PASSWORD '"+rolePassword+"'",
		"CREATE DATABASE "+env.name+" OWNER "+env.owner,
	)
	t.Cleanup(func() {
		for _, db := range []*sql.DB{env.runtimeDB, env.ownerDB, env.db} {
			if db != nil {
				_ = db.Close()
			}
		}
		if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+env.name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the test database: %v", err)
		}
		for _, role := range env.roles() {
			_, _ = admin.ExecContext(ctx, "DROP ROLE IF EXISTS "+role)
		}
	})
	env.db = openURL(t, env.url("", ""), nil)
	env.ownerDB = openURL(t, env.url(env.owner, rolePassword), nil)
	adminExec(t, env.ownerDB,
		"CREATE TABLE items (tenant_id text NOT NULL, note text NOT NULL)",
		"ALTER TABLE items ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE items FORCE ROW LEVEL SECURITY",
		"CREATE POLICY items_tenant ON items USING (tenant_id = current_setting('app.tenant', true))"+
			" WITH CHECK (tenant_id = current_setting('app.tenant', true))",
		"GRANT SELECT, INSERT ON items TO "+env.runtime,
	)
	migration, err := Migration(env.runtime)
	if err != nil {
		t.Fatal(err)
	}
	adminExec(t, env.ownerDB, migration)
	env.runtimeDB = openURL(t, env.url(env.runtime, rolePassword), env.statements)
	return env
}

var extraRoles sync.Map // test database name -> []string

func (e *pgEnv) roles() []string {
	roles := []string{e.runtime, e.owner}
	if extra, ok := extraRoles.Load(e.name); ok {
		roles = append(extra.([]string), roles...)
	}
	return roles
}

// role makes one more login role for the test and drops it when the test ends.
func (e *pgEnv) role(t *testing.T, name string, options string) string {
	t.Helper()
	role := name + "_" + strings.TrimPrefix(e.name, "tenant_")
	adminExec(t, e.admin, "CREATE ROLE "+role+" LOGIN "+options+" PASSWORD '"+rolePassword+"'")
	previous, _ := extraRoles.Load(e.name)
	list, _ := previous.([]string)
	extraRoles.Store(e.name, append(list, role))
	return role
}

// url is the URL of the test database for user, or for the superuser when user is empty.
func (e *pgEnv) url(user, password string) *url.URL {
	u := *e.adminURL
	u.Path = "/" + e.name
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	return &u
}

func (e *pgEnv) dsn(user string) string { return e.url(user, rolePassword).String() }

func openURL(t *testing.T, u *url.URL, tracer pgx.QueryTracer) *sql.DB {
	t.Helper()
	config, err := pgx.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	config.Tracer = tracer
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("connect to %s: %v", u.Redacted(), err)
	}
	return db
}

func adminExec(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// typeCheck type-checks the package source files that are not tests, with go/types.
func typeCheck(t *testing.T) *types.Package {
	t.Helper()
	fset := token.NewFileSet()
	files := packageFiles(t, fset)
	config := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := config.Check("stego.dev/runtime/tenant", fset, files, nil)
	if err != nil {
		t.Fatalf("type-check the package: %v", err)
	}
	return pkg
}

func packageFiles(t *testing.T, fset *token.FileSet) []*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	return files
}

func isTenant(typ types.Type) bool {
	named, ok := types.Unalias(typ).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "stego.dev/runtime/tenant" &&
		named.Obj().Name() == "Tenant"
}

func takesTenant(sig *types.Signature) bool {
	for i := 0; i < sig.Params().Len(); i++ {
		typ := sig.Params().At(i).Type()
		if slice, ok := typ.(*types.Slice); ok {
			typ = slice.Elem()
		}
		if isTenant(typ) {
			return true
		}
	}
	return false
}

func TestTenantIdentity(t *testing.T) {
	t.Run("ST-SUP-RP04-LC01-T01 A Tenant is built only from a verified identity claim through the one constructor", func(t *testing.T) {
		pkg := typeCheck(t)
		scope := pkg.Scope()
		tenantType := scope.Lookup("Tenant").Type()
		structType, ok := tenantType.Underlying().(*types.Struct)
		if !ok {
			t.Fatalf("Tenant has the underlying type %s, so a conversion can build it", tenantType.Underlying())
		}
		for i := 0; i < structType.NumFields(); i++ {
			if structType.Field(i).Exported() {
				t.Errorf("Tenant has the exported field %s", structType.Field(i).Name())
			}
		}
		var builders []string
		for _, name := range scope.Names() {
			fn, ok := scope.Lookup(name).(*types.Func)
			if !ok || !fn.Exported() {
				continue
			}
			sig := fn.Type().(*types.Signature)
			for i := 0; i < sig.Results().Len(); i++ {
				if isTenant(sig.Results().At(i).Type()) {
					builders = append(builders, name)
				}
			}
		}
		sort.Strings(builders)
		if strings.Join(builders, ",") != "FromContext,New" {
			t.Fatalf("the exported functions that return a Tenant are %v, want FromContext and New", builders)
		}
		newSig := scope.Lookup("New").Type().(*types.Signature)
		if newSig.Params().Len() != 1 || newSig.Params().At(0).Type() != scope.Lookup("Source").Type() {
			t.Fatalf("New takes %s, want one Source", newSig.Params())
		}
		source := scope.Lookup("Source").Type().Underlying().(*types.Interface)
		for i := 0; i < source.NumMethods(); i++ {
			if source.Method(i).Exported() {
				t.Errorf("Source has the exported method %s, so code outside the package can be a source", source.Method(i).Name())
			}
		}
		fromContext := scope.Lookup("FromContext").Type().(*types.Signature)
		if fromContext.Params().Len() != 1 || fromContext.Params().At(0).Type().String() != "context.Context" {
			t.Errorf("FromContext takes %s, want one context.Context", fromContext.Params())
		}
		claim := scope.Lookup("Claim").Type().(*types.Signature)
		if claim.Params().Len() != 1 || claim.Params().At(0).Type() != scope.Lookup("Identity").Type() {
			t.Errorf("Claim takes %s, want one verified Identity", claim.Params())
		}

		got, err := New(Claim(identity{claim: "tenant-a"}))
		if err != nil || got.ID() != "tenant-a" {
			t.Fatalf("New(Claim(identity of tenant-a)) = %q, %v", got.ID(), err)
		}
		none, err := New(Claim(identity{}))
		if err == nil || !none.IsZero() {
			t.Fatalf("New(Claim(identity with no claim)) = %q, %v; want an error and no tenant", none.ID(), err)
		}
	})

	t.Run("ST-SUP-RP04-LC01-T02 Every function that takes a Tenant refuses the zero value before any statement", func(t *testing.T) {
		pkg := typeCheck(t)
		var found []string
		for _, name := range pkg.Scope().Names() {
			obj := pkg.Scope().Lookup(name)
			if fn, ok := obj.(*types.Func); ok && fn.Exported() && takesTenant(fn.Type().(*types.Signature)) {
				found = append(found, name)
			}
			if typeName, ok := obj.(*types.TypeName); ok && typeName.Exported() {
				methods := types.NewMethodSet(types.NewPointer(typeName.Type()))
				for i := 0; i < methods.Len(); i++ {
					method := methods.At(i).Obj().(*types.Func)
					if method.Exported() && takesTenant(method.Type().(*types.Signature)) {
						found = append(found, name+"."+method.Name())
					}
				}
			}
		}
		ctx := context.Background()
		db, fake := newFakeDB(t)
		var zero Tenant
		calls := map[string]func() error{
			"WithTenant": func() error { _, err := WithTenant(ctx, zero); return err },
			"Begin":      func() error { _, err := Begin(ctx, db, zero, nil); return err },
			"Each": func() error {
				return Each(ctx, db, []Tenant{zero}, func(context.Context, *sql.Tx, Tenant) error { return nil })
			},
			"Provision": func() error { return Provision(ctx, db, zero, nil) },
			"Retire":    func() error { return Retire(ctx, db, zero, nil) },
		}
		sort.Strings(found)
		var listed []string
		for name := range calls {
			listed = append(listed, name)
		}
		sort.Strings(listed)
		if strings.Join(found, ",") != strings.Join(listed, ",") {
			t.Fatalf("the exported functions that take a Tenant are %v, and the test calls %v: add each new one", found, listed)
		}
		for _, name := range listed {
			if err := calls[name](); !errors.Is(err, ErrZero) {
				t.Errorf("%s with the zero tenant returned %v, want ErrZero", name, err)
			}
		}
		if got := fake.recorded(); len(got) != 0 {
			t.Fatalf("the database fake recorded %v, want no statement", got)
		}
	})

	t.Run("ST-SUP-RP04-LC01-T03 A client-supplied header or body field is not accepted as a tenant source", func(t *testing.T) {
		// A stand-in for the authentication component: it verifies the bearer token, which in this test is the
		// tenant claim itself, builds the tenant from the verified identity and sets it in the context.
		authenticate := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				claim, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
				if !ok {
					http.Error(w, "no token", http.StatusUnauthorized)
					return
				}
				tenant, err := New(Claim(identity{claim: claim}))
				if err != nil {
					http.Error(w, err.Error(), http.StatusUnauthorized)
					return
				}
				ctx, err := WithTenant(r.Context(), tenant)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		}
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant, err := FromContext(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, tenant.ID())
		})
		server := httptest.NewServer(authenticate(handler))
		defer server.Close()
		request, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewBufferString(`{"tenant_id":"tenant-b"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer tenant-a")
		request.Header.Set("X-Tenant-ID", "tenant-b")
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusOK || string(body) != "tenant-a" {
			t.Fatalf("the context holds %q (status %d), want tenant-a", body, response.StatusCode)
		}
	})

	t.Run("ST-SUP-RP04-LC01-T04 A worker gets its tenant only from the tenant list or the outbox row", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		if err := Provision(ctx, env.runtimeDB, mustRequested(t, "t1"), nil); err != nil {
			t.Fatal(err)
		}
		var visited []string
		if err := EachListed(ctx, env.runtimeDB, func(_ context.Context, _ *sql.Tx, t Tenant) error {
			visited = append(visited, t.ID())
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if strings.Join(visited, ",") != "t1" {
			t.Fatalf("the iterator visited %v, want t1", visited)
		}

		// A stand-in for the outbox runtime's queue table: the staging transaction wrote the tenant column from
		// its own tenant, and the payload holds a member tenant that is not a source.
		adminExec(t, env.ownerDB,
			"CREATE TABLE outbox_stand_in (id bigserial PRIMARY KEY, tenant_id text NOT NULL, payload jsonb NOT NULL)",
			"INSERT INTO outbox_stand_in (tenant_id, payload) VALUES ('t2', '{\"member\":{\"tenant\":\"t9\"}}')",
			"GRANT SELECT, UPDATE ON outbox_stand_in TO "+env.runtime,
		)
		tx, err := env.runtimeDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		var column Column
		var payload []byte
		if err := tx.QueryRowContext(ctx,
			"SELECT tenant_id, payload FROM outbox_stand_in ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED").Scan(&column, &payload); err != nil {
			t.Fatal(err)
		}
		fromRow, err := New(column)
		if err != nil || fromRow.ID() != "t2" {
			t.Fatalf("New(the claimed row's tenant column) = %q, %v; want t2", fromRow.ID(), err)
		}

		var message struct {
			Member struct {
				Tenant string `json:"tenant"`
			} `json:"member"`
		}
		if err := json.Unmarshal(payload, &message); err != nil || message.Member.Tenant != "t9" {
			t.Fatalf("the payload member tenant is %q, %v", message.Member.Tenant, err)
		}
		// The payload is no Source: a call of New with it, or with its member tenant, does not type-check. The
		// control call shows that the snippet itself type-checks.
		if err := typeCheckSnippet(t, "tenant.New(tenant.Claim(nil))"); err != nil {
			t.Fatalf("the control snippet does not type-check: %v", err)
		}
		for _, call := range []string{"tenant.New(message)", "tenant.New(message.Member.Tenant)"} {
			if err := typeCheckSnippet(t, call); err == nil {
				t.Errorf("%s type-checks, so a message payload is a tenant source", call)
			}
		}
		if got, err := New(nil); err == nil || !got.IsZero() {
			t.Errorf("New(nil) = %q, %v; want an error and no tenant", got.ID(), err)
		}
		if got, err := New(Column{}); err == nil || !got.IsZero() {
			t.Errorf("New(a column that no statement read) = %q, %v; want an error and no tenant", got.ID(), err)
		}
	})
}

func mustRequested(t *testing.T, id string) Tenant {
	t.Helper()
	tenant, err := New(Requested(id, allow(true)))
	if err != nil {
		t.Fatal(err)
	}
	return tenant
}

// typeCheckSnippet type-checks a file outside the package that decodes a message and calls the package with call.
func typeCheckSnippet(t *testing.T, call string) error {
	t.Helper()
	src := `package worker

import "stego.dev/runtime/tenant"

type message struct{ Member struct{ Tenant string } }

func consume(message message) { _, _ = ` + call + ` }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "worker.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	config := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	_, err = config.Check("example.com/worker", fset, []*ast.File{file}, nil)
	return err
}

func TestTenantContext(t *testing.T) {
	t.Run("ST-SUP-RP04-LC02-T01 The context returns the tenant the authentication component set", func(t *testing.T) {
		tenant, err := New(Claim(identity{claim: "tenant-a"}))
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := WithTenant(context.Background(), tenant)
		if err != nil {
			t.Fatal(err)
		}
		got, err := FromContext(ctx)
		if err != nil || got != tenant {
			t.Fatalf("FromContext = %q, %v; want tenant-a and no error", got.ID(), err)
		}
	})

	t.Run("ST-SUP-RP04-LC02-T02 A context with no tenant returns an error and never a zero tenant", func(t *testing.T) {
		got, err := FromContext(context.Background())
		if !errors.Is(err, ErrNoTenant) {
			t.Fatalf("FromContext(context.Background()) returned the error %v, want ErrNoTenant", err)
		}
		if !got.IsZero() {
			t.Fatalf("FromContext(context.Background()) returned the tenant %q", got.ID())
		}
		db, fake := newFakeDB(t)
		if _, err := Begin(context.Background(), db, got, nil); !errors.Is(err, ErrZero) {
			t.Fatalf("Begin with the tenant of an empty context returned %v, want ErrZero", err)
		}
		if _, err := WithTenant(context.Background(), got); !errors.Is(err, ErrZero) {
			t.Fatalf("WithTenant with the tenant of an empty context returned %v, want ErrZero", err)
		}
		if calls := fake.recorded(); len(calls) != 0 {
			t.Fatalf("the database fake recorded %v, want no statement", calls)
		}
	})
}
