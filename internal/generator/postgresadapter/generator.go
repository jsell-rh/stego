// Package postgresadapter implements the postgres-adapter component Generator.
// It produces Go model structs, a Store implementation with CRUD + list + upsert
// query functions, and migration SQL from the service declaration's entities.
package postgresadapter

import (
	"bytes"
	"fmt"
	"go/format"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/stego-project/stego/internal/gen"
	"github.com/stego-project/stego/internal/types"
)

// validFieldNamePattern defines the safe character set for field names across
// all target systems (Go identifiers, SQL identifiers, URL segments).
var validFieldNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Generator produces the postgres-adapter component's generated code.
type Generator struct{}

// Generate produces model structs, a Store implementation, and migration SQL
// for all entities in the service declaration. It returns wiring instructions
// for main.go assembly.
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

	migrationFile, err := generateMigration(ctx.OutputNamespace, ctx.Entities)
	if err != nil {
		return nil, nil, fmt.Errorf("generating migration: %w", err)
	}

	files := []gen.File{modelsFile, storeFile, migrationFile}

	wiring := &gen.Wiring{
		Imports:      []string{ctx.OutputNamespace},
		Constructors: []string{path.Base(ctx.OutputNamespace) + ".NewStore(db)"},
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
	"Store":    true,
	"NewStore": true,
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
}

// generateModels produces models.go with entity struct definitions.
func generateModels(ns string, entities []types.Entity) (gen.File, error) {
	var buf bytes.Buffer

	needTime := false
	needJSON := false
	for _, e := range entities {
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeTimestamp {
				needTime = true
			}
			if f.Type == types.FieldTypeJsonb {
				needJSON = true
			}
		}
	}

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))

	if needTime || needJSON {
		fmt.Fprintf(&buf, "import (\n")
		if needJSON {
			fmt.Fprintf(&buf, "\t\"encoding/json\"\n")
		}
		if needTime {
			fmt.Fprintf(&buf, "\t\"time\"\n")
		}
		fmt.Fprintf(&buf, ")\n\n")
	}

	for _, e := range entities {
		fmt.Fprintf(&buf, "// %s represents the %s entity.\n", e.Name, e.Name)
		fmt.Fprintf(&buf, "type %s struct {\n", e.Name)
		fmt.Fprintf(&buf, "\tID string `json:\"id\"`\n")
		for _, f := range e.Fields {
			goName := toPascalCase(f.Name)
			goType := fieldTypeToGo(f.Type)
			// Nullable columns (optional or computed) use pointer types so
			// database/sql.Rows.Scan can receive SQL NULL values. []byte and
			// json.RawMessage already handle nil correctly.
			if (f.Optional || f.Computed) && goType != "[]byte" && goType != "json.RawMessage" {
				goType = "*" + goType
			}
			fmt.Fprintf(&buf, "\t%s %s `json:%q`\n", goName, goType, f.Name)
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

// generateStore produces store.go with the Store type and CRUD + list + upsert
// + exists methods that dispatch to entity-specific SQL.
func generateStore(ns string, entities []types.Entity) (gen.File, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"database/sql\"\n")
	fmt.Fprintf(&buf, "\t\"encoding/json\"\n")
	fmt.Fprintf(&buf, "\t\"fmt\"\n")
	fmt.Fprintf(&buf, "\t\"strings\"\n")
	fmt.Fprintf(&buf, ")\n\n")

	// Store struct and constructor.
	fmt.Fprintf(&buf, "// Store provides PostgreSQL-backed storage for all entities.\n")
	fmt.Fprintf(&buf, "type Store struct {\n")
	fmt.Fprintf(&buf, "\tdb *sql.DB\n")
	fmt.Fprintf(&buf, "}\n\n")

	fmt.Fprintf(&buf, "// NewStore creates a new Store with the given database connection.\n")
	fmt.Fprintf(&buf, "func NewStore(db *sql.DB) *Store {\n")
	fmt.Fprintf(&buf, "\treturn &Store{db: db}\n")
	fmt.Fprintf(&buf, "}\n\n")

	emitCreateMethod(&buf, entities)
	emitReadMethod(&buf, entities)
	emitUpdateMethod(&buf, entities)
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

// emitCreateMethod generates the Create dispatcher. Computed fields are
// excluded from the INSERT column list (they are read-only, populated by fills).
func emitCreateMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Create inserts a new entity record. Computed fields are excluded.\n")
	fmt.Fprintf(buf, "func (s *Store) Create(entity string, value any) error {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		table := tableName(e.Name)
		writeCols := writeColumns(e)

		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)
		fmt.Fprintf(buf, "\t\tdata, err := json.Marshal(value)\n")
		fmt.Fprintf(buf, "\t\tif err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn fmt.Errorf(\"marshaling %s: %%w\", err)\n", e.Name)
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\tvar v %s\n", e.Name)
		fmt.Fprintf(buf, "\t\tif err := json.Unmarshal(data, &v); err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn fmt.Errorf(\"unmarshaling %s: %%w\", err)\n", e.Name)
		fmt.Fprintf(buf, "\t\t}\n")

		// Build INSERT with positional parameters.
		cols := append([]string{"id"}, writeCols...)
		placeholders := make([]string, len(cols))
		for i := range placeholders {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}

		fmt.Fprintf(buf, "\t\t_, err = s.db.Exec(\n")
		fmt.Fprintf(buf, "\t\t\t`INSERT INTO \"%s\" (%s) VALUES (%s)`,\n",
			table, sqlQuotedColList(cols), strings.Join(placeholders, ", "))

		// Emit value references.
		args := []string{"v.ID"}
		for _, col := range writeCols {
			args = append(args, "v."+toPascalCase(col))
		}
		fmt.Fprintf(buf, "\t\t\t%s,\n", strings.Join(args, ", "))
		fmt.Fprintf(buf, "\t\t)\n")
		fmt.Fprintf(buf, "\t\treturn err\n")
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitReadMethod generates the Read dispatcher. All fields (including computed)
// are included in the SELECT.
func emitReadMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Read retrieves a single entity by ID.\n")
	fmt.Fprintf(buf, "func (s *Store) Read(entity string, id string) (any, error) {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		table := tableName(e.Name)
		allCols := allColumns(e)

		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)
		fmt.Fprintf(buf, "\t\tvar v %s\n", e.Name)

		cols := append([]string{"id"}, allCols...)
		scanArgs := []string{"&v.ID"}
		for _, col := range allCols {
			scanArgs = append(scanArgs, "&v."+toPascalCase(col))
		}

		fmt.Fprintf(buf, "\t\terr := s.db.QueryRow(\n")
		fmt.Fprintf(buf, "\t\t\t`SELECT %s FROM \"%s\" WHERE \"id\" = $1`, id,\n",
			sqlQuotedColList(cols), table)
		fmt.Fprintf(buf, "\t\t).Scan(%s)\n", strings.Join(scanArgs, ", "))
		fmt.Fprintf(buf, "\t\tif err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn nil, err\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\treturn v, nil\n")
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn nil, fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitUpdateMethod generates the Update dispatcher. Computed fields are
// excluded from the SET clause.
func emitUpdateMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Update modifies an existing entity by ID. Computed fields are excluded.\n")
	fmt.Fprintf(buf, "func (s *Store) Update(entity string, id string, value any) error {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		table := tableName(e.Name)
		writeCols := writeColumns(e)

		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)

		// If all fields are computed, there are no writable columns and the
		// UPDATE SET clause would be empty (invalid SQL).
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

		// Build SET clause with positional parameters.
		setClauses := make([]string, len(writeCols))
		for i, col := range writeCols {
			setClauses[i] = fmt.Sprintf(`"%s" = $%d`, col, i+1)
		}
		idParam := fmt.Sprintf("$%d", len(writeCols)+1)

		fmt.Fprintf(buf, "\t\t_, err = s.db.Exec(\n")
		fmt.Fprintf(buf, "\t\t\t`UPDATE \"%s\" SET %s WHERE \"id\" = %s`,\n",
			table, strings.Join(setClauses, ", "), idParam)

		args := make([]string, 0, len(writeCols)+1)
		for _, col := range writeCols {
			args = append(args, "v."+toPascalCase(col))
		}
		args = append(args, "id")
		fmt.Fprintf(buf, "\t\t\t%s,\n", strings.Join(args, ", "))
		fmt.Fprintf(buf, "\t\t)\n")
		fmt.Fprintf(buf, "\t\treturn err\n")
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitDeleteMethod generates the Delete dispatcher.
func emitDeleteMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Delete removes an entity by ID.\n")
	fmt.Fprintf(buf, "func (s *Store) Delete(entity string, id string) error {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		table := tableName(e.Name)
		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)
		fmt.Fprintf(buf, "\t\t_, err := s.db.Exec(`DELETE FROM \"%s\" WHERE \"id\" = $1`, id)\n", table)
		fmt.Fprintf(buf, "\t\treturn err\n")
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitListMethod generates the List dispatcher with optional scope filtering.
// The scope field name is validated against known columns to prevent SQL injection.
func emitListMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// List retrieves all entities, optionally filtered by a scope field.\n")
	fmt.Fprintf(buf, "func (s *Store) List(entity string, scopeField string, scopeValue string) (any, error) {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		table := tableName(e.Name)
		allCols := allColumns(e)

		cols := append([]string{"id"}, allCols...)
		scanArgs := make([]string, 0, len(cols))
		scanArgs = append(scanArgs, "&v.ID")
		for _, col := range allCols {
			scanArgs = append(scanArgs, "&v."+toPascalCase(col))
		}

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

		fmt.Fprintf(buf, "\t\tquery := `SELECT %s FROM \"%s\"`\n",
			sqlQuotedColList(cols), table)
		fmt.Fprintf(buf, "\t\tvar args []any\n")
		fmt.Fprintf(buf, "\t\tif scopeField != \"\" && scopeValue != \"\" {\n")
		fmt.Fprintf(buf, "\t\t\tif !validCols[scopeField] {\n")
		fmt.Fprintf(buf, "\t\t\t\treturn nil, fmt.Errorf(\"invalid scope field %%q for entity %s\", scopeField)\n", e.Name)
		fmt.Fprintf(buf, "\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\tquery += ` WHERE \"` + scopeField + `\" = $1`\n")
		fmt.Fprintf(buf, "\t\t\targs = append(args, scopeValue)\n")
		fmt.Fprintf(buf, "\t\t}\n")

		fmt.Fprintf(buf, "\t\trows, err := s.db.Query(query, args...)\n")
		fmt.Fprintf(buf, "\t\tif err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn nil, err\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\tdefer rows.Close()\n")
		fmt.Fprintf(buf, "\t\tvar result []%s\n", e.Name)
		fmt.Fprintf(buf, "\t\tfor rows.Next() {\n")
		fmt.Fprintf(buf, "\t\t\tvar v %s\n", e.Name)
		fmt.Fprintf(buf, "\t\t\tif err := rows.Scan(%s); err != nil {\n", strings.Join(scanArgs, ", "))
		fmt.Fprintf(buf, "\t\t\t\treturn nil, err\n")
		fmt.Fprintf(buf, "\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\tresult = append(result, v)\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\tif err := rows.Err(); err != nil {\n")
		fmt.Fprintf(buf, "\t\t\treturn nil, err\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\treturn result, nil\n")
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn nil, fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitUpsertMethod generates the Upsert dispatcher with natural-key conflict
// resolution and optional optimistic concurrency. Computed fields are excluded
// from the INSERT and SET clauses.
func emitUpsertMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Upsert inserts or updates an entity using natural-key conflict resolution.\n")
	fmt.Fprintf(buf, "// When concurrency is \"optimistic\", the update only proceeds if the incoming\n")
	fmt.Fprintf(buf, "// generation value is newer than the existing row's generation.\n")
	fmt.Fprintf(buf, "func (s *Store) Upsert(entity string, value any, upsertKey []string, concurrency string) error {\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		table := tableName(e.Name)
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

		// Columns and placeholders.
		cols := append([]string{"id"}, writeCols...)
		placeholders := make([]string, len(cols))
		for i := range placeholders {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
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

		// Build base INSERT query.
		fmt.Fprintf(buf, "\t\tquery := `INSERT INTO \"%s\" (%s) VALUES (%s)`\n",
			table, sqlQuotedColList(cols), strings.Join(placeholders, ", "))

		fmt.Fprintf(buf, "\t\tif len(upsertKey) > 0 {\n")
		fmt.Fprintf(buf, "\t\t\tkeySet := make(map[string]bool, len(upsertKey))\n")
		fmt.Fprintf(buf, "\t\t\tfor _, k := range upsertKey {\n")
		fmt.Fprintf(buf, "\t\t\t\tkeySet[k] = true\n")
		fmt.Fprintf(buf, "\t\t\t}\n")

		// Quote upsert key columns for SQL.
		fmt.Fprintf(buf, "\t\t\tquotedKeys := make([]string, len(upsertKey))\n")
		fmt.Fprintf(buf, "\t\t\tfor i, k := range upsertKey {\n")
		fmt.Fprintf(buf, "\t\t\t\tquotedKeys[i] = `\"` + k + `\"`\n")
		fmt.Fprintf(buf, "\t\t\t}\n")

		// Build SET clause for non-key, non-id columns.
		fmt.Fprintf(buf, "\t\t\tvar setClauses []string\n")
		fmt.Fprintf(buf, "\t\t\tallCols := []string{%s}\n", quoteStringSlice(writeCols))
		fmt.Fprintf(buf, "\t\t\tfor _, col := range allCols {\n")
		fmt.Fprintf(buf, "\t\t\t\tif !keySet[col] {\n")
		fmt.Fprintf(buf, "\t\t\t\t\tsetClauses = append(setClauses, `\"` + col + `\" = EXCLUDED.\"` + col + `\"`)\n")
		fmt.Fprintf(buf, "\t\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\t}\n")

		fmt.Fprintf(buf, "\t\t\tif len(setClauses) > 0 {\n")
		fmt.Fprintf(buf, "\t\t\t\tquery += \" ON CONFLICT (\" + strings.Join(quotedKeys, \", \") + \") DO UPDATE SET \" + strings.Join(setClauses, \", \")\n")

		// Optimistic concurrency: only emit the WHERE clause if the entity
		// actually has a "generation" field. Otherwise emit a runtime error.
		if hasGeneration {
			fmt.Fprintf(buf, "\t\t\t\tif concurrency == \"optimistic\" {\n")
			fmt.Fprintf(buf, "\t\t\t\t\tquery += ` WHERE EXCLUDED.\"generation\" > \"%s\".\"generation\"`\n", table)
			fmt.Fprintf(buf, "\t\t\t\t}\n")
		} else {
			fmt.Fprintf(buf, "\t\t\t\tif concurrency == \"optimistic\" {\n")
			fmt.Fprintf(buf, "\t\t\t\t\treturn fmt.Errorf(\"optimistic concurrency requires a 'generation' field on entity %s\")\n", e.Name)
			fmt.Fprintf(buf, "\t\t\t\t}\n")
		}

		fmt.Fprintf(buf, "\t\t\t} else {\n")
		fmt.Fprintf(buf, "\t\t\t\tquery += \" ON CONFLICT (\" + strings.Join(quotedKeys, \", \") + \") DO NOTHING\"\n")
		fmt.Fprintf(buf, "\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t}\n")

		// Emit value references.
		args := []string{"v.ID"}
		for _, col := range writeCols {
			args = append(args, "v."+toPascalCase(col))
		}
		fmt.Fprintf(buf, "\t\t_, err = s.db.Exec(query, %s)\n", strings.Join(args, ", "))
		fmt.Fprintf(buf, "\t\treturn err\n")
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn fmt.Errorf(\"unknown entity: %%s\", entity)\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "}\n\n")
}

// emitExistsMethod generates the Exists dispatcher.
func emitExistsMethod(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintf(buf, "// Exists checks whether an entity with the given ID exists.\n")
	fmt.Fprintf(buf, "func (s *Store) Exists(entity string, id string) bool {\n")
	fmt.Fprintf(buf, "\tvar table string\n")
	fmt.Fprintf(buf, "\tswitch entity {\n")

	for _, e := range entities {
		fmt.Fprintf(buf, "\tcase %q:\n", e.Name)
		fmt.Fprintf(buf, "\t\ttable = `\"%s\"`\n", tableName(e.Name))
	}

	fmt.Fprintf(buf, "\tdefault:\n")
	fmt.Fprintf(buf, "\t\treturn false\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tvar exists bool\n")
	// The table name is derived from a compile-time constant entity name,
	// not user input, so string interpolation is safe here.
	fmt.Fprintf(buf, "\terr := s.db.QueryRow(\n")
	fmt.Fprintf(buf, "\t\tfmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %%s WHERE \"id\" = $1)`, table), id,\n")
	fmt.Fprintf(buf, "\t).Scan(&exists)\n")
	fmt.Fprintf(buf, "\treturn err == nil && exists\n")
	fmt.Fprintf(buf, "}\n\n")
}

// generateMigration produces the initial migration SQL file with CREATE TABLE
// statements for all entities. Computed fields are nullable. Constraints
// (UNIQUE, NOT NULL, CHECK) are included where specified.
// validateUniqueComposites checks that all unique_composite field references
// resolve to actual fields in the entity, and returns deduplicated constraints.
func validateUniqueComposites(e types.Entity) ([][]string, error) {
	// Build set of valid field names.
	validFields := make(map[string]bool, len(e.Fields))
	for _, f := range e.Fields {
		validFields[f.Name] = true
	}

	// Collect and deduplicate constraints (normalized by sorted key).
	seen := make(map[string]bool)
	var constraints [][]string
	for _, f := range e.Fields {
		if len(f.UniqueComposite) == 0 {
			continue
		}
		// Validate each referenced field name and detect intra-list duplicates.
		intraListSeen := make(map[string]bool, len(f.UniqueComposite))
		for _, ref := range f.UniqueComposite {
			if intraListSeen[ref] {
				return nil, fmt.Errorf(
					"entity %s: field %q has duplicate entry %q in unique_composite",
					e.Name, f.Name, ref)
			}
			intraListSeen[ref] = true
			if !validFields[ref] {
				return nil, fmt.Errorf(
					"entity %s: field %q declares unique_composite referencing non-existent field %q",
					e.Name, f.Name, ref)
			}
		}
		// Deduplicate by joining the field names as a key.
		// Use the original order for the constraint (no sorting) but
		// normalize the key for dedup by sorting a copy.
		sorted := make([]string, len(f.UniqueComposite))
		copy(sorted, f.UniqueComposite)
		// Simple sort for dedup key.
		for i := 0; i < len(sorted); i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[i] > sorted[j] {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		key := strings.Join(sorted, ",")
		if !seen[key] {
			seen[key] = true
			constraints = append(constraints, f.UniqueComposite)
		}
	}
	return constraints, nil
}

// topoSortEntities topologically sorts entities by ref dependencies so that
// referenced tables are created before referencing tables. Returns an error
// if a dependency cycle is detected.
func topoSortEntities(entities []types.Entity) ([]types.Entity, error) {
	// Build adjacency: entity name → set of entities it depends on (via ref fields).
	entityIndex := make(map[string]int, len(entities))
	for i, e := range entities {
		entityIndex[e.Name] = i
	}

	// deps[name] = set of entity names that "name" depends on.
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

	// Kahn's algorithm for topological sort.
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
			// Cycle detected — find the entities involved.
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

func generateMigration(ns string, entities []types.Entity) (gen.File, error) {
	var buf bytes.Buffer

	// Sort entities by ref dependencies so referenced tables are created first.
	sorted, err := topoSortEntities(entities)
	if err != nil {
		return gen.File{}, err
	}

	fmt.Fprintf(&buf, "-- Code generated by stego. DO NOT EDIT.\n\n")

	for i, e := range sorted {
		if i > 0 {
			fmt.Fprintf(&buf, "\n")
		}
		table := tableName(e.Name)
		fmt.Fprintf(&buf, "CREATE TABLE IF NOT EXISTS %s (\n", sqlQ(table))
		fmt.Fprintf(&buf, "    %s TEXT PRIMARY KEY", sqlQ("id"))

		for _, f := range e.Fields {
			fmt.Fprintf(&buf, ",\n")
			colDef := columnDefinition(f)
			fmt.Fprintf(&buf, "    %s", colDef)
			// Emit FOREIGN KEY reference for ref fields.
			if f.Type == types.FieldTypeRef && f.To != "" {
				refTable := tableName(f.To)
				fmt.Fprintf(&buf, " REFERENCES %s(%s)", sqlQ(refTable), sqlQ("id"))
			}
		}

		// Emit validated, deduplicated table-level UNIQUE constraints.
		composites, err := validateUniqueComposites(e)
		if err != nil {
			return gen.File{}, err
		}
		for _, cols := range composites {
			quotedCols := make([]string, len(cols))
			for i, c := range cols {
				quotedCols[i] = sqlQ(c)
			}
			fmt.Fprintf(&buf, ",\n    UNIQUE (%s)", strings.Join(quotedCols, ", "))
		}

		fmt.Fprintf(&buf, "\n);\n")
	}

	return gen.File{
		Path:    path.Join(ns, "migrations", "001_initial.sql"),
		Content: buf.Bytes(),
	}, nil
}

// columnDefinition produces a SQL column definition for a field, including
// type, nullability, uniqueness, and CHECK constraints. All identifiers are
// double-quoted to prevent collisions with PostgreSQL reserved words.
func columnDefinition(f types.Field) string {
	qName := sqlQ(f.Name)

	var parts []string
	parts = append(parts, qName)
	parts = append(parts, fieldTypeToSQL(f.Type))

	// Nullability: computed and optional fields are nullable.
	if !f.Optional && !f.Computed {
		parts = append(parts, "NOT NULL")
	}

	if f.Unique {
		parts = append(parts, "UNIQUE")
	}

	if f.Default != nil {
		parts = append(parts, fmt.Sprintf("DEFAULT %s", sqlDefault(f)))
	}

	// Enum CHECK constraint.
	if f.Type == types.FieldTypeEnum && len(f.Values) > 0 {
		quoted := make([]string, len(f.Values))
		for i, v := range f.Values {
			quoted[i] = fmt.Sprintf("'%s'", sqlEscapeString(v))
		}
		parts = append(parts, fmt.Sprintf("CHECK (%s IN (%s))", qName, strings.Join(quoted, ", ")))
	}

	// String length constraints.
	if f.MinLength != nil {
		parts = append(parts, fmt.Sprintf("CHECK (length(%s) >= %d)", qName, *f.MinLength))
	}
	if f.MaxLength != nil {
		parts = append(parts, fmt.Sprintf("CHECK (length(%s) <= %d)", qName, *f.MaxLength))
	}

	// Pattern constraint.
	if f.Pattern != "" {
		parts = append(parts, fmt.Sprintf("CHECK (%s ~ '%s')", qName, sqlEscapeString(f.Pattern)))
	}

	// Numeric range constraints.
	if f.Min != nil {
		parts = append(parts, fmt.Sprintf("CHECK (%s >= %g)", qName, *f.Min))
	}
	if f.Max != nil {
		parts = append(parts, fmt.Sprintf("CHECK (%s <= %g)", qName, *f.Max))
	}

	return strings.Join(parts, " ")
}

// sqlDefault returns a SQL literal for a field's default value.
func sqlDefault(f types.Field) string {
	switch v := f.Default.(type) {
	case string:
		return fmt.Sprintf("'%s'", sqlEscapeString(v))
	case bool:
		if v {
			return "TRUE"
		}
		return "FALSE"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// sqlEscapeString escapes a string for use inside a SQL single-quoted literal
// by doubling any embedded single quotes. E.g. "it's" → "it''s".
func sqlEscapeString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// --- Helpers ---

// sqlQ wraps a SQL identifier in double quotes for PostgreSQL.
// This prevents collisions with reserved words (e.g. "order", "group").
// Embedded double quotes are doubled per PostgreSQL §4.1.1.
func sqlQ(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

// sqlQuotedColList produces a SQL column list with each identifier double-quoted.
// e.g. ["id", "email"] → `"id", "email"`
func sqlQuotedColList(cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = sqlQ(c)
	}
	return strings.Join(quoted, ", ")
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
// It handles the common suffixes that require more than just appending "s".
func pluralize(s string) string {
	if s == "" {
		return s
	}
	// Words ending in s, x, z, sh, ch → append "es"
	if strings.HasSuffix(s, "s") || strings.HasSuffix(s, "x") || strings.HasSuffix(s, "z") ||
		strings.HasSuffix(s, "sh") || strings.HasSuffix(s, "ch") {
		return s + "es"
	}
	// Words ending in consonant+y → replace y with "ies"
	if strings.HasSuffix(s, "y") && len(s) >= 2 {
		preceding := s[len(s)-2]
		// If the character before 'y' is not a vowel, it's consonant+y
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
			// Insert underscore when:
			// (a) preceded by a lowercase letter (e.g. "eS" in "AdapterStatus")
			// (b) preceded by uppercase AND followed by lowercase (end of acronym,
			//     e.g. "Ps" in "HTTPServer" → "http_server")
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

// fieldTypeToGo maps a types.FieldType to its Go type representation.
func fieldTypeToGo(ft types.FieldType) string {
	switch ft {
	case types.FieldTypeString, types.FieldTypeEnum, types.FieldTypeRef:
		return "string"
	case types.FieldTypeInt32:
		return "int32"
	case types.FieldTypeInt64:
		return "int64"
	case types.FieldTypeFloat:
		return "float32"
	case types.FieldTypeDouble:
		return "float64"
	case types.FieldTypeBool:
		return "bool"
	case types.FieldTypeBytes:
		return "[]byte"
	case types.FieldTypeTimestamp:
		return "time.Time"
	case types.FieldTypeJsonb:
		return "json.RawMessage"
	}
	return "any"
}

// fieldTypeToSQL maps a types.FieldType to its PostgreSQL column type.
func fieldTypeToSQL(ft types.FieldType) string {
	switch ft {
	case types.FieldTypeString, types.FieldTypeEnum, types.FieldTypeRef:
		return "TEXT"
	case types.FieldTypeInt32:
		return "INTEGER"
	case types.FieldTypeInt64:
		return "BIGINT"
	case types.FieldTypeFloat:
		return "REAL"
	case types.FieldTypeDouble:
		return "DOUBLE PRECISION"
	case types.FieldTypeBool:
		return "BOOLEAN"
	case types.FieldTypeBytes:
		return "BYTEA"
	case types.FieldTypeTimestamp:
		return "TIMESTAMPTZ"
	case types.FieldTypeJsonb:
		return "JSONB"
	}
	return "TEXT"
}

// quoteStringSlice produces a Go source literal for a string slice.
// e.g. ["a", "b"] → `"a", "b"`
func quoteStringSlice(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}

// --- Validation ---

// validateEntityUniqueness checks that no entity name appears more than once.
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

// validateCaseInsensitiveUniqueness checks that no two entity names produce
// the same derived table name (via toSnakeCase + pluralize).
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

// validateReservedNames checks that no entity name collides with
// generator-internal identifiers.
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

// validateRefTargets checks that every ref field's "to" attribute references
// an entity that exists in the entity list.
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

// validateFieldUniqueness checks that no entity has duplicate field names.
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

// validateFieldNameCharset checks that all field names match the safe identifier
// pattern [a-zA-Z_][a-zA-Z0-9_]*, which is valid across Go, SQL, and URL targets.
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

// validateEntityNameCharset checks that all entity names match the safe identifier
// pattern [a-zA-Z_][a-zA-Z0-9_]*, which is valid across Go, SQL, and URL targets.
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

// validateNoImplicitIDCollision checks that no entity declares a field whose
// PascalCase form is "ID", which would collide with the implicit ID primary key
// field the generator adds to every model struct and migration.
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

// validGoIdentifier checks that a string is a valid Go identifier: non-empty,
// starts with a letter or underscore, and contains only letters, digits, or
// underscores. This is used to validate post-transformation output.
var validGoIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validateDerivedFieldValidity checks that toPascalCase(f.Name) produces a
// valid Go identifier for every field. Names like "_" (produces "") or "_123"
// (produces "123") pass the input charset regex but yield invalid Go identifiers
// after transformation.
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

// validateDerivedFieldUniqueness checks that toPascalCase(f.Name) produces
// unique Go struct field names within each entity. Two raw field names that
// differ (e.g. "_name" and "name", or "foo_bar" and "fooBar") but map to the
// same PascalCase identifier cause duplicate struct fields and a compile error.
func validateDerivedFieldUniqueness(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		seen := make(map[string]string, len(e.Fields)) // PascalCase → raw name
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

// validateEnumValues checks that every field with type "enum" has a non-empty
// Values slice. An enum without values is semantically invalid — it degrades
// to a plain TEXT column with no CHECK constraint.
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
			// Check for duplicate enum values within the list.
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
