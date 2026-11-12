package compiler_test

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
)

//go:embed testdata/database_access_test.go
var databaseAccessTests []byte

// accessFixtureSource is the fixture component of TestGeneratedDatabaseAccess: a handler on the database that the
// generated service opens, so the service checks its database access before it configures HTTP.
const accessFixtureSource = `package records

import (
	"database/sql"
	"net/http"
)

// NewHandler answers each request with the count of the records.
func NewHandler(db *sql.DB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var count int
		if err := db.QueryRowContext(r.Context(), "SELECT count(*) FROM public.records").Scan(&count); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
`

func TestGeneratedDatabaseAccess(t *testing.T) {
	t.Parallel()
	if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" && os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("database access checks require PostgreSQL")
	}
	// The database access contract is the compiler's (ST-GL16): it serves every component that declares its
	// database objects. The GORM store and the former outbox that declared them before are gone
	// (ST-SUP-RC20-OC39), so a fixture component declares the objects that the test database holds, and the
	// PostgreSQL adapter, with no entity, gives the database opener.
	ctx := gen.Context{ModuleName: "example.com/access-test", OutDirName: "out", OutputNamespace: "storage"}
	opened, openerWiring, err := new(postgresadapter.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files := append([]gen.File{}, opened...)
	files = append(files, gen.File{Path: "records/records.go", Content: []byte(accessFixtureSource)})
	wirings := []compiler.ComponentWiring{{Name: "postgres-adapter", Wiring: openerWiring}, {Name: "records", Wiring: &gen.Wiring{
		NeedsDB: true, VerifyDatabaseAccess: true, Imports: []string{"records"}, Constructors: []string{"records.NewHandler()"},
		ConstructorResources: map[int][]gen.Resource{0: {gen.SQLDatabase}}, Routes: []string{`mux.Handle("GET /records", handler)`},
		DatabaseAccess: []gen.DatabaseObject{
			{Schema: "public", Name: "records", Kind: "table", Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE"}},
			{Schema: "stego_schema", Name: "generation", Kind: "table", Privileges: []string{"SELECT"}},
			{Schema: "stego_schema", Name: "epoch_seq", Kind: "sequence", Privileges: []string{"SELECT", "USAGE"}},
			{Schema: "stego_outbox", Name: "messages", Kind: "table", Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE"}},
			{Schema: "stego_outbox", Name: "messages_sequence_seq", Kind: "sequence", Privileges: []string{}},
		}}}}
	assembled, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, GoVersion: "1.26.11", OutDirName: "out", Wirings: wirings})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, assembled...)
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, "out", file.Path)
		if file.Path == "go.mod" {
			name = filepath.Join(project, "go.mod")
		}
		if err = os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(project, "out/access_test.go"), databaseAccessTests, 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-mod=readonly", "-count=1", "-timeout=180s", "-v", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("generated database access: %v\n%s", err, output)
		}
		t.Log(string(output))
	}
}
