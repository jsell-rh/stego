package compiler_test

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/outbox"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/restapi"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/database_access_test.go
var databaseAccessTests []byte

func TestGeneratedDatabaseAccess(t *testing.T) {
	if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" && os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("database access checks require PostgreSQL")
	}
	ctx := gen.Context{ModuleName: "example.com/access-test", OutDirName: "out", StorageContract: "example.com/access-test/out/contracts/storage",
		ComponentConfig: map[string]any{"migrations": "external", "schema_generation": "fresh-v1"},
		Entities:        []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}}}}
	ctx.Collections = []types.Collection{{Name: "records", Entity: "Record", Operations: []types.Operation{types.OpRead}}}
	ctx.PeerNamespaces = map[string]string{"postgres-adapter": "storage", "outbox": "queue", "rest-api": "api"}
	var files []gen.File
	var wirings []compiler.ComponentWiring
	for _, c := range []struct {
		name, namespace string
		generator       gen.Generator
	}{
		{"postgres-adapter", "storage", new(postgresadapter.Generator)}, {"outbox", "queue", new(outbox.Generator)},
		{"rest-api", "api", new(restapi.Generator)},
	} {
		ctx.OutputNamespace = c.namespace
		generated, wiring, err := c.generator.Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		wirings = append(wirings, compiler.ComponentWiring{Name: c.name, Wiring: wiring})
	}
	assembled, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, GoVersion: "1.26.8", OutDirName: "out", Wirings: wirings})
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
