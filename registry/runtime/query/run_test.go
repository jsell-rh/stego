package query

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"stego.dev/runtime/audit"
)

// readCharges is a fill that reads the charges ch-1 and ch-2 and returns their amounts.
func readCharges(charges *Entity[Charge]) Fill[struct{}, []int64, noProbe] {
	return func(ctx context.Context, _ struct{}, h *Handle[noProbe]) ([]int64, error) {
		repo, err := Repo(h, charges)
		if err != nil {
			return nil, err
		}
		var amounts []int64
		for _, id := range []string{"ch-1", "ch-2"} {
			charge, err := repo.Get(ctx, id)
			if err != nil {
				return nil, err
			}
			amounts = append(amounts, charge.Value().Amount)
		}
		return amounts, nil
	}
}

func (e *env) count(t *testing.T, statement string, args ...any) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRowContext(context.Background(), statement, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRun(t *testing.T) {
	t.Run("ST-SUP-RP11-LC08-T02 A failed query audit write returns no result", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.charges (tenant_id, id, card_last4, amount) VALUES ('t1', 'ch-1', '4242', 700), ('t1', 'ch-2', '1881', 300)")
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		charges := chargeEntity(t)
		q := mustQuery(t, "open-reservations", "", charges)
		// The control: while the audit table takes inserts, the run gives its result and one read row per charge.
		amounts, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "open-reservations"), struct{}{}, readCharges(charges))
		if err != nil || len(amounts) != 2 {
			t.Fatalf("the control run gives %v, %v", amounts, err)
		}
		if n := e.count(t, "SELECT count(*) FROM "+audit.EventsTable+" WHERE action = 'read' AND actor_id = 'user:u-7'"); n != 2 {
			t.Fatalf("the control run left %d read rows, want 2", n)
		}
		if _, err := e.admin.ExecContext(context.Background(), "REVOKE INSERT ON "+audit.EventsTable+" FROM "+e.runtime); err != nil {
			t.Fatal(err)
		}
		// A fill that swallows the audit error and returns a result still gives no result.
		swallow := func(ctx context.Context, _ struct{}, h *Handle[noProbe]) ([]int64, error) {
			repo, err := Repo(h, charges)
			if err != nil {
				return nil, err
			}
			if _, err := repo.Get(ctx, "ch-1"); err == nil {
				t.Errorf("the read of ch-1 succeeds while its audit row cannot be written")
			}
			return []int64{1}, nil
		}
		for name, fill := range map[string]Fill[struct{}, []int64, noProbe]{"returns the error": readCharges(charges), "swallows the error": swallow} {
			got, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "open-reservations"), struct{}{}, fill)
			if err == nil || got != nil {
				t.Errorf("a fill that %s: Run gives %v, %v; want an error and no result", name, got, err)
			}
		}
	})

	t.Run("ST-SUP-RP11-LC09-T01 A kept query handle reads nothing after the query", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', 'o-1', 5)")
		port := &fakeDecision{decide: allowAll}
		rt := e.runtimeOf(t, port)
		orders := orderEntity(t, false)
		q := mustQuery(t, "order-detail", "", orders)
		var kept *Handle[noProbe]
		var keptRepo *Repository[Order]
		var keptOrder *Loaded[Order]
		calls := func() map[string]error {
			_, repoErr := Repo(kept, orders)
			_, getErr := keptRepo.Get(context.Background(), "o-1")
			_, listErr := keptRepo.List(context.Background(), ListRequest{})
			_, evalErr := kept.Authz().Evaluate(context.Background(), "view", keptOrder)
			return map[string]error{"Repo": repoErr, "Get": getErr, "List": listErr, "Evaluate": evalErr}
		}
		var inWindow map[string]error
		// The window: the fill returned and the transaction is still open.
		rt.afterFill = func() { inWindow = calls() }
		_, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "order-detail"), struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (struct{}, error) {
				kept = h
				repo, err := Repo(h, orders)
				if err != nil {
					return struct{}{}, err
				}
				keptRepo = repo
				keptOrder, err = repo.Get(ctx, "o-1")
				return struct{}{}, err
			})
		if err != nil {
			t.Fatal(err)
		}
		decided := len(port.recorded())
		for when, results := range map[string]map[string]error{"before the transaction ends": inWindow, "after Run": calls()} {
			for name, err := range results {
				if !errors.Is(err, ErrClosed) {
					t.Errorf("%s, %s through the kept handle gives %v, want ErrClosed", when, name, err)
				}
			}
		}
		if got := len(port.recorded()); got != decided || decided != 1 {
			t.Errorf("the decision port has %d calls after the query, want the 1 of the fill", got)
		}
	})

	t.Run("ST-SUP-RP11-LC10-T01 A query fill panic returns no result and hides the panic value", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.reservations (tenant_id, id, sku) VALUES ('t1', 'r-1', 'sku-1')")
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		reservations := reservationEntity(t)
		q := mustQuery(t, "open-reservations", "", reservations)
		var kept *Repository[Reservation]
		got, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "open-reservations"), struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (*Loaded[Reservation], error) {
				repo, err := Repo(h, reservations)
				if err != nil {
					return nil, err
				}
				kept = repo
				r, err := repo.Get(ctx, "r-1")
				if err != nil {
					t.Errorf("the read before the panic: %v", err)
				}
				_ = r
				panic("secret-42")
			})
		if err == nil || got != nil {
			t.Fatalf("Run gives %v, %v; want an error and no result", got, err)
		}
		if strings.Contains(err.Error(), "secret-42") {
			t.Errorf("the error %q holds the panic value", err)
		}
		if _, err := kept.Get(context.Background(), "r-1"); !errors.Is(err, ErrClosed) {
			t.Errorf("the handle after the panic gives %v, want ErrClosed", err)
		}
		// The transaction ended: no connection of the runtime role is left in it.
		if n := e.count(t, "SELECT count(*) FROM pg_stat_activity WHERE usename = $1 AND state = 'idle in transaction'", e.runtime); n != 0 {
			t.Errorf("%d connections of the runtime role are idle in a transaction, want none", n)
		}
	})

	t.Run("ST-SUP-RP11-LC14-T01 The query runtime commits only through tenant.Commit", func(t *testing.T) {
		calls := commitCalls(t)
		if len(calls) != 0 {
			t.Errorf("the package calls Commit on a store transaction: %v", calls)
		}
		e := newEnv(t)
		plain, err := sql.Open("pgx", e.dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = plain.Close() })
		if _, err := New(Config{DB: plain}); err == nil {
			t.Error("New takes a store pool of sql.Open")
		}
		if _, err := New(Config{DB: e.db, AuditDB: plain, Audit: e.writer, Service: "shop"}); err == nil {
			t.Error("New takes an audit pool of sql.Open")
		}
		if _, err := New(Config{DB: e.db, AuditDB: e.auditDB, Audit: e.writer, Service: "shop"}); err != nil {
			t.Errorf("New refuses the pools of tenant.Open: %v", err)
		}
	})
}

// typeCheck type-checks the package's files that are not tests with go/types, against the export data that go list
// builds, and gives the package, the selections of its source and its file set.
func typeCheck(t *testing.T) (*types.Package, *types.Info, *token.FileSet) {
	t.Helper()
	out, err := exec.Command("go", "list", "-export", "-deps", "-f", "{{.ImportPath}}={{.Export}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	exports := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, file, _ := strings.Cut(line, "=")
		exports[path] = file
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, os.ErrNotExist
		}
		return os.Open(file)
	})
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if len(files) < 4 {
		t.Fatalf("the scan reads %d files, want the package's source files", len(files))
	}
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
	pkg, err := (&types.Config{Importer: imp}).Check("stego.dev/runtime/query", fset, files, info)
	if err != nil {
		t.Fatalf("type-check the package: %v", err)
	}
	return pkg, info, fset
}

// commitCalls lists each call in the package's files that are not tests of a method Commit on a *sql.Tx, or on a
// type that holds one as a field, at any depth.
func commitCalls(t *testing.T) []string {
	t.Helper()
	_, info, fset := typeCheck(t)
	var calls []string
	for selector, selection := range info.Selections {
		if selector.Sel.Name != "Commit" || selection.Kind() != types.MethodVal {
			continue
		}
		if holdsTx(selection.Recv(), map[types.Type]bool{}) {
			calls = append(calls, fset.Position(selector.Pos()).String())
		}
	}
	return calls
}

// holdsTx tells whether typ is *sql.Tx, or a struct or a pointer to one that holds a *sql.Tx at any depth.
func holdsTx(typ types.Type, seen map[types.Type]bool) bool {
	if seen[typ] {
		return false
	}
	seen[typ] = true
	if ptr, ok := typ.(*types.Pointer); ok {
		if named, ok := ptr.Elem().(*types.Named); ok && named.Obj().Pkg() != nil &&
			named.Obj().Pkg().Path() == "database/sql" && named.Obj().Name() == "Tx" {
			return true
		}
		return holdsTx(ptr.Elem(), seen)
	}
	if named, ok := typ.(*types.Named); ok {
		return holdsTx(named.Underlying(), seen)
	}
	if st, ok := typ.(*types.Struct); ok {
		for i := 0; i < st.NumFields(); i++ {
			if holdsTx(st.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}
