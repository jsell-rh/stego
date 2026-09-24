package postgresadapter

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/schema_generation_test.go
var schemaGenerationTests []byte

func TestSchemaGenerationValidationAndWiring(t *testing.T) {
	for _, value := range []any{"", "UPPER", "a/b", "a' OR true", strings.Repeat("a", 65), true, nil} {
		ctx := basicContext()
		ctx.ComponentConfig = map[string]any{"schema_generation": value}
		if _, _, err := new(Generator).Generate(ctx); err == nil {
			t.Fatalf("accepted invalid generation %v", value)
		}
	}
	for _, mode := range []string{"startup", "external"} {
		ctx := basicContext()
		ctx.ComponentConfig = map[string]any{"schema_generation": "fresh-v1", "migrations": mode}
		files, wiring, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(findFileContent(t, files, "internal/storage/migrate.go"), "BootstrapSchema(db, func(tx *gorm.DB) error {") {
			t.Fatal("migration has no schema gate")
		}
		if !strings.Contains(findFileContent(t, files, "internal/storage/store.go"), "VerifySchema(db)") {
			t.Fatal("store has no schema gate")
		}
		if (len(wiring.PostDBCalls) == 0) != (mode == "external") {
			t.Fatal("external migration policy changed")
		}
		first := findFileContent(t, files, "internal/storage/schema_generation.go")
		again, _, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if first != findFileContent(t, again, "internal/storage/schema_generation.go") {
			t.Fatal("schema generation is unstable")
		}
	}
	ctx := basicContext()
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file.Path, "schema_generation.go") {
			t.Fatal("unselected schema policy changed")
		}
	}
	for _, name := range []string{"SchemaGeneration", "SchemaDefinition", "BootstrapSchema", "VerifySchema", "readSchemaGeneration", "ErrSchemaGeneration", "WriterLeaseCheck", "ErrWriterFenced", "RestoreRecord", "ReadRestoreRecord", "ReadRestoreRecordDB", "VerifyRestore", "VerifyRestoreDB", "ErrRestore", "ErrMigrationLedger", "AppliedMigrations", "ApplyMigration", "RegisterSQL"} {
		ctx := basicContext()
		ctx.Entities = []types.Entity{{Name: name}}
		if _, _, err := new(Generator).Generate(ctx); err == nil {
			t.Fatal("accepted schema helper entity", name)
		}
	}
}

func TestGeneratedSchemaGeneration(t *testing.T) {
	project := t.TempDir()
	ctx := gen.Context{OutputNamespace: "storage", ModuleName: "example.com/schema-gate", ComponentConfig: map[string]any{"schema_generation": "fresh-v1", "migrations": "external"}, Entities: []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}}, {Name: "Versioned", Versioned: true, Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}}}}
	for _, packageName := range []string{"storage", "legacy", "future", "changed"} {
		c := ctx
		c.OutputNamespace = packageName
		c.ComponentConfig = map[string]any{"schema_generation": "fresh-v1", "migrations": "external"}
		if packageName == "legacy" {
			c.ComponentConfig = map[string]any{"migrations": "external"}
		}
		if packageName == "future" {
			c.ComponentConfig["schema_generation"] = "fresh-v2"
		}
		if packageName == "changed" {
			c.Entities = []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}, {Name: "extra", Type: types.FieldTypeString}}}, {Name: "Versioned", Versioned: true, Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}, {Name: "note", Type: types.FieldTypeString}}}}
		}
		files, _, err := new(Generator).Generate(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			target := filepath.Join(project, file.Path)
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, file.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	module := `module example.com/schema-gate

go 1.25.0

require (
 github.com/google/uuid v1.6.0
 github.com/jackc/pgx/v5 v5.11.0
 gorm.io/gorm v1.25.12
 gorm.io/driver/postgres v1.5.11
)
`
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(module), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "storage/schema_generation_test.go"), schemaGenerationTests, 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy", "-go=1.25.0"}, {"test", "-race", "-mod=readonly", "-count=1", "-timeout=90s", "-v", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		result, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("generated schema test: %v\n%s", err, result)
		}
		t.Log(string(result))
	}
}
