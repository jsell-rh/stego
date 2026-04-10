package tslsearch

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func testContext() gen.Context {
	return gen.Context{
		Entities: []types.Entity{
			{
				Name: "User",
				Fields: []types.Field{
					{Name: "email", Type: "string"},
					{Name: "name", Type: "string"},
					{Name: "role", Type: "enum", Values: []string{"admin", "member"}},
					{Name: "org_id", Type: "ref", To: "Organization"},
				},
			},
			{
				Name: "Organization",
				Fields: []types.Field{
					{Name: "name", Type: "string"},
					{Name: "region", Type: "string"},
				},
			},
		},
		OutputNamespace: "internal/search",
	}
}

func findFile(t *testing.T, files []gen.File, path string) string {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return string(f.Bytes())
		}
	}
	t.Fatalf("file %q not found in generated files", path)
	return ""
}

func TestGenerate_ProducesSearchAndFieldMapFiles(t *testing.T) {
	g := &Generator{}
	files, wiring, err := g.Generate(testContext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// Verify file paths.
	paths := map[string]bool{}
	for _, f := range files {
		paths[f.Path] = true
	}
	if !paths["internal/search/search.go"] {
		t.Error("missing internal/search/search.go")
	}
	if !paths["internal/search/field_map.go"] {
		t.Error("missing internal/search/field_map.go")
	}

	// Verify wiring — search engine is used directly by the postgres-adapter's
	// store (as a package utility), not constructed in main.go. So wiring has
	// no imports or constructors, only GoModRequires.
	if wiring == nil {
		t.Fatal("wiring must not be nil")
	}
	if len(wiring.Imports) != 0 {
		t.Errorf("wiring.Imports = %v, want empty (search engine is not wired in main.go)", wiring.Imports)
	}
	if len(wiring.Constructors) != 0 {
		t.Errorf("wiring.Constructors = %v, want empty (search engine is not wired in main.go)", wiring.Constructors)
	}
}

func TestGenerate_GoModDependencies(t *testing.T) {
	g := &Generator{}
	_, wiring, err := g.Generate(testContext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wiring.GoModRequires == nil {
		t.Fatal("GoModRequires must not be nil")
	}

	tslDep := "github.com/yaacov/tree-search-language"
	if _, ok := wiring.GoModRequires[tslDep]; !ok {
		t.Errorf("GoModRequires missing TSL library dependency %q", tslDep)
	}

	sqDep := "github.com/Masterminds/squirrel"
	if _, ok := wiring.GoModRequires[sqDep]; !ok {
		t.Errorf("GoModRequires missing squirrel dependency %q", sqDep)
	}
}

func TestGenerate_SearchFileContainsSearchEngine(t *testing.T) {
	g := &Generator{}
	files, _, err := g.Generate(testContext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFile(t, files, "internal/search/search.go")

	// Must contain SearchEngine type.
	if !strings.Contains(content, "type SearchEngine struct") {
		t.Error("search.go must contain SearchEngine type")
	}

	// Must contain NewSearchEngine constructor.
	if !strings.Contains(content, "func NewSearchEngine()") {
		t.Error("search.go must contain NewSearchEngine constructor")
	}

	// Must contain ParseSearch method.
	if !strings.Contains(content, "func (s *SearchEngine) ParseSearch(") {
		t.Error("search.go must contain ParseSearch method")
	}

	// Must contain SearchResult type.
	if !strings.Contains(content, "type SearchResult struct") {
		t.Error("search.go must contain SearchResult type")
	}

	// Must import TSL library.
	if !strings.Contains(content, "tree-search-language") {
		t.Error("search.go must import TSL library")
	}

	// Must import squirrel.
	if !strings.Contains(content, "squirrel") {
		t.Error("search.go must import squirrel")
	}

	// Must contain field validation.
	if !strings.Contains(content, "validateSearchFields") {
		t.Error("search.go must contain validateSearchFields function")
	}

	// Must contain field name mapping.
	if !strings.Contains(content, "mapFieldNames") {
		t.Error("search.go must contain mapFieldNames function")
	}

	// Must contain ValidFields method.
	if !strings.Contains(content, "func (s *SearchEngine) ValidFields(") {
		t.Error("search.go must contain ValidFields method")
	}
}

func TestGenerate_FieldMapContainsEntityMappings(t *testing.T) {
	g := &Generator{}
	files, _, err := g.Generate(testContext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFile(t, files, "internal/search/field_map.go")

	// Must contain EntityFieldMaps variable.
	if !strings.Contains(content, "EntityFieldMaps") {
		t.Error("field_map.go must contain EntityFieldMaps variable")
	}

	// Must contain User entity mapping.
	if !strings.Contains(content, `"User"`) {
		t.Error("field_map.go must contain User entity mapping")
	}

	// Must contain Organization entity mapping.
	if !strings.Contains(content, `"Organization"`) {
		t.Error("field_map.go must contain Organization entity mapping")
	}

	// Must contain User field mappings. go/format uses tabs for indentation
	// so we check for the formatted pattern.
	if !strings.Contains(content, `"email":`) {
		t.Error("field_map.go must map email field")
	}
	if !strings.Contains(content, `"role":`) {
		t.Error("field_map.go must map role field")
	}
	if !strings.Contains(content, `"org_id":`) {
		t.Error("field_map.go must map org_id field")
	}
}

func TestGenerate_EmptyEntities(t *testing.T) {
	g := &Generator{}
	files, wiring, err := g.Generate(gen.Context{Entities: nil})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if files != nil {
		t.Error("expected nil files for empty entities")
	}
	if wiring != nil {
		t.Error("expected nil wiring for empty entities")
	}
}

func TestGenerate_DefaultNamespace(t *testing.T) {
	g := &Generator{}
	ctx := testContext()
	ctx.OutputNamespace = ""
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With empty namespace, defaults to internal/search.
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "internal/search/") {
			t.Errorf("file %q should be under internal/search/", f.Path)
		}
	}
}

func TestGenerate_NamespaceValidation(t *testing.T) {
	g := &Generator{}
	ctx := testContext()
	ctx.OutputNamespace = "pkg/search"
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		if !strings.HasPrefix(f.Path, "pkg/search/") {
			t.Errorf("file %q should be under pkg/search/", f.Path)
		}
	}
}

func TestGenerate_SearchResultFields(t *testing.T) {
	g := &Generator{}
	files, _, err := g.Generate(testContext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFile(t, files, "internal/search/search.go")

	// SearchResult must have Where and Args fields.
	if !strings.Contains(content, "Where string") {
		t.Error("SearchResult must have Where field")
	}
	if !strings.Contains(content, "Args []interface{}") {
		t.Error("SearchResult must have Args field")
	}
}

func TestGenerate_DeterministicOutput(t *testing.T) {
	g := &Generator{}
	ctx := testContext()

	files1, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}

	files2, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}

	if len(files1) != len(files2) {
		t.Fatal("non-deterministic file count")
	}

	for i := range files1 {
		if files1[i].Path != files2[i].Path {
			t.Errorf("file %d path differs: %q vs %q", i, files1[i].Path, files2[i].Path)
		}
		if string(files1[i].Content) != string(files2[i].Content) {
			t.Errorf("file %d content differs for %q", i, files1[i].Path)
		}
	}
}

func TestGenerate_GeneratedHeaderOnGoFiles(t *testing.T) {
	g := &Generator{}
	files, _, err := g.Generate(testContext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		content := string(f.Bytes())
		if !strings.HasPrefix(content, gen.Header) {
			t.Errorf("file %q missing generated header", f.Path)
		}
	}
}

func TestGenerate_FieldMapDeterministicOrder(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Entities: []types.Entity{
			{
				Name: "Zebra",
				Fields: []types.Field{
					{Name: "zoo", Type: "string"},
					{Name: "age", Type: "int32"},
				},
			},
			{
				Name: "Apple",
				Fields: []types.Field{
					{Name: "variety", Type: "string"},
					{Name: "color", Type: "string"},
				},
			},
		},
		OutputNamespace: "internal/search",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFile(t, files, "internal/search/field_map.go")

	// Apple must appear before Zebra (alphabetical entity order).
	appleIdx := strings.Index(content, `"Apple"`)
	zebraIdx := strings.Index(content, `"Zebra"`)
	if appleIdx < 0 || zebraIdx < 0 {
		t.Fatal("both Apple and Zebra must appear in field map")
	}
	if appleIdx > zebraIdx {
		t.Error("entities must be in alphabetical order (Apple before Zebra)")
	}

	// Within Apple, "color" must appear before "variety".
	colorIdx := strings.Index(content, `"color"`)
	varietyIdx := strings.Index(content, `"variety"`)
	if colorIdx < 0 || varietyIdx < 0 {
		t.Fatal("both color and variety must appear in field map")
	}
	if colorIdx > varietyIdx {
		t.Error("fields must be in alphabetical order within entity (color before variety)")
	}
}
