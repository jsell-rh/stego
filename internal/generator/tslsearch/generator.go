// Package tslsearch implements the tsl-search component Generator. It produces
// a search package that wraps the Tree Search Language library to parse
// ?search= expressions into parameterized SQL WHERE clauses, with per-entity
// field name validation and field-to-column mapping.
package tslsearch

import (
	"bytes"
	"fmt"
	"go/format"
	"path"
	"sort"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// Generator produces the tsl-search component's generated code.
type Generator struct{}

// Generate produces search helper files that wrap the TSL library for parsing
// search expressions into parameterized SQL WHERE clauses. It generates
// per-entity field mapping and validation.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if len(ctx.Entities) == 0 {
		return nil, nil, nil
	}

	ns := ctx.OutputNamespace
	if ns == "" {
		ns = "internal/search"
	}
	pkg := path.Base(ns)

	searchFile, err := generateSearchFile(ns, pkg, ctx.Entities)
	if err != nil {
		return nil, nil, fmt.Errorf("generating search.go: %w", err)
	}

	fieldMapFile, err := generateFieldMapFile(ns, pkg, ctx.Entities)
	if err != nil {
		return nil, nil, fmt.Errorf("generating field_map.go: %w", err)
	}

	files := []gen.File{searchFile, fieldMapFile}

	// The search engine is used directly by the postgres-adapter's store
	// (imported as a package utility), not constructed in main.go. So we only
	// need GoModRequires for the TSL and squirrel dependencies.
	wiring := &gen.Wiring{
		GoModRequires: map[string]string{
			"github.com/yaacov/tree-search-language": "v0.3.2",
			"github.com/Masterminds/squirrel":        "v1.5.4",
		},
	}

	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}

	return files, wiring, nil
}

// generateSearchFile produces search.go with the SearchEngine type and
// ParseSearch function that wraps TSL parsing, field validation, and SQL
// WHERE clause generation.
func generateSearchFile(ns, pkg string, entities []types.Entity) (gen.File, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", pkg)
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"fmt\"\n")
	fmt.Fprintf(&buf, "\t\"sort\"\n")
	fmt.Fprintf(&buf, "\t\"strings\"\n")
	fmt.Fprintf(&buf, "\n")
	fmt.Fprintf(&buf, "\tsq \"github.com/Masterminds/squirrel\"\n")
	fmt.Fprintf(&buf, "\t\"github.com/yaacov/tree-search-language/v5/pkg/tsl\"\n")
	fmt.Fprintf(&buf, "\twalker \"github.com/yaacov/tree-search-language/v5/pkg/walkers/sql\"\n")
	fmt.Fprintf(&buf, ")\n\n")

	// SearchResult type.
	fmt.Fprintf(&buf, "// SearchResult holds the parsed search expression as a parameterized SQL\n")
	fmt.Fprintf(&buf, "// WHERE clause with its bind parameters.\n")
	fmt.Fprintf(&buf, "type SearchResult struct {\n")
	fmt.Fprintf(&buf, "\t// Where is the SQL WHERE clause fragment (e.g. \"name = ?\").\n")
	fmt.Fprintf(&buf, "\tWhere string\n")
	fmt.Fprintf(&buf, "\t// Args are the bind parameters for the WHERE clause.\n")
	fmt.Fprintf(&buf, "\tArgs []interface{}\n")
	fmt.Fprintf(&buf, "}\n\n")

	// SearchEngine type.
	fmt.Fprintf(&buf, "// SearchEngine provides TSL search expression parsing and SQL WHERE clause\n")
	fmt.Fprintf(&buf, "// generation for list operations. It validates field names against entity\n")
	fmt.Fprintf(&buf, "// definitions and maps entity field names to SQL column names.\n")
	fmt.Fprintf(&buf, "type SearchEngine struct{}\n\n")

	// Constructor.
	fmt.Fprintf(&buf, "// NewSearchEngine creates a SearchEngine.\n")
	fmt.Fprintf(&buf, "func NewSearchEngine() *SearchEngine {\n")
	fmt.Fprintf(&buf, "\treturn &SearchEngine{}\n")
	fmt.Fprintf(&buf, "}\n\n")

	// ParseSearch function.
	fmt.Fprintf(&buf, "// ParseSearch parses a TSL search expression for the given entity, validates\n")
	fmt.Fprintf(&buf, "// field names against the entity's field definitions, and returns a\n")
	fmt.Fprintf(&buf, "// SearchResult with a parameterized SQL WHERE clause. Returns an error if\n")
	fmt.Fprintf(&buf, "// the expression is syntactically invalid or references unknown fields.\n")
	fmt.Fprintf(&buf, "func (s *SearchEngine) ParseSearch(entity string, expr string) (*SearchResult, error) {\n")
	fmt.Fprintf(&buf, "\tif expr == \"\" {\n")
	fmt.Fprintf(&buf, "\t\treturn nil, nil\n")
	fmt.Fprintf(&buf, "\t}\n\n")

	fmt.Fprintf(&buf, "\t// Get field mapping for the entity.\n")
	fmt.Fprintf(&buf, "\tfieldMap, ok := EntityFieldMaps[entity]\n")
	fmt.Fprintf(&buf, "\tif !ok {\n")
	fmt.Fprintf(&buf, "\t\treturn nil, fmt.Errorf(\"unknown entity for search: %%s\", entity)\n")
	fmt.Fprintf(&buf, "\t}\n\n")

	fmt.Fprintf(&buf, "\t// Parse the TSL expression into an AST.\n")
	fmt.Fprintf(&buf, "\ttree, err := tsl.ParseTSL(expr)\n")
	fmt.Fprintf(&buf, "\tif err != nil {\n")
	fmt.Fprintf(&buf, "\t\treturn nil, fmt.Errorf(\"invalid search expression: %%w\", err)\n")
	fmt.Fprintf(&buf, "\t}\n\n")

	fmt.Fprintf(&buf, "\t// Validate field names in the AST against entity fields.\n")
	fmt.Fprintf(&buf, "\tif err := validateSearchFields(tree, fieldMap); err != nil {\n")
	fmt.Fprintf(&buf, "\t\treturn nil, err\n")
	fmt.Fprintf(&buf, "\t}\n\n")

	fmt.Fprintf(&buf, "\t// Map field names to column names in the AST.\n")
	fmt.Fprintf(&buf, "\tmappedTree := mapFieldNames(tree, fieldMap)\n\n")

	fmt.Fprintf(&buf, "\t// Convert TSL AST to SQL WHERE clause using the TSL SQL walker.\n")
	fmt.Fprintf(&buf, "\twhere, err := walker.Walk(mappedTree)\n")
	fmt.Fprintf(&buf, "\tif err != nil {\n")
	fmt.Fprintf(&buf, "\t\treturn nil, fmt.Errorf(\"converting search to SQL: %%w\", err)\n")
	fmt.Fprintf(&buf, "\t}\n\n")

	fmt.Fprintf(&buf, "\t// Use squirrel to parameterize the WHERE clause for SQL injection prevention.\n")
	fmt.Fprintf(&buf, "\tparamWhere, args, err := sq.Expr(where).ToSql()\n")
	fmt.Fprintf(&buf, "\tif err != nil {\n")
	fmt.Fprintf(&buf, "\t\treturn nil, fmt.Errorf(\"parameterizing search clause: %%w\", err)\n")
	fmt.Fprintf(&buf, "\t}\n\n")

	fmt.Fprintf(&buf, "\treturn &SearchResult{Where: paramWhere, Args: args}, nil\n")
	fmt.Fprintf(&buf, "}\n\n")

	// ValidFields function — returns valid field names for an entity.
	fmt.Fprintf(&buf, "// ValidFields returns the sorted list of valid search field names for an entity.\n")
	fmt.Fprintf(&buf, "func (s *SearchEngine) ValidFields(entity string) []string {\n")
	fmt.Fprintf(&buf, "\tfieldMap, ok := EntityFieldMaps[entity]\n")
	fmt.Fprintf(&buf, "\tif !ok {\n")
	fmt.Fprintf(&buf, "\t\treturn nil\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\tfields := make([]string, 0, len(fieldMap))\n")
	fmt.Fprintf(&buf, "\tfor k := range fieldMap {\n")
	fmt.Fprintf(&buf, "\t\tfields = append(fields, k)\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\tsort.Strings(fields)\n")
	fmt.Fprintf(&buf, "\treturn fields\n")
	fmt.Fprintf(&buf, "}\n\n")

	// validateSearchFields walks the TSL tree and rejects unknown field names.
	fmt.Fprintf(&buf, "// validateSearchFields walks the TSL AST and rejects references to unknown fields.\n")
	fmt.Fprintf(&buf, "func validateSearchFields(node tsl.Node, fieldMap map[string]string) error {\n")
	fmt.Fprintf(&buf, "\tswitch node.Func {\n")
	fmt.Fprintf(&buf, "\tcase tsl.IdentOp:\n")
	fmt.Fprintf(&buf, "\t\tname, ok := node.Left.(string)\n")
	fmt.Fprintf(&buf, "\t\tif ok {\n")
	fmt.Fprintf(&buf, "\t\t\tif _, exists := fieldMap[name]; !exists {\n")
	fmt.Fprintf(&buf, "\t\t\t\tvalid := make([]string, 0, len(fieldMap))\n")
	fmt.Fprintf(&buf, "\t\t\t\tfor k := range fieldMap {\n")
	fmt.Fprintf(&buf, "\t\t\t\t\tvalid = append(valid, k)\n")
	fmt.Fprintf(&buf, "\t\t\t\t}\n")
	fmt.Fprintf(&buf, "\t\t\t\tsort.Strings(valid)\n")
	fmt.Fprintf(&buf, "\t\t\t\treturn fmt.Errorf(\"unknown search field %%q; valid fields: %%s\", name, strings.Join(valid, \", \"))\n")
	fmt.Fprintf(&buf, "\t\t\t}\n")
	fmt.Fprintf(&buf, "\t\t}\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\tif nodes, ok := node.Right.([]tsl.Node); ok {\n")
	fmt.Fprintf(&buf, "\t\tfor _, child := range nodes {\n")
	fmt.Fprintf(&buf, "\t\t\tif err := validateSearchFields(child, fieldMap); err != nil {\n")
	fmt.Fprintf(&buf, "\t\t\t\treturn err\n")
	fmt.Fprintf(&buf, "\t\t\t}\n")
	fmt.Fprintf(&buf, "\t\t}\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\treturn nil\n")
	fmt.Fprintf(&buf, "}\n\n")

	// mapFieldNames replaces field names in the TSL AST with column names.
	fmt.Fprintf(&buf, "// mapFieldNames creates a copy of the TSL tree with field names replaced\n")
	fmt.Fprintf(&buf, "// by their SQL column name equivalents from the field map.\n")
	fmt.Fprintf(&buf, "func mapFieldNames(node tsl.Node, fieldMap map[string]string) tsl.Node {\n")
	fmt.Fprintf(&buf, "\tresult := node\n")
	fmt.Fprintf(&buf, "\tif node.Func == tsl.IdentOp {\n")
	fmt.Fprintf(&buf, "\t\tname, ok := node.Left.(string)\n")
	fmt.Fprintf(&buf, "\t\tif ok {\n")
	fmt.Fprintf(&buf, "\t\t\tif col, exists := fieldMap[name]; exists {\n")
	fmt.Fprintf(&buf, "\t\t\t\tresult.Left = col\n")
	fmt.Fprintf(&buf, "\t\t\t}\n")
	fmt.Fprintf(&buf, "\t\t}\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\tif nodes, ok := node.Right.([]tsl.Node); ok {\n")
	fmt.Fprintf(&buf, "\t\tmapped := make([]tsl.Node, len(nodes))\n")
	fmt.Fprintf(&buf, "\t\tfor i, child := range nodes {\n")
	fmt.Fprintf(&buf, "\t\t\tmapped[i] = mapFieldNames(child, fieldMap)\n")
	fmt.Fprintf(&buf, "\t\t}\n")
	fmt.Fprintf(&buf, "\t\tresult.Right = mapped\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\treturn result\n")
	fmt.Fprintf(&buf, "}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting search.go: %w\nraw:\n%s", err, buf.String())
	}

	return gen.File{
		Path:    path.Join(ns, "search.go"),
		Content: formatted,
	}, nil
}

// generateFieldMapFile produces field_map.go with per-entity field-to-column
// mappings used by the search engine for field name validation and SQL
// column resolution.
func generateFieldMapFile(ns, pkg string, entities []types.Entity) (gen.File, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", pkg)

	fmt.Fprintf(&buf, "// EntityFieldMaps maps entity names to their field-to-column name mappings.\n")
	fmt.Fprintf(&buf, "// Field names from search expressions are validated against these maps;\n")
	fmt.Fprintf(&buf, "// unknown fields are rejected. The mapped values are SQL column names.\n")
	fmt.Fprintf(&buf, "// The resolve_field slot allows fills to customize specific field mappings\n")
	fmt.Fprintf(&buf, "// (e.g. JSONB path queries, label queries, or cross-entity JOINs).\n")
	fmt.Fprintf(&buf, "var EntityFieldMaps = map[string]map[string]string{\n")

	// Sort entities for deterministic output.
	sorted := make([]types.Entity, len(entities))
	copy(sorted, entities)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	for _, e := range sorted {
		fmt.Fprintf(&buf, "\t%q: {\n", e.Name)

		// Sort fields for deterministic output.
		fields := make([]types.Field, len(e.Fields))
		copy(fields, e.Fields)
		sort.Slice(fields, func(i, j int) bool {
			return fields[i].Name < fields[j].Name
		})

		for _, f := range fields {
			// Default mapping: entity field name maps to the same SQL column name.
			// The resolve_field slot allows fills to override this for JSONB paths,
			// label queries, or cross-entity JOINs.
			fmt.Fprintf(&buf, "\t\t%q: %q,\n", f.Name, f.Name)
		}
		fmt.Fprintf(&buf, "\t},\n")
	}

	fmt.Fprintf(&buf, "}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting field_map.go: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "field_map.go"),
		Content: formatted,
	}, nil
}
