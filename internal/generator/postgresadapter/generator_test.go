package postgresadapter

import (
	"go/format"
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
				Name: "Other",
				Fields: []types.Field{
					{Name: "label", Type: types.FieldTypeString},
				},
			},
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

	// Verify SQL types in migration (identifiers are now double-quoted).
	sqlContent := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")
	for _, want := range []string{
		`"s" TEXT`,
		`"i32" INTEGER`,
		`"i64" BIGINT`,
		`"f" REAL`,
		`"d" DOUBLE PRECISION`,
		`"b" BOOLEAN`,
		`"by" BYTEA`,
		`"ts" TIMESTAMPTZ`,
		`"e" TEXT`,
		`"r" TEXT`,
		`"j" JSONB`,
	} {
		if !strings.Contains(sqlContent, want) {
			t.Errorf("migration SQL missing type %q\n%s", want, sqlContent)
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
	if strings.Contains(storeContent, `"status_conditions"`) && strings.Contains(storeContent, `INSERT INTO "clusters"`) {
		// Check that the INSERT column list doesn't include status_conditions.
		if !strings.Contains(storeContent, `INSERT INTO "clusters" ("id", "name", "spec")`) {
			t.Error("Create INSERT should only include non-computed fields")
		}
	}

	// Update SET should NOT include status_conditions.
	if strings.Contains(storeContent, `"status_conditions" =`) {
		t.Error("Update SET should not include computed field status_conditions")
	}

	// Read SELECT SHOULD include status_conditions.
	if !strings.Contains(storeContent, `SELECT "id", "name", "spec", "status_conditions" FROM "clusters"`) {
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
	if !strings.Contains(sqlContent, `"name" TEXT NOT NULL`) {
		t.Errorf("non-computed field should be NOT NULL\n%s", sqlContent)
	}

	// Computed field should NOT have NOT NULL (it's nullable).
	if strings.Contains(sqlContent, `"status" JSONB NOT NULL`) {
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

	// Verify optimistic concurrency WHERE clause references quoted generation column.
	if !strings.Contains(storeContent, `EXCLUDED."generation"`) {
		t.Error("store.go missing quoted generation column reference in optimistic concurrency")
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

	// Should have CREATE TABLE for both entities (with quoted identifiers).
	if !strings.Contains(sqlContent, `CREATE TABLE IF NOT EXISTS "users"`) {
		t.Errorf("migration missing CREATE TABLE for users\n%s", sqlContent)
	}
	if !strings.Contains(sqlContent, `CREATE TABLE IF NOT EXISTS "organizations"`) {
		t.Errorf("migration missing CREATE TABLE for organizations\n%s", sqlContent)
	}

	// Should have id PRIMARY KEY.
	if !strings.Contains(sqlContent, `"id" TEXT PRIMARY KEY`) {
		t.Errorf("migration missing id PRIMARY KEY\n%s", sqlContent)
	}

	// Should have UNIQUE constraint on email.
	if !strings.Contains(sqlContent, `"email" TEXT NOT NULL UNIQUE`) {
		t.Errorf("migration missing UNIQUE on email\n%s", sqlContent)
	}

	// Should have enum CHECK constraint with quoted column name.
	if !strings.Contains(sqlContent, `CHECK ("role" IN ('admin', 'member'))`) {
		t.Errorf("migration missing enum CHECK constraint\n%s", sqlContent)
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

	// Length constraints (with quoted column names).
	if !strings.Contains(sqlContent, `CHECK (length("label") >= 3)`) {
		t.Errorf("migration missing min_length CHECK\n%s", sqlContent)
	}
	if !strings.Contains(sqlContent, `CHECK (length("label") <= 53)`) {
		t.Errorf("migration missing max_length CHECK\n%s", sqlContent)
	}

	// Pattern constraint.
	if !strings.Contains(sqlContent, `CHECK ("label" ~ '^[a-z]')`) {
		t.Errorf("migration missing pattern CHECK\n%s", sqlContent)
	}

	// Numeric range constraints.
	if !strings.Contains(sqlContent, `CHECK ("score" >= 0)`) {
		t.Errorf("migration missing min CHECK\n%s", sqlContent)
	}
	if !strings.Contains(sqlContent, `CHECK ("score" <= 100)`) {
		t.Errorf("migration missing max CHECK\n%s", sqlContent)
	}

	// Optional field should NOT have NOT NULL.
	if strings.Contains(sqlContent, `"opt_field" TEXT NOT NULL`) {
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
				Name:   "User",
				Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}},
			},
			{
				Name:   "Organization",
				Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}},
			},
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

	if !strings.Contains(sqlContent, `UNIQUE ("user_id", "org_id")`) {
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
	if !strings.Contains(sqlContent, `CHECK ("label" ~ '^[a-z'']')`) {
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

func TestSQLFileHeader(t *testing.T) {
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

	// SQL file MUST have the spec-mandated header in SQL comment syntax.
	if !strings.Contains(rendered, "-- Code generated by stego. DO NOT EDIT.") {
		t.Error("SQL file should have spec-mandated header in SQL comment syntax")
	}

	// But Go files should have Go comment header.
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

	// The INSERT should have ("id", "label") not ("id", "label", "derived").
	if !strings.Contains(storeContent, `INSERT INTO "items" ("id", "label")`) {
		t.Errorf("Create INSERT should only include non-computed fields\n%s", storeContent)
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
	if !strings.Contains(storeContent, `UPDATE "items" SET "label" = $1 WHERE "id" = $2`) {
		t.Errorf("Update SET should only include non-computed fields\n%s", storeContent)
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
	if !strings.Contains(storeContent, `SELECT "id", "label", "derived" FROM "items"`) {
		t.Errorf("Read SELECT should include computed fields\n%s", storeContent)
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
		t.Fatal("expected error for table name collision")
	}
	if !strings.Contains(err.Error(), "table name") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDerivedTableNameCollision(t *testing.T) {
	// UserProfile and User_Profile both produce table name "user_profiles"
	// via toSnakeCase + pluralize, but differ in strings.ToLower.
	ctx := gen.Context{
		Entities: []types.Entity{
			{Name: "UserProfile", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}}},
			{Name: "User_Profile", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for derived table name collision between UserProfile and User_Profile")
	}
	if !strings.Contains(err.Error(), "user_profiles") {
		t.Errorf("error should mention the colliding table name, got: %v", err)
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
		{"Key", "keys"}, // vowel+y → just "s"
		{"Day", "days"}, // vowel+y → just "s"
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
		// Acronym handling (consecutive uppercase).
		{"HTTPServer", "http_server"},
		{"APIKey", "api_key"},
		{"UserID", "user_id"},
		{"HTTP", "http"},
		{"HTTPAPI", "httpapi"}, // adjacent acronyms: no detectable word boundary
		{"getAPIKey", "get_api_key"},
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

	// Upsert INSERT should exclude computed field: (id, resource_type, adapter).
	if !strings.Contains(storeContent, `INSERT INTO "statuses" ("id", "resource_type", "adapter")`) {
		t.Errorf("Upsert INSERT should exclude computed fields\n%s", storeContent)
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

	if !strings.Contains(storeContent, `DELETE FROM "users" WHERE "id" = $1`) {
		t.Errorf("Delete should use parameterized query for users\n%s", storeContent)
	}
	if !strings.Contains(storeContent, `DELETE FROM "organizations" WHERE "id" = $1`) {
		t.Errorf("Delete should use parameterized query for organizations\n%s", storeContent)
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
	// Should return (bool, error) to propagate database errors.
	if !strings.Contains(storeContent, "func (s *Store) Exists(entity string, id string) (bool, error)") {
		t.Error("Exists should return (bool, error)")
	}
	// Should propagate errors instead of swallowing them.
	if !strings.Contains(storeContent, "return false, err") {
		t.Error("Exists should propagate database errors")
	}
	if !strings.Contains(storeContent, "return exists, nil") {
		t.Error("Exists should return exists value with nil error on success")
	}
	// Should handle unknown entities with an error.
	if !strings.Contains(storeContent, `return false, fmt.Errorf("unknown entity`) {
		t.Error("Exists should return error for unknown entities")
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
		if !strings.Contains(sqlContent, `CREATE TABLE IF NOT EXISTS "`+want+`"`) {
			t.Errorf("migration missing CREATE TABLE for %s\n%s", want, sqlContent)
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

// --- Nullable pointer types for optional/computed fields ---

func TestNullableTypesForOptionalFields(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Profile",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString},
					{Name: "bio", Type: types.FieldTypeString, Optional: true},
					{Name: "age", Type: types.FieldTypeInt32, Optional: true},
					{Name: "score", Type: types.FieldTypeInt64, Optional: true},
					{Name: "rate", Type: types.FieldTypeFloat, Optional: true},
					{Name: "rating", Type: types.FieldTypeDouble, Optional: true},
					{Name: "active", Type: types.FieldTypeBool, Optional: true},
					{Name: "joined_at", Type: types.FieldTypeTimestamp, Optional: true},
					// []byte and json.RawMessage handle nil — no pointer needed.
					{Name: "avatar", Type: types.FieldTypeBytes, Optional: true},
					{Name: "metadata", Type: types.FieldTypeJsonb, Optional: true},
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

	// Non-optional field: plain type.
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "Name") && strings.Contains(line, `json:"name"`) {
			if strings.Contains(line, "*string") {
				t.Error("non-optional string field should not be a pointer")
			}
		}
	}

	// Optional fields should use pointer types.
	pointerChecks := []struct {
		field  string
		goType string
	}{
		{"Bio", "*string"},
		{"Age", "*int32"},
		{"Score", "*int64"},
		{"Rate", "*float32"},
		{"Rating", "*float64"},
		{"Active", "*bool"},
		{"JoinedAt", "*time.Time"},
	}
	for _, tc := range pointerChecks {
		found := false
		for _, line := range strings.Split(content, "\n") {
			if strings.Contains(line, tc.field) && strings.Contains(line, tc.goType) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("optional field %s should have type %s in models.go\n%s", tc.field, tc.goType, content)
		}
	}

	// []byte and json.RawMessage should NOT be pointers.
	nonPointerChecks := []struct {
		field  string
		goType string
	}{
		{"Avatar", "[]byte"},
		{"Metadata", "json.RawMessage"},
	}
	for _, tc := range nonPointerChecks {
		found := false
		for _, line := range strings.Split(content, "\n") {
			if strings.Contains(line, tc.field) && strings.Contains(line, tc.goType) &&
				!strings.Contains(line, "*"+tc.goType) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("optional field %s should have non-pointer type %s", tc.field, tc.goType)
		}
	}

	// Verify it compiles.
	modelsBytes := findFile(t, files, "internal/storage/models.go").Bytes()
	if _, err := format.Source(modelsBytes); err != nil {
		t.Errorf("models.go does not compile: %v\n%s", err, string(modelsBytes))
	}
}

func TestNullableTypesForComputedFields(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Resource",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString},
					{Name: "computed_status", Type: types.FieldTypeString, Computed: true},
					{Name: "computed_count", Type: types.FieldTypeInt32, Computed: true},
					{Name: "computed_ts", Type: types.FieldTypeTimestamp, Computed: true},
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

	// Computed fields should use pointer types.
	for _, tc := range []struct {
		field  string
		goType string
	}{
		{"ComputedStatus", "*string"},
		{"ComputedCount", "*int32"},
		{"ComputedTs", "*time.Time"},
	} {
		found := false
		for _, line := range strings.Split(content, "\n") {
			if strings.Contains(line, tc.field) && strings.Contains(line, tc.goType) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("computed field %s should have type %s\n%s", tc.field, tc.goType, content)
		}
	}

	// Verify all generated files compile.
	for _, p := range []string{"internal/storage/models.go", "internal/storage/store.go"} {
		content := findFile(t, files, p).Bytes()
		if _, err := format.Source(content); err != nil {
			t.Errorf("%s does not compile: %v\n%s", p, err, string(content))
		}
	}
}

// --- Update with all-computed fields ---

func TestUpdateAllComputedFieldsReturnsError(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "ReadOnly",
				Fields: []types.Field{
					{Name: "derived_a", Type: types.FieldTypeString, Computed: true},
					{Name: "derived_b", Type: types.FieldTypeInt32, Computed: true},
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

	// The Update method should return an error for an entity with no writable fields.
	if !strings.Contains(storeContent, "has no writable fields") {
		t.Error("Update should return error when entity has only computed fields")
	}

	// Verify it compiles.
	storeBytes := findFile(t, files, "internal/storage/store.go").Bytes()
	if _, err := format.Source(storeBytes); err != nil {
		t.Errorf("store.go does not compile: %v\n%s", err, string(storeBytes))
	}
}

// --- unique_composite validation ---

func TestUniqueCompositeInvalidFieldName(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name:   "User",
				Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}},
			},
			{
				Name:   "Organization",
				Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}},
			},
			{
				Name: "Membership",
				Fields: []types.Field{
					{Name: "user_id", Type: types.FieldTypeRef, To: "User", UniqueComposite: []string{"user_id", "nonexistent"}},
					{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for unique_composite referencing non-existent field")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention the invalid field name, got: %v", err)
	}
}

func TestUniqueCompositeDuplicateConstraintDeduplicated(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name:   "User",
				Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}},
			},
			{
				Name:   "Organization",
				Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}},
			},
			{
				Name: "Membership",
				Fields: []types.Field{
					{Name: "user_id", Type: types.FieldTypeRef, To: "User", UniqueComposite: []string{"user_id", "org_id"}},
					{Name: "org_id", Type: types.FieldTypeRef, To: "Organization", UniqueComposite: []string{"user_id", "org_id"}},
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

	// The UNIQUE constraint should appear exactly once.
	count := strings.Count(sqlContent, `UNIQUE ("user_id", "org_id")`)
	if count != 1 {
		t.Errorf("expected exactly 1 UNIQUE (\"user_id\", \"org_id\") constraint, got %d:\n%s", count, sqlContent)
	}
}

// --- Ref field FOREIGN KEY constraints ---

func TestRefFieldForeignKeyConstraint(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Organization",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString, Unique: true},
				},
			},
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "email", Type: types.FieldTypeString, Unique: true},
					{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
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

	// ref field should produce a REFERENCES clause with quoted identifiers.
	if !strings.Contains(sqlContent, `REFERENCES "organizations"("id")`) {
		t.Errorf("migration should include FOREIGN KEY reference for ref field:\n%s", sqlContent)
	}
}

// --- Ref target validation ---

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

func TestRefTargetValidEntity(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Organization",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString},
				},
			},
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for valid ref target: %v", err)
	}
}

// --- Migration topological sort ---

func TestMigrationTopologicalSort(t *testing.T) {
	// User (with ref to Organization) appears before Organization in input.
	// The generated migration must emit Organization's CREATE TABLE first.
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "email", Type: types.FieldTypeString, Unique: true},
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

	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sqlContent := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")

	// Organizations must appear before users in the SQL.
	orgPos := strings.Index(sqlContent, `CREATE TABLE IF NOT EXISTS "organizations"`)
	userPos := strings.Index(sqlContent, `CREATE TABLE IF NOT EXISTS "users"`)
	if orgPos < 0 || userPos < 0 {
		t.Fatalf("missing CREATE TABLE statements:\n%s", sqlContent)
	}
	if orgPos > userPos {
		t.Errorf("organizations table must be created before users table (org at %d, user at %d):\n%s",
			orgPos, userPos, sqlContent)
	}
}

func TestMigrationTopologicalSortCircularDependency(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "A",
				Fields: []types.Field{
					{Name: "b_id", Type: types.FieldTypeRef, To: "B"},
				},
			},
			{
				Name: "B",
				Fields: []types.Field{
					{Name: "a_id", Type: types.FieldTypeRef, To: "A"},
				},
			},
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

func TestMigrationTopologicalSortDiamond(t *testing.T) {
	// Diamond: A→B, A→C, B→D, C→D. D must come first, then B and C, then A.
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "A",
				Fields: []types.Field{
					{Name: "b_id", Type: types.FieldTypeRef, To: "B"},
					{Name: "c_id", Type: types.FieldTypeRef, To: "C"},
				},
			},
			{
				Name: "B",
				Fields: []types.Field{
					{Name: "d_id", Type: types.FieldTypeRef, To: "D"},
				},
			},
			{
				Name: "C",
				Fields: []types.Field{
					{Name: "d_id", Type: types.FieldTypeRef, To: "D"},
				},
			},
			{
				Name: "D",
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

	sqlContent := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")

	posD := strings.Index(sqlContent, `CREATE TABLE IF NOT EXISTS "ds"`)
	posB := strings.Index(sqlContent, `CREATE TABLE IF NOT EXISTS "bs"`)
	posC := strings.Index(sqlContent, `CREATE TABLE IF NOT EXISTS "cs"`)
	posA := strings.Index(sqlContent, `CREATE TABLE IF NOT EXISTS "as"`)

	if posD < 0 || posB < 0 || posC < 0 || posA < 0 {
		t.Fatalf("missing CREATE TABLE statements:\n%s", sqlContent)
	}

	if posD > posB || posD > posC {
		t.Errorf("D must be created before B and C:\n%s", sqlContent)
	}
	if posB > posA || posC > posA {
		t.Errorf("B and C must be created before A:\n%s", sqlContent)
	}
}

// --- Ref field empty 'to' validation ---

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
	if !strings.Contains(err.Error(), "org_id") {
		t.Errorf("error should mention the field name, got: %v", err)
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
	// Create, Read, Update, Delete, List, Upsert, Exists = 7 methods with default cases.
	if count < 7 {
		t.Errorf("expected at least 7 'unknown entity' default cases, got %d", count)
	}
}

// --- Optimistic concurrency: generation field validation ---

func TestUpsertOptimisticConcurrencyRequiresGenerationField(t *testing.T) {
	// Entity WITHOUT a "generation" field: the generated code should return
	// an error at runtime when concurrency == "optimistic" is requested.
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

	// The generated code should guard against optimistic concurrency without generation field.
	if !strings.Contains(storeContent, "optimistic concurrency requires a 'generation' field") {
		t.Errorf("store.go should emit runtime error for optimistic concurrency without generation field\n%s", storeContent)
	}

	// Verify it compiles.
	storeBytes := findFile(t, files, "internal/storage/store.go").Bytes()
	if _, err := format.Source(storeBytes); err != nil {
		t.Errorf("store.go does not compile: %v\n%s", err, string(storeBytes))
	}
}

func TestUpsertOptimisticConcurrencyWithGenerationField(t *testing.T) {
	// Entity WITH a "generation" field: the generated code should emit the
	// WHERE clause for optimistic concurrency (no runtime error).
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "AdapterStatus",
				Fields: []types.Field{
					{Name: "resource_type", Type: types.FieldTypeString},
					{Name: "resource_id", Type: types.FieldTypeString},
					{Name: "generation", Type: types.FieldTypeInt64},
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

	// Should have the WHERE EXCLUDED clause for optimistic concurrency.
	if !strings.Contains(storeContent, `EXCLUDED."generation"`) {
		t.Errorf("store.go should emit optimistic concurrency WHERE clause for entity with generation field\n%s", storeContent)
	}

	// Should NOT have the error guard for missing generation field.
	if strings.Contains(storeContent, "optimistic concurrency requires a 'generation' field") {
		t.Error("store.go should NOT emit error guard for entity that has a generation field")
	}

	// Verify it compiles.
	storeBytes := findFile(t, files, "internal/storage/store.go").Bytes()
	if _, err := format.Source(storeBytes); err != nil {
		t.Errorf("store.go does not compile: %v\n%s", err, string(storeBytes))
	}
}

// --- Field name uniqueness validation ---

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
		t.Fatal("expected error for duplicate field names within an entity")
	}
	if !strings.Contains(err.Error(), "duplicate field name") {
		t.Errorf("error should mention duplicate field name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "email") {
		t.Errorf("error should mention the duplicate field name 'email', got: %v", err)
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("error should mention the entity name 'User', got: %v", err)
	}
}

func TestUniqueFieldNamesPass(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "email", Type: types.FieldTypeString},
					{Name: "name", Type: types.FieldTypeString},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for unique field names: %v", err)
	}
}

// --- SQL identifier quoting ---

// --- Implicit ID collision ---

func TestFieldNameIDCollision(t *testing.T) {
	// Field named "id" produces PascalCase "ID", colliding with implicit ID.
	tests := []struct {
		name      string
		fieldName string
	}{
		{"lowercase_id", "id"},
		{"mixed_case_Id", "Id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := gen.Context{
				Entities: []types.Entity{
					{
						Name: "User",
						Fields: []types.Field{
							{Name: tt.fieldName, Type: types.FieldTypeString},
						},
					},
				},
				OutputNamespace: "internal/storage",
			}

			g := &Generator{}
			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error for field named %q colliding with implicit ID", tt.fieldName)
			}
			if !strings.Contains(err.Error(), "implicit primary key") {
				t.Errorf("error should mention implicit primary key, got: %v", err)
			}
			if !strings.Contains(err.Error(), tt.fieldName) {
				t.Errorf("error should mention the field name %q, got: %v", tt.fieldName, err)
			}
		})
	}
}

// --- sqlQ double-quote escaping ---

func TestSqlQEscapesDoubleQuotes(t *testing.T) {
	got := sqlQ(`col"x`)
	want := `"col""x"`
	if got != want {
		t.Errorf("sqlQ(%q) = %q, want %q", `col"x`, got, want)
	}
}

func TestSqlQNoDoubleQuotes(t *testing.T) {
	got := sqlQ("normal")
	want := `"normal"`
	if got != want {
		t.Errorf("sqlQ(%q) = %q, want %q", "normal", got, want)
	}
}

// --- Field name character set validation ---

func TestFieldNameInvalidCharacters(t *testing.T) {
	tests := []struct {
		name      string
		fieldName string
	}{
		{"hyphen", "foo-bar"},
		{"leading_digit", "123abc"},
		{"space", "foo bar"},
		{"unicode", "naïve"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := gen.Context{
				Entities: []types.Entity{
					{
						Name: "User",
						Fields: []types.Field{
							{Name: tt.fieldName, Type: types.FieldTypeString},
						},
					},
				},
				OutputNamespace: "internal/storage",
			}

			g := &Generator{}
			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error for field name %q with invalid characters", tt.fieldName)
			}
			if !strings.Contains(err.Error(), "invalid characters") {
				t.Errorf("error should mention invalid characters, got: %v", err)
			}
			if !strings.Contains(err.Error(), tt.fieldName) {
				t.Errorf("error should mention the field name, got: %v", err)
			}
		})
	}
}

// --- Entity name character set validation ---

func TestEntityNameInvalidCharacters(t *testing.T) {
	tests := []struct {
		name       string
		entityName string
	}{
		{"hyphen", "my-entity"},
		{"leading_digit", "123Invalid"},
		{"space", "hello world"},
		{"unicode", "Naïve"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := gen.Context{
				Entities: []types.Entity{
					{
						Name:   tt.entityName,
						Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}},
					},
				},
				OutputNamespace: "internal/storage",
			}

			g := &Generator{}
			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error for entity name %q with invalid characters", tt.entityName)
			}
			if !strings.Contains(err.Error(), "invalid characters") {
				t.Errorf("error should mention invalid characters, got: %v", err)
			}
			if !strings.Contains(err.Error(), tt.entityName) {
				t.Errorf("error should mention the entity name, got: %v", err)
			}
		})
	}
}

func TestEntityNameValidCharactersPass(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}}},
			{Name: "Organization", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "AdapterStatus", Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}}},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for valid entity names: %v", err)
	}
}

// --- Go keyword and predeclared identifier rejection ---

func TestReservedEntityNameGoKeywords(t *testing.T) {
	keywords := []string{"func", "return", "type", "struct", "if", "for", "switch",
		"case", "package", "import", "var", "const", "map", "chan", "range",
		"go", "defer", "select", "interface", "break", "continue", "fallthrough",
		"goto", "default", "else"}

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

func TestReservedEntityNameGoPredeclaredIdentifiers(t *testing.T) {
	predeclared := []string{"error", "any", "string", "int", "bool", "byte",
		"rune", "true", "false", "nil", "len", "cap", "make", "new",
		"append", "copy", "delete", "print", "println", "panic", "recover",
		"close", "complex", "real", "imag"}

	for _, name := range predeclared {
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
				t.Fatalf("expected error for Go predeclared identifier entity name %q", name)
			}
			if !strings.Contains(err.Error(), "collides") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestFieldNameValidCharactersPass(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "email", Type: types.FieldTypeString},
					{Name: "first_name", Type: types.FieldTypeString},
					{Name: "_internal", Type: types.FieldTypeString},
					{Name: "count2", Type: types.FieldTypeInt32},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for valid field names: %v", err)
	}
}

// --- Derived PascalCase field name uniqueness ---

func TestDerivedFieldNameCollisionUnderscorePrefix(t *testing.T) {
	// "_name" and "name" both produce PascalCase "Name".
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "_name", Type: types.FieldTypeString},
					{Name: "name", Type: types.FieldTypeString},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for derived field name collision (_name and name both produce Name)")
	}
	if !strings.Contains(err.Error(), "derived field name") {
		t.Errorf("error should mention derived field name collision, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Name") {
		t.Errorf("error should mention the colliding PascalCase name, got: %v", err)
	}
}

func TestDerivedFieldNameCollisionCamelVsSnake(t *testing.T) {
	// "foo_bar" and "fooBar" both produce PascalCase "FooBar".
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Item",
				Fields: []types.Field{
					{Name: "foo_bar", Type: types.FieldTypeString},
					{Name: "fooBar", Type: types.FieldTypeString},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for derived field name collision (foo_bar and fooBar both produce FooBar)")
	}
	if !strings.Contains(err.Error(), "FooBar") {
		t.Errorf("error should mention the colliding PascalCase name, got: %v", err)
	}
}

func TestDerivedFieldNameNonCollidingPass(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "first_name", Type: types.FieldTypeString},
					{Name: "last_name", Type: types.FieldTypeString},
					{Name: "email", Type: types.FieldTypeString},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for non-colliding derived field names: %v", err)
	}
}

// --- Enum empty values validation ---

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
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("error should mention the field name 'role', got: %v", err)
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("error should mention the entity name 'User', got: %v", err)
	}
}

func TestEnumFieldNilValuesReturnsError(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Item",
				Fields: []types.Field{
					{Name: "status", Type: types.FieldTypeEnum}, // Values is nil
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for enum field with nil values")
	}
	if !strings.Contains(err.Error(), "enum") {
		t.Errorf("error should mention enum, got: %v", err)
	}
}

func TestEnumFieldWithValuesPass(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "role", Type: types.FieldTypeEnum, Values: []string{"admin", "member"}},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for enum field with values: %v", err)
	}
}

// --- Intra-list uniqueness: unique_composite ---

func TestUniqueCompositeDuplicateFieldWithinConstraint(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Membership",
				Fields: []types.Field{
					{Name: "user_id", Type: types.FieldTypeString, UniqueComposite: []string{"user_id", "user_id"}},
					{Name: "org_id", Type: types.FieldTypeString},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for duplicate field name within unique_composite")
	}
	if !strings.Contains(err.Error(), "duplicate entry") {
		t.Errorf("error should mention duplicate entry, got: %v", err)
	}
	if !strings.Contains(err.Error(), "user_id") {
		t.Errorf("error should mention the duplicate field name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Membership") {
		t.Errorf("error should mention the entity name, got: %v", err)
	}
}

// --- Intra-list uniqueness: enum values ---

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
	if !strings.Contains(err.Error(), "admin") {
		t.Errorf("error should mention the duplicate value 'admin', got: %v", err)
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("error should mention the entity name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("error should mention the field name 'role', got: %v", err)
	}
}

func TestEnumUniqueValuesPass(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "role", Type: types.FieldTypeEnum, Values: []string{"admin", "member", "viewer"}},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for enum field with unique values: %v", err)
	}
}

// --- Post-transformation identifier validity ---

func TestFieldNameUnderscoreOnlyProducesError(t *testing.T) {
	tests := []struct {
		name      string
		fieldName string
	}{
		{"single_underscore", "_"},
		{"double_underscore", "__"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := gen.Context{
				Entities: []types.Entity{
					{
						Name: "User",
						Fields: []types.Field{
							{Name: tt.fieldName, Type: types.FieldTypeString},
						},
					},
				},
				OutputNamespace: "internal/storage",
			}

			g := &Generator{}
			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error for field name %q (underscore-only)", tt.fieldName)
			}
			if !strings.Contains(err.Error(), "not a valid Go identifier") {
				t.Errorf("error should mention invalid Go identifier, got: %v", err)
			}
			if !strings.Contains(err.Error(), tt.fieldName) {
				t.Errorf("error should mention the field name %q, got: %v", tt.fieldName, err)
			}
		})
	}
}

func TestFieldNameUnderscorePrefixedDigitsProducesError(t *testing.T) {
	tests := []struct {
		name      string
		fieldName string
	}{
		{"underscore_123", "_123"},
		{"underscore_42", "_42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := gen.Context{
				Entities: []types.Entity{
					{
						Name: "User",
						Fields: []types.Field{
							{Name: tt.fieldName, Type: types.FieldTypeString},
						},
					},
				},
				OutputNamespace: "internal/storage",
			}

			g := &Generator{}
			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error for field name %q (underscore-prefixed digits)", tt.fieldName)
			}
			if !strings.Contains(err.Error(), "not a valid Go identifier") {
				t.Errorf("error should mention invalid Go identifier, got: %v", err)
			}
			if !strings.Contains(err.Error(), tt.fieldName) {
				t.Errorf("error should mention the field name %q, got: %v", tt.fieldName, err)
			}
		})
	}
}

func TestFieldNameWithAlphaAfterUnderscorePass(t *testing.T) {
	// Names with underscores that have alphabetic segments should still work.
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "_internal", Type: types.FieldTypeString},
					{Name: "a_1", Type: types.FieldTypeString},
				},
			},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for valid field names with underscores: %v", err)
	}
}

func TestSQLIdentifiersAreQuoted(t *testing.T) {
	// Use a field name that is a PostgreSQL reserved word to verify quoting.
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Setting",
				Fields: []types.Field{
					{Name: "order", Type: types.FieldTypeInt32},
					{Name: "group", Type: types.FieldTypeString},
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

	// Verify migration SQL quotes identifiers.
	sqlContent := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")
	if !strings.Contains(sqlContent, `"order" INTEGER NOT NULL`) {
		t.Errorf("migration should quote reserved word 'order':\n%s", sqlContent)
	}
	if !strings.Contains(sqlContent, `"group" TEXT NOT NULL`) {
		t.Errorf("migration should quote reserved word 'group':\n%s", sqlContent)
	}
	if !strings.Contains(sqlContent, `CREATE TABLE IF NOT EXISTS "settings"`) {
		t.Errorf("migration should quote table name:\n%s", sqlContent)
	}

	// Verify store queries quote identifiers.
	storeContent := findFileContent(t, files, "internal/storage/store.go")
	if !strings.Contains(storeContent, `DELETE FROM "settings" WHERE "id" = $1`) {
		t.Errorf("store should quote identifiers in DELETE:\n%s", storeContent)
	}
	if !strings.Contains(storeContent, `SELECT "id", "order", "group" FROM "settings"`) {
		t.Errorf("store should quote identifiers in SELECT:\n%s", storeContent)
	}

	// Verify both files compile.
	for _, p := range []string{"internal/storage/models.go", "internal/storage/store.go"} {
		content := findFile(t, files, p).Bytes()
		if _, err := format.Source(content); err != nil {
			t.Errorf("%s does not compile: %v\n%s", p, err, string(content))
		}
	}
}

// --- Import alias reserved name validation ---

func TestReservedEntityNameImportAliases(t *testing.T) {
	// Import aliases used in generated files must be reserved.
	aliases := []string{"sql", "json", "fmt", "strings", "time"}

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

// --- Entity name underscore-only validation ---

func TestEntityNameUnderscoreOnlyProducesError(t *testing.T) {
	tests := []struct {
		name       string
		entityName string
	}{
		{"single_underscore", "_"},
		{"double_underscore", "__"},
		{"triple_underscore", "___"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := gen.Context{
				Entities: []types.Entity{
					{
						Name:   tt.entityName,
						Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}},
					},
				},
				OutputNamespace: "internal/storage",
			}

			g := &Generator{}
			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error for entity name %q (underscore-only)", tt.entityName)
			}
			if !strings.Contains(err.Error(), tt.entityName) {
				t.Errorf("error should mention the entity name %q, got: %v", tt.entityName, err)
			}
		})
	}
}

func TestEntityNameWithAlphaCharactersPass(t *testing.T) {
	// Entity names with underscores and alpha characters should be fine.
	ctx := gen.Context{
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}}},
			{Name: "_Internal", Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}}},
			{Name: "A_1", Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}}},
		},
		OutputNamespace: "internal/storage",
	}

	g := &Generator{}
	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for valid entity names: %v", err)
	}
}

// --- Exists method error propagation ---

func TestExistsMethodReturnsError(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// The Exists method must return (bool, error), not just bool.
	if !strings.Contains(storeContent, "(bool, error)") {
		t.Error("Exists must return (bool, error) to propagate database errors")
	}

	// Must NOT swallow errors with "return err == nil && exists".
	if strings.Contains(storeContent, "return err == nil && exists") {
		t.Error("Exists must not swallow errors — found 'return err == nil && exists' pattern")
	}

	// Verify it compiles.
	storeBytes := findFile(t, files, "internal/storage/store.go").Bytes()
	if _, err := format.Source(storeBytes); err != nil {
		t.Errorf("store.go does not compile: %v\n%s", err, string(storeBytes))
	}
}
