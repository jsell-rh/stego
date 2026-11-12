package postgresadapter

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/schema_generation_test.go
var schemaGenerationTests []byte

// schemaContext gives a service with entities on the runtime data path in namespace ns, with the given settings.
func schemaContext(ns string, config map[string]any) gen.Context {
	return gen.Context{OutputNamespace: ns, ModuleName: "example.com/schema-gate", RuntimeData: true,
		PeerNamespaces:  map[string]string{"command": "commands", "query": "queries"},
		ComponentConfig: config,
		Entities:        []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}}, {Name: "Note", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}}}}
}

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
	ctx.ComponentConfig = map[string]any{}
	if files, _, err := new(Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "schema_generation") || files != nil {
		t.Fatal("a service with entities and no schema_generation is accepted", err)
	}
	for _, name := range []string{"SchemaGeneration", "SchemaDefinition", "BootstrapSchema", "VerifySchema", "readSchemaGeneration", "ErrSchemaGeneration", "RestoreRecord", "ErrRestore", "ErrMigrationLedger", "AppliedMigrations", "ApplyMigration", "RegisterSQL"} {
		ctx := basicContext()
		ctx.Entities = []types.Entity{{Name: name}}
		if _, _, err := new(Generator).Generate(ctx); err == nil {
			t.Fatal("accepted schema helper entity", name)
		}
	}
}

func TestWriterLeaseModeValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*gen.Context)
		wantErr string
	}{
		{"left out accepted", func(c *gen.Context) {}, ""},
		{"off accepted", func(c *gen.Context) { c.ComponentConfig["writer_lease"] = "off" }, ""},
		{"single refused", func(c *gen.Context) { c.ComponentConfig["writer_lease"] = "single" }, "writer_lease single is refused"},
		{"invalid value rejected", func(c *gen.Context) { c.ComponentConfig["writer_lease"] = "shared" }, "writer_lease must be off or left out"},
		{"non-string rejected", func(c *gen.Context) { c.ComponentConfig["writer_lease"] = 3 }, "writer_lease must be off or left out"},
		{"requires schema_generation", func(c *gen.Context) { c.ComponentConfig["writer_lease"] = "off"; delete(c.ComponentConfig, "schema_generation") }, "writer_lease requires schema_generation"},
	}
	for _, tc := range cases {
		ctx := schemaContext("storage", map[string]any{"schema_generation": "fresh-v1"})
		tc.mutate(&ctx)
		files, _, err := new(Generator).Generate(ctx)
		if tc.wantErr == "" {
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) || files != nil {
			t.Fatalf("%s: got %v, want %q", tc.name, err, tc.wantErr)
		}
		if tc.name == "single refused" && !strings.Contains(err.Error(), "ST-SUP-RC20-OC39") {
			t.Fatalf("the refusal of the lease does not name its cause: %v", err)
		}
	}
}

func TestWriterLeaseOffOmitsFence(t *testing.T) {
	for _, config := range []map[string]any{{"schema_generation": "fresh-v1", "writer_lease": "off"}, {"schema_generation": "fresh-v1"}} {
		files, wiring, err := new(Generator).Generate(schemaContext("storage", config))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			for _, banned := range []string{"writerFence", "WriterLeaseCheck", "ErrWriterFenced", "fenceAutocommit", "writer_lease"} {
				if strings.Contains(string(file.Bytes()), banned) {
					t.Fatalf("%s contains %q", file.Path, banned)
				}
			}
		}
		for _, object := range append(append([]gen.DatabaseObject{}, wiring.DatabaseAccess...), backupObjects(wiring)...) {
			if object.Name == "writer_lease" {
				t.Fatal("the wiring names writer_lease")
			}
		}
		if LeaseEnabled(config) {
			t.Fatal("LeaseEnabled reports a lease that the adapter does not emit")
		}
	}
}

// backupObjects gives the backup manifest of the wiring as database objects, so that one loop reads both lists.
func backupObjects(wiring *gen.Wiring) []gen.DatabaseObject {
	var objects []gen.DatabaseObject
	for _, object := range wiring.BackupObjects {
		objects = append(objects, gen.DatabaseObject{Schema: object.Schema, Name: object.Name, Kind: object.Kind})
	}
	return objects
}

func TestGeneratedSchemaGeneration(t *testing.T) {
	project := t.TempDir()
	for _, packageName := range []string{"storage", "future", "changed"} {
		c := schemaContext(packageName, map[string]any{"schema_generation": "fresh-v1", "migrations": "external"})
		if packageName == "future" {
			c.ComponentConfig["schema_generation"] = "fresh-v2"
		}
		if packageName == "changed" {
			c.Entities = []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}, {Name: "extra", Type: types.FieldTypeString}}}, {Name: "Note", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}}}
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
	// runtime_access.go imports the runtime library, so the module takes it from this repository's registry/runtime.
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	module := `module example.com/schema-gate

go 1.26.11

require (
 github.com/google/uuid v1.6.0
 github.com/jackc/pgx/v5 v5.11.0
 gorm.io/gorm v1.25.12
 gorm.io/driver/postgres v1.5.11
 github.com/jsell-rh/stego/registry/runtime v0.38.0
)

replace github.com/jsell-rh/stego/registry/runtime => ` + runtimeDir + "\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(module), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "storage/schema_generation_test.go"), schemaGenerationTests, 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-mod=readonly", "-count=1", "-timeout=90s", "-v", "./..."}} {
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

func TestFunctionMigrationDigestCoversBody(t *testing.T) {
	seal := func(statement string) string {
		t.Helper()
		source := "package storage\n\nconst probeMigration = " + strconv.Quote(statement) + "\n\nfunc init() {\n\tRegister(\"900_probe\", func(db *gorm.DB) error { return db.Exec(probeMigration).Error })\n}\n"
		files := []gen.File{{Path: "storage/probe.go", Content: []byte(source)}}
		if err := sealMigrations("storage", files); err != nil {
			t.Fatal(err)
		}
		content := string(files[0].Content)
		start := strings.Index(content, `Register("900_probe", "`)
		if start < 0 {
			t.Fatal("the migration has no digest", content)
		}
		digest := content[start+len(`Register("900_probe", "`):]
		return digest[:strings.Index(digest, `"`)]
	}
	first := seal("CREATE TABLE probe (id boolean PRIMARY KEY)")
	if first != seal("CREATE TABLE probe (id boolean PRIMARY KEY)") {
		t.Fatal("the digest is unstable")
	}
	if first == seal("CREATE TABLE probe (id bigint PRIMARY KEY)") {
		t.Fatal("an edited migration body kept its digest")
	}
	sum := sha256.Sum256([]byte("900_probe"))
	if first == hex.EncodeToString(sum[:]) {
		t.Fatal("the digest covers only the name")
	}
	files, _, err := new(Generator).Generate(basicContext())
	if err != nil {
		t.Fatal(err)
	}
	migrate := findFileContent(t, files, "internal/storage/migrate.go")
	if !strings.Contains(migrate, `RegisterSQL("001_initial", InitialTables)`) || strings.Contains(migrate, "Digest(name)") || strings.Contains(migrate, "Digest(row.Name)") {
		t.Fatal("the initial migration is not sealed by its body", migrate)
	}
}
