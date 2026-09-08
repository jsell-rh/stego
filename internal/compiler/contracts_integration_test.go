package compiler_test

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/outbox"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/restapi"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/shared_storage_test.go
var sharedStorageTests []byte

const domainRuleSource = `package rules
import (
 "context"
 storage "example.com/shared/out/contracts/storage"
)
// Create records the resource and its explicit notification in one transaction.
func Create(ctx context.Context, store storage.Transactor, value any, event storage.Notification) error {
 return store.WithTransaction(ctx, func(ctx context.Context, tx storage.Transaction) error {
  if err := tx.Create(ctx, "Record", value); err != nil { return err }
  return tx.Notify(event)
 })
}
`

func TestSharedStorageDomainAndHTTP(t *testing.T) {
	dsn, required := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_REQUIRE_POSTGRES")
	if required == "1" && dsn == "" {
		t.Fatal("PostgreSQL integration tests require STEGO_TEST_POSTGRES_DSN")
	}
	ctx := gen.Context{
		ModuleName: "example.com/shared", OutDirName: "out", StorageContract: "example.com/shared/out/contracts/storage",
		Entities:       []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString, Unique: true}}}},
		Collections:    []types.Collection{{Name: "records", Entity: "Record", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList}}},
		PeerNamespaces: map[string]string{"rest-api": "internal/api", "postgres-adapter": "internal/store", "outbox": "internal/queue"},
	}
	var files []gen.File
	var wirings []compiler.ComponentWiring
	for _, component := range []struct {
		name      string
		generator gen.Generator
	}{
		{"rest-api", new(restapi.Generator)}, {"postgres-adapter", new(postgresadapter.Generator)}, {"outbox", new(outbox.Generator)},
	} {
		ctx.OutputNamespace = ctx.PeerNamespaces[component.name]
		generated, wiring, err := component.generator.Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		wirings = append(wirings, compiler.ComponentWiring{Name: component.name, Wiring: wiring})
	}
	shared, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, GoVersion: "1.26.8", OutDirName: "out", Wirings: wirings})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, shared...)
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, "out", file.Path)
		if file.Path == "go.mod" {
			name = filepath.Join(project, "go.mod")
		}
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string][]byte{"fills/rules/rules.go": []byte(domainRuleSource), "out/main_test.go": sharedStorageTests} {
		name = filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-mod=readonly", "-timeout=30s", "-v", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off", "STEGO_TEST_POSTGRES_DSN="+dsn, "STEGO_REQUIRE_POSTGRES="+required)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("shared storage, go %v: %v\n%s", args, err, output)
		}
		t.Log(string(output))
	}
	cmd := exec.Command("go", "list", "-mod=readonly", "-f", "{{join .Imports \"\\n\"}}", "./out/internal/store")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("storage imports: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "/internal/api") || !strings.Contains(string(output), "/contracts/storage") {
		t.Fatalf("storage does not use the independent contract:\n%s", output)
	}

}
