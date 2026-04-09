// Package postgresadapter implements the postgres-adapter component Generator.
// It produces GORM-based model structs, a Store implementation with CRUD + list
// + upsert methods, migration code, SessionFactory, and GenericDao from the
// service declaration's entities.
package postgresadapter

import (
	"bytes"
	"fmt"
	"go/format"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// validFieldNamePattern defines the safe character set for field names across
// all target systems (Go identifiers, SQL identifiers, URL segments).
var validFieldNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Generator produces the postgres-adapter component's generated code.
type Generator struct{}

// Generate produces GORM-based model structs, a Store implementation, migration
// code, SessionFactory, and GenericDao for all entities in the service declaration.
// It returns wiring instructions for main.go assembly.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if len(ctx.Entities) == 0 {
		return nil, nil, nil
	}

	// Validate no duplicate entity names.
	if err := validateEntityUniqueness(ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Validate no case-insensitive entity name collisions (would produce
	// the same table name).
	if err := validateCaseInsensitiveUniqueness(ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Validate entity names don't collide with generator-internal identifiers.
	if err := validateReservedNames(ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Validate that ref fields' "to" attributes reference existing entities.
	if err := validateRefTargets(ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Validate no duplicate field names within any entity.
	if err := validateFieldUniqueness(ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Validate entity names use a safe character set for all target systems.
	if err := validateEntityNameCharset(ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Validate field names use a safe character set for all target systems.
	if err := validateFieldNameCharset(ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Validate no field name collides with the implicit "id" primary key.
	if err := validateNoImplicitIDCollision(ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Validate derived PascalCase field names are unique within each entity.
	if err := validateDerivedFieldUniqueness(ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Validate derived entity names are valid, usable Go type names.
	if err := validateDerivedEntityValidity(ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Validate derived PascalCase field names are valid Go identifiers.
	if err := validateDerivedFieldValidity(ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Validate that enum fields have non-empty values.
	if err := validateEnumValues(ctx.Entities); err != nil {
		return nil, nil, err
	}

	modelsFile, err := generateModels(ctx.OutputNamespace, ctx.Entities)
	if err != nil {
		return nil, nil, fmt.Errorf("generating models: %w", err)
	}

	storeFile, err := generateStore(ctx.OutputNamespace, ctx.Entities)
	if err != nil {
		return nil, nil, fmt.Errorf("generating store: %w", err)
	}

	migrateFile, err := generateMigrate(ctx.OutputNamespace, ctx.Entities)
	if err != nil {
		return nil, nil, fmt.Errorf("generating migrate: %w", err)
	}

	sessionFactoryFile, err := generateSessionFactory(ctx.OutputNamespace)
	if err != nil {
		return nil, nil, fmt.Errorf("generating session_factory: %w", err)
	}

	genericDaoFile, err := generateGenericDao(ctx.OutputNamespace)
	if err != nil {
		return nil, nil, fmt.Errorf("generating generic_dao: %w", err)
	}

	files := []gen.File{modelsFile, storeFile, migrateFile, sessionFactoryFile, genericDaoFile}

	base := path.Base(ctx.OutputNamespace)
	wiring := &gen.Wiring{
		Imports:      []string{ctx.OutputNamespace},
		Constructors: []string{base + ".NewStore(db)"},
		NeedsDB:      true,
		DBBackend:    "gorm",
		PostDBCalls:  []string{base + ".Migrate(db)"},
		GoModRequires: map[string]string{
			"gorm.io/gorm":            "v1.25.12",
			"gorm.io/driver/postgres": "v1.5.11",
			"gorm.io/datatypes":       "v1.2.5",
		},
	}

	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}

	return files, wiring, nil
}

// reservedTypeNames is the union of: (1) Go keywords, (2) Go predeclared
// identifiers, and (3) generator-internal identifiers. Entity names that
// match any of these produce uncompilable or shadowed generated code.
var reservedTypeNames = map[string]bool{
	// Generator-internal identifiers.
	"Store":       true,
	"NewStore":    true,
	"Meta":        true,
	"GenericDao":  true,
	"NewGenericDao": true,
	"SessionFactory": true,
	"Migrate":     true,
	// Go keywords.
	"break": true, "case": true, "chan": true, "const": true,
	"continue": true, "default": true, "defer": true, "else": true,
	"fallthrough": true, "for": true, "func": true, "go": true,
	"goto": true, "if": true, "import": true, "interface": true,
	"map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true,
	"var": true,
	// Go predeclared identifiers.
	"any": true, "bool": true, "byte": true, "comparable": true,
	"complex64": true, "complex128": true, "error": true, "float32": true,
	"float64": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true,
	"uint64": true, "uintptr": true,
	"true": true, "false": true, "iota": true, "nil": true,
	"append": true, "cap": true, "clear": true, "close": true,
	"complex": true, "copy": true, "delete": true, "imag": true,
	"len": true, "make": true, "max": true, "min": true,
	"new": true, "panic": true, "print": true, "println": true,
	"real": true, "recover": true,
	// Import aliases used in generated files.
	"gorm":       true,
	"clause":     true,
	"datatypes":  true,
	"json":       true,
	"fmt":        true,
	"time":       true,
}

// --- Models ---

// generateModels produces models.go with Meta base struct and entity struct
// definitions using GORM struct tags.
func generateModels(ns string, entities []types.Entity) (gen.File, error) {
	var buf bytes.Buffer

	needTime := true // Meta always uses time.Time
	needDatatypes := false
	needGormDeletedAt := true // Meta always uses gorm.DeletedAt
	hasRef := false

	for _, e := range entities {
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeJsonb {
				needDatatypes = true
			}
			if f.Type == types.FieldTypeRef && f.To != "" {
				hasRef = true
			}
		}
	}

	_ = needTime       // always true
	_ = needGormDeletedAt // always true
	_ = hasRef

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))

	fmt.Fprintf(&buf, "import (\n")
	if needDatatypes {
		fmt.Fprintf(&buf, "\t\"gorm.io/datatypes\"\n")
	}
	fmt.Fprintf(&buf, "\t\"gorm.io/gorm\"\n")
	fmt.Fprintf(&buf, "\t\"time\"\n")
	fmt.Fprintf(&buf, ")\n\n")

	// Meta base struct.
	fmt.Fprintf(&buf, "// Meta is the base model, embedded in all entities.\n")
	fmt.Fprintf(&buf, "type Meta struct {\n")
	fmt.Fprintf(&buf, "\tID          string         `json:\"id\"`\n")
	fmt.Fprintf(&buf, "\tCreatedTime time.Time      `json:\"created_time\" gorm:\"autoCreateTime\"`\n")
	fmt.Fprintf(&buf, "\tUpdatedTime time.Time      `json:\"updated_time\" gorm:\"autoUpdateTime\"`\n")
	fmt.Fprintf(&buf, "\tDeletedAt   gorm.DeletedAt `json:\"-\" gorm:\"index\"`\n")
	fmt.Fprintf(&buf, "}\n\n")

	// Entity structs.
	for _, e := range entities {
		fmt.Fprintf(&buf, "// %s represents the %s entity.\n", e.Name, e.Name)
		fmt.Fprintf(&buf, "type %s struct {\n", e.Name)
		fmt.Fprintf(&buf, "\tMeta\n")
		for _, f := range e.Fields {
			goName := toPascalCase(f.Name)
			goType := fieldTypeToGo(f)
			gormTag := buildGormTag(f)
			jsonTag := f.Name
			if f.Optional {
				jsonTag += ",omitempty"
			}
			fmt.Fprintf(&buf, "\t%s %s `json:%q gorm:%q`\n", goName, goType, jsonTag, gormTag)

			// Add GORM relationship field for ref types (enables FK constraints
			// via AutoMigrate).
			if f.Type == types.FieldTypeRef && f.To != "" {
				refFieldName := toPascalCase(f.Name) + "Ref"
				fmt.Fprintf(&buf, "\t%s *%s `json:\"-\" gorm:\"foreignKey:%s\"`\n",
					refFieldName, f.To, goName)
			}
		}
		fmt.Fprintf(&buf, "}\n\n")
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting models: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "models.go"),
		Content: formatted,
	}, nil
}

// fieldTypeToGo maps a field to its Go type representation, accounting for
// GORM-specific types (datatypes.JSON for jsonb) and nullability.
func fieldTypeToGo(f types.Field) string {
	var base string
	switch f.Type {
	case types.FieldTypeString, types.FieldTypeEnum, types.FieldTypeRef:
		base = "string"
	case types.FieldTypeInt32:
		base = "int32"
	case types.FieldTypeInt64:
		base = "int64"
	case types.FieldTypeFloat:
		base = "float32"
	case types.FieldTypeDouble:
		base = "float64"
	case types.FieldTypeBool:
		base = "bool"
	case types.FieldTypeBytes:
		base = "[]byte"
	case types.FieldTypeTimestamp:
		base = "time.Time"
	case types.FieldTypeJsonb:
		base = "datatypes.JSON"
	default:
		base = "any"
	}

	// Nullable columns (optional or computed) use pointer types so GORM can
	// distinguish between zero values and NULL. []byte and datatypes.JSON
	// already handle nil correctly.
	if (f.Optional || f.Computed) && base != "[]byte" && base != "datatypes.JSON" {
		base = "*" + base
	}
	return base
}

// buildGormTag constructs the GORM struct tag value for a field.
func buildGormTag(f types.Field) string {
	var parts []string

	// Column name mapping.
	parts = append(parts, "column:"+f.Name)

	// Type overrides.
	if f.Type == types.FieldTypeJsonb {
		parts = append(parts, "type:jsonb")
	}

	// Nullability.
	if !f.Optional && !f.Computed {
		parts = append(parts, "not null")
	}

	// Constraints.
	if f.Unique {
		parts = append(parts, "uniqueIndex")
	}

	// Composite unique constraints: each field in the group gets the same
	// named uniqueIndex so GORM creates a single composite index.
	if len(f.UniqueComposite) > 0 {
		idxName := "composite_" + strings.Join(f.UniqueComposite, "_")
		parts = append(parts, "uniqueIndex:"+idxName)
	}

	if f.MaxLength != nil {
		parts = append(parts, fmt.Sprintf("size:%d", *f.MaxLength))
	}

	if f.MinLength != nil {
		parts = append(parts, fmt.Sprintf("check:length(%s) >= %d", f.Name, *f.MinLength))
	}

	if f.Default != nil {
		parts = append(parts, fmt.Sprintf("default:%v", f.Default))
	}

	return strings.Join(parts, ";")
}

// --- Store ---

// generateStore produces store.go with the Store type and CRUD + list + upsert
// + exists methods using GORM.
func generateStore(ns string, entities []types.Entity) (gen.File, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"context\"\n")
	fmt.Fprintf(&buf, "\t\"encoding/json\"\n")
	fmt.Fprintf(&buf, "\t\"fmt\"\n")
	fmt.Fprintf(&buf, "\n")
	fmt.Fprintf(&buf, "\t\"gorm.io/gorm\"\n")
	fmt.Fprintf(&buf, "\t\"gorm.io/gorm/clause\"\n")
	fmt.Fprintf(&buf, ")\n\n")

	// Store struct and constructor.
	fmt.Fprintf(&buf, "// Store provides GORM-backed storage for all entities.\n")
	fmt.Fprintf(&buf, "type Store struct {\n")
	fmt.Fprintf(&buf, "\tdb *gorm.DB\n")
	fmt.Fprintf(&buf, "}\n\n")

	fmt.Fprintf(&buf, "// NewStore creates a new Store with the given GORM connection.\n")
	fmt.Fprintf(&buf, "func NewStore(db *gorm.DB) *Store {\n")
	fmt.Fprintf(&buf, "\treturn &Store{db: db}\n")
	fmt.Fprintf(&buf, "}\n\n")

	emitCreateMethod(&buf, entities)
	emitGetMethod(&buf, entities)
	emitReplaceMethod(&buf, entities)
	emitDeleteMethod(&buf, entities)
	emitListMethod(&buf, entities)
	emitUpsertMethod(&buf, entities)
	emitExistsMethod(&buf, entities)

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting store: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "store.go"),
		Content: formatted,
	}, nil
}

// emitCreateMethod generates the Create dispatcher using GORM.
func emitCreateMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Create inserts a new entity record. Computed fields are excluded.\n")
	fmt.Fprintf(buf, "func (s *Store) Create(ctx context.Context, entity string, value any) error {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)
		fmt.Fprintf(buf, "\t\tdata, err := json.Marshal(value)\n")
		fmt.Fprintf(buf, "\t\tif err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn fmt.Errorf(\"marshaling %s: %%w\", err)\n", e.Name)
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\tvar v %s\n", e.Name)
		fmt.Fprintf(buf, "\t\tif err := json.Unmarshal(data, &v); err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn fmt.Errorf(\"unmarshaling %s: %%w\", err)\n", e.Name)
		fmt.Fprintf(buf, "\t\t}\n")

		// Clear computed fields — they are read-only.
		for _, f := range e.Fields {
			if f.Computed {
				goName := toPascalCase(f.Name)
				goType := fieldTypeToGo(f)
				fmt.Fprintf(buf, "\t\tv.%s = %s\n", goName, zeroValue(goType))
			}
		}

		fmt.Fprintf(buf, "\t\treturn s.db.WithContext(ctx).Create(&v).Error\n")
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitGetMethod generates the Get dispatcher using GORM.
func emitGetMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Get retrieves a single entity by ID.\n")
	fmt.Fprintf(buf, "func (s *Store) Get(ctx context.Context, entity string, id string) (any, error) {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)
		fmt.Fprintf(buf, "\t\tvar v %s\n", e.Name)
		fmt.Fprintf(buf, "\t\tif err := s.db.WithContext(ctx).First(&v, \"id = ?\", id).Error; err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn nil, err\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\treturn v, nil\n")
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn nil, fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitReplaceMethod generates the Replace dispatcher using GORM selective Updates.
func emitReplaceMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Replace modifies an existing entity by ID. Computed fields are excluded.\n")
	fmt.Fprintf(buf, "func (s *Store) Replace(ctx context.Context, entity string, id string, value any) error {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		writeCols := writeColumns(e)

		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)

		if len(writeCols) == 0 {
			fmt.Fprintf(buf, "\t\treturn fmt.Errorf(\"entity %s has no writable fields\")\n", e.Name)
			continue
		}

		fmt.Fprintf(buf, "\t\tdata, err := json.Marshal(value)\n")
		fmt.Fprintf(buf, "\t\tif err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn fmt.Errorf(\"marshaling %s: %%w\", err)\n", e.Name)
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\tvar v %s\n", e.Name)
		fmt.Fprintf(buf, "\t\tif err := json.Unmarshal(data, &v); err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn fmt.Errorf(\"unmarshaling %s: %%w\", err)\n", e.Name)
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\tv.ID = id\n")

		// Clear computed fields — they are read-only and must not be overwritten.
		for _, f := range e.Fields {
			if f.Computed {
				goName := toPascalCase(f.Name)
				goType := fieldTypeToGo(f)
				fmt.Fprintf(buf, "\t\tv.%s = %s\n", goName, zeroValue(goType))
			}
		}

		// Use selective column update (not Save) so computed fields are
		// preserved in the database rather than overwritten with zeros.
		fmt.Fprintf(buf, "\t\treturn s.db.WithContext(ctx).Model(&%s{}).Where(\"id = ?\", id).Select([]string{%s}).Updates(&v).Error\n",
			e.Name, quoteStringSlice(writeCols))
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitDeleteMethod generates the Delete dispatcher using GORM soft delete.
func emitDeleteMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Delete soft-deletes an entity by ID.\n")
	fmt.Fprintf(buf, "func (s *Store) Delete(ctx context.Context, entity string, id string) error {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)
		fmt.Fprintf(buf, "\t\treturn s.db.WithContext(ctx).Where(\"id = ?\", id).Delete(&%s{}).Error\n", e.Name)
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitListMethod generates the List dispatcher using GORM with scope filtering.
func emitListMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// List retrieves entities with optional scope filtering and pagination.\n")
	fmt.Fprintf(buf, "func (s *Store) List(ctx context.Context, entity string, scopeField string, scopeValue string, offset int, limit int) (any, error) {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		allCols := allColumns(e)

		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)

		// Build valid column set for scope validation.
		fmt.Fprintf(buf, "\t\tvalidCols := map[string]bool{")
		for i, col := range allCols {
			if i > 0 {
				fmt.Fprintf(buf, ", ")
			}
			fmt.Fprintf(buf, "%q: true", col)
		}
		fmt.Fprintf(buf, "}\n")

		fmt.Fprintf(buf, "\t\tquery := s.db.WithContext(ctx).Model(&%s{})\n", e.Name)
		fmt.Fprintf(buf, "\t\tif scopeField != \"\" && scopeValue != \"\" {\n")
		fmt.Fprintf(buf, "\t\t\tif !validCols[scopeField] {\n")
		fmt.Fprintf(buf, "\t\t\t\treturn nil, fmt.Errorf(\"invalid scope field %%q for entity %s\", scopeField)\n", e.Name)
		fmt.Fprintf(buf, "\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\tquery = query.Where(scopeField+\" = ?\", scopeValue)\n")
		fmt.Fprintf(buf, "\t\t}\n")

		fmt.Fprintf(buf, "\t\tif offset > 0 {\n")
		fmt.Fprintf(buf, "\t\t\tquery = query.Offset(offset)\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\tif limit > 0 {\n")
		fmt.Fprintf(buf, "\t\t\tquery = query.Limit(limit)\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\tvar result []%s\n", e.Name)
		fmt.Fprintf(buf, "\t\tif err := query.Find(&result).Error; err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn nil, err\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\treturn result, nil\n")
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn nil, fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitUpsertMethod generates the Upsert dispatcher using GORM OnConflict clause.
func emitUpsertMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Upsert inserts or updates an entity using natural-key conflict resolution.\n")
	fmt.Fprintf(buf, "// When concurrency is \"optimistic\", the update only proceeds if the incoming\n")
	fmt.Fprintf(buf, "// generation value is newer than the existing row's generation.\n")
	fmt.Fprintf(buf, "func (s *Store) Upsert(ctx context.Context, entity string, value any, upsertKey []string, concurrency string) error {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		writeCols := writeColumns(e)
		hasGeneration := entityHasField(e, "generation")

		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)
		fmt.Fprintf(buf, "\t\tdata, err := json.Marshal(value)\n")
		fmt.Fprintf(buf, "\t\tif err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn fmt.Errorf(\"marshaling %s: %%w\", err)\n", e.Name)
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\tvar v %s\n", e.Name)
		fmt.Fprintf(buf, "\t\tif err := json.Unmarshal(data, &v); err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn fmt.Errorf(\"unmarshaling %s: %%w\", err)\n", e.Name)
		fmt.Fprintf(buf, "\t\t}\n")

		// Clear computed fields — they are read-only and must not be
		// persisted on the INSERT path.
		for _, f := range e.Fields {
			if f.Computed {
				goName := toPascalCase(f.Name)
				goType := fieldTypeToGo(f)
				fmt.Fprintf(buf, "\t\tv.%s = %s\n", goName, zeroValue(goType))
			}
		}

		// Build valid column set for upsert key validation.
		fmt.Fprintf(buf, "\t\tvalidCols := map[string]bool{")
		for i, col := range writeCols {
			if i > 0 {
				fmt.Fprintf(buf, ", ")
			}
			fmt.Fprintf(buf, "%q: true", col)
		}
		fmt.Fprintf(buf, "}\n")

		fmt.Fprintf(buf, "\t\tfor _, k := range upsertKey {\n")
		fmt.Fprintf(buf, "\t\t\tif !validCols[k] {\n")
		fmt.Fprintf(buf, "\t\t\t\treturn fmt.Errorf(\"invalid upsert key field %%q for entity %s\", k)\n", e.Name)
		fmt.Fprintf(buf, "\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t}\n")

		// Build conflict columns.
		fmt.Fprintf(buf, "\t\tconflictCols := make([]clause.Column, len(upsertKey))\n")
		fmt.Fprintf(buf, "\t\tfor i, k := range upsertKey {\n")
		fmt.Fprintf(buf, "\t\t\tconflictCols[i] = clause.Column{Name: k}\n")
		fmt.Fprintf(buf, "\t\t}\n")

		// Build update columns (non-key, non-id).
		fmt.Fprintf(buf, "\t\tkeySet := make(map[string]bool, len(upsertKey))\n")
		fmt.Fprintf(buf, "\t\tfor _, k := range upsertKey {\n")
		fmt.Fprintf(buf, "\t\t\tkeySet[k] = true\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\tvar updateCols []string\n")
		fmt.Fprintf(buf, "\t\tfor _, col := range []string{%s} {\n", quoteStringSlice(writeCols))
		fmt.Fprintf(buf, "\t\t\tif !keySet[col] {\n")
		fmt.Fprintf(buf, "\t\t\t\tupdateCols = append(updateCols, col)\n")
		fmt.Fprintf(buf, "\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t}\n")

		fmt.Fprintf(buf, "\t\tonConflict := clause.OnConflict{\n")
		fmt.Fprintf(buf, "\t\t\tColumns: conflictCols,\n")
		fmt.Fprintf(buf, "\t\t}\n")

		fmt.Fprintf(buf, "\t\tif len(updateCols) > 0 {\n")
		fmt.Fprintf(buf, "\t\t\tonConflict.DoUpdates = clause.AssignmentColumns(updateCols)\n")

		if hasGeneration {
			fmt.Fprintf(buf, "\t\t\tif concurrency == \"optimistic\" {\n")
			fmt.Fprintf(buf, "\t\t\t\tonConflict.Where = clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: `EXCLUDED.\"generation\" > \"generation\"`}}}\n")
			fmt.Fprintf(buf, "\t\t\t}\n")
		} else {
			fmt.Fprintf(buf, "\t\t\tif concurrency == \"optimistic\" {\n")
			fmt.Fprintf(buf, "\t\t\t\treturn fmt.Errorf(\"optimistic concurrency requires a 'generation' field on entity %s\")\n", e.Name)
			fmt.Fprintf(buf, "\t\t\t}\n")
		}

		fmt.Fprintf(buf, "\t\t} else {\n")
		fmt.Fprintf(buf, "\t\t\tonConflict.DoNothing = true\n")
		fmt.Fprintf(buf, "\t\t}\n")

		fmt.Fprintf(buf, "\t\treturn s.db.WithContext(ctx).Clauses(onConflict).Create(&v).Error\n")
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitExistsMethod generates the Exists dispatcher using GORM Count.
func emitExistsMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Exists checks whether an entity with the given ID exists.\n")
	fmt.Fprintf(buf, "func (s *Store) Exists(ctx context.Context, entity string, id string) (bool, error) {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)
		fmt.Fprintf(buf, "\t\tvar count int64\n")
		fmt.Fprintf(buf, "\t\tif err := s.db.WithContext(ctx).Model(&%s{}).Where(\"id = ?\", id).Count(&count).Error; err != nil {\n", e.Name)
		fmt.Fprintf(buf, "\t\t\treturn false, err\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\treturn count > 0, nil\n")
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn false, fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// --- Migration ---

// generateMigrate produces migrate.go with migration infrastructure and the
// initial migration using GORM AutoMigrate.
func generateMigrate(ns string, entities []types.Entity) (gen.File, error) {
	var buf bytes.Buffer

	// Sort entities by ref dependencies so referenced tables are created first.
	sorted, err := topoSortEntities(entities)
	if err != nil {
		return gen.File{}, err
	}

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"fmt\"\n")
	fmt.Fprintf(&buf, "\n")
	fmt.Fprintf(&buf, "\t\"gorm.io/gorm\"\n")
	fmt.Fprintf(&buf, ")\n\n")

	// Migration infrastructure.
	fmt.Fprintf(&buf, "// MigrationFunc is a function that performs a database migration.\n")
	fmt.Fprintf(&buf, "type MigrationFunc func(db *gorm.DB) error\n\n")

	fmt.Fprintf(&buf, "// Migration represents a named database migration.\n")
	fmt.Fprintf(&buf, "type Migration struct {\n")
	fmt.Fprintf(&buf, "\tName string\n")
	fmt.Fprintf(&buf, "\tFunc MigrationFunc\n")
	fmt.Fprintf(&buf, "}\n\n")

	fmt.Fprintf(&buf, "var migrations []Migration\n\n")

	fmt.Fprintf(&buf, "// Register adds a migration to the ordered migration list.\n")
	fmt.Fprintf(&buf, "func Register(name string, fn MigrationFunc) {\n")
	fmt.Fprintf(&buf, "\tmigrations = append(migrations, Migration{Name: name, Func: fn})\n")
	fmt.Fprintf(&buf, "}\n\n")

	fmt.Fprintf(&buf, "// Migrate runs all registered migrations in order.\n")
	fmt.Fprintf(&buf, "func Migrate(db *gorm.DB) error {\n")
	fmt.Fprintf(&buf, "\tfor _, m := range migrations {\n")
	fmt.Fprintf(&buf, "\t\tif err := m.Func(db); err != nil {\n")
	fmt.Fprintf(&buf, "\t\t\treturn fmt.Errorf(\"migration %%s: %%w\", m.Name, err)\n")
	fmt.Fprintf(&buf, "\t\t}\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\treturn nil\n")
	fmt.Fprintf(&buf, "}\n\n")

	// Initial migration registration.
	fmt.Fprintf(&buf, "func init() {\n")
	fmt.Fprintf(&buf, "\tRegister(\"001_initial\", func(db *gorm.DB) error {\n")
	fmt.Fprintf(&buf, "\t\treturn db.AutoMigrate(\n")
	for _, e := range sorted {
		fmt.Fprintf(&buf, "\t\t\t&%s{},\n", e.Name)
	}
	fmt.Fprintf(&buf, "\t\t)\n")
	fmt.Fprintf(&buf, "\t})\n")
	fmt.Fprintf(&buf, "}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting migrate: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "migrate.go"),
		Content: formatted,
	}, nil
}

// --- SessionFactory ---

// generateSessionFactory produces session_factory.go with the SessionFactory
// interface for database connection management.
func generateSessionFactory(ns string) (gen.File, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))
	fmt.Fprintf(&buf, "import \"gorm.io/gorm\"\n\n")
	fmt.Fprintf(&buf, "// SessionFactory provides database connections for production and testing.\n")
	fmt.Fprintf(&buf, "type SessionFactory interface {\n")
	fmt.Fprintf(&buf, "\tNew() *gorm.DB\n")
	fmt.Fprintf(&buf, "\tClose() error\n")
	fmt.Fprintf(&buf, "}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting session_factory: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "session_factory.go"),
		Content: formatted,
	}, nil
}

// --- GenericDao ---

// generateGenericDao produces generic_dao.go with the GenericDao base type
// that the tsl-search component uses for query building.
func generateGenericDao(ns string) (gen.File, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))
	fmt.Fprintf(&buf, "import \"gorm.io/gorm\"\n\n")
	fmt.Fprintf(&buf, "// GenericDao provides a base DAO type that the tsl-search component\n")
	fmt.Fprintf(&buf, "// can build queries on top of with ordering, filtering, JOINs, and pagination.\n")
	fmt.Fprintf(&buf, "type GenericDao struct {\n")
	fmt.Fprintf(&buf, "\tDB *gorm.DB\n")
	fmt.Fprintf(&buf, "}\n\n")
	fmt.Fprintf(&buf, "// NewGenericDao creates a GenericDao with the given GORM connection.\n")
	fmt.Fprintf(&buf, "func NewGenericDao(db *gorm.DB) *GenericDao {\n")
	fmt.Fprintf(&buf, "\treturn &GenericDao{DB: db}\n")
	fmt.Fprintf(&buf, "}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting generic_dao: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "generic_dao.go"),
		Content: formatted,
	}, nil
}

// --- Helpers ---

// zeroValue returns the Go zero-value expression for a type string.
func zeroValue(goType string) string {
	if strings.HasPrefix(goType, "*") {
		return "nil"
	}
	switch goType {
	case "string":
		return `""`
	case "bool":
		return "false"
	case "int32", "int64", "float32", "float64":
		return "0"
	case "[]byte":
		return "nil"
	case "time.Time":
		return "time.Time{}"
	case "datatypes.JSON":
		return "nil"
	default:
		return "nil"
	}
}

// entityHasField checks whether an entity has a field with the given name.
func entityHasField(e types.Entity, name string) bool {
	for _, f := range e.Fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// writeColumns returns the list of non-computed field names for an entity,
// suitable for INSERT/UPDATE operations.
func writeColumns(e types.Entity) []string {
	var cols []string
	for _, f := range e.Fields {
		if !f.Computed {
			cols = append(cols, f.Name)
		}
	}
	return cols
}

// allColumns returns all field names for an entity, including computed.
func allColumns(e types.Entity) []string {
	cols := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		cols[i] = f.Name
	}
	return cols
}

// tableName converts an entity name to a PostgreSQL table name:
// PascalCase to snake_case, then pluralize using English rules.
func tableName(entityName string) string {
	return pluralize(toSnakeCase(entityName))
}

// pluralize applies basic English pluralization rules to a snake_case word.
func pluralize(s string) string {
	if s == "" {
		return s
	}
	if strings.HasSuffix(s, "s") || strings.HasSuffix(s, "x") || strings.HasSuffix(s, "z") ||
		strings.HasSuffix(s, "sh") || strings.HasSuffix(s, "ch") {
		return s + "es"
	}
	if strings.HasSuffix(s, "y") && len(s) >= 2 {
		preceding := s[len(s)-2]
		if !strings.ContainsRune("aeiou", rune(preceding)) {
			return s[:len(s)-1] + "ies"
		}
	}
	return s + "s"
}

// toSnakeCase converts PascalCase to snake_case, correctly handling
// consecutive uppercase letters (acronyms) like HTTP, API, ID.
func toSnakeCase(s string) string {
	runes := []rune(s)
	var result []rune
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			if unicode.IsLower(prev) {
				result = append(result, '_')
			} else if unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
				result = append(result, '_')
			}
		}
		result = append(result, unicode.ToLower(r))
	}
	return string(result)
}

// toPascalCase converts a snake_case string to PascalCase, treating "id" as
// the acronym "ID" per Go conventions.
func toPascalCase(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if strings.EqualFold(p, "id") {
			parts[i] = "ID"
		} else if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// quoteStringSlice produces a Go source literal for a string slice.
func quoteStringSlice(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}

// --- Topological Sort ---

// topoSortEntities topologically sorts entities by ref dependencies so that
// referenced tables are created before referencing tables.
func topoSortEntities(entities []types.Entity) ([]types.Entity, error) {
	entityIndex := make(map[string]int, len(entities))
	for i, e := range entities {
		entityIndex[e.Name] = i
	}

	deps := make(map[string]map[string]bool, len(entities))
	for _, e := range entities {
		d := make(map[string]bool)
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeRef && f.To != "" {
				if _, ok := entityIndex[f.To]; ok && f.To != e.Name {
					d[f.To] = true
				}
			}
		}
		deps[e.Name] = d
	}

	var sorted []types.Entity
	visited := make(map[string]bool, len(entities))

	for len(sorted) < len(entities) {
		var ready []string
		for _, e := range entities {
			if visited[e.Name] {
				continue
			}
			allSatisfied := true
			for dep := range deps[e.Name] {
				if !visited[dep] {
					allSatisfied = false
					break
				}
			}
			if allSatisfied {
				ready = append(ready, e.Name)
			}
		}
		if len(ready) == 0 {
			var cycle []string
			for _, e := range entities {
				if !visited[e.Name] {
					cycle = append(cycle, e.Name)
				}
			}
			return nil, fmt.Errorf("circular ref dependency among entities: %s",
				strings.Join(cycle, " → "))
		}
		for _, name := range ready {
			visited[name] = true
			sorted = append(sorted, entities[entityIndex[name]])
		}
	}

	return sorted, nil
}

// --- Validation ---

func validateEntityUniqueness(entities []types.Entity) error {
	seen := make(map[string]int, len(entities))
	var dupes []string
	for _, e := range entities {
		seen[e.Name]++
		if seen[e.Name] == 2 {
			dupes = append(dupes, e.Name)
		}
	}
	if len(dupes) > 0 {
		return fmt.Errorf("duplicate entity names: %s; each entity must have a unique name",
			strings.Join(dupes, ", "))
	}
	return nil
}

func validateCaseInsensitiveUniqueness(entities []types.Entity) error {
	seen := make(map[string]string, len(entities))
	var errs []string
	for _, e := range entities {
		tbl := tableName(e.Name)
		if existing, ok := seen[tbl]; ok {
			errs = append(errs, fmt.Sprintf(
				"entities %q and %q both produce table name %q",
				existing, e.Name, tbl))
		} else {
			seen[tbl] = e.Name
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("derived table name collisions:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateReservedNames(entities []types.Entity) error {
	for _, e := range entities {
		if reservedTypeNames[e.Name] {
			return fmt.Errorf(
				"entity name %q collides with postgres-adapter generator internal identifier %q; rename the entity to avoid a redeclaration error in generated code",
				e.Name, e.Name)
		}
	}
	return nil
}

func validateRefTargets(entities []types.Entity) error {
	entityNames := make(map[string]bool, len(entities))
	for _, e := range entities {
		entityNames[e.Name] = true
	}

	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeRef {
				if f.To == "" {
					errs = append(errs, fmt.Sprintf(
						"entity %s: field %q has type ref but no 'to' attribute — ref fields must specify the target entity",
						e.Name, f.Name))
				} else if !entityNames[f.To] {
					errs = append(errs, fmt.Sprintf(
						"entity %s: field %q has type ref with to: %q, but entity %q does not exist",
						e.Name, f.Name, f.To, f.To))
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("unresolved ref targets:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func validateFieldUniqueness(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		seen := make(map[string]bool, len(e.Fields))
		for _, f := range e.Fields {
			if seen[f.Name] {
				errs = append(errs, fmt.Sprintf(
					"entity %s: duplicate field name %q",
					e.Name, f.Name))
			}
			seen[f.Name] = true
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("field name uniqueness violations:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateFieldNameCharset(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if !validFieldNamePattern.MatchString(f.Name) {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field name %q contains invalid characters; field names must match %s",
					e.Name, f.Name, validFieldNamePattern.String()))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid field name characters:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateEntityNameCharset(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		if !validFieldNamePattern.MatchString(e.Name) {
			errs = append(errs, fmt.Sprintf(
				"entity name %q contains invalid characters; entity names must match %s",
				e.Name, validFieldNamePattern.String()))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid entity name characters:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateNoImplicitIDCollision(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if toPascalCase(f.Name) == "ID" {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field %q collides with the implicit primary key column — entities cannot declare a field named %q",
					e.Name, f.Name, f.Name))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("implicit ID collisions:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

var validGoIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func validateDerivedEntityValidity(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		if e.Name == "_" {
			errs = append(errs, fmt.Sprintf(
				"entity name %q is the Go blank identifier and cannot be used as a type name",
				e.Name))
			continue
		}
		if !validGoIdentifier.MatchString(e.Name) {
			errs = append(errs, fmt.Sprintf(
				"entity name %q is not a valid Go identifier",
				e.Name))
			continue
		}
		allUnderscore := true
		for _, r := range e.Name {
			if r != '_' {
				allUnderscore = false
				break
			}
		}
		if allUnderscore {
			errs = append(errs, fmt.Sprintf(
				"entity name %q consists only of underscores and cannot be used as a Go type name",
				e.Name))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid derived entity identifiers:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateDerivedFieldValidity(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			pascal := toPascalCase(f.Name)
			if !validGoIdentifier.MatchString(pascal) {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field %q transforms to %q via toPascalCase, which is not a valid Go identifier",
					e.Name, f.Name, pascal))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid derived field identifiers:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateDerivedFieldUniqueness(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		seen := make(map[string]string, len(e.Fields))
		for _, f := range e.Fields {
			pascal := toPascalCase(f.Name)
			if existing, ok := seen[pascal]; ok && existing != f.Name {
				errs = append(errs, fmt.Sprintf(
					"entity %s: fields %q and %q both produce Go struct field name %q",
					e.Name, existing, f.Name, pascal))
			} else {
				seen[pascal] = f.Name
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("derived field name collisions:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateEnumValues(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if f.Type != types.FieldTypeEnum {
				continue
			}
			if len(f.Values) == 0 {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field %q has type enum but no values — enum fields must specify at least one value",
					e.Name, f.Name))
				continue
			}
			seen := make(map[string]bool, len(f.Values))
			for _, v := range f.Values {
				if seen[v] {
					errs = append(errs, fmt.Sprintf(
						"entity %s: field %q has duplicate enum value %q",
						e.Name, f.Name, v))
				}
				seen[v] = true
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid enum fields:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}
