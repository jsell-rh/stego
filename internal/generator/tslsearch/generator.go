// Package tslsearch implements the tsl-search component Generator. It produces
// a search package that wraps the Tree Search Language library to parse
// ?search= expressions into parameterized SQL WHERE clauses, with per-entity
// field name validation and field-to-column mapping.
package tslsearch

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// Generator produces the tsl-search component's generated code.
type Generator struct{}

// ValidateContext rejects fields that conflict with search metadata.
func (*Generator) ValidateContext(ctx gen.Context) error {
	if len(ctx.Entities) == 0 {
		return nil
	}

	for _, entity := range ctx.Entities {
		for _, field := range entity.Fields {
			switch field.Name {
			case "id", "created_time", "updated_time", "created_at", "updated_at":
				return fmt.Errorf("search field %q conflicts with common metadata", field.Name)
			}
		}
	}

	return nil
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
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
	// need the pinned TSL parser dependency.
	wiring := &gen.Wiring{
		GoModRequires: map[string]string{
			"github.com/yaacov/tree-search-language/v5": "v5.2.12",
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
//
//go:embed search.go.tmpl
var searchSource string

func generateSearchFile(ns, pkg string, entities []types.Entity) (gen.File, error) {
	code, err := format.Source([]byte(strings.Replace(searchSource, "package search", "package "+pkg, 1)))
	if err != nil {
		return gen.File{}, err
	}
	return gen.File{Path: path.Join(ns, "search.go"), Content: code}, nil
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
	fmt.Fprintf(&buf, "// Only declared fields and common metadata aliases are available.\n")
	fmt.Fprintf(&buf, "var EntityFieldMaps = map[string]map[string]string{\n")

	// Sort entities for deterministic output.
	sorted := make([]types.Entity, len(entities))
	copy(sorted, entities)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	for _, e := range sorted {
		fmt.Fprintf(&buf, "\t%q: {\n", e.Name)
		for _, name := range []string{"id", "created_time", "updated_time"} {
			fmt.Fprintf(&buf, "\t\t%q: %q,\n", name, name)
		}
		fmt.Fprintf(&buf, "\t\t%q: %q,\n", "created_at", "created_time")
		fmt.Fprintf(&buf, "\t\t%q: %q,\n", "updated_at", "updated_time")

		// Sort fields for deterministic output.
		fields := make([]types.Field, len(e.Fields))
		copy(fields, e.Fields)
		sort.Slice(fields, func(i, j int) bool {
			return fields[i].Name < fields[j].Name
		})

		for _, f := range fields {
			// Default mapping: entity field name maps to the same SQL column name.
			// Application aliases must not change the declared column type.
			fmt.Fprintf(&buf, "\t\t%q: %q,\n", f.Name, f.Name)
		}
		fmt.Fprintf(&buf, "\t},\n")
	}

	fmt.Fprintf(&buf, "}\n")

	fmt.Fprintln(&buf, "var entityFieldTypes = map[string]map[string]string{")
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%q: {\n", e.Name)
		for _, field := range []string{"id", "created_time", "updated_time", "created_at", "updated_at"} {
			kind := "timestamp"
			if field == "id" {
				kind = "string"
			}
			fmt.Fprintf(&buf, "%q:%q,\n", field, kind)
		}
		for _, field := range e.Fields {
			fmt.Fprintf(&buf, "%q:%q,\n", field.Name, string(field.Type))
		}
		fmt.Fprintln(&buf, "},")
	}
	fmt.Fprintln(&buf, "}")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting field_map.go: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "field_map.go"),
		Content: formatted,
	}, nil
}
