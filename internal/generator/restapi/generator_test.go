package restapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stego-project/stego/internal/gen"
	"github.com/stego-project/stego/internal/types"
)

// basicContext returns a gen.Context for a simple User entity with CRUD operations.
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
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList},
			},
		},
		OutputNamespace: "internal/api",
	}
}

func TestGenerate_EmptyExpose(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{OutputNamespace: "internal/api"}
	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if files != nil {
		t.Errorf("expected nil files, got %d", len(files))
	}
	if wiring != nil {
		t.Errorf("expected nil wiring, got %+v", wiring)
	}
}

func TestGenerate_BasicCRUD(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expect 3 files: handler_user.go, router.go, openapi.json
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}

	// Verify file paths.
	expectedPaths := map[string]bool{
		"internal/api/handler_user.go": true,
		"internal/api/router.go":       true,
		"internal/api/openapi.json":    true,
	}
	for _, f := range files {
		if !expectedPaths[f.Path] {
			t.Errorf("unexpected file path: %s", f.Path)
		}
	}

	// Verify wiring.
	if wiring == nil {
		t.Fatal("expected wiring, got nil")
	}
	if len(wiring.Imports) != 1 || wiring.Imports[0] != "internal/api" {
		t.Errorf("unexpected imports: %v", wiring.Imports)
	}
	if len(wiring.Constructors) != 1 {
		t.Fatalf("expected 1 constructor, got %d", len(wiring.Constructors))
	}
	if !strings.Contains(wiring.Constructors[0], "NewUserHandler") {
		t.Errorf("constructor should reference NewUserHandler, got: %s", wiring.Constructors[0])
	}
	if len(wiring.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(wiring.Routes))
	}
	if !strings.Contains(wiring.Routes[0], "/users") {
		t.Errorf("route should contain /users, got: %s", wiring.Routes[0])
	}
}

func TestGenerate_HandlerContainsCRUDMethods(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handlerContent := findFileContent(t, files, "internal/api/handler_user.go")

	for _, method := range []string{"create", "read", "update", "delete", "list"} {
		if !strings.Contains(handlerContent, "func (h *UserHandler) "+method+"(") {
			t.Errorf("handler missing %s method", method)
		}
	}

	if !strings.Contains(handlerContent, "type UserHandler struct") {
		t.Error("handler missing UserHandler struct")
	}
	if !strings.Contains(handlerContent, "func NewUserHandler(") {
		t.Error("handler missing NewUserHandler constructor")
	}
	if !strings.Contains(handlerContent, "func (h *UserHandler) ServeHTTP(") {
		t.Error("handler missing ServeHTTP dispatcher")
	}
}

func TestGenerate_RouterContainsStorageInterface(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	router := findFileContent(t, files, "internal/api/router.go")

	if !strings.Contains(router, "type Storage interface") {
		t.Error("router missing Storage interface")
	}
	if !strings.Contains(router, "func NewRouter(") {
		t.Error("router missing NewRouter function")
	}
	if !strings.Contains(router, "auth") {
		t.Error("router should reference auth middleware parameter")
	}
}

func TestGenerate_NestedRouting(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "NodePool", Fields: []types.Field{{Name: "cluster_id", Type: types.FieldTypeRef, To: "Cluster"}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Cluster", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList}},
			{
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList},
				Parent:     "Cluster",
			},
		},
		OutputNamespace: "internal/api",
	}

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// NodePool handler should verify parent Cluster exists.
	npHandler := findFileContent(t, files, "internal/api/handler_nodepool.go")
	if !strings.Contains(npHandler, "Verify parent Cluster exists") {
		t.Error("nested handler missing parent existence verification comment")
	}
	if !strings.Contains(npHandler, `h.store.Exists("Cluster"`) {
		t.Error("nested handler missing parent Exists check")
	}
	if !strings.Contains(npHandler, `"Cluster not found"`) {
		t.Error("nested handler missing parent not-found error")
	}

	// Route should contain nested path.
	if wiring == nil {
		t.Fatal("expected wiring")
	}
	foundNestedRoute := false
	for _, r := range wiring.Routes {
		if strings.Contains(r, "cluster_id") && strings.Contains(r, "nodepools") {
			foundNestedRoute = true
		}
	}
	if !foundNestedRoute {
		t.Errorf("expected nested route with cluster_id and nodepools, got routes: %v", wiring.Routes)
	}
}

func TestGenerate_NestedRoutingWithPathPrefix(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "NodePool", Fields: []types.Field{{Name: "cluster_id", Type: types.FieldTypeRef, To: "Cluster"}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Cluster", Operations: []types.Operation{types.OpRead}, PathPrefix: "/clusters"},
			{
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpList},
				Parent:     "Cluster",
				PathPrefix: "/clusters/{cluster_id}/nodepools",
			},
		},
		OutputNamespace: "internal/api",
	}

	_, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wiring == nil {
		t.Fatal("expected wiring")
	}

	// When PathPrefix is explicitly set, it should be used directly.
	foundPath := false
	for _, r := range wiring.Routes {
		if strings.Contains(r, "/clusters/{cluster_id}/nodepools") {
			foundPath = true
		}
	}
	if !foundPath {
		t.Errorf("expected explicit path prefix in routes, got: %v", wiring.Routes)
	}
}

func TestGenerate_ScopeFiltering(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      "org_id",
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_user.go")
	if !strings.Contains(handler, "org_id") {
		t.Error("handler list method should reference scope field org_id")
	}
	if !strings.Contains(handler, "scopeValue") {
		t.Error("handler list method should extract scope value")
	}
}

func TestGenerate_UpsertOperation(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "AdapterStatus", Fields: []types.Field{
				{Name: "resource_type", Type: types.FieldTypeString},
				{Name: "resource_id", Type: types.FieldTypeString},
				{Name: "adapter", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:      "AdapterStatus",
				Operations:  []types.Operation{types.OpList, types.OpUpsert},
				UpsertKey:   []string{"resource_type", "resource_id", "adapter"},
				Concurrency: types.ConcurrencyOptimistic,
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_adapterstatus.go")
	if !strings.Contains(handler, "func (h *AdapterStatusHandler) upsert(") {
		t.Error("handler missing upsert method")
	}
	if !strings.Contains(handler, `"resource_type"`) {
		t.Error("upsert handler missing upsert key field resource_type")
	}
	if !strings.Contains(handler, `"resource_id"`) {
		t.Error("upsert handler missing upsert key field resource_id")
	}
	if !strings.Contains(handler, `"adapter"`) {
		t.Error("upsert handler missing upsert key field adapter")
	}
	if !strings.Contains(handler, `"optimistic"`) {
		t.Error("upsert handler missing optimistic concurrency mode")
	}
}

func TestGenerate_OpenAPISpec(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiContent := findFileContent(t, files, "internal/api/openapi.json")

	// Parse the OpenAPI spec.
	var spec map[string]any
	if err := json.Unmarshal([]byte(openapiContent), &spec); err != nil {
		t.Fatalf("failed to parse openapi spec: %v", err)
	}

	// Verify basic structure.
	if spec["openapi"] != "3.0.3" {
		t.Errorf("unexpected openapi version: %v", spec["openapi"])
	}

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("missing paths in openapi spec")
	}
	if _, ok := paths["/users"]; !ok {
		t.Error("missing /users path in openapi spec")
	}
	if _, ok := paths["/users/{id}"]; !ok {
		t.Error("missing /users/{id} path in openapi spec")
	}

	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatal("missing components in openapi spec")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("missing schemas in openapi spec")
	}
	userSchema, ok := schemas["User"].(map[string]any)
	if !ok {
		t.Fatal("missing User schema in openapi spec")
	}
	props, ok := userSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing User schema properties")
	}
	if _, ok := props["email"]; !ok {
		t.Error("missing email property in User schema")
	}
	if _, ok := props["role"]; !ok {
		t.Error("missing role property in User schema")
	}
}

func TestGenerate_OpenAPIFieldTypes(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Thing", Fields: []types.Field{
				{Name: "s", Type: types.FieldTypeString},
				{Name: "i32", Type: types.FieldTypeInt32},
				{Name: "i64", Type: types.FieldTypeInt64},
				{Name: "f", Type: types.FieldTypeFloat},
				{Name: "d", Type: types.FieldTypeDouble},
				{Name: "b", Type: types.FieldTypeBool},
				{Name: "raw", Type: types.FieldTypeBytes},
				{Name: "ts", Type: types.FieldTypeTimestamp},
				{Name: "e", Type: types.FieldTypeEnum, Values: []string{"x", "y"}},
				{Name: "r", Type: types.FieldTypeRef, To: "Other"},
				{Name: "j", Type: types.FieldTypeJsonb},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Thing", Operations: []types.Operation{types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/api/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(content), &spec); err != nil {
		t.Fatalf("parse error: %v", err)
	}

	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	thingSchema := schemas["Thing"].(map[string]any)
	props := thingSchema["properties"].(map[string]any)

	tests := []struct {
		field    string
		wantType string
	}{
		{"s", "string"},
		{"i32", "integer"},
		{"i64", "integer"},
		{"f", "number"},
		{"d", "number"},
		{"b", "boolean"},
		{"raw", "string"},
		{"ts", "string"},
		{"e", "string"},
		{"r", "string"},
		{"j", "object"},
	}

	for _, tt := range tests {
		p := props[tt.field].(map[string]any)
		if p["type"] != tt.wantType {
			t.Errorf("field %s: expected type %q, got %q", tt.field, tt.wantType, p["type"])
		}
	}
}

func TestGenerate_UnknownEntity(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Entities: []types.Entity{},
		Expose: []types.ExposeBlock{
			{Entity: "Missing", Operations: []types.Operation{types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for unknown entity")
	}
	if !strings.Contains(err.Error(), "Missing") {
		t.Errorf("error should mention unknown entity name, got: %v", err)
	}
}

func TestGenerate_AllFilesInNamespace(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		if !strings.HasPrefix(f.Path, "internal/api/") {
			t.Errorf("file %q is outside namespace internal/api", f.Path)
		}
	}
}

func TestGenerate_FilesBytesIncludesHeader(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		content := string(f.Bytes())
		if !strings.HasPrefix(content, gen.Header) {
			t.Errorf("file %s missing generated header", f.Path)
		}
	}
}

func TestGenerate_MultipleEntities(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Organization", Operations: []types.Operation{types.OpCreate, types.OpRead}},
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpList},
				Scope:      "org_id",
				Parent:     "Organization",
			},
		},
		OutputNamespace: "internal/api",
	}

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have handler_organization.go, handler_user.go, router.go, openapi.json
	if len(files) != 4 {
		t.Fatalf("expected 4 files, got %d", len(files))
	}

	expectedPaths := map[string]bool{
		"internal/api/handler_organization.go": true,
		"internal/api/handler_user.go":         true,
		"internal/api/router.go":               true,
		"internal/api/openapi.json":            true,
	}
	for _, f := range files {
		if !expectedPaths[f.Path] {
			t.Errorf("unexpected file path: %s", f.Path)
		}
	}

	// Verify wiring has constructors for both entities.
	if wiring == nil {
		t.Fatal("expected wiring")
	}
	if len(wiring.Constructors) != 2 {
		t.Fatalf("expected 2 constructors, got %d", len(wiring.Constructors))
	}
}

func TestGenerate_ListOnlyGETRoute(t *testing.T) {
	// When only list is exposed (no read), GET should map to list.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Event", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Event", Operations: []types.Operation{types.OpList}},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_event.go")
	if !strings.Contains(handler, "func (h *EventHandler) list(") {
		t.Error("handler missing list method")
	}
	// Should not have a read method.
	if strings.Contains(handler, "func (h *EventHandler) read(") {
		t.Error("handler should not have read method when only list is exposed")
	}
}

func TestGenerate_GeneratedGoCodeCompiles(t *testing.T) {
	// Verify Go files are valid Go by checking they contain package declaration.
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		content := string(f.Content)
		if !strings.Contains(content, "package api") {
			t.Errorf("file %s missing package declaration", f.Path)
		}
	}
}

func TestEntityBasePath_Default(t *testing.T) {
	eb := types.ExposeBlock{Entity: "User"}
	got := entityBasePath(eb, nil)
	if got != "/users" {
		t.Errorf("expected /users, got %s", got)
	}
}

func TestEntityBasePath_WithPathPrefix(t *testing.T) {
	eb := types.ExposeBlock{Entity: "User", PathPrefix: "/api/v1/users"}
	got := entityBasePath(eb, nil)
	if got != "/api/v1/users" {
		t.Errorf("expected /api/v1/users, got %s", got)
	}
}

func TestEntityBasePath_Nested(t *testing.T) {
	exposeMap := map[string]types.ExposeBlock{
		"Cluster": {Entity: "Cluster"},
	}
	eb := types.ExposeBlock{Entity: "NodePool", Parent: "Cluster"}
	got := entityBasePath(eb, exposeMap)
	if got != "/clusters/{cluster_id}/nodepools" {
		t.Errorf("expected /clusters/{cluster_id}/nodepools, got %s", got)
	}
}

// findFileContent finds a file by path in the file list and returns its content as string.
func findFileContent(t *testing.T, files []gen.File, path string) string {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return string(f.Content)
		}
	}
	t.Fatalf("file %s not found in output", path)
	return ""
}
