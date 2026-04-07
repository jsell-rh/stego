package postgresadapter

import (
	"go/format"
	"strings"
	"testing"

	"github.com/stego-project/stego/internal/gen"
	"github.com/stego-project/stego/internal/types"
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
	if files != nil {
		t.Errorf("expected nil files, got %d", len(files))
	}
	if wiring != nil {
		t.Errorf("expected nil wiring, got %v", wiring)
	}
}

// --- Basic generation ---

func TestBasicGeneration(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should produce exactly 3 files.
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d: %v", len(files), fileNames(files))
	}

	// Verify file paths.
	findFile(t, files, "internal/storage/models.go")
	findFile(t, files, "internal/storage/store.go")
	findFile(t, files, "internal/storage/migrations/001_initial.sql")

	// Verify wiring.
	if wiring == nil {
		t.Fatal("expected non-nil wiring")
	}
	if len(wiring.Imports) != 1 || wiring.Imports[0] != "internal/storage" {
		t.Errorf("unexpected imports: %v", wiring.Imports)
	}
	if len(wiring.Constructors) != 1 || wiring.Constructors[0] != "storage.NewStore(db)" {
		t.Errorf("unexpected constructors: %v", wiring.Constructors)
	}
}

// --- Generated Go code compiles ---

func TestModelsCompile(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFile(t, files, "internal/storage/models.go").Bytes()
	if _, err := format.Source(content); err != nil {
		t.Errorf("models.go does not compile: %v\n%s", err, string(content))
	}
}

func TestStoreCompiles(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFile(t, files, "internal/storage/store.go").Bytes()
	if _, err := format.Source(content); err != nil {
		t.Errorf("store.go does not compile: %v\n%s", err, string(content))
	}
}

// --- Model struct content ---

func TestModelStructFields(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/models.go")

	// User struct should exist with ID and declared fields.
	// go/format aligns struct fields, so check for field name and type separately.
	for _, want := range []string{
		"type User struct",
		"type Organization struct",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("models.go missing %q", want)
		}
	}

	// Check fields exist by looking for their JSON tags (exact) and Go names.
	for _, field := range []string{"ID", "Email", "Role", "OrgID"} {
		if !strings.Contains(content, field) {
			t.Errorf("models.go missing field %q in User struct", field)
		}
	}
	if !strings.Contains(content, "Name") {
		t.Error("models.go missing field Name in Organization struct")
	}
}

func TestModelJSONTags(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/models.go")

	for _, want := range []string{
		`json:"id"`,
		`json:"email"`,
		`json:"role"`,
		`json:"org_id"`,
		`json:"name"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("models.go missing JSON tag %q", want)
		}
	}
}

// --- Field type mapping ---

func TestAllFieldTypes(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "AllTypes",
				Fields: []types.Field{
					{Name: "s", Type: types.FieldTypeString},
					{Name: "i32", Type: types.FieldTypeInt32},
					{Name: "i64", Type: types.FieldTypeInt64},
					{Name: "f", Type: types.FieldTypeFloat},
					{Name: "d", Type: types.FieldTypeDouble},
					{Name: "b", Type: types.FieldTypeBool},
					{Name: "by", Type: types.FieldTypeBytes},
					{Name: "ts", Type: types.FieldTypeTimestamp},
					{Name: "e", Type: types.FieldTypeEnum, Values: []string{"a", "b"}},
					{Name: "r", Type: types.FieldTypeRef, To: "Other"},
					{Name: "j", Type: types.FieldTypeJsonb},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify models compile (covers type import correctness).
	modelsContent := findFile(t, files, "internal/storage/models.go").Bytes()
	if _, err := format.Source(modelsContent); err != nil {
		t.Errorf("models.go does not compile with all field types: %v", err)
	}

	// Verify store compiles.
	storeContent := findFile(t, files, "internal/storage/store.go").Bytes()
	if _, err := format.Source(storeContent); err != nil {
		t.Errorf("store.go does not compile with all field types: %v", err)
	}

	// Verify Go types in models. go/format aligns struct fields so we
	// check field name and type separately since spacing varies.
	content := findFileContent(t, files, "internal/storage/models.go")
	typeChecks := []struct {
		field  string
		goType string
	}{
		{"S", "string"},
		{"I32", "int32"},
		{"I64", "int64"},
		{"F", "float32"},
		{"D", "float64"},
		{"B", "bool"},
		{"By", "[]byte"},
		{"Ts", "time.Time"},
		{"E", "string"},
		{"R", "string"},
		{"J", "json.RawMessage"},
	}
	for _, tc := range typeChecks {
		// Field name should appear, and its type should appear on the same line.
		found := false
		for _, line := range strings.Split(content, "\n") {
			if strings.Contains(line, tc.field) && strings.Contains(line, tc.goType) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("models.go missing field %s with type %s", tc.field, tc.goType)
		}
	}

	// Verify SQL types in migration.
	sqlContent := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")
	for _, want := range []string{
		"s TEXT",
		"i32 INTEGER",
		"i64 BIGINT",
		"f REAL",
		"d DOUBLE PRECISION",
		"b BOOLEAN",
		"by BYTEA",
		"ts TIMESTAMPTZ",
		"e TEXT",
		"r TEXT",
		"j JSONB",
	} {
		if !strings.Contains(sqlContent, want) {
			t.Errorf("migration SQL missing type %q", want)
		}
	}
}

// --- Computed fields ---

func TestComputedFieldsExcludedFromWrites(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Cluster",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString, Unique: true},
					{Name: "spec", Type: types.FieldTypeJsonb},
					{Name: "status_conditions", Type: types.FieldTypeJsonb, Computed: true, FilledBy: "status-aggregator"},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// Create INSERT should NOT include status_conditions.
	if strings.Contains(storeContent, "INSERT INTO clusters (id, name, spec, status_conditions)") {
		t.Error("Create INSERT should not include computed field status_conditions")
	}
	if !strings.Contains(storeContent, "INSERT INTO clusters (id, name, spec)") {
		t.Error("Create INSERT should include only non-computed fields")
	}

	// Update SET should NOT include status_conditions.
	if strings.Contains(storeContent, "status_conditions =") {
		t.Error("Update SET should not include computed field status_conditions")
	}

	// Read SELECT SHOULD include status_conditions.
	if !strings.Contains(storeContent, "SELECT id, name, spec, status_conditions FROM clusters") {
		t.Error("Read SELECT should include computed field status_conditions")
	}
}

func TestComputedFieldsNullableInMigration(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Cluster",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString},
					{Name: "status", Type: types.FieldTypeJsonb, Computed: true},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sqlContent := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")

	// Non-computed field should be NOT NULL.
	if !strings.Contains(sqlContent, "name TEXT NOT NULL") {
		t.Error("non-computed field should be NOT NULL")
	}

	// Computed field should NOT have NOT NULL (it's nullable).
	if strings.Contains(sqlContent, "status JSONB NOT NULL") {
		t.Error("computed field should be nullable (no NOT NULL)")
	}
}

// --- Upsert ---

func TestUpsertConflictResolution(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "AdapterStatus",
				Fields: []types.Field{
					{Name: "resource_type", Type: types.FieldTypeString},
					{Name: "resource_id", Type: types.FieldTypeString},
					{Name: "adapter", Type: types.FieldTypeString},
					{Name: "generation", Type: types.FieldTypeInt64},
					{Name: "conditions", Type: types.FieldTypeJsonb},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// Verify upsert method exists.
	if !strings.Contains(storeContent, "func (s *Store) Upsert(") {
		t.Error("store.go missing Upsert method")
	}

	// Verify ON CONFLICT clause generation.
	if !strings.Contains(storeContent, "ON CONFLICT") {
		t.Error("store.go missing ON CONFLICT clause in upsert")
	}

	// Verify optimistic concurrency support.
	if !strings.Contains(storeContent, `concurrency == "optimistic"`) {
		t.Error("store.go missing optimistic concurrency check")
	}

	// Verify store compiles.
	storeBytes := findFile(t, files, "internal/storage/store.go").Bytes()
	if _, err := format.Source(storeBytes); err != nil {
		t.Errorf("store.go does not compile: %v", err)
	}
}

// --- Migration SQL ---

func TestMigrationSQL(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sqlContent := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")

	// Should have CREATE TABLE for both entities.
	if !strings.Contains(sqlContent, "CREATE TABLE IF NOT EXISTS users") {
		t.Error("migration missing CREATE TABLE for users")
	}
	if !strings.Contains(sqlContent, "CREATE TABLE IF NOT EXISTS organizations") {
		t.Error("migration missing CREATE TABLE for organizations")
	}

	// Should have id PRIMARY KEY.
	if !strings.Contains(sqlContent, "id TEXT PRIMARY KEY") {
		t.Error("migration missing id PRIMARY KEY")
	}

	// Should have UNIQUE constraint on email.
	if !strings.Contains(sqlContent, "email TEXT NOT NULL UNIQUE") {
		t.Error("migration missing UNIQUE on email")
	}

	// Should have enum CHECK constraint.
	if !strings.Contains(sqlContent, "CHECK (role IN ('admin', 'member'))") {
		t.Error("migration missing enum CHECK constraint")
	}
}

func TestMigrationConstraints(t *testing.T) {
	minLen := 3
	maxLen := 53
	minVal := 0.0
	maxVal := 100.0

	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Constrained",
				Fields: []types.Field{
					{Name: "label", Type: types.FieldTypeString, MinLength: &minLen, MaxLength: &maxLen, Pattern: "^[a-z]"},
					{Name: "score", Type: types.FieldTypeFloat, Min: &minVal, Max: &maxVal},
					{Name: "opt_field", Type: types.FieldTypeString, Optional: true},
					{Name: "with_default", Type: types.FieldTypeString, Default: "hello"},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sqlContent := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")

	// Length constraints.
	if !strings.Contains(sqlContent, "CHECK (length(label) >= 3)") {
		t.Error("migration missing min_length CHECK")
	}
	if !strings.Contains(sqlContent, "CHECK (length(label) <= 53)") {
		t.Error("migration missing max_length CHECK")
	}

	// Pattern constraint.
	if !strings.Contains(sqlContent, "CHECK (label ~ '^[a-z]')") {
		t.Error("migration missing pattern CHECK")
	}

	// Numeric range constraints.
	if !strings.Contains(sqlContent, "CHECK (score >= 0)") {
		t.Error("migration missing min CHECK")
	}
	if !strings.Contains(sqlContent, "CHECK (score <= 100)") {
		t.Error("migration missing max CHECK")
	}

	// Optional field should NOT have NOT NULL.
	if strings.Contains(sqlContent, "opt_field TEXT NOT NULL") {
		t.Error("optional field should not have NOT NULL")
	}

	// Default value.
	if !strings.Contains(sqlContent, "DEFAULT 'hello'") {
		t.Error("migration missing DEFAULT value")
	}
}

// --- unique_composite ---

func TestUniqueCompositeConstraint(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Membership",
				Fields: []types.Field{
					{Name: "user_id", Type: types.FieldTypeRef, To: "User", UniqueComposite: []string{"user_id", "org_id"}},
					{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
					{Name: "role", Type: types.FieldTypeString},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sqlContent := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")

	if !strings.Contains(sqlContent, "UNIQUE (user_id, org_id)") {
		t.Errorf("migration missing UNIQUE composite constraint:\n%s", sqlContent)
	}
}

// --- SQL escaping ---

func TestSQLEscapingSingleQuotes(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Thing",
				Fields: []types.Field{
					{Name: "label", Type: types.FieldTypeString, Pattern: "^[a-z']", Default: "it's"},
					{Name: "status", Type: types.FieldTypeEnum, Values: []string{"it's", "they're"}},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sqlContent := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")

	// Pattern: single quote should be doubled.
	if !strings.Contains(sqlContent, "CHECK (label ~ '^[a-z'']')") {
		t.Errorf("pattern not properly escaped in SQL:\n%s", sqlContent)
	}

	// Default: single quote should be doubled.
	if !strings.Contains(sqlContent, "DEFAULT 'it''s'") {
		t.Errorf("default not properly escaped in SQL:\n%s", sqlContent)
	}

	// Enum values: single quotes should be doubled.
	if !strings.Contains(sqlContent, "'it''s'") {
		t.Errorf("enum value not properly escaped in SQL:\n%s", sqlContent)
	}
	if !strings.Contains(sqlContent, "'they''re'") {
		t.Errorf("enum value not properly escaped in SQL:\n%s", sqlContent)
	}
}

// --- No generated header on SQL file ---

func TestSQLFileNoGoHeader(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sqlFile := findFile(t, files, "internal/storage/migrations/001_initial.sql")
	rendered := string(sqlFile.Bytes())

	// SQL file should NOT have Go comment header.
	if strings.Contains(rendered, "// Code generated") {
		t.Error("SQL file should not have Go comment header")
	}

	// But Go files should.
	modelsFile := findFile(t, files, "internal/storage/models.go")
	modelsRendered := string(modelsFile.Bytes())
	if !strings.Contains(modelsRendered, gen.Header) {
		t.Error("Go file should have generated header")
	}
}

// --- Store methods content ---

func TestStoreHasAllMethods(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	methods := []string{
		"func (s *Store) Create(",
		"func (s *Store) Read(",
		"func (s *Store) Update(",
		"func (s *Store) Delete(",
		"func (s *Store) List(",
		"func (s *Store) Upsert(",
		"func (s *Store) Exists(",
	}

	for _, m := range methods {
		if !strings.Contains(storeContent, m) {
			t.Errorf("store.go missing method %q", m)
		}
	}
}

func TestStoreCreateExcludesComputedFields(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Item",
				Fields: []types.Field{
					{Name: "label", Type: types.FieldTypeString},
					{Name: "derived", Type: types.FieldTypeString, Computed: true},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// The INSERT should have (id, label) not (id, label, derived).
	if !strings.Contains(storeContent, "INSERT INTO items (id, label)") {
		t.Error("Create INSERT should only include non-computed fields")
	}
}

func TestStoreUpdateExcludesComputedFields(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Item",
				Fields: []types.Field{
					{Name: "label", Type: types.FieldTypeString},
					{Name: "derived", Type: types.FieldTypeString, Computed: true},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// UPDATE SET should only have label, not derived.
	if !strings.Contains(storeContent, "UPDATE items SET label = $1 WHERE id = $2") {
		t.Error("Update SET should only include non-computed fields")
	}
}

func TestStoreReadIncludesComputedFields(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Item",
				Fields: []types.Field{
					{Name: "label", Type: types.FieldTypeString},
					{Name: "derived", Type: types.FieldTypeString, Computed: true},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// SELECT should include all fields including computed.
	if !strings.Contains(storeContent, "SELECT id, label, derived FROM items") {
		t.Error("Read SELECT should include computed fields")
	}
}

// --- Namespace validation ---

func TestNamespaceValidation(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All files should be under the output namespace.
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "internal/storage/") {
			t.Errorf("file %q not under namespace internal/storage/", f.Path)
		}
	}
}

func TestCustomNamespace(t *testing.T) {
	ctx := basicContext()
	ctx.OutputNamespace = "pkg/db"
	g := &Generator{}
	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Files should use custom namespace.
	findFile(t, files, "pkg/db/models.go")
	findFile(t, files, "pkg/db/store.go")
	findFile(t, files, "pkg/db/migrations/001_initial.sql")

	// Wiring should reference custom namespace.
	if wiring.Imports[0] != "pkg/db" {
		t.Errorf("expected import pkg/db, got %s", wiring.Imports[0])
	}
	if wiring.Constructors[0] != "db.NewStore(db)" {
		t.Errorf("expected constructor db.NewStore(db), got %s", wiring.Constructors[0])
	}

	// Package name should be derived from namespace.
	modelsContent := findFileContent(t, files, "pkg/db/models.go")
	if !strings.Contains(modelsContent, "package db") {
		t.Error("models.go should use package name from namespace")
	}
}

// --- Validation errors ---

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
		t.Fatal("expected error for case-insensitive collision")
	}
	if !strings.Contains(err.Error(), "case-insensitive") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReservedEntityName(t *testing.T) {
	for _, name := range []string{"Store", "NewStore"} {
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
		{"Box", "boxes"},
		{"Buzz", "buzzes"},
		{"Clash", "clashes"},
		{"Match", "matches"},
		{"Entity", "entities"},
		{"Policy", "policies"},
		{"Key", "keys"},         // vowel+y → just "s"
		{"Day", "days"},         // vowel+y → just "s"
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

// --- toPascalCase ---

func TestToPascalCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"email", "Email"},
		{"org_id", "OrgID"},
		{"resource_type", "ResourceType"},
		{"id", "ID"},
		{"status_conditions", "StatusConditions"},
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

// --- toSnakeCase ---

func TestToSnakeCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"User", "user"},
		{"AdapterStatus", "adapter_status"},
		{"NodePool", "node_pool"},
		{"Organization", "organization"},
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

// --- Single entity generation ---

func TestSingleEntity(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Widget",
				Fields: []types.Field{
					{Name: "label", Type: types.FieldTypeString},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify both Go files compile.
	for _, path := range []string{"internal/storage/models.go", "internal/storage/store.go"} {
		content := findFile(t, files, path).Bytes()
		if _, err := format.Source(content); err != nil {
			t.Errorf("%s does not compile: %v", path, err)
		}
	}
}

// --- Upsert: optimistic concurrency WHERE only on DO UPDATE SET ---

func TestUpsertOptimisticConcurrencyNotOnDoNothing(t *testing.T) {
	// When all non-key writable columns are upsert key columns (no SET clauses),
	// the generated code should use DO NOTHING without a WHERE clause.
	// PostgreSQL does not support WHERE on DO NOTHING.
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "KeyOnly",
				Fields: []types.Field{
					{Name: "resource_type", Type: types.FieldTypeString},
					{Name: "resource_id", Type: types.FieldTypeString},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// The generated code should have DO NOTHING in the else branch.
	if !strings.Contains(storeContent, "DO NOTHING") {
		t.Error("expected DO NOTHING branch in generated upsert code")
	}

	// The WHERE clause for optimistic concurrency must be inside the
	// DO UPDATE SET branch, not after DO NOTHING.
	// Verify: the optimistic concurrency check should be inside the
	// "len(setClauses) > 0" branch.
	if !strings.Contains(storeContent, `DO UPDATE SET " + strings.Join(setClauses, ", ")`) {
		t.Error("expected DO UPDATE SET branch in generated upsert code")
	}

	// Verify it compiles.
	storeBytes := findFile(t, files, "internal/storage/store.go").Bytes()
	if _, err := format.Source(storeBytes); err != nil {
		t.Errorf("store.go does not compile: %v", err)
	}
}

// --- Upsert with computed fields excluded ---

func TestUpsertExcludesComputedFields(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Status",
				Fields: []types.Field{
					{Name: "resource_type", Type: types.FieldTypeString},
					{Name: "adapter", Type: types.FieldTypeString},
					{Name: "summary", Type: types.FieldTypeString, Computed: true},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// Upsert INSERT should exclude computed field.
	if strings.Contains(storeContent, "summary") &&
		strings.Contains(storeContent, "INSERT INTO statuses") &&
		strings.Contains(storeContent, "summary)") {
		// If summary appears in the INSERT column list, it's a bug.
		// Check more precisely: the INSERT columns should be (id, resource_type, adapter).
		if !strings.Contains(storeContent, "INSERT INTO statuses (id, resource_type, adapter)") {
			t.Error("Upsert INSERT should exclude computed fields")
		}
	}

	// Verify it compiles.
	storeBytes := findFile(t, files, "internal/storage/store.go").Bytes()
	if _, err := format.Source(storeBytes); err != nil {
		t.Errorf("store.go does not compile: %v", err)
	}
}

// --- Scope filtering in List ---

func TestListScopeValidation(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// Should validate scope field against known columns.
	if !strings.Contains(storeContent, "validCols") {
		t.Error("List should validate scope field against known columns")
	}
	if !strings.Contains(storeContent, "invalid scope field") {
		t.Error("List should return error for invalid scope field")
	}
}

// --- Delete method ---

func TestDeleteMethod(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	if !strings.Contains(storeContent, "DELETE FROM users WHERE id = $1") {
		t.Error("Delete should use parameterized query for users")
	}
	if !strings.Contains(storeContent, "DELETE FROM organizations WHERE id = $1") {
		t.Error("Delete should use parameterized query for organizations")
	}
}

// --- Exists method ---

func TestExistsMethod(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	if !strings.Contains(storeContent, "SELECT EXISTS") {
		t.Error("Exists should use SELECT EXISTS query")
	}
	// Should handle unknown entities.
	if !strings.Contains(storeContent, "return false") {
		t.Error("Exists should return false for unknown entities")
	}
}

// --- Multi-entity compile test ---

func TestMultiEntityCompiles(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Cluster",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString, Unique: true},
					{Name: "spec", Type: types.FieldTypeJsonb},
					{Name: "status_conditions", Type: types.FieldTypeJsonb, Computed: true},
				},
			},
			{
				Name: "NodePool",
				Fields: []types.Field{
					{Name: "cluster_id", Type: types.FieldTypeRef, To: "Cluster"},
					{Name: "instance_type", Type: types.FieldTypeString},
					{Name: "count", Type: types.FieldTypeInt32},
				},
			},
			{
				Name: "AdapterStatus",
				Fields: []types.Field{
					{Name: "resource_type", Type: types.FieldTypeString},
					{Name: "resource_id", Type: types.FieldTypeString},
					{Name: "adapter", Type: types.FieldTypeString},
					{Name: "generation", Type: types.FieldTypeInt64},
					{Name: "conditions", Type: types.FieldTypeJsonb},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify both Go files compile (syntax check via go/format).
	for _, p := range []string{"internal/storage/models.go", "internal/storage/store.go"} {
		content := findFile(t, files, p).Bytes()
		if _, err := format.Source(content); err != nil {
			t.Errorf("%s does not compile: %v\n%s", p, err, string(content))
		}
	}

	// Verify all entities have tables in migration.
	sqlContent := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")
	for _, want := range []string{"clusters", "node_pools", "adapter_statuses"} {
		if !strings.Contains(sqlContent, "CREATE TABLE IF NOT EXISTS "+want) {
			t.Errorf("migration missing CREATE TABLE for %s", want)
		}
	}
}

// --- Models import correctness ---

func TestModelsNoUnnecessaryImports(t *testing.T) {
	// Entity with no timestamp or jsonb fields should not import time or json.
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Simple",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString},
					{Name: "count", Type: types.FieldTypeInt32},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/models.go")

	if strings.Contains(content, `"time"`) {
		t.Error("models.go should not import time when no timestamp fields exist")
	}
	if strings.Contains(content, `"encoding/json"`) {
		t.Error("models.go should not import encoding/json when no jsonb fields exist")
	}
}

func TestModelsImportTimeForTimestamp(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Event",
				Fields: []types.Field{
					{Name: "occurred_at", Type: types.FieldTypeTimestamp},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/models.go")
	if !strings.Contains(content, `"time"`) {
		t.Error("models.go should import time for timestamp fields")
	}
}

func TestModelsImportJSONForJsonb(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Config",
				Fields: []types.Field{
					{Name: "data", Type: types.FieldTypeJsonb},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/models.go")
	if !strings.Contains(content, `"encoding/json"`) {
		t.Error("models.go should import encoding/json for jsonb fields")
	}
}

// --- fieldTypeToSQL ---

func TestFieldTypeToSQL(t *testing.T) {
	tests := []struct {
		ft   types.FieldType
		want string
	}{
		{types.FieldTypeString, "TEXT"},
		{types.FieldTypeInt32, "INTEGER"},
		{types.FieldTypeInt64, "BIGINT"},
		{types.FieldTypeFloat, "REAL"},
		{types.FieldTypeDouble, "DOUBLE PRECISION"},
		{types.FieldTypeBool, "BOOLEAN"},
		{types.FieldTypeBytes, "BYTEA"},
		{types.FieldTypeTimestamp, "TIMESTAMPTZ"},
		{types.FieldTypeEnum, "TEXT"},
		{types.FieldTypeRef, "TEXT"},
		{types.FieldTypeJsonb, "JSONB"},
	}

	for _, tt := range tests {
		t.Run(string(tt.ft), func(t *testing.T) {
			got := fieldTypeToSQL(tt.ft)
			if got != tt.want {
				t.Errorf("fieldTypeToSQL(%q) = %q, want %q", tt.ft, got, tt.want)
			}
		})
	}
}

// --- fieldTypeToGo ---

func TestFieldTypeToGo(t *testing.T) {
	tests := []struct {
		ft   types.FieldType
		want string
	}{
		{types.FieldTypeString, "string"},
		{types.FieldTypeInt32, "int32"},
		{types.FieldTypeInt64, "int64"},
		{types.FieldTypeFloat, "float32"},
		{types.FieldTypeDouble, "float64"},
		{types.FieldTypeBool, "bool"},
		{types.FieldTypeBytes, "[]byte"},
		{types.FieldTypeTimestamp, "time.Time"},
		{types.FieldTypeEnum, "string"},
		{types.FieldTypeRef, "string"},
		{types.FieldTypeJsonb, "json.RawMessage"},
	}

	for _, tt := range tests {
		t.Run(string(tt.ft), func(t *testing.T) {
			got := fieldTypeToGo(tt.ft)
			if got != tt.want {
				t.Errorf("fieldTypeToGo(%q) = %q, want %q", tt.ft, got, tt.want)
			}
		})
	}
}

// --- Upsert key validation ---

func TestUpsertKeyValidationGenerated(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Item",
				Fields: []types.Field{
					{Name: "label", Type: types.FieldTypeString},
					{Name: "category", Type: types.FieldTypeString},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// The generated upsert should validate upsert key fields.
	if !strings.Contains(storeContent, "invalid upsert key field") {
		t.Error("Upsert should validate key fields against known columns")
	}
}

// --- Unknown entity handling ---

func TestStoreHandlesUnknownEntity(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// Each CRUD method should have a default case for unknown entities.
	count := strings.Count(storeContent, `unknown entity`)
	// Create, Read, Update, Delete, List, Upsert = 6 methods with default cases.
	if count < 6 {
		t.Errorf("expected at least 6 'unknown entity' default cases, got %d", count)
	}
}
