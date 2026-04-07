package restapi

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/slot"
	"github.com/jsell-rh/stego/internal/types"
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
	if len(wiring.Routes) == 0 {
		t.Fatal("expected routes, got none")
	}
	foundUsers := false
	for _, r := range wiring.Routes {
		if strings.Contains(r, "/users") {
			foundUsers = true
		}
	}
	if !foundUsers {
		t.Errorf("expected a route containing /users, got: %v", wiring.Routes)
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
		if !strings.Contains(handlerContent, "func (h *UserHandler) "+strings.ToUpper(method[:1])+method[1:]+"(") {
			t.Errorf("handler missing %s method", method)
		}
	}

	if !strings.Contains(handlerContent, "type UserHandler struct") {
		t.Error("handler missing UserHandler struct")
	}
	if !strings.Contains(handlerContent, "func NewUserHandler(") {
		t.Error("handler missing NewUserHandler constructor")
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

func TestGenerate_RouterEntityStructsHaveFields(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	router := findFileContent(t, files, "internal/api/router.go")

	// Entity struct must have real fields, not be empty.
	if !strings.Contains(router, "type User struct") {
		t.Error("router missing User struct")
	}
	if !strings.Contains(router, `Email string`) {
		t.Error("User struct missing Email field")
	}
	if !strings.Contains(router, `Role  string`) || !strings.Contains(router, `Role string`) {
		// go/format may adjust spacing
		if !strings.Contains(router, "Role") {
			t.Error("User struct missing Role field")
		}
	}
	if !strings.Contains(router, `OrgID string`) {
		t.Error("User struct missing OrgID field")
	}
	if !strings.Contains(router, `json:"email"`) {
		t.Error("User struct missing email json tag")
	}
}

func TestGenerate_RouterUsesMethodPatternRoutes(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	router := findFileContent(t, files, "internal/api/router.go")

	// Go 1.22 method+pattern routes.
	if !strings.Contains(router, `"POST /users"`) {
		t.Error("router missing POST /users route")
	}
	if !strings.Contains(router, `"GET /users/{id}"`) {
		t.Error("router missing GET /users/{id} route")
	}
	if !strings.Contains(router, `"PUT /users/{id}"`) {
		t.Error("router missing PUT /users/{id} route")
	}
	if !strings.Contains(router, `"DELETE /users/{id}"`) {
		t.Error("router missing DELETE /users/{id} route")
	}
	if !strings.Contains(router, `"GET /users"`) {
		t.Error("router missing GET /users route")
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

	// NodePool handler should verify ancestor Cluster exists via checkAncestors.
	npHandler := findFileContent(t, files, "internal/api/handler_nodepool.go")
	if !strings.Contains(npHandler, "checkAncestors") {
		t.Error("nested handler missing checkAncestors method")
	}
	if !strings.Contains(npHandler, `h.store.Exists("Cluster"`) {
		t.Error("nested handler missing parent Exists check")
	}
	if !strings.Contains(npHandler, `"Cluster not found"`) {
		t.Error("nested handler missing parent not-found error")
	}

	// Route should contain nested path with parent param.
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

func TestGenerate_ScopeFilteringWithParent(t *testing.T) {
	// When scope is set with a parent, the scope value must come from the
	// parent's path parameter (which is already in the route pattern).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      "org_id",
				Parent:     "Organization",
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_user.go")
	// Must use PathValue with the parent path parameter name, not the scope field name.
	if !strings.Contains(handler, `r.PathValue("organization_id")`) {
		t.Error("scope with parent must extract value from parent path parameter (organization_id)")
	}
	if !strings.Contains(handler, `h.store.List("User", "org_id", scopeValue)`) {
		t.Error("scope filtering must pass the scope field name to store.List")
	}
}

func TestGenerate_ScopeFilteringWithoutParent(t *testing.T) {
	// When scope is set without a parent, the scope value must come from
	// a query parameter since no path parameter exists.
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
	// Must use URL query parameter, not PathValue (no path param exists).
	if !strings.Contains(handler, `r.URL.Query().Get("org_id")`) {
		t.Error("scope without parent must extract value from query parameter")
	}
	if !strings.Contains(handler, `h.store.List("User", "org_id", scopeValue)`) {
		t.Error("scope filtering must pass the scope field name to store.List")
	}
}

func TestGenerate_ParentOnlyListPassesRawFieldName(t *testing.T) {
	// When an entity has a parent but no scope, the List method must pass
	// the raw YAML field name (e.g. "cluster_id") to store.List, not the
	// PascalCase Go field name ("ClusterID"). This ensures consistency with
	// scope+parent and scope-only branches.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "NodePool", Fields: []types.Field{
				{Name: "cluster_id", Type: types.FieldTypeRef, To: "Cluster"},
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
			{
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpList},
				Parent:     "Cluster",
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_nodepool.go")
	// Must pass raw YAML field name "cluster_id", not PascalCase "ClusterID".
	if !strings.Contains(handler, `h.store.List("NodePool", "cluster_id", clusterID)`) {
		t.Error("parent-only List must pass raw YAML field name to store.List, not PascalCase")
	}
	if strings.Contains(handler, `h.store.List("NodePool", "ClusterID"`) {
		t.Error("parent-only List must not pass PascalCase field name to store.List")
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
	if !strings.Contains(handler, "func (h *AdapterStatusHandler) Upsert(") {
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

func TestGenerate_UpdateAndUpsertOnSameEntity(t *testing.T) {
	// Verifies that update (PUT /path/{id}) and upsert (PUT /path) use
	// different route patterns and do not create duplicate switch cases.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "key", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Item",
				Operations: []types.Operation{types.OpUpdate, types.OpUpsert},
				UpsertKey:  []string{"key"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_item.go")
	if !strings.Contains(handler, "func (h *ItemHandler) Update(") {
		t.Error("handler missing update method")
	}
	if !strings.Contains(handler, "func (h *ItemHandler) Upsert(") {
		t.Error("handler missing upsert method")
	}

	router := findFileContent(t, files, "internal/api/router.go")
	// Update uses PUT /items/{id}, upsert uses PUT /items — different patterns.
	if !strings.Contains(router, `"PUT /items/{id}"`) {
		t.Error("router missing PUT /items/{id} for update")
	}
	if !strings.Contains(router, `"PUT /items"`) {
		t.Error("router missing PUT /items for upsert")
	}
}

func TestGenerate_OpenAPISpec(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Use rendered Bytes() output to verify the JSON is valid after rendering.
	openapiBytes := findFileBytes(t, files, "internal/api/openapi.json")

	// Parse the rendered OpenAPI spec.
	var spec map[string]any
	if err := json.Unmarshal(openapiBytes, &spec); err != nil {
		t.Fatalf("rendered openapi.json is not valid JSON: %v", err)
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

func TestGenerate_OpenAPINestedRoutePathParams(t *testing.T) {
	// Every {param} placeholder in an OpenAPI path must have a corresponding
	// parameter declaration. Nested routes must declare parent path parameters.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "NodePool", Fields: []types.Field{{Name: "cluster_id", Type: types.FieldTypeRef, To: "Cluster"}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Cluster", Operations: []types.Operation{types.OpCreate, types.OpRead}},
			{
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList},
				Parent:     "Cluster",
			},
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

	paths := spec["paths"].(map[string]any)

	// Check nested collection path: /clusters/{cluster_id}/nodepools
	npCollection, ok := paths["/clusters/{cluster_id}/nodepools"]
	if !ok {
		t.Fatal("missing /clusters/{cluster_id}/nodepools path")
	}
	npColOps := npCollection.(map[string]any)

	// POST (create) must declare cluster_id parameter.
	postOp := npColOps["post"].(map[string]any)
	postParams, _ := postOp["parameters"].([]any)
	if !hasParam(postParams, "cluster_id") {
		t.Error("POST /clusters/{cluster_id}/nodepools missing cluster_id parameter")
	}

	// GET (list) must declare cluster_id parameter.
	getOp := npColOps["get"].(map[string]any)
	getParams, _ := getOp["parameters"].([]any)
	if !hasParam(getParams, "cluster_id") {
		t.Error("GET /clusters/{cluster_id}/nodepools missing cluster_id parameter")
	}

	// Check nested item path: /clusters/{cluster_id}/nodepools/{id}
	npItem, ok := paths["/clusters/{cluster_id}/nodepools/{id}"]
	if !ok {
		t.Fatal("missing /clusters/{cluster_id}/nodepools/{id} path")
	}
	npItemOps := npItem.(map[string]any)

	// GET (read) must declare both cluster_id and id parameters.
	readOp := npItemOps["get"].(map[string]any)
	readParams, _ := readOp["parameters"].([]any)
	if !hasParam(readParams, "cluster_id") {
		t.Error("GET /clusters/{cluster_id}/nodepools/{id} missing cluster_id parameter")
	}
	if !hasParam(readParams, "id") {
		t.Error("GET /clusters/{cluster_id}/nodepools/{id} missing id parameter")
	}
}

func hasParam(params []any, name string) bool {
	for _, p := range params {
		param := p.(map[string]any)
		if param["name"] == name {
			return true
		}
	}
	return false
}

func TestGenerate_OpenAPIScopeQueryParamWithoutParent(t *testing.T) {
	// When scope is set without a parent, the handler reads the scope value
	// from a query parameter. The OpenAPI spec must declare this query parameter.
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

	content := findFileContent(t, files, "internal/api/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(content), &spec); err != nil {
		t.Fatalf("parse error: %v", err)
	}

	paths := spec["paths"].(map[string]any)
	usersPath := paths["/users"].(map[string]any)
	getOp := usersPath["get"].(map[string]any)
	params, ok := getOp["parameters"].([]any)
	if !ok || len(params) == 0 {
		t.Fatal("GET /users (list with scope) must have parameters declared")
	}

	// Find the scope query parameter.
	foundScopeParam := false
	for _, p := range params {
		param := p.(map[string]any)
		if param["name"] == "org_id" && param["in"] == "query" {
			foundScopeParam = true
		}
	}
	if !foundScopeParam {
		t.Error("OpenAPI list operation must declare org_id query parameter when scope is set without parent")
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

func TestGenerate_ComputedFieldsExcludedFromWriteOps(t *testing.T) {
	// Computed fields (read-only, filled by a fill) must be zeroed after JSON
	// decode in create, update, and upsert handlers.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString, Unique: true},
				{Name: "spec", Type: types.FieldTypeJsonb},
				{Name: "status_conditions", Type: types.FieldTypeJsonb, Computed: true, FilledBy: "status-aggregator"},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Cluster",
				Operations: []types.Operation{types.OpCreate, types.OpUpdate, types.OpUpsert},
				UpsertKey:  []string{"name"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_cluster.go")

	// Each write method must zero the computed field after decode.
	for _, method := range []string{"Create", "Update", "Upsert"} {
		if !strings.Contains(handler, "func (h *ClusterHandler) "+method+"(") {
			t.Errorf("handler missing %s method", method)
			continue
		}
	}
	// The handler must zero StatusConditions (computed field).
	if !strings.Contains(handler, "cluster.StatusConditions = nil") {
		t.Error("handler must zero computed field StatusConditions after decode")
	}
}

func TestGenerate_ComputedFieldsReadOnlyInOpenAPI(t *testing.T) {
	// Computed fields must be marked readOnly in OpenAPI schema.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "status_conditions", Type: types.FieldTypeJsonb, Computed: true, FilledBy: "status-aggregator"},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
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
	clusterSchema := schemas["Cluster"].(map[string]any)
	props := clusterSchema["properties"].(map[string]any)

	// status_conditions must be readOnly.
	sc := props["status_conditions"].(map[string]any)
	if sc["readOnly"] != true {
		t.Error("computed field status_conditions must have readOnly: true in OpenAPI schema")
	}

	// name must NOT be readOnly.
	nameField := props["name"].(map[string]any)
	if _, hasReadOnly := nameField["readOnly"]; hasReadOnly {
		t.Error("non-computed field name should not have readOnly in OpenAPI schema")
	}
}

func TestGenerate_ComputedFieldsCompilesAsPackage(t *testing.T) {
	// Verify that generated code with computed field zeroing compiles.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString, Unique: true},
				{Name: "spec", Type: types.FieldTypeJsonb},
				{Name: "status_conditions", Type: types.FieldTypeJsonb, Computed: true, FilledBy: "status-aggregator"},
				{Name: "healthy", Type: types.FieldTypeBool, Computed: true, FilledBy: "health-checker"},
				{Name: "last_aggregated", Type: types.FieldTypeTimestamp, Computed: true, FilledBy: "aggregator"},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Cluster",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpUpsert},
				UpsertKey:  []string{"name"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmpDir := t.TempDir()
	goMod := "module testpkg\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}

	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		dst := filepath.Join(tmpDir, filepath.Base(f.Path))
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}

	cmd := exec.Command("go", "build", ".")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated code with computed fields does not compile:\n%s\n%s", err, output)
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
		if strings.HasSuffix(f.Path, ".go") {
			if !strings.HasPrefix(content, gen.Header) {
				t.Errorf("Go file %s missing generated header", f.Path)
			}
		} else if strings.HasSuffix(f.Path, ".json") {
			// JSON files must NOT have Go-comment header — it makes them unparseable.
			if strings.HasPrefix(content, "//") {
				t.Errorf("JSON file %s has Go-comment header, which makes it invalid JSON", f.Path)
			}
			// Verify the rendered JSON is parseable.
			var parsed any
			if err := json.Unmarshal(f.Bytes(), &parsed); err != nil {
				t.Errorf("JSON file %s rendered via Bytes() is not valid JSON: %v", f.Path, err)
			}
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
	if !strings.Contains(handler, "func (h *EventHandler) List(") {
		t.Error("handler missing list method")
	}
	// Should not have a read method.
	if strings.Contains(handler, "func (h *EventHandler) Read(") {
		t.Error("handler should not have read method when only list is exposed")
	}
}

func TestGenerate_GeneratedCodeCompilesAsPackage(t *testing.T) {
	// Verify all generated Go files compile together as a single package.
	// This catches cross-file errors: duplicate case branches, references to
	// undefined types, field assignments on empty structs, etc.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString, Unique: true},
				{Name: "created_at", Type: types.FieldTypeTimestamp},
				{Name: "metadata", Type: types.FieldTypeJsonb, Optional: true},
			}},
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString, Unique: true},
				{Name: "role", Type: types.FieldTypeEnum, Values: []string{"admin", "member"}},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Organization", Operations: []types.Operation{types.OpCreate, types.OpRead}},
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList, types.OpUpsert},
				Parent:     "Organization",
				Scope:      "org_id",
				UpsertKey:  []string{"email"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmpDir := t.TempDir()

	// Write go.mod for the temp package.
	goMod := "module testpkg\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}

	// Write all generated Go files to the temp directory.
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		dst := filepath.Join(tmpDir, filepath.Base(f.Path))
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}

	// Build the package — any cross-file compile error will be caught here.
	cmd := exec.Command("go", "build", ".")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated code does not compile:\n%s\n%s", err, output)
	}
}

func TestEntityBasePath_Default(t *testing.T) {
	eb := types.ExposeBlock{Entity: "User"}
	got, err := entityBasePath(eb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/users" {
		t.Errorf("expected /users, got %s", got)
	}
}

func TestEntityBasePath_WithPathPrefix(t *testing.T) {
	eb := types.ExposeBlock{Entity: "User", PathPrefix: "/api/v1/users"}
	got, err := entityBasePath(eb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/api/v1/users" {
		t.Errorf("expected /api/v1/users, got %s", got)
	}
}

func TestEntityBasePath_Nested(t *testing.T) {
	exposeMap := map[string]types.ExposeBlock{
		"Cluster": {Entity: "Cluster"},
	}
	eb := types.ExposeBlock{Entity: "NodePool", Parent: "Cluster"}
	got, err := entityBasePath(eb, exposeMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/clusters/{cluster_id}/nodepools" {
		t.Errorf("expected /clusters/{cluster_id}/nodepools, got %s", got)
	}
}

func TestToPascalCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"email", "Email"},
		{"org_id", "OrgID"},
		{"cluster_id", "ClusterID"},
		{"resource_type", "ResourceType"},
		{"name", "Name"},
		{"id", "ID"},
		{"status_conditions", "StatusConditions"},
	}
	for _, tt := range tests {
		got := toPascalCase(tt.input)
		if got != tt.want {
			t.Errorf("toPascalCase(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

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
		got := fieldTypeToGo(tt.ft)
		if got != tt.want {
			t.Errorf("fieldTypeToGo(%q) = %q, want %q", tt.ft, got, tt.want)
		}
	}
}

func TestGenerate_UpsertWithParentSetsParentID(t *testing.T) {
	// When an entity has a parent and exposes upsert, the handler must assign
	// the parent's path parameter to the entity's ref field (same as create).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "NodePool", Fields: []types.Field{
				{Name: "cluster_id", Type: types.FieldTypeRef, To: "Cluster"},
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
			{
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpUpsert},
				Parent:     "Cluster",
				UpsertKey:  []string{"name"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_nodepool.go")
	// Upsert must assign parent ID from path parameter, just like create does.
	if !strings.Contains(handler, `nodepool.ClusterID = r.PathValue("cluster_id")`) {
		t.Error("upsert handler must assign parent ID from path parameter")
	}
}

func TestGenerate_DeleteOnlyEntityCompiles(t *testing.T) {
	// A delete-only entity must not import encoding/json (unused import = compile error).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Session", Fields: []types.Field{{Name: "token", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Session", Operations: []types.Operation{types.OpDelete}},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_session.go")
	if strings.Contains(handler, `"encoding/json"`) {
		t.Error("delete-only handler must not import encoding/json (unused import)")
	}

	// Verify the generated code actually compiles.
	tmpDir := t.TempDir()
	goMod := "module testpkg\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		dst := filepath.Join(tmpDir, filepath.Base(f.Path))
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}
	cmd := exec.Command("go", "build", ".")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("delete-only entity does not compile:\n%s\n%s", err, output)
	}
}

func TestGenerate_OpenAPIRenderedBytesValidJSON(t *testing.T) {
	// Verify that openapi.json rendered via File.Bytes() is valid JSON.
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rendered := findFileBytes(t, files, "internal/api/openapi.json")
	var parsed any
	if err := json.Unmarshal(rendered, &parsed); err != nil {
		t.Fatalf("openapi.json rendered via Bytes() is not valid JSON: %v", err)
	}
}

func TestGenerate_HandlersSetContentTypeJSON(t *testing.T) {
	// Every handler method that writes a JSON response body must set
	// Content-Type: application/json to match the OpenAPI spec.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "key", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Item",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpList, types.OpUpsert},
				UpsertKey:  []string{"key"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_item.go")
	// Count occurrences of Content-Type header setting — must match the number
	// of methods that write JSON responses (create, read, update, list, upsert = 5).
	ct := strings.Count(handler, `w.Header().Set("Content-Type", "application/json")`)
	if ct != 5 {
		t.Errorf("expected 5 Content-Type header sets (one per JSON-returning method), got %d", ct)
	}
}

func TestGenerate_ComputedTimestampFieldCompiles(t *testing.T) {
	// A computed timestamp field must use time.Time{} as zero value, not nil.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Record", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "last_updated", Type: types.FieldTypeTimestamp, Computed: true, FilledBy: "updater"},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Record",
				Operations: []types.Operation{types.OpCreate, types.OpRead},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_record.go")
	if !strings.Contains(handler, "record.LastUpdated = time.Time{}") {
		t.Error("computed timestamp field must be zeroed with time.Time{}, not nil")
	}
	if !strings.Contains(handler, `"time"`) {
		t.Error("handler with computed timestamp field must import time package")
	}

	// Verify it compiles.
	tmpDir := t.TempDir()
	goMod := "module testpkg\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		dst := filepath.Join(tmpDir, filepath.Base(f.Path))
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}
	cmd := exec.Command("go", "build", ".")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("computed timestamp field does not compile:\n%s\n%s", err, output)
	}
}

func TestGenerate_OpenAPIResponseContentForWriteOps(t *testing.T) {
	// Create, update, and upsert handlers write JSON response bodies.
	// The OpenAPI spec must declare response content for these operations.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "key", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Item",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpUpsert},
				UpsertKey:  []string{"key"},
			},
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

	paths := spec["paths"].(map[string]any)

	// POST /items (create) — response 201 must have content.
	collectionPath := paths["/items"].(map[string]any)
	postOp := collectionPath["post"].(map[string]any)
	postResponses := postOp["responses"].(map[string]any)
	created := postResponses["201"].(map[string]any)
	if _, ok := created["content"]; !ok {
		t.Error("POST (create) 201 response must declare content with application/json")
	}

	// PUT /items (upsert) — response 200 must have content.
	putCollOp := collectionPath["put"].(map[string]any)
	putCollResponses := putCollOp["responses"].(map[string]any)
	upserted := putCollResponses["200"].(map[string]any)
	if _, ok := upserted["content"]; !ok {
		t.Error("PUT (upsert) 200 response must declare content with application/json")
	}

	// PUT /items/{id} (update) — response 200 must have content.
	itemPath := paths["/items/{id}"].(map[string]any)
	putItemOp := itemPath["put"].(map[string]any)
	putItemResponses := putItemOp["responses"].(map[string]any)
	updated := putItemResponses["200"].(map[string]any)
	if _, ok := updated["content"]; !ok {
		t.Error("PUT (update) 200 response must declare content with application/json")
	}

	// GET /items/{id} (read) — verify it still has content (regression check).
	getItemOp := itemPath["get"].(map[string]any)
	getItemResponses := getItemOp["responses"].(map[string]any)
	readResp := getItemResponses["200"].(map[string]any)
	if _, ok := readResp["content"]; !ok {
		t.Error("GET (read) 200 response must declare content with application/json")
	}
}

func TestGenerate_WiringRoutesUseExportedMethods(t *testing.T) {
	// Wiring routes are code fragments for cmd/main.go (package main).
	// Handler methods must be exported (capitalized) to be accessible
	// from a different package.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "key", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Item",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList, types.OpUpsert},
				UpsertKey:  []string{"key"},
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

	// Verify each route uses an exported method name.
	unexported := []string{".create)", ".read)", ".update)", ".delete)", ".list)", ".upsert)"}
	exported := []string{".Create)", ".Read)", ".Update)", ".Delete)", ".List)", ".Upsert)"}

	for _, route := range wiring.Routes {
		for _, u := range unexported {
			if strings.Contains(route, u) {
				t.Errorf("wiring route uses unexported method: %s", route)
			}
		}
	}

	// Verify at least one exported method is present for each operation.
	foundExported := make(map[string]bool)
	for _, route := range wiring.Routes {
		for _, e := range exported {
			if strings.Contains(route, e) {
				foundExported[e] = true
			}
		}
	}
	for _, e := range exported {
		if !foundExported[e] {
			t.Errorf("expected a wiring route with exported method %s", e)
		}
	}
}

func TestGenerate_MissingParentRefFieldReturnsError(t *testing.T) {
	// Finding 19: when an entity declares parent but has no ref field to the parent,
	// the generator must return a clear error instead of producing broken code.
	g := &Generator{}

	tests := []struct {
		name string
		ops  []types.Operation
	}{
		{"create", []types.Operation{types.OpCreate}},
		{"list (parent-only)", []types.Operation{types.OpList}},
		{"upsert", []types.Operation{types.OpUpsert}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := gen.Context{
				Conventions: types.Convention{Layout: "flat"},
				Entities: []types.Entity{
					{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
					{Name: "Widget", Fields: []types.Field{
						{Name: "name", Type: types.FieldTypeString},
						// No ref field to Cluster — this is the bug trigger.
					}},
				},
				Expose: []types.ExposeBlock{
					{Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
					{
						Entity:     "Widget",
						Operations: tt.ops,
						Parent:     "Cluster",
						UpsertKey:  []string{"name"},
					},
				},
				OutputNamespace: "internal/api",
			}

			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatal("expected error when parent ref field is missing")
			}
			if !strings.Contains(err.Error(), "Cluster") {
				t.Errorf("error should mention parent entity name, got: %v", err)
			}
			if !strings.Contains(err.Error(), "ref") {
				t.Errorf("error should mention missing ref field, got: %v", err)
			}
		})
	}
}

func TestGenerate_OpenAPIRequiredFields(t *testing.T) {
	// Finding 20: OpenAPI entity schemas must include a "required" array listing
	// all non-optional, non-computed fields.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString, Unique: true},
				{Name: "role", Type: types.FieldTypeEnum, Values: []string{"admin", "member"}},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
				{Name: "metadata", Type: types.FieldTypeJsonb, Optional: true},
				{Name: "status", Type: types.FieldTypeJsonb, Computed: true, FilledBy: "aggregator"},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "User", Operations: []types.Operation{types.OpRead}},
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
	userSchema := schemas["User"].(map[string]any)

	requiredRaw, ok := userSchema["required"]
	if !ok {
		t.Fatal("User schema missing 'required' array")
	}
	required := requiredRaw.([]any)

	// email, role, org_id should be required.
	// metadata (optional) and status (computed) should NOT be required.
	requiredSet := make(map[string]bool)
	for _, r := range required {
		requiredSet[r.(string)] = true
	}

	for _, want := range []string{"email", "role", "org_id"} {
		if !requiredSet[want] {
			t.Errorf("field %q should be in required array", want)
		}
	}
	for _, notWant := range []string{"metadata", "status"} {
		if requiredSet[notWant] {
			t.Errorf("field %q should NOT be in required array (optional or computed)", notWant)
		}
	}
}

func TestGenerate_GoKeywordEntityName(t *testing.T) {
	// Finding 21: entity names that are Go keywords must not produce compile errors.
	// The generator should escape the variable name.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Type", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Type",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the generated code actually compiles.
	tmpDir := t.TempDir()
	goMod := "module testpkg\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		dst := filepath.Join(tmpDir, filepath.Base(f.Path))
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}
	cmd := exec.Command("go", "build", ".")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entity named 'Type' (Go keyword) does not compile:\n%s\n%s", err, output)
	}
}

func TestSafeVarName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"user", "user"},
		{"type", "type_"},
		{"map", "map_"},
		{"range", "range_"},
		{"select", "select_"},
		{"string", "string_"},
		{"var", "var_"},
		{"cluster", "cluster"},
	}
	for _, tt := range tests {
		got := safeVarName(tt.input)
		if got != tt.want {
			t.Errorf("safeVarName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGenerate_OpenAPIConstraintAttributes(t *testing.T) {
	// Finding 22: constraint attributes (minLength, maxLength, pattern, minimum,
	// maximum) and type format (int32, int64, float, double) must be propagated
	// to the generated OpenAPI schema.
	g := &Generator{}
	minLen := 3
	maxLen := 53
	minVal := 0.0
	maxVal := 100.0
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Resource", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString, MinLength: &minLen, MaxLength: &maxLen, Pattern: "^[a-z0-9]"},
				{Name: "count32", Type: types.FieldTypeInt32, Min: &minVal, Max: &maxVal},
				{Name: "count64", Type: types.FieldTypeInt64},
				{Name: "ratio", Type: types.FieldTypeFloat},
				{Name: "precise", Type: types.FieldTypeDouble},
				{Name: "label", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Resource", Operations: []types.Operation{types.OpRead}},
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
	props := schemas["Resource"].(map[string]any)["properties"].(map[string]any)

	// String field with constraints.
	nameField := props["name"].(map[string]any)
	if nameField["type"] != "string" {
		t.Error("name field should be string type")
	}
	if nameField["minLength"] != float64(3) {
		t.Errorf("name field minLength: want 3, got %v", nameField["minLength"])
	}
	if nameField["maxLength"] != float64(53) {
		t.Errorf("name field maxLength: want 53, got %v", nameField["maxLength"])
	}
	if nameField["pattern"] != "^[a-z0-9]" {
		t.Errorf("name field pattern: want ^[a-z0-9], got %v", nameField["pattern"])
	}

	// Integer field with constraints and format.
	count32 := props["count32"].(map[string]any)
	if count32["type"] != "integer" {
		t.Error("count32 should be integer type")
	}
	if count32["format"] != "int32" {
		t.Errorf("count32 format: want int32, got %v", count32["format"])
	}
	if count32["minimum"] != float64(0) {
		t.Errorf("count32 minimum: want 0, got %v", count32["minimum"])
	}
	if count32["maximum"] != float64(100) {
		t.Errorf("count32 maximum: want 100, got %v", count32["maximum"])
	}

	// int64 format.
	count64 := props["count64"].(map[string]any)
	if count64["format"] != "int64" {
		t.Errorf("count64 format: want int64, got %v", count64["format"])
	}

	// float format.
	ratioField := props["ratio"].(map[string]any)
	if ratioField["type"] != "number" {
		t.Error("ratio should be number type")
	}
	if ratioField["format"] != "float" {
		t.Errorf("ratio format: want float, got %v", ratioField["format"])
	}

	// double format.
	preciseField := props["precise"].(map[string]any)
	if preciseField["format"] != "double" {
		t.Errorf("precise format: want double, got %v", preciseField["format"])
	}

	// String field without constraints should have no extra attributes.
	labelField := props["label"].(map[string]any)
	if _, ok := labelField["minLength"]; ok {
		t.Error("label field should not have minLength")
	}
	if _, ok := labelField["maxLength"]; ok {
		t.Error("label field should not have maxLength")
	}
	if _, ok := labelField["pattern"]; ok {
		t.Error("label field should not have pattern")
	}
}

func TestGenerate_MultiLevelAncestorVerification(t *testing.T) {
	// Finding 23: nested routing must verify ALL ancestor entities, not just the
	// immediate parent. For Cluster -> NodePool -> AdapterStatus, a request with
	// an invalid Cluster ID must fail even if NodePool exists.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "NodePool", Fields: []types.Field{
				{Name: "cluster_id", Type: types.FieldTypeRef, To: "Cluster"},
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "AdapterStatus", Fields: []types.Field{
				{Name: "nodepool_id", Type: types.FieldTypeRef, To: "NodePool"},
				{Name: "resource_type", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Cluster", Operations: []types.Operation{types.OpCreate, types.OpRead}},
			{
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList},
				Parent:     "Cluster",
			},
			{
				Entity:     "AdapterStatus",
				Operations: []types.Operation{types.OpList, types.OpUpsert},
				Parent:     "NodePool",
				UpsertKey:  []string{"resource_type"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// AdapterStatus handler must verify BOTH Cluster and NodePool.
	asHandler := findFileContent(t, files, "internal/api/handler_adapterstatus.go")

	if !strings.Contains(asHandler, "checkAncestors") {
		t.Error("AdapterStatus handler missing checkAncestors method")
	}
	// Must check Cluster existence (grandparent).
	if !strings.Contains(asHandler, `h.store.Exists("Cluster"`) {
		t.Error("AdapterStatus handler must verify Cluster (grandparent) existence")
	}
	// Must check NodePool existence (parent).
	if !strings.Contains(asHandler, `h.store.Exists("NodePool"`) {
		t.Error("AdapterStatus handler must verify NodePool (parent) existence")
	}
	// Cluster check must come before NodePool check (top-down order).
	clusterIdx := strings.Index(asHandler, `h.store.Exists("Cluster"`)
	nodepoolIdx := strings.Index(asHandler, `h.store.Exists("NodePool"`)
	if clusterIdx > nodepoolIdx {
		t.Error("Cluster (grandparent) verification must come before NodePool (parent)")
	}

	// NodePool handler should still only verify Cluster (its single ancestor).
	npHandler := findFileContent(t, files, "internal/api/handler_nodepool.go")
	if !strings.Contains(npHandler, `h.store.Exists("Cluster"`) {
		t.Error("NodePool handler must verify Cluster existence")
	}
	// NodePool should NOT verify NodePool (it's not its own ancestor).
	clusterCount := strings.Count(npHandler, `h.store.Exists(`)
	if clusterCount != 1 {
		t.Errorf("NodePool handler should have exactly 1 Exists check (Cluster), got %d", clusterCount)
	}

	// Verify the code compiles with three-level nesting.
	tmpDir := t.TempDir()
	goMod := "module testpkg\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		dst := filepath.Join(tmpDir, filepath.Base(f.Path))
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}
	cmd := exec.Command("go", "build", ".")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("three-level nested routing does not compile:\n%s\n%s", err, output)
	}
}

func TestGenerate_CircularParentSelfReference(t *testing.T) {
	// An entity that declares itself as its own parent must produce an error,
	// not an infinite loop or stack overflow.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Node", Fields: []types.Field{
				{Name: "node_id", Type: types.FieldTypeRef, To: "Node"},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Node",
				Operations: []types.Operation{types.OpList},
				Parent:     "Node",
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for self-referencing parent")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error should mention circular reference, got: %v", err)
	}
}

func TestGenerate_CircularParentTwoNodeCycle(t *testing.T) {
	// A -> B -> A cycle must produce an error, not an infinite loop.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Alpha", Fields: []types.Field{
				{Name: "beta_id", Type: types.FieldTypeRef, To: "Beta"},
			}},
			{Name: "Beta", Fields: []types.Field{
				{Name: "alpha_id", Type: types.FieldTypeRef, To: "Alpha"},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Alpha",
				Operations: []types.Operation{types.OpList},
				Parent:     "Beta",
			},
			{
				Entity:     "Beta",
				Operations: []types.Operation{types.OpList},
				Parent:     "Alpha",
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for two-node cycle in parent chain")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error should mention circular reference, got: %v", err)
	}
}

func TestEntityBasePath_CircularParentReturnsError(t *testing.T) {
	exposeMap := map[string]types.ExposeBlock{
		"A": {Entity: "A", Parent: "B"},
		"B": {Entity: "B", Parent: "A"},
	}
	_, err := entityBasePath(exposeMap["A"], exposeMap)
	if err == nil {
		t.Fatal("expected error for circular parent reference")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error should mention circular, got: %v", err)
	}
}

func TestCollectAncestors_CircularParentReturnsError(t *testing.T) {
	exposeMap := map[string]types.ExposeBlock{
		"A": {Entity: "A", Parent: "B"},
		"B": {Entity: "B", Parent: "A"},
	}
	_, err := collectAncestors(exposeMap["A"], exposeMap)
	if err == nil {
		t.Fatal("expected error for circular parent reference")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error should mention circular, got: %v", err)
	}
}

func TestGenerate_UpdateWithParentSetsParentID(t *testing.T) {
	// Finding 26: update handler must assign parent ID from path parameter after
	// body decode, just like create and upsert do. Without this, a client can
	// overwrite or clear the parent ref field via the request body.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "NodePool", Fields: []types.Field{
				{Name: "cluster_id", Type: types.FieldTypeRef, To: "Cluster"},
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
			{
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpUpdate},
				Parent:     "Cluster",
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_nodepool.go")
	// Update must assign parent ID from path parameter, just like create does.
	if !strings.Contains(handler, `nodepool.ClusterID = r.PathValue("cluster_id")`) {
		t.Error("update handler must assign parent ID from path parameter")
	}
}

func TestGenerate_UpdateWithParentMissingRefFieldReturnsError(t *testing.T) {
	// Update with parent but no matching ref field must return an error,
	// consistent with create and upsert behavior.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				// No ref field to Cluster.
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
			{
				Entity:     "Widget",
				Operations: []types.Operation{types.OpUpdate},
				Parent:     "Cluster",
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error when parent ref field is missing for update")
	}
	if !strings.Contains(err.Error(), "Cluster") {
		t.Errorf("error should mention parent entity name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ref") {
		t.Errorf("error should mention missing ref field, got: %v", err)
	}
}

func TestGenerate_OpenAPIDefaultAttribute(t *testing.T) {
	// Finding 25: the `default` constraint attribute must be propagated to the
	// generated OpenAPI schema.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "role", Type: types.FieldTypeEnum, Values: []string{"admin", "member"}, Default: "member"},
				{Name: "active", Type: types.FieldTypeBool, Default: true},
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "User", Operations: []types.Operation{types.OpRead}},
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
	props := schemas["User"].(map[string]any)["properties"].(map[string]any)

	// role field must have default: "member".
	roleField := props["role"].(map[string]any)
	if roleField["default"] != "member" {
		t.Errorf("role field default: want \"member\", got %v", roleField["default"])
	}

	// active field must have default: true.
	activeField := props["active"].(map[string]any)
	if activeField["default"] != true {
		t.Errorf("active field default: want true, got %v", activeField["default"])
	}

	// name field must NOT have a default.
	nameField := props["name"].(map[string]any)
	if _, ok := nameField["default"]; ok {
		t.Error("name field should not have a default value")
	}
}

func TestGenerate_EntityNamedStorageReturnsError(t *testing.T) {
	// Finding 27: entity named "Storage" collides with the generated Storage
	// interface in router.go. The generator must return a clear error at
	// generation time rather than producing code with a redeclaration.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Storage", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Storage",
				Operations: []types.Operation{types.OpCreate, types.OpRead},
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for entity named 'Storage' (collides with generated Storage interface)")
	}
	if !strings.Contains(err.Error(), "Storage") {
		t.Errorf("error should mention 'Storage', got: %v", err)
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("error should mention collision, got: %v", err)
	}
}

func TestGenerate_EntityNamedNewRouterReturnsError(t *testing.T) {
	// Entity named "NewRouter" collides with the generated NewRouter function.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "NewRouter", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "NewRouter",
				Operations: []types.Operation{types.OpRead},
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for entity named 'NewRouter' (collides with generated NewRouter function)")
	}
	if !strings.Contains(err.Error(), "NewRouter") {
		t.Errorf("error should mention 'NewRouter', got: %v", err)
	}
}

func TestSafeVarName_HandlerScopeIdentifiers(t *testing.T) {
	// Finding 28: safeVarName must guard against function-scoped identifiers
	// in generated handler methods (receiver, params, import aliases).
	tests := []struct {
		input string
		want  string
	}{
		// Receiver.
		{"h", "h_"},
		// Parameters.
		{"w", "w_"},
		{"r", "r_"},
		// Import aliases.
		{"json", "json_"},
		{"http", "http_"},
		{"time", "time_"},
		{"fmt", "fmt_"},
		// Non-colliding names should pass through unchanged.
		{"user", "user"},
		{"cluster", "cluster"},
	}
	for _, tt := range tests {
		got := safeVarName(tt.input)
		if got != tt.want {
			t.Errorf("safeVarName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGenerate_EntityNameMatchingReceiverOrParamCompiles(t *testing.T) {
	// Finding 28: entity names whose lowercased form matches the handler method
	// receiver (h) or parameter names (w, r) must not produce compile errors.
	g := &Generator{}

	for _, name := range []string{"W", "R", "H"} {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{
				Conventions: types.Convention{Layout: "flat"},
				Entities: []types.Entity{
					{Name: name, Fields: []types.Field{
						{Name: "name", Type: types.FieldTypeString},
					}},
				},
				Expose: []types.ExposeBlock{
					{
						Entity:     name,
						Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList},
					},
				},
				OutputNamespace: "internal/api",
			}

			files, _, err := g.Generate(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			tmpDir := t.TempDir()
			goMod := "module testpkg\n\ngo 1.22\n"
			if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
				t.Fatalf("writing go.mod: %v", err)
			}
			for _, f := range files {
				if !strings.HasSuffix(f.Path, ".go") {
					continue
				}
				dst := filepath.Join(tmpDir, filepath.Base(f.Path))
				if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
					t.Fatalf("writing %s: %v", f.Path, err)
				}
			}
			cmd := exec.Command("go", "build", ".")
			cmd.Dir = tmpDir
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("entity named %q does not compile:\n%s\n%s", name, err, output)
			}
		})
	}
}

func TestGenerate_EntityNameMatchingImportAliasCompiles(t *testing.T) {
	// Finding 28: entity names whose lowercased form matches import aliases
	// (json, http, time) must not produce compile errors.
	g := &Generator{}

	for _, name := range []string{"Json", "Http", "Time"} {
		t.Run(name, func(t *testing.T) {
			fields := []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}
			// For "Time" entity, add a computed timestamp field to ensure
			// the time import is present and exercised.
			if name == "Time" {
				fields = append(fields, types.Field{
					Name: "last_seen", Type: types.FieldTypeTimestamp, Computed: true, FilledBy: "tracker",
				})
			}
			ctx := gen.Context{
				Conventions: types.Convention{Layout: "flat"},
				Entities:    []types.Entity{{Name: name, Fields: fields}},
				Expose: []types.ExposeBlock{
					{
						Entity:     name,
						Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList},
					},
				},
				OutputNamespace: "internal/api",
			}

			files, _, err := g.Generate(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			tmpDir := t.TempDir()
			goMod := "module testpkg\n\ngo 1.22\n"
			if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
				t.Fatalf("writing go.mod: %v", err)
			}
			for _, f := range files {
				if !strings.HasSuffix(f.Path, ".go") {
					continue
				}
				dst := filepath.Join(tmpDir, filepath.Base(f.Path))
				if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
					t.Fatalf("writing %s: %v", f.Path, err)
				}
			}
			cmd := exec.Command("go", "build", ".")
			cmd.Dir = tmpDir
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("entity named %q does not compile:\n%s\n%s", name, err, output)
			}
		})
	}
}

func TestGenerate_NonDefaultOutputNamespace(t *testing.T) {
	// Finding 29: all generated output — file paths, package declarations, wiring
	// imports, and constructor qualifiers — must derive from ctx.OutputNamespace,
	// not hardcode "api" / "internal/api".
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList},
			},
		},
		OutputNamespace: "pkg/http",
	}

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// File paths must use the non-default namespace.
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "pkg/http/") {
			t.Errorf("file %q should be under pkg/http/, not internal/api/", f.Path)
		}
	}

	// Package declarations must use the base of the namespace ("http"), not "api".
	handlerContent := findFileContent(t, files, "pkg/http/handler_user.go")
	if !strings.Contains(handlerContent, "package http") {
		t.Error("handler file should declare 'package http', not 'package api'")
	}
	if strings.Contains(handlerContent, "package api") {
		t.Error("handler file should not contain 'package api' when namespace is pkg/http")
	}

	routerContent := findFileContent(t, files, "pkg/http/router.go")
	if !strings.Contains(routerContent, "package http") {
		t.Error("router file should declare 'package http', not 'package api'")
	}

	// Wiring imports must reference the actual namespace.
	if wiring == nil {
		t.Fatal("expected wiring")
	}
	if len(wiring.Imports) != 1 || wiring.Imports[0] != "pkg/http" {
		t.Errorf("wiring imports should be [\"pkg/http\"], got %v", wiring.Imports)
	}

	// Wiring constructors must use the package qualifier from the namespace.
	for _, c := range wiring.Constructors {
		if strings.Contains(c, "api.") {
			t.Errorf("wiring constructor should not use 'api.' qualifier, got: %s", c)
		}
		if !strings.Contains(c, "http.") {
			t.Errorf("wiring constructor should use 'http.' qualifier, got: %s", c)
		}
	}

	// OpenAPI file must be under the non-default namespace.
	_ = findFileContent(t, files, "pkg/http/openapi.json")

	// Verify the generated code compiles with the non-default package name.
	tmpDir := t.TempDir()
	goMod := "module testpkg\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		dst := filepath.Join(tmpDir, filepath.Base(f.Path))
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}
	cmd := exec.Command("go", "build", ".")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated code with non-default namespace does not compile:\n%s\n%s", err, output)
	}
}

func TestGenerate_OpenAPISchemasOnlyForExposedEntities(t *testing.T) {
	// Finding 30: OpenAPI schema generation must iterate only over exposed entities,
	// not all entities in ctx.Entities. Non-exposed entities (e.g. ref targets
	// managed by a different component) should not appear in the generated spec.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
			}},
			// Team is in Entities (for ref resolution) but NOT in Expose.
			{Name: "Team", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Organization", Operations: []types.Operation{types.OpCreate, types.OpRead}},
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpRead, types.OpList},
				Scope:      "org_id",
				Parent:     "Organization",
			},
			// Team is NOT exposed.
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

	// Organization and User should be present (they are exposed).
	if _, ok := schemas["Organization"]; !ok {
		t.Error("exposed entity Organization should have an OpenAPI schema")
	}
	if _, ok := schemas["User"]; !ok {
		t.Error("exposed entity User should have an OpenAPI schema")
	}

	// Team should NOT be present (not exposed).
	if _, ok := schemas["Team"]; ok {
		t.Error("non-exposed entity Team should NOT have an OpenAPI schema — it has no path operations")
	}
}

func TestGenerate_ParentNotInExposeListReturnsError(t *testing.T) {
	// Finding 31: When an expose block references a parent entity that is not
	// itself in the expose list, the generator must return an error at generation
	// time rather than producing silently non-functional code.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "NodePool", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "cluster_id", Type: types.FieldTypeRef, To: "Cluster"},
			}},
		},
		Expose: []types.ExposeBlock{
			// NodePool references parent Cluster, but Cluster is NOT exposed.
			{
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList},
				Parent:     "Cluster",
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error when parent entity is not in expose list, got nil")
	}
	if !strings.Contains(err.Error(), "Cluster") {
		t.Errorf("error should mention the missing parent entity 'Cluster', got: %v", err)
	}
	if !strings.Contains(err.Error(), "NodePool") {
		t.Errorf("error should mention the referencing entity 'NodePool', got: %v", err)
	}
	if !strings.Contains(err.Error(), "expose") {
		t.Errorf("error should mention the expose list, got: %v", err)
	}
}

func TestGenerate_MultipleParentsNotInExposeListReportsAll(t *testing.T) {
	// Verify all unresolved parent references are reported together.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Org", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "Team", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Org"},
			}},
			{Name: "Project", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "team_id", Type: types.FieldTypeRef, To: "Team"},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Team", Operations: []types.Operation{types.OpList}, Parent: "Org"},
			{Entity: "Project", Operations: []types.Operation{types.OpList}, Parent: "Team"},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error when parent entities are not in expose list, got nil")
	}
	// Org is not in the expose list, so Team's reference should fail.
	if !strings.Contains(err.Error(), "Org") {
		t.Errorf("error should mention missing parent 'Org', got: %v", err)
	}
}

func TestGenerate_PathPrefixDivergentParamNames(t *testing.T) {
	// Finding 32: when path_prefix is set with parent and the prefix uses
	// non-conventional parameter names (e.g. {org_id} instead of {organization_id}),
	// the generated handler code must use the actual prefix parameter names in
	// r.PathValue() calls, not convention-derived names.
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
			{Entity: "Organization", Operations: []types.Operation{types.OpRead}, PathPrefix: "/orgs"},
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpList},
				Parent:     "Organization",
				PathPrefix: "/orgs/{org_id}/users",
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_user.go")

	// checkAncestors must use {org_id} from the prefix, NOT {organization_id}
	// from the convention.
	if !strings.Contains(handler, `r.PathValue("org_id")`) {
		t.Error("checkAncestors must use actual prefix parameter name 'org_id', not convention-derived 'organization_id'")
	}
	if strings.Contains(handler, `r.PathValue("organization_id")`) {
		t.Error("handler must NOT use convention-derived 'organization_id' when path_prefix provides 'org_id'")
	}

	// Create method parent ID assignment must use {org_id}.
	if !strings.Contains(handler, `user.OrgID = r.PathValue("org_id")`) {
		t.Error("create handler must assign parent ID using actual prefix parameter name 'org_id'")
	}

	// Update method parent ID assignment must use {org_id}.
	if !strings.Contains(handler, `user.OrgID = r.PathValue("org_id")`) {
		t.Error("update handler must assign parent ID using actual prefix parameter name 'org_id'")
	}

	// Verify the generated code compiles.
	tmpDir := t.TempDir()
	goMod := "module testpkg\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		dst := filepath.Join(tmpDir, filepath.Base(f.Path))
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}
	cmd := exec.Command("go", "build", ".")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated code with divergent path_prefix params does not compile:\n%s\n%s", err, output)
	}
}

func TestGenerate_PathPrefixDivergentParamNamesOpenAPI(t *testing.T) {
	// Verify that OpenAPI spec also uses the actual prefix parameter names
	// (this should already work via extractPathParams, but verify as a regression test).
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
			{Entity: "Organization", Operations: []types.Operation{types.OpRead}, PathPrefix: "/orgs"},
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpList, types.OpCreate},
				Parent:     "Organization",
				PathPrefix: "/orgs/{org_id}/users",
			},
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

	paths := spec["paths"].(map[string]any)

	// The path should use the prefix as-is.
	usersPath, ok := paths["/orgs/{org_id}/users"]
	if !ok {
		t.Fatal("missing /orgs/{org_id}/users path in OpenAPI spec")
	}

	// List operation must declare org_id parameter (not organization_id).
	listOp := usersPath.(map[string]any)["get"].(map[string]any)
	params, _ := listOp["parameters"].([]any)
	if !hasParam(params, "org_id") {
		t.Error("OpenAPI list operation must declare 'org_id' parameter from path_prefix")
	}
	if hasParam(params, "organization_id") {
		t.Error("OpenAPI must NOT declare convention-derived 'organization_id' when path_prefix provides 'org_id'")
	}
}

func TestGenerate_PathPrefixScopeWithDivergentParamNames(t *testing.T) {
	// When scope+parent are both set and path_prefix uses non-conventional
	// param names, the List handler must extract scope from the actual prefix
	// parameter, not the convention-derived one.
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
			{Entity: "Organization", Operations: []types.Operation{types.OpRead}, PathPrefix: "/orgs"},
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      "org_id",
				Parent:     "Organization",
				PathPrefix: "/orgs/{org_id}/users",
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_user.go")
	// Scope+parent: must use actual prefix param name for PathValue.
	if !strings.Contains(handler, `r.PathValue("org_id")`) {
		t.Error("scope+parent list must use actual prefix parameter name 'org_id' for scope extraction")
	}
	if strings.Contains(handler, `r.PathValue("organization_id")`) {
		t.Error("scope+parent list must NOT use convention-derived 'organization_id'")
	}
}

func TestGenerate_PathPrefixMultiLevelDivergentParams(t *testing.T) {
	// Three-level nesting with custom path_prefix where parameter names diverge
	// from convention at all levels.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "NodePool", Fields: []types.Field{
				{Name: "cluster_id", Type: types.FieldTypeRef, To: "Cluster"},
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "AdapterStatus", Fields: []types.Field{
				{Name: "nodepool_id", Type: types.FieldTypeRef, To: "NodePool"},
				{Name: "resource_type", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Cluster", Operations: []types.Operation{types.OpRead}, PathPrefix: "/clusters"},
			{
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpRead, types.OpList},
				Parent:     "Cluster",
				PathPrefix: "/clusters/{cid}/pools",
			},
			{
				Entity:     "AdapterStatus",
				Operations: []types.Operation{types.OpList, types.OpUpsert},
				Parent:     "NodePool",
				PathPrefix: "/clusters/{cid}/pools/{pid}/statuses",
				UpsertKey:  []string{"resource_type"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	asHandler := findFileContent(t, files, "internal/api/handler_adapterstatus.go")

	// checkAncestors must use {cid} and {pid} from the prefix.
	if !strings.Contains(asHandler, `r.PathValue("cid")`) {
		t.Error("checkAncestors must use 'cid' from path_prefix for Cluster ancestor")
	}
	if !strings.Contains(asHandler, `r.PathValue("pid")`) {
		t.Error("checkAncestors must use 'pid' from path_prefix for NodePool ancestor")
	}
	if strings.Contains(asHandler, `r.PathValue("cluster_id")`) {
		t.Error("must NOT use convention-derived 'cluster_id' when path_prefix provides 'cid'")
	}
	if strings.Contains(asHandler, `r.PathValue("nodepool_id")`) {
		t.Error("must NOT use convention-derived 'nodepool_id' when path_prefix provides 'pid'")
	}

	// Upsert parent ID assignment must use {pid} from the prefix.
	if !strings.Contains(asHandler, `adapterstatus.NodepoolID = r.PathValue("pid")`) {
		t.Error("upsert handler must assign parent ID using actual prefix parameter 'pid'")
	}

	// Verify the generated code compiles.
	tmpDir := t.TempDir()
	goMod := "module testpkg\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		dst := filepath.Join(tmpDir, filepath.Base(f.Path))
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}
	cmd := exec.Command("go", "build", ".")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated code with multi-level divergent path_prefix does not compile:\n%s\n%s", err, output)
	}
}

func TestResolveAncestorParams_MismatchedParamCount(t *testing.T) {
	// If path_prefix has a different number of params than ancestors,
	// resolveAncestorParams must return an error.
	exposeMap := map[string]types.ExposeBlock{
		"Cluster":  {Entity: "Cluster"},
		"NodePool": {Entity: "NodePool", Parent: "Cluster"},
	}
	// path_prefix has 2 params but NodePool only has 1 ancestor (Cluster).
	eb := types.ExposeBlock{
		Entity:     "Widget",
		Parent:     "NodePool",
		PathPrefix: "/a/{x}/b/{y}/c/{z}/widgets",
	}
	exposeMap["Widget"] = eb
	exposeMap["NodePool"] = types.ExposeBlock{Entity: "NodePool", Parent: "Cluster"}

	_, err := resolveAncestorParams(eb, exposeMap)
	if err == nil {
		t.Fatal("expected error for mismatched param count")
	}
	if !strings.Contains(err.Error(), "Widget") {
		t.Errorf("error should mention entity name, got: %v", err)
	}
}

func TestGenerate_InvalidScopeFieldReturnsError(t *testing.T) {
	// Finding 33: scope field must reference an existing entity field.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      "nonexistent_field",
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for scope referencing non-existent field")
	}
	if !strings.Contains(err.Error(), "nonexistent_field") {
		t.Errorf("error should mention the invalid scope field name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("error should mention the entity name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Errorf("error should mention 'scope', got: %v", err)
	}
}

func TestGenerate_InvalidUpsertKeyFieldReturnsError(t *testing.T) {
	// Finding 34: upsert_key field names must reference existing entity fields.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Item",
				Operations: []types.Operation{types.OpUpsert},
				UpsertKey:  []string{"name", "nonexistent_key"},
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for upsert_key referencing non-existent field")
	}
	if !strings.Contains(err.Error(), "nonexistent_key") {
		t.Errorf("error should mention the invalid upsert_key field name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Item") {
		t.Errorf("error should mention the entity name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "upsert_key") {
		t.Errorf("error should mention 'upsert_key', got: %v", err)
	}
}

func TestGenerate_CrossEntityHandlerTypeCollisionReturnsError(t *testing.T) {
	// Finding 35: entity A named "User" produces "UserHandler" type; if entity B
	// is named "UserHandler", it collides with A's handler type in the same package.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "UserHandler", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "User", Operations: []types.Operation{types.OpRead}},
			{Entity: "UserHandler", Operations: []types.Operation{types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for cross-entity handler type name collision")
	}
	if !strings.Contains(err.Error(), "UserHandler") {
		t.Errorf("error should mention the colliding name 'UserHandler', got: %v", err)
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("error should mention the source entity 'User', got: %v", err)
	}
}

func TestGenerate_DuplicateExposeBlocksReturnsError(t *testing.T) {
	// Finding 36: two expose blocks for the same entity must return an error,
	// not silently overwrite in the map or produce duplicate type declarations.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "User", Operations: []types.Operation{types.OpCreate}},
			{Entity: "User", Operations: []types.Operation{types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for duplicate expose blocks for the same entity")
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("error should mention the duplicated entity 'User', got: %v", err)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention 'duplicate', got: %v", err)
	}
}

func TestGenerate_ValidScopeFieldSucceeds(t *testing.T) {
	// Verify that a valid scope field reference does not produce an error.
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

	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for valid scope field: %v", err)
	}
}

func TestGenerate_ValidUpsertKeyFieldsSucceeds(t *testing.T) {
	// Verify that valid upsert_key field references do not produce an error.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Status", Fields: []types.Field{
				{Name: "resource_type", Type: types.FieldTypeString},
				{Name: "resource_id", Type: types.FieldTypeString},
				{Name: "adapter", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{
				Entity:     "Status",
				Operations: []types.Operation{types.OpUpsert},
				UpsertKey:  []string{"resource_type", "resource_id", "adapter"},
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for valid upsert_key fields: %v", err)
	}
}

func TestGenerate_EmptyOperationsListReturnsError(t *testing.T) {
	// Finding 37: an expose block with zero operations must return an error,
	// not produce uncompilable code (unused net/http import, unused handler variable).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "User", Operations: []types.Operation{}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for expose block with empty operations list")
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("error should mention entity name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "operation") {
		t.Errorf("error should mention operations, got: %v", err)
	}
}

func TestGenerate_EmptyOperationsAmongValid(t *testing.T) {
	// One valid expose block + one empty operations expose block => error.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}}},
			{Name: "Org", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "User", Operations: []types.Operation{types.OpRead}},
			{Entity: "Org", Operations: []types.Operation{}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for expose block with empty operations list")
	}
	if !strings.Contains(err.Error(), "Org") {
		t.Errorf("error should mention entity with empty operations, got: %v", err)
	}
}

func TestGenerate_DuplicateOperationsReturnsError(t *testing.T) {
	// Finding 44: duplicate operations within a single expose block produce
	// duplicate method declarations (compile error), duplicate route
	// registrations (runtime panic), and duplicate OpenAPI entries (overwrite).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "User", Operations: []types.Operation{types.OpCreate, types.OpCreate, types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for duplicate operations in expose block")
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("error should mention the entity with duplicate operations, got: %v", err)
	}
	if !strings.Contains(err.Error(), "create") {
		t.Errorf("error should mention the duplicated operation, got: %v", err)
	}
}

func TestGenerate_DuplicateOperationsMultipleEntities(t *testing.T) {
	// Verify that duplicate operation detection reports all affected entities.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}}},
			{Name: "Org", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "User", Operations: []types.Operation{types.OpRead, types.OpRead}},
			{Entity: "Org", Operations: []types.Operation{types.OpDelete, types.OpDelete}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for duplicate operations")
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("error should mention User, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Org") {
		t.Errorf("error should mention Org, got: %v", err)
	}
}

func TestGenerate_RouteCollisionSamePathPrefix(t *testing.T) {
	// Finding 38: two entities with the same path_prefix must be rejected at
	// generation time — duplicate route registrations cause runtime panics.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Alpha", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "Beta", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Alpha", Operations: []types.Operation{types.OpList}, PathPrefix: "/items"},
			{Entity: "Beta", Operations: []types.Operation{types.OpList}, PathPrefix: "/items"},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for route path collision")
	}
	if !strings.Contains(err.Error(), "Alpha") || !strings.Contains(err.Error(), "Beta") {
		t.Errorf("error should mention both colliding entities, got: %v", err)
	}
	if !strings.Contains(err.Error(), "/items") {
		t.Errorf("error should mention the colliding path, got: %v", err)
	}
}

func TestGenerate_RouteCollisionCaseInsensitive(t *testing.T) {
	// Two entity names that are case-insensitively equivalent produce the
	// same auto-derived path (both → /items).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "ITEM", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Item", Operations: []types.Operation{types.OpList}},
			{Entity: "ITEM", Operations: []types.Operation{types.OpList}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for case-insensitive route path collision")
	}
	if !strings.Contains(err.Error(), "Item") && !strings.Contains(err.Error(), "ITEM") {
		t.Errorf("error should mention colliding entities, got: %v", err)
	}
}

func TestGenerate_RouteCollisionAutoVsExplicitPrefix(t *testing.T) {
	// An auto-derived path that matches another entity's explicit path_prefix.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "Other", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Widget", Operations: []types.Operation{types.OpList}},                        // auto: /widgets
			{Entity: "Other", Operations: []types.Operation{types.OpList}, PathPrefix: "/widgets"}, // explicit: /widgets
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for auto-derived path matching explicit path_prefix")
	}
	if !strings.Contains(err.Error(), "Widget") || !strings.Contains(err.Error(), "Other") {
		t.Errorf("error should mention both colliding entities, got: %v", err)
	}
}

func TestGenerate_NoRouteCollisionDifferentPaths(t *testing.T) {
	// Verify that non-colliding paths succeed.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}}},
			{Name: "Team", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "User", Operations: []types.Operation{types.OpList}},
			{Entity: "Team", Operations: []types.Operation{types.OpList}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for non-colliding routes: %v", err)
	}
}

func TestGenerate_ScopeParentInconsistencyReturnsError(t *testing.T) {
	// Finding 39: when scope and parent are both set but scope is not the
	// entity's ref field to the parent, the generator must return an error.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
				{Name: "department", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      "department", // not the ref field to Organization
				Parent:     "Organization",
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error when scope is not the parent ref field")
	}
	if !strings.Contains(err.Error(), "department") {
		t.Errorf("error should mention the scope field 'department', got: %v", err)
	}
	if !strings.Contains(err.Error(), "Organization") {
		t.Errorf("error should mention the parent 'Organization', got: %v", err)
	}
	if !strings.Contains(err.Error(), "org_id") {
		t.Errorf("error should mention the correct ref field 'org_id', got: %v", err)
	}
}

func TestGenerate_ScopeParentConsistentSucceeds(t *testing.T) {
	// Verify that scope+parent with matching ref field succeeds.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      "org_id", // matches the ref field to Organization
				Parent:     "Organization",
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for consistent scope+parent: %v", err)
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

func TestGenerate_MultipleRefFieldsToSameParentReturnsError(t *testing.T) {
	// Finding 40: when an entity has multiple ref fields pointing to the same
	// parent entity, the generator must reject the ambiguity rather than
	// silently selecting the first match.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Account", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "Transfer", Fields: []types.Field{
				{Name: "amount", Type: types.FieldTypeInt64},
				{Name: "from_account_id", Type: types.FieldTypeRef, To: "Account"},
				{Name: "to_account_id", Type: types.FieldTypeRef, To: "Account"},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Account", Operations: []types.Operation{types.OpRead}},
			{Entity: "Transfer", Operations: []types.Operation{types.OpCreate}, Parent: "Account"},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error when entity has multiple ref fields to the same parent")
	}
	if !strings.Contains(err.Error(), "multiple ref fields") {
		t.Errorf("error should mention multiple ref fields, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Account") {
		t.Errorf("error should mention parent entity name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "from_account_id") {
		t.Errorf("error should mention the first matching field, got: %v", err)
	}
	if !strings.Contains(err.Error(), "to_account_id") {
		t.Errorf("error should mention the second matching field, got: %v", err)
	}
}

func TestGenerate_MultipleRefFieldsTwoNodeCycle(t *testing.T) {
	// Finding 40: test with list (parent-only) and upsert to confirm ambiguity
	// is caught for those operation paths too.
	g := &Generator{}

	tests := []struct {
		name string
		ops  []types.Operation
	}{
		{"list", []types.Operation{types.OpList}},
		{"upsert", []types.Operation{types.OpUpsert}},
		{"update", []types.Operation{types.OpUpdate}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := gen.Context{
				Conventions: types.Convention{Layout: "flat"},
				Entities: []types.Entity{
					{Name: "Org", Fields: []types.Field{
						{Name: "name", Type: types.FieldTypeString},
					}},
					{Name: "Member", Fields: []types.Field{
						{Name: "primary_org_id", Type: types.FieldTypeRef, To: "Org"},
						{Name: "secondary_org_id", Type: types.FieldTypeRef, To: "Org"},
					}},
				},
				Expose: []types.ExposeBlock{
					{Entity: "Org", Operations: []types.Operation{types.OpRead}},
					{Entity: "Member", Operations: tt.ops, Parent: "Org", UpsertKey: []string{"primary_org_id"}},
				},
				OutputNamespace: "internal/api",
			}

			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error for ambiguous ref fields with %s operation", tt.name)
			}
			if !strings.Contains(err.Error(), "multiple ref fields") {
				t.Errorf("error should mention multiple ref fields, got: %v", err)
			}
		})
	}
}

func TestGenerate_ParentWithReadDeleteOnlyNoRefFieldReturnsError(t *testing.T) {
	// Finding 41: when an entity declares parent but only exposes read and/or
	// delete operations, the generator must still validate that a ref field to
	// the parent exists. Previously, the validation was lazy (only in
	// create/update/upsert/list methods), so read-only and delete-only
	// entities silently passed.
	g := &Generator{}

	tests := []struct {
		name string
		ops  []types.Operation
	}{
		{"read-only", []types.Operation{types.OpRead}},
		{"delete-only", []types.Operation{types.OpDelete}},
		{"read-and-delete", []types.Operation{types.OpRead, types.OpDelete}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := gen.Context{
				Conventions: types.Convention{Layout: "flat"},
				Entities: []types.Entity{
					{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
					{Name: "Widget", Fields: []types.Field{
						{Name: "name", Type: types.FieldTypeString},
						// No ref field to Cluster.
					}},
				},
				Expose: []types.ExposeBlock{
					{Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
					{Entity: "Widget", Operations: tt.ops, Parent: "Cluster"},
				},
				OutputNamespace: "internal/api",
			}

			_, _, err := g.Generate(ctx)
			if err == nil {
				t.Fatalf("expected error when parent ref field is missing with %s operations", tt.name)
			}
			if !strings.Contains(err.Error(), "Cluster") {
				t.Errorf("error should mention parent entity name, got: %v", err)
			}
			if !strings.Contains(err.Error(), "ref") {
				t.Errorf("error should mention missing ref field, got: %v", err)
			}
		})
	}
}

func TestGenerate_SingleRefFieldToParentSucceeds(t *testing.T) {
	// Ensure the normal case (exactly one ref field to parent) still works.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Org", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "User", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Org"},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Org", Operations: []types.Operation{types.OpRead}},
			{Entity: "User", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpDelete}, Parent: "Org"},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error with single ref field to parent: %v", err)
	}
}

// findFileBytes finds a file by path and returns its rendered output via Bytes().
func findFileBytes(t *testing.T, files []gen.File, path string) []byte {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f.Bytes()
		}
	}
	t.Fatalf("file %s not found in output", path)
	return nil
}

func TestGenerate_EntityNameIdCompiles(t *testing.T) {
	// Finding 42: entity named "Id" or "ID" produces strings.ToLower → "id",
	// which collides with the hardcoded `id := r.PathValue("id")` in
	// Read/Update/Delete methods. safeVarName must escape "id" to "id_".
	g := &Generator{}

	for _, name := range []string{"Id", "ID"} {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{
				Conventions: types.Convention{Layout: "flat"},
				Entities: []types.Entity{
					{Name: name, Fields: []types.Field{
						{Name: "name", Type: types.FieldTypeString},
					}},
				},
				Expose: []types.ExposeBlock{
					{
						Entity:     name,
						Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList},
					},
				},
				OutputNamespace: "internal/api",
			}

			files, _, err := g.Generate(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify the generated code compiles as a package.
			tmpDir := t.TempDir()
			goMod := "module testpkg\n\ngo 1.22\n"
			if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
				t.Fatalf("writing go.mod: %v", err)
			}
			for _, f := range files {
				if !strings.HasSuffix(f.Path, ".go") {
					continue
				}
				dst := filepath.Join(tmpDir, filepath.Base(f.Path))
				if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
					t.Fatalf("writing %s: %v", f.Path, err)
				}
			}
			cmd := exec.Command("go", "build", ".")
			cmd.Dir = tmpDir
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("entity named %q does not compile:\n%s\n%s", name, err, output)
			}
		})
	}
}

func TestGenerate_CaseInsensitiveEntityNamesReturnsError(t *testing.T) {
	// Finding 43: two entities whose names differ only in case (e.g. "Item"
	// and "ITEM") produce colliding handler variable names, handler file
	// paths, and router variable declarations via strings.ToLower.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "ITEM", Fields: []types.Field{
				{Name: "label", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Item", Operations: []types.Operation{types.OpRead}, PathPrefix: "/items"},
			{Entity: "ITEM", Operations: []types.Operation{types.OpRead}, PathPrefix: "/things"},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for case-insensitively equivalent entity names")
	}
	if !strings.Contains(err.Error(), "case-insensitive") {
		t.Errorf("error should mention case-insensitive collision, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Item") {
		t.Errorf("error should mention first entity name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ITEM") {
		t.Errorf("error should mention second entity name, got: %v", err)
	}
}

func TestGenerate_CaseInsensitiveEntityNamesAutoPathCaught(t *testing.T) {
	// Verify that case-insensitive check catches collisions even when both
	// entities use auto-derived paths (which also collide via route detection,
	// but the case-insensitive check should fire first).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Foo", Fields: []types.Field{{Name: "a", Type: types.FieldTypeString}}},
			{Name: "FOO", Fields: []types.Field{{Name: "b", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Foo", Operations: []types.Operation{types.OpRead}},
			{Entity: "FOO", Operations: []types.Operation{types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for case-insensitively equivalent entity names with auto paths")
	}
	if !strings.Contains(err.Error(), "case-insensitive") {
		t.Errorf("error should mention case-insensitive collision, got: %v", err)
	}
}

func TestGenerate_DifferentCaseEntityNamesDifferentEnoughSucceeds(t *testing.T) {
	// Entities with different lowercased names should not be rejected.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "Order", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Item", Operations: []types.Operation{types.OpRead}},
			{Entity: "Order", Operations: []types.Operation{types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for entities with distinct lowercased names: %v", err)
	}
}

func TestGenerate_EntityNameErrCompiles(t *testing.T) {
	// Finding 45: entity named "Err" or "ERR" produces strings.ToLower → "err",
	// which collides with the hardcoded `err` in the Read method's dual-assignment
	// `%s, err := h.store.Read(...)`. safeVarName must escape "err" to "err_".
	g := &Generator{}

	for _, name := range []string{"Err", "ERR"} {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{
				Conventions: types.Convention{Layout: "flat"},
				Entities: []types.Entity{
					{Name: name, Fields: []types.Field{
						{Name: "name", Type: types.FieldTypeString},
					}},
				},
				Expose: []types.ExposeBlock{
					{
						Entity:     name,
						Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList},
					},
				},
				OutputNamespace: "internal/api",
			}

			files, _, err := g.Generate(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify the generated code compiles as a package.
			tmpDir := t.TempDir()
			goMod := "module testpkg\n\ngo 1.22\n"
			if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
				t.Fatalf("writing go.mod: %v", err)
			}
			for _, f := range files {
				if !strings.HasSuffix(f.Path, ".go") {
					continue
				}
				dst := filepath.Join(tmpDir, filepath.Base(f.Path))
				if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
					t.Fatalf("writing %s: %v", f.Path, err)
				}
			}
			cmd := exec.Command("go", "build", ".")
			cmd.Dir = tmpDir
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("entity named %q does not compile:\n%s\n%s", name, err, output)
			}
		})
	}
}

func TestGenerate_HandlerWithSlotBindings(t *testing.T) {
	// Verify handler constructor accepts slot operator parameters and
	// methods invoke slot operators at appropriate lifecycle points.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString, Unique: true},
				{Name: "role", Type: types.FieldTypeEnum, Values: []string{"admin", "member"}},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "User", Operations: []types.Operation{
				types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList,
			}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Entity: "User", Gate: []string{"rbac-policy"}},
			{Slot: "on_entity_changed", Entity: "User", FanOut: []string{"audit-logger"}},
		},
		OutputNamespace: "internal/api",
		ModuleName:      "github.com/myorg/svc",
		SlotsPackage:    "internal/slots",
	}

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the handler file.
	var handlerContent string
	for _, f := range files {
		if strings.Contains(f.Path, "handler_user.go") {
			handlerContent = string(f.Content)
		}
	}
	if handlerContent == "" {
		t.Fatal("handler_user.go not found")
	}

	// Verify slot package import.
	if !strings.Contains(handlerContent, `"github.com/myorg/svc/internal/slots"`) {
		t.Errorf("handler missing slots package import:\n%s", handlerContent)
	}

	// Verify handler struct has slot operator fields.
	if !strings.Contains(handlerContent, "beforeCreateGate slots.BeforeCreateSlot") {
		t.Errorf("handler struct missing beforeCreateGate field:\n%s", handlerContent)
	}
	if !strings.Contains(handlerContent, "onEntityChangedFanOut slots.OnEntityChangedSlot") {
		t.Errorf("handler struct missing onEntityChangedFanOut field:\n%s", handlerContent)
	}

	// Verify constructor accepts slot params.
	if !strings.Contains(handlerContent, "func NewUserHandler(store Storage, beforeCreateGate slots.BeforeCreateSlot, onEntityChangedFanOut slots.OnEntityChangedSlot)") {
		t.Errorf("constructor missing slot params:\n%s", handlerContent)
	}

	// Verify slot operator invocation in Create method (before_create gate fires).
	if !strings.Contains(handlerContent, "h.beforeCreateGate.Evaluate(r.Context()") {
		t.Errorf("Create method missing before_create gate invocation:\n%s", handlerContent)
	}
	// Verify on_entity_changed fires after create.
	if !strings.Contains(handlerContent, "h.onEntityChangedFanOut.Evaluate(r.Context()") {
		t.Errorf("Create method missing on_entity_changed invocation:\n%s", handlerContent)
	}

	// Finding 26: Verify before-slot request is populated with entity data,
	// not zero-valued. The request must reference the in-scope entity variable
	// so fills can inspect the entity being processed.
	if !strings.Contains(handlerContent, `Input: &slots.CreateRequest{`) {
		t.Errorf("before-slot request missing populated Input field:\n%s", handlerContent)
	}
	if !strings.Contains(handlerContent, `Entity: "User"`) {
		t.Errorf("before-slot request missing Entity in CreateRequest:\n%s", handlerContent)
	}
	// The entity variable (user) must be referenced in the Fields map.
	// go/format may add alignment spacing, so check for the field name and
	// the entity variable reference separately.
	if !strings.Contains(handlerContent, `"email":`) || !strings.Contains(handlerContent, `user.Email`) {
		t.Errorf("before-slot request missing entity field reference for email:\n%s", handlerContent)
	}
	if !strings.Contains(handlerContent, `"role":`) || !strings.Contains(handlerContent, `user.Role`) {
		t.Errorf("before-slot request missing entity field reference for role:\n%s", handlerContent)
	}

	// Finding 26: Verify after-slot request is populated with entity name and action.
	if !strings.Contains(handlerContent, `Entity: "User", Action: "create"`) {
		t.Errorf("after-slot request in Create missing entity/action:\n%s", handlerContent)
	}

	// Finding 27: Verify nil guards on slot operator invocations.
	if !strings.Contains(handlerContent, "if h.beforeCreateGate != nil {") {
		t.Errorf("before-slot invocation missing nil guard:\n%s", handlerContent)
	}
	if !strings.Contains(handlerContent, "if h.onEntityChangedFanOut != nil {") {
		t.Errorf("after-slot invocation missing nil guard:\n%s", handlerContent)
	}

	// Finding 29: Verify Caller field is populated with non-nil Identity.
	// The spec's canonical fill accesses req.Caller.Role — nil Caller panics.
	if !strings.Contains(handlerContent, `Caller: &slots.Identity{}`) {
		t.Errorf("before-slot request missing non-nil Caller field:\n%s", handlerContent)
	}

	// Finding 28: Verify halt check in before-slot invocation. A chain step
	// returning {Ok: true, Halt: true, StatusCode: 204} must stop the handler.
	if !strings.Contains(handlerContent, "if slotResult.Halt {") {
		t.Errorf("before-slot invocation missing halt check:\n%s", handlerContent)
	}
	// Verify halt branch writes status code and returns.
	if !strings.Contains(handlerContent, "w.WriteHeader(sc)") {
		t.Errorf("before-slot halt branch missing WriteHeader:\n%s", handlerContent)
	}

	// Finding 27: Verify NewRouter passes nil for slot params (convenience
	// constructor for self-contained use without fills).
	var routerContent string
	for _, f := range files {
		if strings.Contains(f.Path, "router.go") {
			routerContent = string(f.Content)
		}
	}
	if routerContent == "" {
		t.Fatal("router.go not found")
	}
	if !strings.Contains(routerContent, "NewUserHandler(store, nil, nil)") {
		t.Errorf("NewRouter should pass nil for slot operators:\n%s", routerContent)
	}

	// Verify ConstructorEntities wiring.
	if wiring == nil {
		t.Fatal("wiring is nil")
	}
	if wiring.ConstructorEntities == nil || wiring.ConstructorEntities[0] != "User" {
		t.Errorf("unexpected ConstructorEntities: %v", wiring.ConstructorEntities)
	}
}

func TestGenerate_HandlerWithoutSlotBindings_NoSlotCode(t *testing.T) {
	// Verify that handlers without slot bindings do NOT import the slots
	// package or have slot operator fields.
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		if strings.Contains(f.Path, "handler_user.go") {
			content := string(f.Content)
			if strings.Contains(content, "slots.") {
				t.Errorf("handler without slot bindings should not reference slots package:\n%s", content)
			}
		}
	}
}

func TestGenerate_SlotRequestPopulationWithNonStringFields(t *testing.T) {
	// Finding 26: verify that when an entity has non-string fields (int32, bool),
	// the before-slot request uses fmt.Sprintf for type conversion and the handler
	// imports "fmt".
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Counter", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "count", Type: types.FieldTypeInt32},
				{Name: "active", Type: types.FieldTypeBool},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Counter", Operations: []types.Operation{types.OpCreate}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Entity: "Counter", Gate: []string{"policy"}},
		},
		OutputNamespace: "internal/api",
		ModuleName:      "github.com/myorg/svc",
		SlotsPackage:    "internal/slots",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var handlerContent string
	for _, f := range files {
		if strings.Contains(f.Path, "handler_counter.go") {
			handlerContent = string(f.Content)
		}
	}
	if handlerContent == "" {
		t.Fatal("handler_counter.go not found")
	}

	// fmt must be imported for non-string field conversion.
	if !strings.Contains(handlerContent, `"fmt"`) {
		t.Errorf("handler with non-string slot fields missing fmt import:\n%s", handlerContent)
	}

	// Non-string fields must use fmt.Sprintf for conversion.
	if !strings.Contains(handlerContent, `fmt.Sprintf("%v", counter.Count)`) {
		t.Errorf("non-string field 'count' missing fmt.Sprintf conversion:\n%s", handlerContent)
	}
	if !strings.Contains(handlerContent, `fmt.Sprintf("%v", counter.Active)`) {
		t.Errorf("non-string field 'active' missing fmt.Sprintf conversion:\n%s", handlerContent)
	}

	// String fields should be direct references.
	if !strings.Contains(handlerContent, "counter.Name") {
		t.Errorf("string field 'name' should use direct reference:\n%s", handlerContent)
	}
}

func TestGenerate_NilGuardPassthrough(t *testing.T) {
	// Finding 27: verify that handler methods with slot bindings include nil
	// guards so that NewRouter (which passes nil for slot operators) does not
	// cause runtime panics. The handler should degrade to passthrough semantics.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{
				{Name: "label", Type: types.FieldTypeString},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "Item", Operations: []types.Operation{
				types.OpCreate, types.OpUpdate, types.OpDelete, types.OpUpsert,
			}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Entity: "Item", Gate: []string{"gate-fill"}},
			{Slot: "validate", Entity: "Item", Chain: []string{"validate-fill"}},
			{Slot: "on_entity_changed", Entity: "Item", FanOut: []string{"fanout-fill"}},
		},
		OutputNamespace: "internal/api",
		ModuleName:      "github.com/myorg/svc",
		SlotsPackage:    "internal/slots",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var handlerContent string
	for _, f := range files {
		if strings.Contains(f.Path, "handler_item.go") {
			handlerContent = string(f.Content)
		}
	}
	if handlerContent == "" {
		t.Fatal("handler_item.go not found")
	}

	// Every slot operator invocation must have a nil guard.
	// Gate before create.
	if !strings.Contains(handlerContent, "if h.beforeCreateGate != nil {") {
		t.Errorf("missing nil guard for beforeCreateGate:\n%s", handlerContent)
	}
	// Chain for validate (fires on create, update, upsert).
	if !strings.Contains(handlerContent, "if h.validateChain != nil {") {
		t.Errorf("missing nil guard for validateChain:\n%s", handlerContent)
	}
	// Fan-out for on_entity_changed (fires on create, update, delete, upsert).
	if !strings.Contains(handlerContent, "if h.onEntityChangedFanOut != nil {") {
		t.Errorf("missing nil guard for onEntityChangedFanOut:\n%s", handlerContent)
	}

	// Verify the after-slot request for different operations includes the
	// correct action string.
	for _, action := range []string{"create", "update", "delete", "upsert"} {
		expected := fmt.Sprintf(`Action: "%s"`, action)
		if !strings.Contains(handlerContent, expected) {
			t.Errorf("after-slot request missing action %q:\n%s", action, handlerContent)
		}
	}

	// Finding 30: verify polymorphic slot request field emission.
	// before_create has Caller (from proto), validate does NOT.
	if !strings.Contains(handlerContent, "Caller: &") {
		t.Errorf("before_create request should have Caller field:\n%s", handlerContent)
	}
	// ValidateRequest has Entity string field (distinct from CreateRequest.Entity).
	// Count occurrences to ensure validate request populates Entity.
	// The validate slot fires on create, update, and upsert — each should
	// emit a ValidateRequest with an Entity field.
	validateReqCount := strings.Count(handlerContent, "ValidateRequest{")
	if validateReqCount == 0 {
		t.Errorf("expected ValidateRequest struct literals in handler:\n%s", handlerContent)
	}

	// Split handler into before_create and validate sections to verify
	// Caller only appears in before_create requests, not validate requests.
	// Each ValidateRequest literal must have Entity but NOT Caller.
	sections := strings.Split(handlerContent, "ValidateRequest{")
	for i, section := range sections[1:] { // skip pre-first-match
		closing := strings.Index(section, "}\n")
		if closing < 0 {
			continue
		}
		reqBody := section[:closing]
		if strings.Contains(reqBody, "Caller:") {
			t.Errorf("ValidateRequest occurrence %d should NOT have Caller field:\n%s", i, reqBody)
		}
		if !strings.Contains(reqBody, "Entity:") {
			t.Errorf("ValidateRequest occurrence %d should have Entity field:\n%s", i, reqBody)
		}
	}
}

func TestGenerate_CrossGeneratorIntegrationCompilation(t *testing.T) {
	// Checklist item 95: verify that rest-api generator output, slot
	// interface/operator output, and assembler output compile together.
	// This catches signature mismatches between handler constructors and
	// the assembler's injected slot operator arguments.

	moduleName := "testmod"
	slotsPackage := "internal/slots"

	// 1. Generate rest-api files with slot bindings.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString, Unique: true},
				{Name: "role", Type: types.FieldTypeEnum, Values: []string{"admin", "member"}},
			}},
		},
		Expose: []types.ExposeBlock{
			{Entity: "User", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Entity: "User", Gate: []string{"rbac-policy"}},
			{Slot: "validate", Entity: "User", Chain: []string{"validate-fill"}},
			{Slot: "on_entity_changed", Entity: "User", FanOut: []string{"audit-logger"}},
		},
		OutputNamespace: "internal/api",
		ModuleName:      moduleName,
		SlotsPackage:    slotsPackage,
	}

	apiFiles, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("rest-api Generate: %v", err)
	}

	// 2. Generate slot interface + operator files. We write the slot types
	// manually (not via GenerateInterface) to avoid duplicate type declarations
	// across files. The handler populates Input (*CreateRequest) on before-slots
	// and Entity/Action on after-slots — the types must match.
	commonTypesFile := gen.File{
		Path: slotsPackage + "/common_types.go",
		Content: []byte(`package slots

// SlotResult is generated from proto message SlotResult.
type SlotResult struct {
	Ok           bool
	ErrorMessage string
	Halt         bool
	StatusCode   int32
}

// CreateRequest is generated from proto message CreateRequest.
type CreateRequest struct {
	Entity string
	Fields map[string]string
}

// Identity is generated from proto message Identity.
type Identity struct {
	UserID string
	Role   string
}
`),
	}
	bcIfaceFile := gen.File{
		Path: slotsPackage + "/before_create.go",
		Content: []byte(`package slots

import "context"

// BeforeCreateRequest is generated from proto message BeforeCreateRequest.
type BeforeCreateRequest struct {
	Input  *CreateRequest
	Caller *Identity
}

// BeforeCreateSlot is the interface that fills implement for the BeforeCreate slot.
type BeforeCreateSlot interface {
	Evaluate(ctx context.Context, req *BeforeCreateRequest) (*SlotResult, error)
}
`),
	}
	// Operator files: use GenerateOperators for the before_create slot which
	// only needs the service definition (no imports needed for operators).
	beforeCreateProto := &slot.ProtoFile{
		Package: "stego.components.rest_api.slots",
		Services: []slot.Service{
			{Name: "BeforeCreate", Methods: []slot.Method{
				{Name: "Evaluate", InputType: "BeforeCreateRequest", OutputType: "stego.common.SlotResult"},
			}},
		},
		Messages: []slot.Message{
			{Name: "BeforeCreateRequest", Fields: []slot.MessageField{
				{Name: "input", Type: "stego.common.CreateRequest"},
				{Name: "caller", Type: "stego.common.Identity"},
			}},
		},
	}
	bcOps, err := slot.GenerateOperators(slotsPackage+"/before_create_ops.go", "slots", beforeCreateProto)
	if err != nil {
		t.Fatalf("GenerateOperators(before_create): %v", err)
	}
	oecIfaceFile := gen.File{
		Path: slotsPackage + "/on_entity_changed.go",
		Content: []byte(`package slots

import "context"

// OnEntityChangedRequest is generated from proto message OnEntityChangedRequest.
type OnEntityChangedRequest struct {
	Entity string
	Action string
}

// OnEntityChangedSlot is the interface that fills implement for the OnEntityChanged slot.
type OnEntityChangedSlot interface {
	Evaluate(ctx context.Context, req *OnEntityChangedRequest) (*SlotResult, error)
}
`),
	}
	onEntityChangedProto := &slot.ProtoFile{
		Package: "stego.mixins.event_publisher.slots",
		Services: []slot.Service{
			{Name: "OnEntityChanged", Methods: []slot.Method{
				{Name: "Evaluate", InputType: "OnEntityChangedRequest", OutputType: "stego.common.SlotResult"},
			}},
		},
		Messages: []slot.Message{
			{Name: "OnEntityChangedRequest", Fields: []slot.MessageField{
				{Name: "entity", Type: "string"},
				{Name: "action", Type: "string"},
			}},
		},
	}
	oecOps, err := slot.GenerateOperators(slotsPackage+"/on_entity_changed_ops.go", "slots", onEntityChangedProto)
	if err != nil {
		t.Fatalf("GenerateOperators(on_entity_changed): %v", err)
	}

	// Validate slot: request type has Input + entity string (no Caller).
	valIfaceFile := gen.File{
		Path: slotsPackage + "/validate.go",
		Content: []byte(`package slots

import "context"

// ValidateRequest is generated from proto message ValidateRequest.
type ValidateRequest struct {
	Input  *CreateRequest
	Entity string
}

// ValidateSlot is the interface that fills implement for the Validate slot.
type ValidateSlot interface {
	Evaluate(ctx context.Context, req *ValidateRequest) (*SlotResult, error)
}
`),
	}
	validateProto := &slot.ProtoFile{
		Package: "stego.components.rest_api.slots",
		Services: []slot.Service{
			{Name: "Validate", Methods: []slot.Method{
				{Name: "Evaluate", InputType: "ValidateRequest", OutputType: "stego.common.SlotResult"},
			}},
		},
		Messages: []slot.Message{
			{Name: "ValidateRequest", Fields: []slot.MessageField{
				{Name: "input", Type: "stego.common.CreateRequest"},
				{Name: "entity", Type: "string"},
			}},
		},
	}
	valOps, err := slot.GenerateOperators(slotsPackage+"/validate_ops.go", "slots", validateProto)
	if err != nil {
		t.Fatalf("GenerateOperators(validate): %v", err)
	}

	slotFiles := []gen.File{commonTypesFile, bcIfaceFile, bcOps, valIfaceFile, valOps, oecIfaceFile, oecOps}

	// 3. Assemble main.go from wiring + slot bindings.
	assemblerInput := compiler.AssemblerInput{
		ModuleName:  moduleName,
		ServiceName: "test-svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []compiler.ComponentWiring{
			{Name: "rest-api", Wiring: wiring},
		},
		SlotBindings: ctx.SlotBindings,
		SlotsPackage: slotsPackage,
	}
	assembledFiles, err := compiler.Assemble(assemblerInput)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	// 4. Write all files to a temp directory and compile.
	tmpDir := t.TempDir()

	goMod := "module " + moduleName + "\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}

	// Write slot files.
	for _, f := range slotFiles {
		dst := filepath.Join(tmpDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatalf("mkdir for %s: %v", f.Path, err)
		}
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}

	// Write rest-api handler/router files.
	for _, f := range apiFiles {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		dst := filepath.Join(tmpDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatalf("mkdir for %s: %v", f.Path, err)
		}
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}

	// Write assembled files (main.go, go.mod — skip assembled go.mod since
	// we already wrote one).
	for _, f := range assembledFiles {
		if f.Path == "go.mod" {
			continue
		}
		dst := filepath.Join(tmpDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatalf("mkdir for %s: %v", f.Path, err)
		}
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}

	// Write stub fill packages that implement the slot interfaces.
	fillSlotTypes := map[string]struct{ iface, reqType, pkg string }{
		"rbac-policy":   {iface: "BeforeCreateSlot", reqType: "BeforeCreateRequest", pkg: "rbacpolicy"},
		"validate-fill": {iface: "ValidateSlot", reqType: "ValidateRequest", pkg: "validatefill"},
		"audit-logger":  {iface: "OnEntityChangedSlot", reqType: "OnEntityChangedRequest", pkg: "auditlogger"},
	}
	for fillName, info := range fillSlotTypes {
		fillDir := filepath.Join(tmpDir, "fills", fillName)
		if err := os.MkdirAll(fillDir, 0755); err != nil {
			t.Fatalf("mkdir for fill %s: %v", fillName, err)
		}
		stub := "package " + info.pkg + "\n\n" +
			"import (\n\t\"context\"\n\tslots \"" + moduleName + "/" + slotsPackage + "\"\n)\n\n" +
			"type impl struct{}\n\n" +
			"func New() slots." + info.iface + " { return &impl{} }\n\n" +
			"func (i *impl) Evaluate(_ context.Context, _ *slots." + info.reqType + ") (*slots.SlotResult, error) {\n" +
			"\treturn &slots.SlotResult{Ok: true}, nil\n" +
			"}\n"
		if err := os.WriteFile(filepath.Join(fillDir, "fill.go"), []byte(stub), 0644); err != nil {
			t.Fatalf("writing fill stub %s: %v", fillName, err)
		}
	}

	// Write a stub storage package so the `store` variable resolves in main.go.
	// The wiring constructor is `api.NewUserHandler(store, ...)` — the assembler
	// creates `store` from the postgres-adapter wiring. Since we only have the
	// rest-api wiring here, we need to provide a store variable.
	storeStub := filepath.Join(tmpDir, "cmd", "store_stub.go")
	if err := os.MkdirAll(filepath.Dir(storeStub), 0755); err != nil {
		t.Fatalf("mkdir for store stub: %v", err)
	}
	storeStubContent := "package main\n\nimport api \"" + moduleName + "/internal/api\"\n\nvar store api.Storage\n"
	if err := os.WriteFile(storeStub, []byte(storeStubContent), 0644); err != nil {
		t.Fatalf("writing store stub: %v", err)
	}

	// Compile the entire module.
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cross-generator integration compilation failed:\n%s\n%s", err, output)
	}
}
