package restapi

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
