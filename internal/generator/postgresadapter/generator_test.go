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

	// Should produce exactly 5 files.
	if len(files) != 5 {
		t.Fatalf("expected 5 files, got %d: %v", len(files), fileNames(files))
	}

	// Verify file paths.
	findFile(t, files, "internal/storage/models.go")
	findFile(t, files, "internal/storage/store.go")
	findFile(t, files, "internal/storage/migrate.go")
	findFile(t, files, "internal/storage/session_factory.go")
	findFile(t, files, "internal/storage/generic_dao.go")

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
	if wiring.DBBackend != "gorm" {
		t.Errorf("expected DBBackend 'gorm', got %q", wiring.DBBackend)
	}
	if !wiring.NeedsDB {
		t.Error("expected NeedsDB to be true")
	}
	if wiring.GoModRequires == nil {
		t.Fatal("expected non-nil GoModRequires")
	}
	for _, mod := range []string{"gorm.io/gorm", "gorm.io/driver/postgres", "gorm.io/datatypes"} {
		if _, ok := wiring.GoModRequires[mod]; !ok {
			t.Errorf("GoModRequires missing %q", mod)
		}
	}

	// Verify PostDBCalls includes migration call.
	if len(wiring.PostDBCalls) != 1 || wiring.PostDBCalls[0] != "storage.Migrate(db)" {
		t.Errorf("expected PostDBCalls=[\"storage.Migrate(db)\"], got %v", wiring.PostDBCalls)
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

// --- Model struct content: Meta embed and GORM tags ---

func TestModelStructHasMetaEmbed(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/models.go")

	// Meta struct should exist.
	if !strings.Contains(content, "type Meta struct") {
		t.Error("models.go missing Meta struct")
	}
	if !strings.Contains(content, "gorm.DeletedAt") {
		t.Error("models.go Meta should contain gorm.DeletedAt for soft delete")
	}
	if !strings.Contains(content, `gorm:"index"`) {
		t.Error("models.go Meta DeletedAt should have gorm:\"index\" tag")
	}

	// Entity structs should embed Meta.
	for _, entity := range []string{"User", "Organization"} {
		if !strings.Contains(content, "type "+entity+" struct") {
			t.Errorf("models.go missing %s struct", entity)
		}
	}

	// Check that Meta is embedded (appears inside struct body).
	if strings.Count(content, "\tMeta\n") < 2 {
		t.Error("models.go entities should embed Meta")
	}
}

func TestModelGORMTags(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/models.go")

	// Unique field should have uniqueIndex GORM tag.
	if !strings.Contains(content, "uniqueIndex") {
		t.Error("models.go should have uniqueIndex GORM tag for unique fields")
	}

	// Non-optional fields should have "not null" GORM tag.
	if !strings.Contains(content, "not null") {
		t.Error("models.go should have 'not null' GORM tag for non-optional fields")
	}
}

func TestMetaTimestampAutoPopulateTags(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/models.go")

	// CreatedTime should have autoCreateTime GORM tag so GORM populates it.
	if !strings.Contains(content, `gorm:"autoCreateTime"`) {
		t.Error("Meta CreatedTime should have gorm:\"autoCreateTime\" tag")
	}

	// UpdatedTime should have autoUpdateTime GORM tag so GORM populates it.
	if !strings.Contains(content, `gorm:"autoUpdateTime"`) {
		t.Error("Meta UpdatedTime should have gorm:\"autoUpdateTime\" tag")
	}
}

func TestMetaBeforeCreateHookGeneratesUUID(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/models.go")

	// models.go should import github.com/google/uuid.
	if !strings.Contains(content, `"github.com/google/uuid"`) {
		t.Error("models.go should import github.com/google/uuid")
	}

	// Meta should have a BeforeCreate GORM hook.
	if !strings.Contains(content, "func (m *Meta) BeforeCreate(tx *gorm.DB) error") {
		t.Error("models.go should have BeforeCreate hook on Meta")
	}

	// The hook should assign uuid.New().String() when ID is empty.
	if !strings.Contains(content, "uuid.New().String()") {
		t.Error("BeforeCreate hook should use uuid.New().String() for ID generation")
	}

	// The hook should only assign when ID is empty (not overwrite provided IDs).
	if !strings.Contains(content, `m.ID == ""`) {
		t.Error("BeforeCreate hook should check m.ID == \"\" before assigning")
	}

	// Verify it compiles.
	modelsBytes := findFile(t, files, "internal/storage/models.go").Bytes()
	if _, err := format.Source(modelsBytes); err != nil {
		t.Errorf("models.go does not compile: %v\n%s", err, string(modelsBytes))
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

func TestModelUniqueCompositeGeneratesGormTag(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Binding",
				Fields: []types.Field{
					{Name: "resource_type", Type: types.FieldTypeString, UniqueComposite: []string{"resource_type", "resource_id"}},
					{Name: "resource_id", Type: types.FieldTypeString, UniqueComposite: []string{"resource_type", "resource_id"}},
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

	content := findFileContent(t, files, "internal/storage/models.go")

	// Both fields in the composite should reference the same named unique index.
	expectedTag := "uniqueIndex:composite_resource_type_resource_id"
	count := strings.Count(content, expectedTag)
	if count != 2 {
		t.Errorf("expected %q to appear 2 times (once per field), got %d\n%s",
			expectedTag, count, content)
	}

	// A field NOT in the composite should NOT have the composite tag.
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "Label") && strings.Contains(line, "uniqueIndex:composite") {
			t.Error("field 'label' should not have composite uniqueIndex tag")
		}
	}

	// Verify it compiles.
	modelsBytes := findFile(t, files, "internal/storage/models.go").Bytes()
	if _, err := format.Source(modelsBytes); err != nil {
		t.Errorf("models.go does not compile: %v", err)
	}
}

func TestModelMinLengthGeneratesCheckTag(t *testing.T) {
	minLen := 3
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString, MinLength: &minLen},
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

	// min_length should produce a GORM check constraint tag.
	expectedTag := "check:length(name) >= 3"
	if !strings.Contains(content, expectedTag) {
		t.Errorf("models.go should have gorm tag %q for min_length fields\n%s",
			expectedTag, content)
	}

	// Verify it compiles.
	modelsBytes := findFile(t, files, "internal/storage/models.go").Bytes()
	if _, err := format.Source(modelsBytes); err != nil {
		t.Errorf("models.go does not compile: %v", err)
	}
}

func TestModelMinAndMaxLengthCombined(t *testing.T) {
	minLen := 3
	maxLen := 53
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString, MinLength: &minLen, MaxLength: &maxLen},
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

	// Both constraints should be present.
	if !strings.Contains(content, "size:53") {
		t.Error("models.go should have size:53 for max_length")
	}
	if !strings.Contains(content, "check:length(name) >= 3") {
		t.Error("models.go should have check constraint for min_length")
	}

	// Verify it compiles.
	modelsBytes := findFile(t, files, "internal/storage/models.go").Bytes()
	if _, err := format.Source(modelsBytes); err != nil {
		t.Errorf("models.go does not compile: %v", err)
	}
}

func TestModelRefFieldHasRelationship(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/models.go")

	// Ref field should generate a relationship field.
	if !strings.Contains(content, "OrgIDRef") {
		t.Error("models.go should generate relationship field OrgIDRef for ref field org_id")
	}
	if !strings.Contains(content, "*Organization") {
		t.Error("models.go relationship field should reference *Organization")
	}
	if !strings.Contains(content, `foreignKey:OrgID`) {
		t.Error("models.go relationship field should have foreignKey:OrgID GORM tag")
	}
	if !strings.Contains(content, `json:"-"`) {
		t.Error("models.go relationship field should have json:\"-\" to exclude from JSON")
	}
}

func TestModelJsonbFieldUsesDatatypesJSON(t *testing.T) {
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

	// jsonb fields should use datatypes.JSON.
	if !strings.Contains(content, "datatypes.JSON") {
		t.Error("models.go should use datatypes.JSON for jsonb fields")
	}
	if !strings.Contains(content, `type:jsonb`) {
		t.Error("models.go should have gorm:\"type:jsonb\" tag for jsonb fields")
	}
	if !strings.Contains(content, `"gorm.io/datatypes"`) {
		t.Error("models.go should import gorm.io/datatypes")
	}
}

func TestModelMaxLengthGeneratesSizeTag(t *testing.T) {
	maxLen := 255
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "email", Type: types.FieldTypeString, MaxLength: &maxLen},
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
	if !strings.Contains(content, "size:255") {
		t.Error("models.go should have gorm:\"size:255\" tag for max_length fields")
	}
}

// --- Store uses GORM ---

func TestStoreUsesGORM(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// Store should import GORM.
	if !strings.Contains(storeContent, `"gorm.io/gorm"`) {
		t.Error("store.go should import gorm.io/gorm")
	}

	// Store struct should have *gorm.DB field.
	if !strings.Contains(storeContent, "db *gorm.DB") {
		t.Error("store.go Store should have db *gorm.DB field")
	}

	// Constructor should accept *gorm.DB.
	if !strings.Contains(storeContent, "func NewStore(db *gorm.DB) *Store") {
		t.Error("store.go NewStore should accept *gorm.DB")
	}

	// Create should use s.db.WithContext(ctx).Create.
	if !strings.Contains(storeContent, "s.db.WithContext(ctx).Create(&v).Error") {
		t.Error("store.go Create should use s.db.WithContext(ctx).Create")
	}

	// Get should use s.db.First.
	if !strings.Contains(storeContent, `s.db.WithContext(ctx).First(&v, "id = ?", id).Error`) {
		t.Error("store.go Get should use s.db.WithContext(ctx).First")
	}

	// Replace should use selective column update (not Save) to preserve computed fields.
	if !strings.Contains(storeContent, ".Select(") || !strings.Contains(storeContent, ".Updates(&v).Error") {
		t.Error("store.go Replace should use selective column update via .Select(...).Updates, not Save")
	}

	// Delete should use s.db.Delete (soft delete).
	if !strings.Contains(storeContent, "s.db.Where") && !strings.Contains(storeContent, "Delete(&") {
		t.Error("store.go Delete should use GORM soft delete")
	}
}

func TestStoreHasAllMethods(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	methods := []string{
		"func (s *Store) Create(ctx context.Context,",
		"func (s *Store) Get(ctx context.Context,",
		"func (s *Store) Replace(ctx context.Context,",
		"func (s *Store) Delete(ctx context.Context,",
		"func (s *Store) List(ctx context.Context,",
		"func (s *Store) Upsert(ctx context.Context,",
		"func (s *Store) Exists(ctx context.Context,",
	}

	for _, m := range methods {
		if !strings.Contains(storeContent, m) {
			t.Errorf("store.go missing method %q", m)
		}
	}
}

// --- Upsert ---

func TestUpsertUsesGORMOnConflict(t *testing.T) {
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

	// Should import gorm/clause.
	if !strings.Contains(storeContent, `"gorm.io/gorm/clause"`) {
		t.Error("store.go should import gorm.io/gorm/clause for upsert")
	}

	// Should use clause.OnConflict.
	if !strings.Contains(storeContent, "clause.OnConflict") {
		t.Error("store.go upsert should use clause.OnConflict")
	}

	// Verify optimistic concurrency.
	if !strings.Contains(storeContent, `concurrency == "optimistic"`) {
		t.Error("store.go missing optimistic concurrency check")
	}

	if !strings.Contains(storeContent, `EXCLUDED."generation"`) {
		t.Error("store.go missing quoted generation column reference in optimistic concurrency")
	}

	// Verify the upsert uses a transaction to atomically check existence
	// and perform the INSERT...ON CONFLICT (no TOCTOU race).
	upsertIdx := strings.Index(storeContent, "func (s *Store) Upsert(")
	if upsertIdx < 0 {
		t.Fatal("Upsert method not found in store.go")
	}
	upsertBody := storeContent[upsertIdx:]

	if !strings.Contains(upsertBody, "s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {") {
		t.Error("Upsert should wrap COUNT + INSERT in a transaction to prevent TOCTOU race")
	}

	// Verify the COUNT query error is checked inside the transaction.
	if !strings.Contains(upsertBody, "tx.Model(&AdapterStatus{}).Where(whereClause).Count(&existingCount).Error") {
		t.Error("Upsert should check COUNT query error via .Error")
	}

	// Verify the INSERT uses the transaction handle (tx), not the store's db.
	if !strings.Contains(upsertBody, "tx.Clauses(onConflict).Create(&v)") {
		t.Error("Upsert should use the transaction handle (tx) for the INSERT, not s.db")
	}

	// Verify it compiles.
	storeBytes := findFile(t, files, "internal/storage/store.go").Bytes()
	if _, err := format.Source(storeBytes); err != nil {
		t.Errorf("store.go does not compile: %v", err)
	}
}

// --- Migration uses AutoMigrate ---

func TestMigrationUsesAutoMigrate(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	migrateContent := findFileContent(t, files, "internal/storage/migrate.go")

	// Should use AutoMigrate.
	if !strings.Contains(migrateContent, "AutoMigrate") {
		t.Error("migrate.go should use GORM AutoMigrate")
	}

	// Should have Register infrastructure.
	if !strings.Contains(migrateContent, "func Register(") {
		t.Error("migrate.go should have Register function")
	}

	// Should have Migrate function.
	if !strings.Contains(migrateContent, "func Migrate(") {
		t.Error("migrate.go should have Migrate function")
	}

	// Should register initial migration.
	if !strings.Contains(migrateContent, `"001_initial"`) {
		t.Error("migrate.go should register 001_initial migration")
	}

	// Should reference all entities.
	if !strings.Contains(migrateContent, "&User{}") {
		t.Error("migrate.go AutoMigrate should include User")
	}
	if !strings.Contains(migrateContent, "&Organization{}") {
		t.Error("migrate.go AutoMigrate should include Organization")
	}
}

func TestMigrationTopologicalSort(t *testing.T) {
	// User (with ref to Organization) appears before Organization in input.
	// The AutoMigrate call must list Organization before User.
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

	migrateContent := findFileContent(t, files, "internal/storage/migrate.go")

	orgPos := strings.Index(migrateContent, "&Organization{}")
	userPos := strings.Index(migrateContent, "&User{}")
	if orgPos < 0 || userPos < 0 {
		t.Fatalf("missing entity references in AutoMigrate:\n%s", migrateContent)
	}
	if orgPos > userPos {
		t.Errorf("Organization must appear before User in AutoMigrate (org at %d, user at %d)",
			orgPos, userPos)
	}
}

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

func TestSessionFactoryGenerated(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/session_factory.go")

	if !strings.Contains(content, "type SessionFactory interface") {
		t.Error("session_factory.go should define SessionFactory interface")
	}
	if !strings.Contains(content, "New() *gorm.DB") {
		t.Error("session_factory.go should have New() *gorm.DB method")
	}
	if !strings.Contains(content, "Close() error") {
		t.Error("session_factory.go should have Close() error method")
	}

	// Should compile.
	bytes := findFile(t, files, "internal/storage/session_factory.go").Bytes()
	if _, err := format.Source(bytes); err != nil {
		t.Errorf("session_factory.go does not compile: %v", err)
	}
}

// --- GenericDao ---

func TestGenericDaoGenerated(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/storage/generic_dao.go")

	if !strings.Contains(content, "type GenericDao struct") {
		t.Error("generic_dao.go should define GenericDao struct")
	}
	if !strings.Contains(content, "DB *gorm.DB") {
		t.Error("generic_dao.go should have DB *gorm.DB field")
	}
	if !strings.Contains(content, "func NewGenericDao(") {
		t.Error("generic_dao.go should have NewGenericDao constructor")
	}

	// Should compile.
	bytes := findFile(t, files, "internal/storage/generic_dao.go").Bytes()
	if _, err := format.Source(bytes); err != nil {
		t.Errorf("generic_dao.go does not compile: %v", err)
	}
}

// --- Soft delete ---

func TestDeleteIsSoftDelete(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// Delete should use GORM's Delete which does soft delete with DeletedAt.
	if !strings.Contains(storeContent, ".Delete(") {
		t.Error("store.go Delete should use GORM Delete for soft delete")
	}
	// Should NOT contain raw SQL DELETE.
	if strings.Contains(storeContent, `DELETE FROM`) {
		t.Error("store.go should not use raw SQL DELETE — should use GORM soft delete")
	}
}

// --- Computed fields ---

func TestComputedFieldsClearedOnCreate(t *testing.T) {
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

	// Computed fields should be zeroed before Create.
	if !strings.Contains(storeContent, "v.StatusConditions = nil") {
		t.Error("Create should zero computed fields before writing")
	}
}

func TestComputedFieldsNullableModel(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Cluster",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString},
					{Name: "status", Type: types.FieldTypeString, Computed: true},
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

	// Computed string field should be *string (nullable).
	found := false
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "Status") && strings.Contains(line, "*string") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("computed field Status should be *string (nullable)\n%s", content)
	}

	// Non-computed field should NOT be nullable.
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "Name") && strings.Contains(line, `json:"name"`) && strings.Contains(line, "*string") {
			t.Error("non-computed string field Name should not be a pointer")
		}
	}
}

func TestReplaceAllComputedFieldsReturnsError(t *testing.T) {
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
	if !strings.Contains(storeContent, "has no writable fields") {
		t.Error("Replace should return error when entity has only computed fields")
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
	findFile(t, files, "pkg/db/migrate.go")

	// Wiring should reference custom namespace.
	if wiring.Imports[0] != "pkg/db" {
		t.Errorf("expected import pkg/db, got %s", wiring.Imports[0])
	}
	if wiring.Constructors[0] != "db.NewStore(db)" {
		t.Errorf("expected constructor db.NewStore(db), got %s", wiring.Constructors[0])
	}

	modelsContent := findFileContent(t, files, "pkg/db/models.go")
	if !strings.Contains(modelsContent, "package db") {
		t.Error("models.go should use package name from namespace")
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

	// Verify all Go files compile.
	for _, p := range []string{
		"internal/storage/models.go",
		"internal/storage/store.go",
		"internal/storage/migrate.go",
		"internal/storage/session_factory.go",
		"internal/storage/generic_dao.go",
	} {
		content := findFile(t, files, p).Bytes()
		if _, err := format.Source(content); err != nil {
			t.Errorf("%s does not compile: %v", p, err)
		}
	}

	// Verify Go types in models.
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
		{"J", "datatypes.JSON"},
	}
	for _, tc := range typeChecks {
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
		{types.FieldTypeJsonb, "datatypes.JSON"},
	}

	for _, tt := range tests {
		t.Run(string(tt.ft), func(t *testing.T) {
			f := types.Field{Type: tt.ft}
			got := fieldTypeToGo(f)
			if got != tt.want {
				t.Errorf("fieldTypeToGo(%q) = %q, want %q", tt.ft, got, tt.want)
			}
		})
	}
}

// --- Nullable pointer types ---

func TestNullableTypesForOptionalFields(t *testing.T) {
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Profile",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString},
					{Name: "bio", Type: types.FieldTypeString, Optional: true},
					{Name: "age", Type: types.FieldTypeInt32, Optional: true},
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

	// Optional string: *string.
	found := false
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "Bio") && strings.Contains(line, "*string") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("optional field Bio should have type *string\n%s", content)
	}

	// Optional jsonb: datatypes.JSON (no pointer needed).
	found = false
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "Metadata") && strings.Contains(line, "datatypes.JSON") &&
			!strings.Contains(line, "*datatypes.JSON") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("optional jsonb field should use datatypes.JSON without pointer\n%s", content)
	}

	// Verify it compiles.
	modelsBytes := findFile(t, files, "internal/storage/models.go").Bytes()
	if _, err := format.Source(modelsBytes); err != nil {
		t.Errorf("models.go does not compile: %v", err)
	}
}

// --- Exists method ---

func TestExistsMethodUsesCount(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// Should use GORM Count, not raw SQL SELECT EXISTS.
	if !strings.Contains(storeContent, ".Count(&count)") {
		t.Error("Exists should use GORM Count")
	}
	if !strings.Contains(storeContent, "func (s *Store) Exists(ctx context.Context, entity string, id string) (bool, error)") {
		t.Error("Exists should return (bool, error)")
	}
	if !strings.Contains(storeContent, "return false, err") {
		t.Error("Exists should propagate database errors")
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

	if !strings.Contains(storeContent, "validCols") {
		t.Error("List should validate scope field against known columns")
	}
	if !strings.Contains(storeContent, "invalid scope field") {
		t.Error("List should return error for invalid scope field")
	}
	// Scope validation error should return empty ListResult alongside the error.
	if !strings.Contains(storeContent, "return ListResult{}, fmt.Errorf") {
		t.Error("List scope validation error should return (ListResult{}, error)")
	}
}

func TestListPagination(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	storeContent := findFileContent(t, files, "internal/storage/store.go")

	// List should accept ListOptions parameter.
	if !strings.Contains(storeContent, "opts ListOptions") {
		t.Error("List should accept opts ListOptions parameter")
	}

	// List should return (ListResult, error) to carry items and total count.
	if !strings.Contains(storeContent, "(ListResult, error)") {
		t.Error("List should return (ListResult, error)")
	}

	// List should perform COUNT(*) before fetching the page.
	if !strings.Contains(storeContent, "query.Count(&total)") {
		t.Error("List should perform COUNT query for total matching records")
	}

	// List should declare a total variable.
	if !strings.Contains(storeContent, "var total int64") {
		t.Error("List should declare total count variable")
	}

	// List should return ListResult with items and total.
	if !strings.Contains(storeContent, "return ListResult{Items: result, Total: total}, nil") {
		t.Error("List should return ListResult with Items and Total")
	}

	// List should apply Offset computed from Page and Size.
	if !strings.Contains(storeContent, "query.Offset(offset)") {
		t.Error("List should apply Offset for pagination")
	}

	// List should apply Limit from opts.Size.
	if !strings.Contains(storeContent, "query.Limit(opts.Size)") {
		t.Error("List should apply Limit for pagination")
	}

	// store.go should define ListOptions and ListResult types.
	if !strings.Contains(storeContent, "type ListOptions struct") {
		t.Error("store.go should define ListOptions type")
	}
	if !strings.Contains(storeContent, "type ListResult struct") {
		t.Error("store.go should define ListResult type")
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

	count := strings.Count(storeContent, `unknown entity`)
	if count < 7 {
		t.Errorf("expected at least 7 'unknown entity' default cases, got %d", count)
	}
}

// --- Optimistic concurrency ---

func TestUpsertOptimisticConcurrencyRequiresGenerationField(t *testing.T) {
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

	if !strings.Contains(storeContent, "optimistic concurrency requires a 'generation' field") {
		t.Error("store.go should emit runtime error for optimistic concurrency without generation field")
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
	for _, name := range []string{"Store", "NewStore", "Meta", "GenericDao", "SessionFactory", "Migrate"} {
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
	aliases := []string{"gorm", "clause", "datatypes", "json", "fmt", "time"}

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

func TestGoFileHeader(t *testing.T) {
	ctx := basicContext()
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Go files should have generated header.
	for _, p := range []string{
		"internal/storage/models.go",
		"internal/storage/store.go",
		"internal/storage/migrate.go",
	} {
		f := findFile(t, files, p)
		rendered := string(f.Bytes())
		if !strings.Contains(rendered, gen.Header) {
			t.Errorf("%s should have generated header", p)
		}
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

	// Verify all Go files compile.
	for _, p := range []string{
		"internal/storage/models.go",
		"internal/storage/store.go",
		"internal/storage/migrate.go",
	} {
		content := findFile(t, files, p).Bytes()
		if _, err := format.Source(content); err != nil {
			t.Errorf("%s does not compile: %v\n%s", p, err, string(content))
		}
	}
}

// --- Computed field handling in Upsert ---

func TestComputedFieldsClearedOnUpsert(t *testing.T) {
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

	// The Upsert method should zero computed fields before the GORM Create call,
	// matching the same behavior as the Create method.
	upsertIdx := strings.Index(storeContent, "func (s *Store) Upsert(")
	if upsertIdx < 0 {
		t.Fatal("Upsert method not found in store.go")
	}
	upsertBody := storeContent[upsertIdx:]

	if !strings.Contains(upsertBody, "v.StatusConditions = nil") {
		t.Error("Upsert should zero computed fields before writing (matching Create)")
	}
}

// --- Replace uses selective update ---

func TestReplaceUsesSelectiveUpdate(t *testing.T) {
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

	// Replace should NOT use Save (which overwrites all columns including computed).
	replaceIdx := strings.Index(storeContent, "func (s *Store) Replace(")
	if replaceIdx < 0 {
		t.Fatal("Replace method not found in store.go")
	}
	replaceBody := storeContent[replaceIdx:]

	if strings.Contains(replaceBody, ".Save(&v)") {
		t.Error("Replace must NOT use Save — Save overwrites computed fields with zero values")
	}
	if !strings.Contains(replaceBody, ".Select(") {
		t.Error("Replace should use .Select(writeCols) for selective column update")
	}
	if !strings.Contains(replaceBody, ".Updates(&v)") {
		t.Error("Replace should use .Updates for selective column update")
	}
}

func TestSearchIntegrationWhenTslSearchPeer(t *testing.T) {
	// When tsl-search is a peer component, the postgres-adapter should import
	// the search package and apply TSL search filtering in the List method.
	g := &Generator{}
	ctx := basicContext()
	ctx.ModuleName = "github.com/myorg/svc"
	ctx.OutDirName = "out"
	ctx.PeerNamespaces = map[string]string{
		"rest-api":   "internal/api",
		"tsl-search": "internal/search",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var storeContent string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "store.go") {
			storeContent = string(f.Bytes())
			break
		}
	}
	if storeContent == "" {
		t.Fatal("store.go not found in generated files")
	}

	// Must import the search package.
	if !strings.Contains(storeContent, `"github.com/myorg/svc/out/internal/search"`) {
		t.Error("store.go must import the search package when tsl-search is a peer")
	}

	// List method must handle search.
	if !strings.Contains(storeContent, "opts.Search") {
		t.Error("List method must check opts.Search when tsl-search is a peer")
	}

	// Must call ParseSearch.
	if !strings.Contains(storeContent, "ParseSearch") {
		t.Error("List method must call search.ParseSearch when tsl-search is a peer")
	}

	// Must apply WHERE clause from search result.
	if !strings.Contains(storeContent, "searchResult.Where") {
		t.Error("List method must apply search WHERE clause")
	}
}

func TestNoSearchImportWithoutTslSearchPeer(t *testing.T) {
	// When tsl-search is NOT a peer component, the search package should not
	// be imported and search filtering should not be generated.
	g := &Generator{}
	ctx := basicContext()
	ctx.ModuleName = "github.com/myorg/svc"
	ctx.OutDirName = "out"
	ctx.PeerNamespaces = map[string]string{
		"rest-api": "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var storeContent string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "store.go") {
			storeContent = string(f.Bytes())
			break
		}
	}
	if storeContent == "" {
		t.Fatal("store.go not found in generated files")
	}

	// Must NOT import the search package.
	if strings.Contains(storeContent, "internal/search") {
		t.Error("store.go must NOT import search package when tsl-search is not a peer")
	}

	// Must NOT have search handling.
	if strings.Contains(storeContent, "ParseSearch") {
		t.Error("List method must NOT call ParseSearch when tsl-search is not a peer")
	}
}
