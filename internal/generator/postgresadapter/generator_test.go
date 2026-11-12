package postgresadapter

import (
	"go/format"
	"math"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// --- Test helpers ---

func basicContext() gen.Context {
	return gen.Context{
		Conventions: types.Convention{
			Layout:        "flat",
			ErrorHandling: "problem-details-rfc",
			Logging:       "structured-json",
			TestPattern:   "table-driven",
		},
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "email", Type: types.FieldTypeString, Unique: true},
					{Name: "role", Type: types.FieldTypeEnum, Values: []string{"admin", "member"}},
					{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
				},
			},
			{
				Name: "Organization",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString, Unique: true},
				},
			},
		},
		OutputNamespace: "internal/storage",
		PeerNamespaces:  map[string]string{"command": "commands", "query": "queries"},
		ComponentConfig: map[string]any{"schema_generation": "basic-v1"},
	}
}

func findFile(t *testing.T, files []gen.File, path string) gen.File {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("file not found: %s; got files: %v", path, fileNames(files))
	return gen.File{}
}

func findFileContent(t *testing.T, files []gen.File, path string) string {
	t.Helper()
	return string(findFile(t, files, path).Content)
}

func fileNames(files []gen.File) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Path
	}
	return names
}

// --- Interface compliance ---

func TestGeneratorImplementsInterface(t *testing.T) {
	var _ gen.Generator = (*Generator)(nil)
}

// --- Empty input ---

func TestEmptyEntities(t *testing.T) {
	ctx := gen.Context{
		Entities:        nil,
		OutputNamespace: "internal/storage",
	}
	g := &Generator{}
	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 || files[0].Path != "internal/storage/database.go" {
		t.Fatal("missing pool-only output")
	}
	if wiring == nil || wiring.DatabaseOpener == nil || wiring.DatabaseOpener.Function != "OpenDatabase" || len(wiring.Constructors) != 0 || len(wiring.PostDBCalls) != 0 {
		t.Fatal("invalid pool-only wiring")
	}
	ctx.OutputNamespace = "../invalid"
	if g.ValidateContext(ctx) == nil {
		t.Fatal("pool-only namespace escaped validation")
	}
	if files, _, err := g.Generate(ctx); err == nil || len(files) != 0 {
		t.Fatal("invalid pool-only namespace rendered output")
	}
}

// --- Basic generation ---

// TestBasicGeneration checks the files and the wiring of a service with entities: the tables, the ledger, the
// marker, the opener and the runtime access, and no store.
func TestBasicGeneration(t *testing.T) {
	ctx := basicContext()
	files, wiring, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"internal/storage/tables.go", "internal/storage/migrations/001_initial.sql", "internal/storage/migrate.go",
		"internal/storage/schema_generation.go", "internal/storage/database.go", "internal/storage/runtime_access.go"}
	if strings.Join(fileNames(files), " ") != strings.Join(want, " ") {
		t.Fatalf("files %v, want %v", fileNames(files), want)
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".go") {
			if _, err := format.Source(f.Bytes()); err != nil {
				t.Errorf("%s does not parse: %v", f.Path, err)
			}
			if !strings.Contains(string(f.Bytes()), gen.Header) {
				t.Errorf("%s has no generated header", f.Path)
			}
		}
	}
	if len(wiring.Imports) != 1 || wiring.Imports[0] != "internal/storage" || len(wiring.Constructors) != 0 || !wiring.NeedsDB {
		t.Errorf("unexpected wiring: %+v", wiring)
	}
	// The migration job of the runtime wiring runs the ledger, so the wiring has no call after the database opens.
	if len(wiring.PostDBCalls) != 0 {
		t.Errorf("expected no PostDBCalls, got %v", wiring.PostDBCalls)
	}
	// The ledger runs on database/sql with the pgx driver: the wiring requires uuid and pgx, and no other module.
	if len(wiring.GoModRequires) != 2 {
		t.Errorf("GoModRequires = %v, want github.com/google/uuid and github.com/jackc/pgx/v5 only", wiring.GoModRequires)
	}
	for _, mod := range []string{"github.com/google/uuid", "github.com/jackc/pgx/v5"} {
		if _, ok := wiring.GoModRequires[mod]; !ok {
			t.Errorf("GoModRequires missing %q", mod)
		}
	}
}

// TestTableOrderFollowsReferences checks that a referenced table comes first in 001_initial.
func TestTableOrderFollowsReferences(t *testing.T) {
	ctx := basicContext()
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")
	org, user := strings.Index(body, `CREATE TABLE public."organizations"`), strings.Index(body, `CREATE TABLE public."users"`)
	if org < 0 || user < 0 || org > user {
		t.Errorf("organizations must come before users in 001_initial:\n%s", body)
	}
}

// --- Generated Go code compiles ---

func TestMigrateCompiles(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFile(t, files, "internal/storage/migrate.go").Bytes()
	if _, err := format.Source(content); err != nil {
		t.Errorf("migrate.go does not compile: %v\n%s", err, string(content))
	}
}

func TestWiringIncludesUUIDDependency(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	_, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wiring == nil {
		t.Fatal("expected non-nil wiring")
	}

	if _, ok := wiring.GoModRequires["github.com/google/uuid"]; !ok {
		t.Error("GoModRequires should include github.com/google/uuid")
	}
}

func TestReservedEntityNameUUID(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{Name: "uuid", Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}}},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for entity name 'uuid' (import alias collision)")
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("error should mention collision, got: %v", err)
	}
}

// --- Upsert ---

// --- Migration uses AutoMigrate ---

func TestMigrationCircularDependency(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{Name: "A", Fields: []types.Field{{Name: "b_id", Type: types.FieldTypeRef, To: "B"}}},
			{Name: "B", Fields: []types.Field{{Name: "a_id", Type: types.FieldTypeRef, To: "A"}}},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for circular ref dependency")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error should mention circular dependency, got: %v", err)
	}
}

// --- SessionFactory ---

// --- GenericDao ---

// --- Soft delete ---

// --- Computed fields ---

// --- Namespace validation ---

func TestNamespaceValidation(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		if !strings.HasPrefix(f.Path, "internal/storage/") {
			t.Errorf("file %q not under namespace internal/storage/", f.Path)
		}
	}
}

// --- Field type mapping ---

// --- fieldTypeToGo ---

// --- Nullable pointer types ---

// --- Exists method ---

// --- Scope filtering in List ---

// --- Unknown entity handling ---

// --- Optimistic concurrency ---

// --- Table name derivation ---

func TestTableNameDerivation(t *testing.T) {
	tests := []struct {
		entity string
		want   string
	}{
		{"User", "users"},
		{"Organization", "organizations"},
		{"AdapterStatus", "adapter_statuses"},
		{"NodePool", "node_pools"},
		{"Address", "addresses"},
		{"Entity", "entities"},
		{"Policy", "policies"},
		{"Key", "keys"},
		{"Day", "days"},
	}

	for _, tt := range tests {
		t.Run(tt.entity, func(t *testing.T) {
			got := tableName(tt.entity)
			if got != tt.want {
				t.Errorf("tableName(%q) = %q, want %q", tt.entity, got, tt.want)
			}
		})
	}
}

// --- toPascalCase / toSnakeCase ---

func TestToPascalCase(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"email", "Email"},
		{"org_id", "OrgID"},
		{"resource_type", "ResourceType"},
		{"id", "ID"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := toPascalCase(tt.input)
			if got != tt.want {
				t.Errorf("toPascalCase(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestToSnakeCase(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"User", "user"},
		{"AdapterStatus", "adapter_status"},
		{"NodePool", "node_pool"},
		{"HTTPServer", "http_server"},
		{"APIKey", "api_key"},
		{"UserID", "user_id"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := toSnakeCase(tt.input)
			if got != tt.want {
				t.Errorf("toSnakeCase(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- Validation errors (unchanged from raw SQL) ---

func TestDuplicateEntityNames(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}}},
			{Name: "User", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for duplicate entity names")
	}
	if !strings.Contains(err.Error(), "duplicate entity names") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCaseInsensitiveEntityCollision(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}}},
			{Name: "USER", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for table name collision")
	}
	if !strings.Contains(err.Error(), "table name") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReservedEntityName(t *testing.T) {
	for _, name := range []string{"Migrate", "OpenDatabase", "BootstrapSchema", "ApplyMigration"} {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{
				Entities: []types.Entity{
					{Name: name, Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}}},
				},
				OutputNamespace: "internal/storage",
			}

			g := &Generator{}
			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error for reserved entity name %q", name)
			}
			if !strings.Contains(err.Error(), "collides") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestReservedEntityNameGoKeywords(t *testing.T) {
	keywords := []string{"func", "return", "type", "struct", "if", "for", "switch",
		"case", "package", "import", "var", "const", "map"}

	for _, name := range keywords {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{
				Entities: []types.Entity{
					{Name: name, Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}}},
				},
				OutputNamespace: "internal/storage",
			}

			g := &Generator{}
			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error for Go keyword entity name %q", name)
			}
			if !strings.Contains(err.Error(), "collides") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestReservedEntityNameImportAliases(t *testing.T) {
	aliases := []string{"fmt", "sql", "time", "uuid", "Database", "inTransaction", "lockLedger", "recordMigration"}

	for _, name := range aliases {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{
				Entities: []types.Entity{
					{Name: name, Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}}},
				},
				OutputNamespace: "internal/storage",
			}

			g := &Generator{}
			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error for entity name %q (import alias collision)", name)
			}
			if !strings.Contains(err.Error(), "collides") {
				t.Errorf("error should mention collision, got: %v", err)
			}
		})
	}
}

func TestDuplicateFieldNames(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "email", Type: types.FieldTypeString},
					{Name: "email", Type: types.FieldTypeString},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for duplicate field names")
	}
	if !strings.Contains(err.Error(), "duplicate field name") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRefTargetNonExistentEntity(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "org_id", Type: types.FieldTypeRef, To: "Organziation"},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for ref field referencing non-existent entity")
	}
	if !strings.Contains(err.Error(), "Organziation") {
		t.Errorf("error should mention the invalid ref target, got: %v", err)
	}
}

func TestRefFieldEmptyToReturnsError(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "org_id", Type: types.FieldTypeRef, To: ""},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for ref field with empty 'to' attribute")
	}
	if !strings.Contains(err.Error(), "no 'to' attribute") {
		t.Errorf("error should mention missing 'to' attribute, got: %v", err)
	}
}

func TestFieldNameIDCollision(t *testing.T) {
	for _, fieldName := range []string{"id", "Id"} {
		t.Run(fieldName, func(t *testing.T) {
			ctx := gen.Context{
				Entities: []types.Entity{
					{
						Name:   "User",
						Fields: []types.Field{{Name: fieldName, Type: types.FieldTypeString}},
					},
				},
				OutputNamespace: "internal/storage",
			}

			g := &Generator{}
			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error for field named %q colliding with implicit ID", fieldName)
			}
			if !strings.Contains(err.Error(), "implicit primary key") {
				t.Errorf("error should mention implicit primary key, got: %v", err)
			}
		})
	}
}

func TestEnumFieldEmptyValuesReturnsError(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "role", Type: types.FieldTypeEnum, Values: []string{}},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for enum field with empty values")
	}
	if !strings.Contains(err.Error(), "enum") {
		t.Errorf("error should mention enum, got: %v", err)
	}
}

func TestEnumDuplicateValues(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "role", Type: types.FieldTypeEnum, Values: []string{"admin", "admin", "member"}},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for duplicate enum values")
	}
	if !strings.Contains(err.Error(), "duplicate enum value") {
		t.Errorf("error should mention duplicate enum value, got: %v", err)
	}
}

// --- Go header ---

// --- Multi-entity compile test ---

// --- Computed field handling in Upsert ---

// --- Replace uses selective update ---

func TestGeneratorRejectsInvalidNumericBounds(t *testing.T) {
	zero, one, nan, infinity := 0.0, 1.0, math.NaN(), math.Inf(1)
	for _, field := range []types.Field{
		{Name: "value", Type: types.FieldTypeString, Min: &zero},
		{Name: "value", Type: types.FieldTypeDouble, Min: &nan},
		{Name: "value", Type: types.FieldTypeInt64, Max: &infinity},
		{Name: "value", Type: types.FieldTypeInt32, Min: &one, Max: &zero},
	} {
		ctx := gen.Context{OutputNamespace: "storage", Entities: []types.Entity{{Name: "Record", Fields: []types.Field{field}}}}
		if _, _, err := new(Generator).Generate(ctx); err == nil {
			t.Fatal("invalid numeric bounds were accepted", field)
		}
	}
}
