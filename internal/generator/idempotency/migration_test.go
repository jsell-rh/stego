package idempotency

import (
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/migration_test.go
var emittedMigrationTest []byte

// runtimeConstant reads the text of a string constant of the runtime package stego.dev/runtime/idempotency from
// its source in this repository. The generator module does not import the runtime module, so the test reads the
// source.
func runtimeConstant(t *testing.T, file, name string) string {
	t.Helper()
	source := filepath.Join("..", "..", "..", "registry", "runtime", "idempotency", file)
	tree, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", source, err)
	}
	var text string
	found := false
	ast.Inspect(tree, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, ident := range spec.Names {
			if ident.Name == name && i < len(spec.Values) {
				if literal, ok := spec.Values[i].(*ast.BasicLit); ok {
					text, err = strconv.Unquote(literal.Value)
					found = err == nil
				}
			}
		}
		return true
	})
	if !found {
		t.Fatalf("%s holds no string constant %s", source, name)
	}
	return text
}

// pinnedGo is the Go command that builds the emitted package: STEGO_TEST_PINNED_GO, else bin/go of the SDK that
// runs the test.
func pinnedGo() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

func TestMigration(t *testing.T) {
	t.Run("ST-SUP-RC30-OC04-T04 The emitted migration makes the key table scoped by tenant, principal and key", func(t *testing.T) {
		files := generated(t, todoContext([]types.Operation{types.OpCreate, types.OpPatch}, "48h", todoBase))
		if got, want := keyTableSQL, runtimeConstant(t, "migration.go", "Migration"); got != want {
			t.Fatalf("the generator's key table text differs from Migration of the runtime:\n%s\nwant:\n%s", got, want)
		}
		for i, slug := range []string{"ProblemMissing", "ProblemInProgress", "ProblemReused"} {
			if got, want := problemSlugs[i], runtimeConstant(t, "wrap.go", slug); got != want {
				t.Fatalf("problem slug %d is %q, want %s of the runtime, %q", i, got, slug, want)
			}
		}
		if got := string(files["internal/idempotency/migrations/000001_idempotency_keys.sql"]); got != keyTableSQL {
			t.Fatalf("the emitted migration file is not the key table text:\n%s", got)
		}

		dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
		if dsn == "" {
			if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
				t.Fatal("PostgreSQL integration tests require STEGO_TEST_POSTGRES_DSN")
			}
			t.Skip("STEGO_TEST_POSTGRES_DSN is not set")
		}
		project := t.TempDir()
		write := func(name string, data []byte) {
			full := filepath.Join(project, name)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for name, data := range files {
			write(name, data)
		}
		write("internal/idempotency/migration_test.go", emittedMigrationTest)
		write("go.mod", []byte("module example.com/idempotency-test\n\ngo "+new(Generator).MinimumGoVersion()+
			"\n\nrequire github.com/jackc/pgx/v5 v5.11.0\n"))
		for _, args := range [][]string{
			{"mod", "tidy"},
			{"vet", "-mod=readonly", "./..."},
			{"test", "-mod=readonly", "-count=1", "-timeout=60s", "-v", "./..."},
		} {
			cmd := exec.Command(pinnedGo(), args...)
			cmd.Dir = project
			cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "STEGO_TEST_POSTGRES_DSN="+dsn)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go %v: %v\n%s", args, err, output)
			}
			t.Log(string(output))
		}
	})
}
