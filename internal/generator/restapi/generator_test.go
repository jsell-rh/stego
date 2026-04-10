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
		Collections: []types.Collection{
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList},
			},
		},
		OutputNamespace: "internal/api",
	}
}

func TestGenerate_EmptyCollections(t *testing.T) {
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

	// Expect 4 files: handler_user.go, router.go, errors.go, openapi.json
	if len(files) != 4 {
		t.Fatalf("expected 4 files, got %d", len(files))
	}

	// Verify file paths.
	expectedPaths := map[string]bool{
		"internal/api/handler_users.go": true,
		"internal/api/router.go":        true,
		"internal/api/errors.go":        true,
		"internal/api/openapi.json":     true,
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
	if !strings.Contains(wiring.Constructors[0], "NewUsersHandler") {
		t.Errorf("constructor should reference NewUsersHandler, got: %s", wiring.Constructors[0])
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

	handlerContent := findFileContent(t, files, "internal/api/handler_users.go")

	for _, method := range []string{"create", "read", "update", "delete", "list"} {
		if !strings.Contains(handlerContent, "func (h *UsersHandler) "+strings.ToUpper(method[:1])+method[1:]+"(") {
			t.Errorf("handler missing %s method", method)
		}
	}

	if !strings.Contains(handlerContent, "type UsersHandler struct") {
		t.Error("handler missing UsersHandler struct")
	}
	if !strings.Contains(handlerContent, "func NewUsersHandler(") {
		t.Error("handler missing NewUsersHandler constructor")
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

func TestGenerate_WiringUsesMethodPatternRoutes(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	_, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wiring == nil {
		t.Fatal("wiring is nil")
	}

	// Go 1.22 method+pattern routes via Wiring.Routes.
	routeStr := strings.Join(wiring.Routes, "\n")
	for _, want := range []string{
		`"POST /users"`,
		`"GET /users/{id}"`,
		`"PUT /users/{id}"`,
		`"DELETE /users/{id}"`,
		`"GET /users"`,
	} {
		if !strings.Contains(routeStr, want) {
			t.Errorf("wiring routes missing %s route", want)
		}
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
		Collections: []types.Collection{
			{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList}},
			{
				Name:       "node-pools",
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList},
				Scope:      map[string]string{"cluster_id": "Cluster"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// NodePool handler should verify ancestor Cluster exists via checkAncestors.
	npHandler := findFileContent(t, files, "internal/api/handler_node_pools.go")
	if !strings.Contains(npHandler, "checkAncestors") {
		t.Error("nested handler missing checkAncestors method")
	}
	if !strings.Contains(npHandler, `h.store.Exists(r.Context(), "Cluster"`) {
		t.Error("nested handler missing parent Exists check")
	}
	if !strings.Contains(npHandler, `NotFound("Cluster"`) {
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
		Collections: []types.Collection{
			{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpRead}, PathPrefix: "/clusters"},
			{
				Name:       "node-pools",
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"cluster_id": "Cluster"},
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"org_id": "Organization"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_users.go")
	// Must use PathValue with the scope field name as the path parameter name.
	if !strings.Contains(handler, `r.PathValue("org_id")`) {
		t.Error("scope with parent must extract value from scope field path parameter (org_id)")
	}
	if !strings.Contains(handler, `h.store.List(r.Context(), "User", "org_id", scopeValue, opts)`) {
		t.Error("scope filtering must pass the scope field name and opts to store.List")
	}

	// List handler must parse page/size (spec-defined) query parameters, not
	// raw offset/limit.
	if !strings.Contains(handler, `r.URL.Query().Get("page")`) {
		t.Error("list handler must parse 'page' query parameter (spec-defined pagination)")
	}
	if !strings.Contains(handler, `r.URL.Query().Get("size")`) {
		t.Error("list handler must parse 'size' query parameter (spec-defined pagination)")
	}
	// Default page=1, size=100.
	if !strings.Contains(handler, "page = 1") {
		t.Error("list handler must default page to 1 when missing or invalid")
	}
	if !strings.Contains(handler, "size = 100") {
		t.Error("list handler must default size to 100 when missing or invalid")
	}
	// Clamp size to 65500 (PostgreSQL parameter limit).
	if !strings.Contains(handler, "size > 65500") {
		t.Error("list handler must clamp size to max 65500")
	}
	// Build ListOptions with Page and Size.
	if !strings.Contains(handler, "opts := ListOptions{Page: page, Size: size") {
		t.Error("list handler must construct ListOptions with Page and Size")
	}

	// List handler must capture listResult from store.List.
	if !strings.Contains(handler, "listResult, err := h.store.List(") {
		t.Error("list handler must capture listResult from store.List 2-return-value")
	}

	// In bare mode (no envelope), list handler encodes items directly.
	if !strings.Contains(handler, "listResult.Items") {
		t.Error("list handler must use listResult.Items")
	}
}

func TestGenerate_ListStorageInterfaceReturnsTotalCount(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpList}},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	router := findFileContent(t, files, "internal/api/router.go")

	// Storage interface List must accept ListOptions and return (ListResult, error).
	if !strings.Contains(router, "opts ListOptions) (ListResult, error)") {
		t.Error("Storage.List must accept ListOptions and return (ListResult, error) for pagination")
	}
}

func TestGenerate_ListResponseIncludesPaginationEnvelope(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat", ResponseFormat: "envelope"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpList}},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_items.go")

	// List handler must import reflect for computing actual item count.
	if !strings.Contains(handler, `"reflect"`) {
		t.Error("list handler must import reflect for actual item count computation")
	}

	// List handler must construct a pagination envelope with kind, page, size, total, items.
	if !strings.Contains(handler, `"kind":  "ItemList"`) {
		t.Error("list response must include kind with entity name + 'List'")
	}
	if !strings.Contains(handler, `"page":  page`) {
		t.Error("list response must include the requested page number")
	}
	if !strings.Contains(handler, `"size":  actualSize`) {
		t.Error("list response must include the actual number of items returned")
	}
	if !strings.Contains(handler, `itemsSlice := reflect.ValueOf(listResult.Items)`) {
		t.Error("list response must use reflect to access list items")
	}
	if !strings.Contains(handler, `actualSize := itemsSlice.Len()`) {
		t.Error("list response must compute actual item count via itemsSlice.Len()")
	}
	if !strings.Contains(handler, `"total": listResult.Total`) {
		t.Error("list response must include total count of matching records")
	}
	if !strings.Contains(handler, `"items":`) {
		t.Error("list response must include items array")
	}
}

func TestGenerate_ScopeFilteringWithoutParent(t *testing.T) {
	// When scope is set with a parent entity that IS in the collections list,
	// the scope value comes from the parent's path parameter (nested routing).
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"org_id": "Organization"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_users.go")
	// Scope with parent entity in collections list uses scope field as path parameter.
	if !strings.Contains(handler, `r.PathValue("org_id")`) {
		t.Error("scope with parent in collections must extract value from scope field path parameter")
	}
	if !strings.Contains(handler, `h.store.List(r.Context(), "User", "org_id", scopeValue, opts)`) {
		t.Error("scope filtering must pass the scope field name and opts to store.List")
	}
}

func TestGenerate_ParentOnlyListPassesRawFieldName(t *testing.T) {
	// When an entity has scope pointing to a parent entity that is in
	// collections, the List method uses the scope+parent path: scope value
	// comes from the path parameter, and the raw YAML field name (e.g.
	// "cluster_id") is passed to store.List.
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
		Collections: []types.Collection{
			{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "node-pools",
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"cluster_id": "Cluster"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_node_pools.go")
	// Must pass raw YAML field name "cluster_id", not PascalCase "ClusterID".
	if !strings.Contains(handler, `h.store.List(r.Context(), "NodePool", "cluster_id", scopeValue, opts)`) {
		t.Error("scope+parent List must pass raw YAML field name to store.List, not PascalCase")
	}
	if strings.Contains(handler, `h.store.List(r.Context(), "NodePool", "ClusterID"`) {
		t.Error("scope+parent List must not pass PascalCase field name to store.List")
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
		Collections: []types.Collection{
			{
				Name:        "adapter-statuses",
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

	handler := findFileContent(t, files, "internal/api/handler_adapter_statuses.go")
	if !strings.Contains(handler, "func (h *AdapterStatusesHandler) Upsert(") {
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

func TestGenerate_UpsertStatusCodes(t *testing.T) {
	// Verifies that the upsert handler distinguishes created (201),
	// updated (200), and conflict (409) via the store return values.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "generation", Type: types.FieldTypeInt64},
			}},
		},
		Collections: []types.Collection{
			{
				Name:        "widgets",
				Entity:      "Widget",
				Operations:  []types.Operation{types.OpUpsert},
				UpsertKey:   []string{"name"},
				Concurrency: types.ConcurrencyOptimistic,
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	// Handler must use the (bool, error) return from store.Upsert.
	if !strings.Contains(handler, "created, err := h.store.Upsert(") {
		t.Error("handler must capture 'created' return value from store.Upsert")
	}

	// Handler must check for ErrConflict and return 409.
	if !strings.Contains(handler, "errors.Is(err, ErrConflict)") {
		t.Error("handler must check for ErrConflict")
	}
	if !strings.Contains(handler, "Conflict(") {
		t.Error("handler must call Conflict() for optimistic concurrency failure")
	}

	// Handler must return 201 when created.
	if !strings.Contains(handler, "http.StatusCreated") {
		t.Error("handler must return 201 Created for new inserts")
	}

	// Handler must return 200 when updated.
	if !strings.Contains(handler, "http.StatusOK") {
		t.Error("handler must return 200 OK for updates")
	}

	// Router must define ErrConflict sentinel.
	router := findFileContent(t, files, "internal/api/router.go")
	if !strings.Contains(router, "ErrConflict") {
		t.Error("router must define ErrConflict sentinel variable")
	}

	// Store interface must return (bool, error) for Upsert.
	if !strings.Contains(router, "Upsert(ctx context.Context, entity string, value any, upsertKey []string, concurrency string) (bool, error)") {
		t.Error("Store.Upsert must return (bool, error)")
	}
}

func TestGenerate_UpsertOpenAPIResponses(t *testing.T) {
	// Verifies that the OpenAPI spec includes 200, 201, and 409 responses
	// for upsert operations.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "widgets",
				Entity:     "Widget",
				Operations: []types.Operation{types.OpUpsert},
				UpsertKey:  []string{"name"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiBytes := findFileBytes(t, files, "internal/api/openapi.json")
	var specMap map[string]any
	if err := json.Unmarshal(openapiBytes, &specMap); err != nil {
		t.Fatalf("invalid OpenAPI JSON: %v", err)
	}

	paths := specMap["paths"].(map[string]any)
	widgetsPath := paths["/widgets"].(map[string]any)
	putOp := widgetsPath["put"].(map[string]any)
	responses := putOp["responses"].(map[string]any)

	if _, ok := responses["200"]; !ok {
		t.Error("upsert OpenAPI must include 200 response")
	}
	if _, ok := responses["201"]; !ok {
		t.Error("upsert OpenAPI must include 201 response")
	}
	if _, ok := responses["409"]; !ok {
		t.Error("upsert OpenAPI must include 409 response")
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
		Collections: []types.Collection{
			{
				Name:       "items",
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

	handler := findFileContent(t, files, "internal/api/handler_items.go")
	if !strings.Contains(handler, "func (h *ItemsHandler) Update(") {
		t.Error("handler missing update method")
	}
	if !strings.Contains(handler, "func (h *ItemsHandler) Upsert(") {
		t.Error("handler missing upsert method")
	}

	// Update uses PUT /items/{id}, upsert uses PUT /items — verify via wiring routes.
	_, wiring, err2 := g.Generate(ctx)
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if wiring == nil {
		t.Fatal("wiring is nil")
	}
	routeStr := strings.Join(wiring.Routes, "\n")
	if !strings.Contains(routeStr, `"PUT /items/{id}"`) {
		t.Error("wiring routes missing PUT /items/{id} for update")
	}
	if !strings.Contains(routeStr, `"PUT /items"`) {
		t.Error("wiring routes missing PUT /items for upsert")
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
		Collections: []types.Collection{
			{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpCreate, types.OpRead}},
			{
				Name:       "node-pools",
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList},
				Scope:      map[string]string{"cluster_id": "Cluster"},
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

	// GET (list) must declare cluster_id parameter and pagination params.
	getOp := npColOps["get"].(map[string]any)
	getParams, _ := getOp["parameters"].([]any)
	if !hasParam(getParams, "cluster_id") {
		t.Error("GET /clusters/{cluster_id}/nodepools missing cluster_id parameter")
	}
	if !hasParam(getParams, "page") {
		t.Error("GET (list) must declare 'page' query parameter in OpenAPI spec")
	}
	if !hasParam(getParams, "size") {
		t.Error("GET (list) must declare 'size' query parameter in OpenAPI spec")
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
	// When scope references a parent entity that is in the collections list,
	// the handler reads the scope value from the parent's path parameter.
	// The OpenAPI spec must declare this path parameter on the nested route.
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"org_id": "Organization"},
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
	// With scope pointing to Organization in collections, the route is nested.
	nestedPath := paths["/organizations/{org_id}/users"].(map[string]any)
	getOp := nestedPath["get"].(map[string]any)
	params, ok := getOp["parameters"].([]any)
	if !ok || len(params) == 0 {
		t.Fatal("GET nested users (list with scope+parent) must have parameters declared")
	}

	// Find the parent path parameter — uses scope field name.
	foundPathParam := false
	for _, p := range params {
		param := p.(map[string]any)
		if param["name"] == "org_id" && param["in"] == "path" {
			foundPathParam = true
		}
	}
	if !foundPathParam {
		t.Error("OpenAPI list operation must declare org_id path parameter when scope references parent in collections")
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
		Collections: []types.Collection{
			{Name: "things", Entity: "Thing", Operations: []types.Operation{types.OpRead}},
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
	}

	for _, tt := range tests {
		p := props[tt.field].(map[string]any)
		if p["type"] != tt.wantType {
			t.Errorf("field %s: expected type %q, got %q", tt.field, tt.wantType, p["type"])
		}
	}

	// jsonb fields should have no "type" constraint (accepts any JSON value)
	// and a description indicating arbitrary JSON.
	jsonbField := props["j"].(map[string]any)
	if _, hasType := jsonbField["type"]; hasType {
		t.Errorf("jsonb field j: expected no type constraint, got type=%q", jsonbField["type"])
	}
	if desc, ok := jsonbField["description"].(string); !ok || desc == "" {
		t.Errorf("jsonb field j: expected non-empty description, got %q", jsonbField["description"])
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
		Collections: []types.Collection{
			{
				Name:       "clusters",
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

	handler := findFileContent(t, files, "internal/api/handler_clusters.go")

	// Each write method must zero the computed field after decode.
	for _, method := range []string{"Create", "Update", "Upsert"} {
		if !strings.Contains(handler, "func (h *ClustersHandler) "+method+"(") {
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
		Collections: []types.Collection{
			{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
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
		Collections: []types.Collection{
			{
				Name:       "clusters",
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
		Collections: []types.Collection{
			{Name: "missings", Entity: "Missing", Operations: []types.Operation{types.OpRead}},
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpCreate, types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpList},
				Scope:      map[string]string{"org_id": "Organization"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have handler_organizations.go, handler_users.go, router.go, errors.go, openapi.json
	if len(files) != 5 {
		t.Fatalf("expected 5 files, got %d", len(files))
	}

	expectedPaths := map[string]bool{
		"internal/api/handler_organizations.go": true,
		"internal/api/handler_users.go":         true,
		"internal/api/router.go":                true,
		"internal/api/errors.go":                true,
		"internal/api/openapi.json":             true,
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
		Collections: []types.Collection{
			{Name: "events", Entity: "Event", Operations: []types.Operation{types.OpList}},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_events.go")
	if !strings.Contains(handler, "func (h *EventsHandler) List(") {
		t.Error("handler missing list method")
	}
	// Should not have a read method.
	if strings.Contains(handler, "func (h *EventsHandler) Read(") {
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
		Collections: []types.Collection{
			{
				Name:       "organizations",
				Entity:     "Organization",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpPatch},
				Patchable:  []string{"name", "metadata"},
			},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList, types.OpUpsert},
				Scope:      map[string]string{"org_id": "Organization"},
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

func TestCollectionBasePath_Default(t *testing.T) {
	eb := types.Collection{Name: "users", Entity: "User"}
	got, err := collectionBasePath(eb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/users" {
		t.Errorf("expected /users, got %s", got)
	}
}

func TestCollectionBasePath_WithPathPrefix(t *testing.T) {
	eb := types.Collection{Name: "users", Entity: "User", PathPrefix: "/api/v1/users"}
	got, err := collectionBasePath(eb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/api/v1/users" {
		t.Errorf("expected /api/v1/users, got %s", got)
	}
}

func TestCollectionBasePath_Nested(t *testing.T) {
	collectionMap := map[string]types.Collection{
		"Cluster": {Name: "clusters", Entity: "Cluster"},
	}
	eb := types.Collection{Name: "cluster-nodepools", Entity: "NodePool", Scope: map[string]string{"cluster_id": "Cluster"}}
	got, err := collectionBasePath(eb, collectionMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/clusters/{cluster_id}/nodepools" {
		t.Errorf("expected /clusters/{cluster_id}/nodepools, got %s", got)
	}
}

func TestCollectionBasePath_MultiLevel(t *testing.T) {
	collectionMap := map[string]types.Collection{
		"Cluster":  {Name: "clusters", Entity: "Cluster"},
		"NodePool": {Name: "cluster-nodepools", Entity: "NodePool", Scope: map[string]string{"cluster_id": "Cluster"}},
	}
	eb := types.Collection{Name: "adapter-statuses", Entity: "AdapterStatus", Scope: map[string]string{"nodepool_id": "NodePool"}}
	got, err := collectionBasePath(eb, collectionMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/clusters/{cluster_id}/nodepools/{nodepool_id}/adapterstatuses"
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

func TestEntityPathSegment(t *testing.T) {
	tests := []struct {
		entity string
		want   string
	}{
		{"User", "users"},
		{"Cluster", "clusters"},
		{"NodePool", "nodepools"},
		{"AdapterStatus", "adapterstatuses"},
		{"Organization", "organizations"},
		{"OrgSetting", "orgsettings"},
		{"Address", "addresses"},
		{"Entity", "entities"},
		{"Box", "boxes"},
		{"Status", "statuses"},
		{"Index", "indexes"},
	}
	for _, tt := range tests {
		got := entityPathSegment(tt.entity)
		if got != tt.want {
			t.Errorf("entityPathSegment(%q) = %q, want %q", tt.entity, got, tt.want)
		}
	}
}

func TestPluralize(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"user", "users"},
		{"cluster", "clusters"},
		{"node_pool", "node_pools"},
		{"status", "statuses"},
		{"address", "addresses"},
		{"box", "boxes"},
		{"entity", "entities"},
		{"index", "indexes"},
		{"", ""},
	}
	for _, tt := range tests {
		got := pluralize(tt.input)
		if got != tt.want {
			t.Errorf("pluralize(%q) = %q, want %q", tt.input, got, tt.want)
		}
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
		Collections: []types.Collection{
			{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "node-pools",
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpUpsert},
				Scope:      map[string]string{"cluster_id": "Cluster"},
				UpsertKey:  []string{"name"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_node_pools.go")
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
		Collections: []types.Collection{
			{Name: "sessions", Entity: "Session", Operations: []types.Operation{types.OpDelete}},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_sessions.go")
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
		Collections: []types.Collection{
			{
				Name:       "items",
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

	handler := findFileContent(t, files, "internal/api/handler_items.go")
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
		Collections: []types.Collection{
			{
				Name:       "records",
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

	handler := findFileContent(t, files, "internal/api/handler_records.go")
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
		Collections: []types.Collection{
			{
				Name:       "items",
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
		Collections: []types.Collection{
			{
				Name:       "items",
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
				Collections: []types.Collection{
					{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
					{
						Name:       "widgets",
						Entity:     "Widget",
						Operations: tt.ops,
						Scope:      map[string]string{"cluster_id": "Cluster"},
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
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpRead}},
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
		Collections: []types.Collection{
			{
				Name:       "types",
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
		Collections: []types.Collection{
			{Name: "resources", Entity: "Resource", Operations: []types.Operation{types.OpRead}},
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
		Collections: []types.Collection{
			{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpCreate, types.OpRead}},
			{
				Name:       "node-pools",
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList},
				Scope:      map[string]string{"cluster_id": "Cluster"},
			},
			{
				Name:       "adapter-statuses",
				Entity:     "AdapterStatus",
				Operations: []types.Operation{types.OpList, types.OpUpsert},
				Scope:      map[string]string{"nodepool_id": "NodePool"},
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
	asHandler := findFileContent(t, files, "internal/api/handler_adapter_statuses.go")

	if !strings.Contains(asHandler, "checkAncestors") {
		t.Error("AdapterStatus handler missing checkAncestors method")
	}
	// Must check Cluster existence (grandparent).
	if !strings.Contains(asHandler, `h.store.Exists(r.Context(), "Cluster"`) {
		t.Error("AdapterStatus handler must verify Cluster (grandparent) existence")
	}
	// Must check NodePool existence (parent).
	if !strings.Contains(asHandler, `h.store.Exists(r.Context(), "NodePool"`) {
		t.Error("AdapterStatus handler must verify NodePool (parent) existence")
	}
	// Cluster check must come before NodePool check (top-down order).
	clusterIdx := strings.Index(asHandler, `h.store.Exists(r.Context(), "Cluster"`)
	nodepoolIdx := strings.Index(asHandler, `h.store.Exists(r.Context(), "NodePool"`)
	if clusterIdx > nodepoolIdx {
		t.Error("Cluster (grandparent) verification must come before NodePool (parent)")
	}

	// NodePool handler should still only verify Cluster (its single ancestor).
	npHandler := findFileContent(t, files, "internal/api/handler_node_pools.go")
	if !strings.Contains(npHandler, `h.store.Exists(r.Context(), "Cluster"`) {
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
		Collections: []types.Collection{
			{
				Name:       "nodes",
				Entity:     "Node",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"node_id": "Node"},
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
		Collections: []types.Collection{
			{
				Name:       "alphas",
				Entity:     "Alpha",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"beta_id": "Beta"},
			},
			{
				Name:       "betas",
				Entity:     "Beta",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"alpha_id": "Alpha"},
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

func TestCollectionBasePath_CircularParentReturnsError(t *testing.T) {
	collectionMap := map[string]types.Collection{
		"A": {Name: "as", Entity: "A", Scope: map[string]string{"b_id": "B"}},
		"B": {Name: "bs", Entity: "B", Scope: map[string]string{"a_id": "A"}},
	}
	_, err := collectionBasePath(collectionMap["A"], collectionMap)
	if err == nil {
		t.Fatal("expected error for circular parent reference")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error should mention circular, got: %v", err)
	}
}

func TestCollectAncestors_CircularParentReturnsError(t *testing.T) {
	collectionMap := map[string]types.Collection{
		"A": {Entity: "A", Scope: map[string]string{"b_id": "B"}},
		"B": {Entity: "B", Scope: map[string]string{"a_id": "A"}},
	}
	_, err := collectAncestors(collectionMap["A"], collectionMap)
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
		Collections: []types.Collection{
			{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "node-pools",
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpUpdate},
				Scope:      map[string]string{"cluster_id": "Cluster"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_node_pools.go")
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
		Collections: []types.Collection{
			{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "widgets",
				Entity:     "Widget",
				Operations: []types.Operation{types.OpUpdate},
				Scope:      map[string]string{"cluster_id": "Cluster"},
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
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpRead}},
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

func TestGenerate_CollectionNamedStorageReturnsError(t *testing.T) {
	// A collection named "storage" produces collectionToPascalCase → "Storage",
	// which collides with the generated Storage interface in router.go.
	// The generator must return a clear error at generation time.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Warehouse", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "storage",
				Entity:     "Warehouse",
				Operations: []types.Operation{types.OpCreate, types.OpRead},
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for collection named 'storage' (collides with generated Storage interface)")
	}
	if !strings.Contains(err.Error(), "Storage") {
		t.Errorf("error should mention 'Storage', got: %v", err)
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("error should mention collision, got: %v", err)
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
		// Generator-emitted locals in Patch method.
		{"existing", "existing_"},
		{"patch", "patch_"},
		// Generator-emitted locals in Upsert method.
		{"created", "created_"},
		{"upsertKey", "upsertKey_"},
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

	collNames := map[string]string{"W": "ws", "R": "rs", "H": "hs"}
	for _, name := range []string{"W", "R", "H"} {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{
				Conventions: types.Convention{Layout: "flat"},
				Entities: []types.Entity{
					{Name: name, Fields: []types.Field{
						{Name: "name", Type: types.FieldTypeString},
					}},
				},
				Collections: []types.Collection{
					{
						Name:       collNames[name],
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

	importCollNames := map[string]string{"Json": "jsons", "Http": "https", "Time": "times"}
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
				Collections: []types.Collection{
					{
						Name:       importCollNames[name],
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
		Collections: []types.Collection{
			{
				Name:       "users",
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
	handlerContent := findFileContent(t, files, "pkg/http/handler_users.go")
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

func TestGenerate_OpenAPISchemasOnlyForCollectionEntities(t *testing.T) {
	// Finding 30: OpenAPI schema generation must iterate only over collection entities,
	// not all entities in ctx.Entities. Entities not referenced by any collection
	// (e.g. ref targets managed by a different component) should not appear in the spec.
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
			// Team is in Entities (for ref resolution) but NOT in Collections.
			{Name: "Team", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
			}},
		},
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpCreate, types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpRead, types.OpList},
				Scope:      map[string]string{"org_id": "Organization"},
			},
			// Team has no collection.
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

	// Organization and User should be present (they have collections).
	if _, ok := schemas["Organization"]; !ok {
		t.Error("entity Organization with a collection should have an OpenAPI schema")
	}
	if _, ok := schemas["User"]; !ok {
		t.Error("entity User with a collection should have an OpenAPI schema")
	}

	// Team should NOT be present (no collection references it).
	if _, ok := schemas["Team"]; ok {
		t.Error("entity Team with no collection should NOT have an OpenAPI schema — it has no path operations")
	}
}

func TestGenerate_ParentNotInCollectionsListReturnsError(t *testing.T) {
	// Finding 31: When a collection references a parent entity that is not
	// itself in the collections list, the generator must return an error at
	// generation time rather than producing silently non-functional code.
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
		Collections: []types.Collection{
			// NodePool references parent Cluster, but Cluster has no collection.
			{
				Name:       "node-pools",
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList},
				Scope:      map[string]string{"cluster_id": "Cluster"},
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error when parent entity is not in collections list, got nil")
	}
	if !strings.Contains(err.Error(), "Cluster") {
		t.Errorf("error should mention the missing parent entity 'Cluster', got: %v", err)
	}
	if !strings.Contains(err.Error(), "node-pools") {
		t.Errorf("error should mention the referencing collection 'node-pools', got: %v", err)
	}
	if !strings.Contains(err.Error(), "collection") {
		t.Errorf("error should mention collection, got: %v", err)
	}
}

func TestGenerate_MultipleParentsNotInCollectionsListReportsAll(t *testing.T) {
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
		Collections: []types.Collection{
			{Name: "teams", Entity: "Team", Operations: []types.Operation{types.OpList}, Scope: map[string]string{"org_id": "Org"}},
			{Name: "projects", Entity: "Project", Operations: []types.Operation{types.OpList}, Scope: map[string]string{"team_id": "Team"}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error when parent entities are not in collections list, got nil")
	}
	// Org is not in the collections list, so Team's reference should fail.
	if !strings.Contains(err.Error(), "Org") {
		t.Errorf("error should mention missing parent 'Org', got: %v", err)
	}
}

func TestGenerate_PathPrefixDivergentParamNames(t *testing.T) {
	// When path_prefix is set with parent, the generated handler code must use
	// the actual prefix parameter names in r.PathValue() calls.
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}, PathPrefix: "/orgs"},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpList},
				Scope:      map[string]string{"org_id": "Organization"},
				PathPrefix: "/orgs/{org_id}/users",
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_users.go")

	// checkAncestors must use {org_id} from the prefix.
	if !strings.Contains(handler, `r.PathValue("org_id")`) {
		t.Error("checkAncestors must use prefix parameter name 'org_id'")
	}
	if strings.Contains(handler, `r.PathValue("organization_id")`) {
		t.Error("handler must NOT use 'organization_id' when path_prefix provides 'org_id'")
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}, PathPrefix: "/orgs"},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpList, types.OpCreate},
				Scope:      map[string]string{"org_id": "Organization"},
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}, PathPrefix: "/orgs"},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"org_id": "Organization"},
				PathPrefix: "/orgs/{org_id}/users",
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_users.go")
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
		Collections: []types.Collection{
			{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpRead}, PathPrefix: "/clusters"},
			{
				Name:       "node-pools",
				Entity:     "NodePool",
				Operations: []types.Operation{types.OpRead, types.OpList},
				Scope:      map[string]string{"cluster_id": "Cluster"},
				PathPrefix: "/clusters/{cid}/pools",
			},
			{
				Name:       "adapter-statuses",
				Entity:     "AdapterStatus",
				Operations: []types.Operation{types.OpList, types.OpUpsert},
				Scope:      map[string]string{"nodepool_id": "NodePool"},
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

	asHandler := findFileContent(t, files, "internal/api/handler_adapter_statuses.go")

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
	collectionMap := map[string]types.Collection{
		"Cluster":  {Entity: "Cluster"},
		"NodePool": {Entity: "NodePool", Scope: map[string]string{"cluster_id": "Cluster"}},
	}
	// path_prefix has 2 params but NodePool only has 1 ancestor (Cluster).
	eb := types.Collection{
		Entity:     "Widget",
		Scope:      map[string]string{"nodepool_id": "NodePool"},
		PathPrefix: "/a/{x}/b/{y}/c/{z}/widgets",
	}
	collectionMap["Widget"] = eb
	collectionMap["NodePool"] = types.Collection{Entity: "NodePool", Scope: map[string]string{"cluster_id": "Cluster"}}

	_, err := resolveAncestorParams(eb, collectionMap)
	if err == nil {
		t.Fatal("expected error for mismatched param count")
	}
	if !strings.Contains(err.Error(), "Widget") {
		t.Errorf("error should mention entity name, got: %v", err)
	}
}

func TestGenerate_MultiFieldScopeRejected(t *testing.T) {
	// Round 4 finding: multi-field scope cardinality must be enforced on the
	// Reconcile() path (via Generate()), not only via Validate(). ScopeField()
	// and ParentEntity() are non-deterministic for maps with >1 entry.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "Team", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "Widget", Fields: []types.Field{
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
				{Name: "team_id", Type: types.FieldTypeRef, To: "Team"},
			}},
		},
		Collections: []types.Collection{
			{Name: "orgs", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{Name: "teams", Entity: "Team", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "org-team-widgets",
				Entity:     "Widget",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"org_id": "Organization", "team_id": "Team"},
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for multi-field scope")
	}
	if !strings.Contains(err.Error(), "multi-field scopes are not yet supported") {
		t.Errorf("error should mention multi-field scope unsupported, got: %v", err)
	}
	if !strings.Contains(err.Error(), "org-team-widgets") {
		t.Errorf("error should mention the collection name, got: %v", err)
	}
}

func TestGenerate_InvalidScopeFieldReturnsError(t *testing.T) {
	// Finding 33: scope field must reference an existing entity field.
	// The scope's parent entity must also be in collections for parent
	// validation to pass, allowing the scope field check to fire.
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"nonexistent_field": "Organization"},
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
		Collections: []types.Collection{
			{
				Name:       "items",
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

func TestGenerate_CrossCollectionHandlerTypeCollisionReturnsError(t *testing.T) {
	// Two collections whose derived PascalCase names produce the same handler
	// type must be rejected. E.g., "user-handler" and "users" where one produces
	// "UserHandler" + "Handler" = "UserHandlerHandler" — but more directly,
	// a collection named "users" produces handler type "UsersHandler", and if
	// another entity has a struct named "UsersHandler", that collides.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "UsersHandler", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpRead}},
			{Name: "users-handlers", Entity: "UsersHandler", Operations: []types.Operation{types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for collection-derived handler type name collision")
	}
	if !strings.Contains(err.Error(), "UsersHandler") {
		t.Errorf("error should mention the colliding name 'UsersHandler', got: %v", err)
	}
	if !strings.Contains(err.Error(), "users") {
		t.Errorf("error should mention the source collection 'users', got: %v", err)
	}
}

func TestGenerate_DuplicateCollectionNamesReturnsError(t *testing.T) {
	// Two collections with the same name must return an error,
	// not silently overwrite in the map or produce duplicate type declarations.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpCreate}},
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for duplicate collection names")
	}
	if !strings.Contains(err.Error(), "users") {
		t.Errorf("error should mention the duplicated collection name 'users', got: %v", err)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention 'duplicate', got: %v", err)
	}
}

func TestGenerate_ValidScopeFieldSucceeds(t *testing.T) {
	// Verify that a valid scope field reference does not produce an error.
	// The scope's parent entity must also be in entities and collections.
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"org_id": "Organization"},
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
		Collections: []types.Collection{
			{
				Name:       "statuses",
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
	// Finding 37: a collection with zero operations must return an error,
	// not produce uncompilable code (unused net/http import, unused handler variable).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for collection with empty operations list")
	}
	if !strings.Contains(err.Error(), "users") {
		t.Errorf("error should mention collection name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "operation") {
		t.Errorf("error should mention operations, got: %v", err)
	}
}

func TestGenerate_EmptyOperationsAmongValid(t *testing.T) {
	// One valid collection + one empty operations collection => error.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}}},
			{Name: "Org", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpRead}},
			{Name: "orgs", Entity: "Org", Operations: []types.Operation{}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for collection with empty operations list")
	}
	if !strings.Contains(err.Error(), "orgs") {
		t.Errorf("error should mention collection with empty operations, got: %v", err)
	}
}

func TestGenerate_DuplicateOperationsReturnsError(t *testing.T) {
	// Finding 44: duplicate operations within a single collection produce
	// duplicate method declarations (compile error), duplicate route
	// registrations (runtime panic), and duplicate OpenAPI entries (overwrite).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpCreate, types.OpCreate, types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for duplicate operations in collection")
	}
	if !strings.Contains(err.Error(), "users") {
		t.Errorf("error should mention the collection with duplicate operations, got: %v", err)
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
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpRead, types.OpRead}},
			{Name: "orgs", Entity: "Org", Operations: []types.Operation{types.OpDelete, types.OpDelete}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for duplicate operations")
	}
	if !strings.Contains(err.Error(), "users") {
		t.Errorf("error should mention users collection, got: %v", err)
	}
	if !strings.Contains(err.Error(), "orgs") {
		t.Errorf("error should mention orgs collection, got: %v", err)
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
		Collections: []types.Collection{
			{Name: "alphas", Entity: "Alpha", Operations: []types.Operation{types.OpList}, PathPrefix: "/items"},
			{Name: "betas", Entity: "Beta", Operations: []types.Operation{types.OpList}, PathPrefix: "/items"},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for route path collision")
	}
	if !strings.Contains(err.Error(), "alphas") || !strings.Contains(err.Error(), "betas") {
		t.Errorf("error should mention both colliding collections, got: %v", err)
	}
	if !strings.Contains(err.Error(), "/items") {
		t.Errorf("error should mention the colliding path, got: %v", err)
	}
}

func TestGenerate_RouteCollisionCaseInsensitive(t *testing.T) {
	// Two collection names that are case-insensitively equivalent produce the
	// same route path (both → /items).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "Widget", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpList}},
			{Name: "Items", Entity: "Widget", Operations: []types.Operation{types.OpList}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for case-insensitive route path collision")
	}
	if !strings.Contains(err.Error(), "items") && !strings.Contains(err.Error(), "Items") {
		t.Errorf("error should mention colliding collections, got: %v", err)
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
		Collections: []types.Collection{
			{Name: "widgets", Entity: "Widget", Operations: []types.Operation{types.OpList}},                        // auto: /widgets
			{Name: "others", Entity: "Other", Operations: []types.Operation{types.OpList}, PathPrefix: "/widgets"}, // explicit: /widgets
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for auto-derived path matching explicit path_prefix")
	}
	if !strings.Contains(err.Error(), "widgets") || !strings.Contains(err.Error(), "others") {
		t.Errorf("error should mention both colliding collections, got: %v", err)
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
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpList}},
			{Name: "teams", Entity: "Team", Operations: []types.Operation{types.OpList}},
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"department": "Organization"}, // department is not the ref field to Organization
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"org_id": "Organization"}, // matches the ref field to Organization
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
		Collections: []types.Collection{
			{Name: "accounts", Entity: "Account", Operations: []types.Operation{types.OpRead}},
			{Name: "transfers", Entity: "Transfer", Operations: []types.Operation{types.OpCreate}, Scope: map[string]string{"account_id": "Account"}},
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
				Collections: []types.Collection{
					{Name: "orgs", Entity: "Org", Operations: []types.Operation{types.OpRead}},
					{Name: "members", Entity: "Member", Operations: tt.ops, Scope: map[string]string{"org_id": "Org"}, UpsertKey: []string{"primary_org_id"}},
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
				Collections: []types.Collection{
					{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
					{Name: "widgets", Entity: "Widget", Operations: tt.ops, Scope: map[string]string{"cluster_id": "Cluster"}},
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
		Collections: []types.Collection{
			{Name: "orgs", Entity: "Org", Operations: []types.Operation{types.OpRead}},
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpDelete}, Scope: map[string]string{"org_id": "Org"}},
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

	idCollNames := map[string]string{"Id": "ids", "ID": "id-items"}
	for _, name := range []string{"Id", "ID"} {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{
				Conventions: types.Convention{Layout: "flat"},
				Entities: []types.Entity{
					{Name: name, Fields: []types.Field{
						{Name: "name", Type: types.FieldTypeString},
					}},
				},
				Collections: []types.Collection{
					{
						Name:       idCollNames[name],
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

func TestGenerate_CollectionDerivedIdentifierCollision(t *testing.T) {
	// Two collections whose names produce the same PascalCase identifier
	// (e.g. "org-users" and "org_users" both → "OrgUsers") must be rejected.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{Name: "org-users", Entity: "User", Operations: []types.Operation{types.OpRead}, PathPrefix: "/org-users"},
			{Name: "org_users", Entity: "User", Operations: []types.Operation{types.OpRead}, PathPrefix: "/org_users"},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for collections with colliding derived identifiers")
	}
	if !strings.Contains(err.Error(), "org-users") {
		t.Errorf("error should mention first collection name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "org_users") {
		t.Errorf("error should mention second collection name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "OrgUsers") {
		t.Errorf("error should mention the colliding identifier, got: %v", err)
	}
}

func TestGenerate_CollectionDerivedIdentifierCollisionAutoPath(t *testing.T) {
	// Verify that derived identifier collision check catches collections whose
	// names produce the same PascalCase even with auto-derived paths.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Foo", Fields: []types.Field{{Name: "a", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "my-foos", Entity: "Foo", Operations: []types.Operation{types.OpRead}, PathPrefix: "/my-foos"},
			{Name: "my_foos", Entity: "Foo", Operations: []types.Operation{types.OpRead}, PathPrefix: "/my_foos"},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for collections with colliding derived identifiers")
	}
	if !strings.Contains(err.Error(), "MyFoos") {
		t.Errorf("error should mention colliding identifier, got: %v", err)
	}
}

func TestGenerate_DifferentCollectionNamesDifferentEnoughSucceeds(t *testing.T) {
	// Collections with distinctly different names should not be rejected.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "Order", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpRead}},
			{Name: "orders", Entity: "Order", Operations: []types.Operation{types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error for collections with distinct names: %v", err)
	}
}

func TestGenerate_EntityNameErrCompiles(t *testing.T) {
	// Finding 45: entity named "Err" or "ERR" produces strings.ToLower → "err",
	// which collides with the hardcoded `err` in the Read method's dual-assignment
	// `%s, err := h.store.Get(...)`. safeVarName must escape "err" to "err_".
	g := &Generator{}

	errCollNames := map[string]string{"Err": "errs", "ERR": "err-items"}
	for _, name := range []string{"Err", "ERR"} {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{
				Conventions: types.Convention{Layout: "flat"},
				Entities: []types.Entity{
					{Name: name, Fields: []types.Field{
						{Name: "name", Type: types.FieldTypeString},
					}},
				},
				Collections: []types.Collection{
					{
						Name:       errCollNames[name],
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
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{
				types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList,
			}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Collection: "users", Gate: []string{"rbac-policy"}},
			{Slot: "on_entity_changed", Collection: "users", FanOut: []string{"audit-logger"}},
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
		if strings.Contains(f.Path, "handler_users.go") {
			handlerContent = string(f.Content)
		}
	}
	if handlerContent == "" {
		t.Fatal("handler_users.go not found")
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
	if !strings.Contains(handlerContent, "func NewUsersHandler(store Storage, beforeCreateGate slots.BeforeCreateSlot, onEntityChangedFanOut slots.OnEntityChangedSlot)") {
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

	// Verify ConstructorCollections wiring.
	if wiring == nil {
		t.Fatal("wiring is nil")
	}
	if wiring.ConstructorCollections == nil || wiring.ConstructorCollections[0] != "users" {
		t.Errorf("unexpected ConstructorCollections: %v", wiring.ConstructorCollections)
	}
}

func TestGenerate_HandlerWithAuthPackage_IdentityFromContext(t *testing.T) {
	// When AuthPackage is set and a before-slot has HasCaller, the generated
	// handler must import the auth package and call IdentityFromContext to
	// extract the caller identity from the request context.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
				{Name: "role", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpCreate, types.OpRead},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:       "before_create",
				Collection: "users",
				Gate:       []string{"test-policy"},
			},
		},
		OutputNamespace: "internal/api",
		ModuleName:      "github.com/myorg/svc",
		SlotsPackage:    "internal/slots",
		AuthPackage:     "github.com/myorg/svc/out/internal/auth",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var handlerContent string
	for _, f := range files {
		if strings.Contains(f.Path, "handler_users.go") {
			handlerContent = string(f.Content)
		}
	}
	if handlerContent == "" {
		t.Fatal("handler_users.go not found")
	}

	// Must import the auth package.
	if !strings.Contains(handlerContent, `auth "github.com/myorg/svc/out/internal/auth"`) {
		t.Errorf("handler should import auth package:\n%s", handlerContent)
	}

	// Must call IdentityFromContext instead of using zero-value Identity.
	if !strings.Contains(handlerContent, "auth.IdentityFromContext(r.Context())") {
		t.Errorf("handler should call auth.IdentityFromContext:\n%s", handlerContent)
	}

	// Must NOT use zero-value Identity{} when auth is available.
	if strings.Contains(handlerContent, `Caller: &slots.Identity{}`) {
		t.Errorf("handler should not use zero-value Identity when auth package is available:\n%s", handlerContent)
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
		if strings.Contains(f.Path, "handler_users.go") {
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
		Collections: []types.Collection{
			{Name: "counters", Entity: "Counter", Operations: []types.Operation{types.OpCreate}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Collection: "counters", Gate: []string{"policy"}},
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
		if strings.Contains(f.Path, "handler_counters.go") {
			handlerContent = string(f.Content)
		}
	}
	if handlerContent == "" {
		t.Fatal("handler_counters.go not found")
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
	// Verify that handler methods with slot bindings include nil guards so that
	// the handler degrades to passthrough semantics when no fills are wired.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{
				{Name: "label", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{Name: "items", Entity: "Item", Operations: []types.Operation{
				types.OpCreate, types.OpUpdate, types.OpDelete, types.OpUpsert,
			}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Collection: "items", Gate: []string{"gate-fill"}},
			{Slot: "validate", Collection: "items", Chain: []string{"validate-fill"}},
			{Slot: "on_entity_changed", Collection: "items", FanOut: []string{"fanout-fill"}},
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
		if strings.Contains(f.Path, "handler_items.go") {
			handlerContent = string(f.Content)
		}
	}
	if handlerContent == "" {
		t.Fatal("handler_items.go not found")
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
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Collection: "users", Gate: []string{"rbac-policy"}},
			{Slot: "validate", Collection: "users", Chain: []string{"validate-fill"}},
			{Slot: "on_entity_changed", Collection: "users", FanOut: []string{"audit-logger"}},
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

	// Write a stub storage file so the `store` variable resolves in main.go.
	// The wiring constructor is `api.NewUserHandler(store, ...)` — the assembler
	// creates `store` from the postgres-adapter wiring. Since we only have the
	// rest-api wiring here, we need to provide a store variable.
	storeStub := filepath.Join(tmpDir, "store_stub.go")
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

// --- base_path tests ---

func TestGenerate_BasePathPrependedToRoutes(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.BasePath = "/api/v1"

	_, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, r := range wiring.Routes {
		if !strings.Contains(r, "/api/v1/users") {
			t.Errorf("route should contain /api/v1/users, got: %s", r)
		}
	}
}

func TestGenerate_BasePathOmittedRoutesFromRoot(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	// BasePath is empty — paths should start from root.

	_, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, r := range wiring.Routes {
		if strings.Contains(r, "/api/") {
			t.Errorf("route should not contain /api/ when base_path is empty, got: %s", r)
		}
	}
	foundRoot := false
	for _, r := range wiring.Routes {
		if strings.Contains(r, "\"/users") || strings.Contains(r, " /users") {
			foundRoot = true
		}
	}
	if !foundRoot {
		t.Errorf("expected route containing /users (from root), got: %v", wiring.Routes)
	}
}

func TestGenerate_BasePathInOpenAPIPaths(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.BasePath = "/api/hyperfleet/v1"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiBytes := findFileBytes(t, files, "internal/api/openapi.json")

	var spec map[string]any
	if err := json.Unmarshal(openapiBytes, &spec); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("missing paths in openapi spec")
	}

	if _, ok := paths["/api/hyperfleet/v1/users"]; !ok {
		t.Errorf("missing /api/hyperfleet/v1/users in openapi paths, got: %v", mapKeys(paths))
	}
	if _, ok := paths["/api/hyperfleet/v1/users/{id}"]; !ok {
		t.Errorf("missing /api/hyperfleet/v1/users/{id} in openapi paths, got: %v", mapKeys(paths))
	}
}

func TestGenerate_BasePathWithNestedRoutes(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "NodePool", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "cluster_id", Type: types.FieldTypeRef, To: "Cluster"},
			}},
		},
		Collections: []types.Collection{
			{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpList}},
			{Name: "nodepools", Entity: "NodePool", Operations: []types.Operation{types.OpList},
				Scope: map[string]string{"cluster_id": "Cluster"}},
		},
		BasePath:        "/api/v1",
		OutputNamespace: "internal/api",
	}

	_, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	routeStr := strings.Join(wiring.Routes, "\n")
	if !strings.Contains(routeStr, "/api/v1/clusters") {
		t.Errorf("expected route with /api/v1/clusters, got:\n%s", routeStr)
	}
	if !strings.Contains(routeStr, "/api/v1/clusters/{cluster_id}/nodepools") {
		t.Errorf("expected route with /api/v1/clusters/{cluster_id}/nodepools, got:\n%s", routeStr)
	}
}

func TestGenerate_BasePathWithPathPrefix(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "widgets", Entity: "Widget", Operations: []types.Operation{types.OpList},
				PathPrefix: "/custom/widgets"},
		},
		BasePath:        "/api/v2",
		OutputNamespace: "internal/api",
	}

	_, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	routeStr := strings.Join(wiring.Routes, "\n")
	if !strings.Contains(routeStr, "/api/v2/custom/widgets") {
		t.Errorf("expected route with /api/v2/custom/widgets, got:\n%s", routeStr)
	}
}

func TestGenerate_BasePathValidation(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.BasePath = "no-leading-slash"

	_, _, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for base_path without leading slash")
	}
	if !strings.Contains(err.Error(), "base_path") {
		t.Errorf("error should mention base_path, got: %v", err)
	}
}

func TestDeriveErrorPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hyperfleet-api", "HYPERFLEET"},            // spec example: strip -api, uppercase
		{"order-service", "ORDER"},                  // spec example: strip -service, uppercase
		{"my-cool-server", "MYCOOL"},                // strip -server suffix
		{"user-management", "USERMANAGEMENT"},       // no suffix to strip, hyphens removed
		{"simple", "SIMPLE"},                        // no hyphens, kept as-is
		{"a-b-c", "ABC"},                            // all segments joined, uppercased
		{"just-api", "JUST"},                        // strip -api from short name
		{"api", "API"},                              // no leading hyphen, not stripped
		{"", ""},                                    // edge case
	}
	for _, tt := range tests {
		got := deriveErrorPrefix(tt.input)
		if got != tt.want {
			t.Errorf("deriveErrorPrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGenerate_ErrorsFileGenerated(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.ServiceName = "hyperfleet-api"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	errorsContent := findFileContent(t, files, "internal/api/errors.go")

	// AC1: ServiceError type with RFC 9457 fields.
	for _, field := range []string{
		"Type ", "Title ", "Status ", "Detail ", "Code ",
		"Instance ", "TraceID ", "Timestamp ",
		"ValidationErrors []ValidationError",
	} {
		if !strings.Contains(errorsContent, field) {
			t.Errorf("errors.go missing ServiceError field containing %q", field)
		}
	}

	// AC7: ValidationError type with field+message.
	if !strings.Contains(errorsContent, "type ValidationError struct") {
		t.Error("errors.go missing ValidationError struct")
	}
	if !strings.Contains(errorsContent, `Field   string`) {
		t.Error("ValidationError missing Field")
	}
	if !strings.Contains(errorsContent, `Message string`) {
		t.Error("ValidationError missing Message")
	}

	// AC2: Error constructors for all six categories.
	// Prefix for "hyperfleet-api" is "HYPERFLEET" (strip -api, remove hyphens, uppercase).
	constructors := []struct {
		name string
		code string
	}{
		{"func NotFound(", "HYPERFLEET-NTF-001"},
		{"func BadRequest(", "HYPERFLEET-VAL-001"},
		{"func Conflict(", "HYPERFLEET-CNF-001"},
		{"func Validation(", "HYPERFLEET-VAL-000"},
		{"func Unauthorized(", "HYPERFLEET-AUT-001"},
		{"func Forbidden(", "HYPERFLEET-AUZ-001"},
		{"func InternalError(", "HYPERFLEET-INT-001"},
	}
	for _, c := range constructors {
		if !strings.Contains(errorsContent, c.name) {
			t.Errorf("errors.go missing constructor %s", c.name)
		}
		if !strings.Contains(errorsContent, c.code) {
			t.Errorf("errors.go missing error code %s", c.code)
		}
	}

	// AC4: handleError writes application/problem+json.
	if !strings.Contains(errorsContent, `"application/problem+json"`) {
		t.Error("errors.go missing application/problem+json content type")
	}
	if !strings.Contains(errorsContent, "func handleError(") {
		t.Error("errors.go missing handleError function")
	}
}

func TestGenerate_ErrorCodePrefixDerived(t *testing.T) {
	// AC3: Error code prefix derived from service name.
	// "user-management" → hyphens removed, uppercased → "USERMANAGEMENT"
	g := &Generator{}
	ctx := basicContext()
	ctx.ServiceName = "user-management"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	errorsContent := findFileContent(t, files, "internal/api/errors.go")

	if !strings.Contains(errorsContent, "USERMANAGEMENT-NTF-001") {
		t.Error("error code prefix not derived from service name 'user-management'")
	}
	if !strings.Contains(errorsContent, "USERMANAGEMENT-VAL-001") {
		t.Error("error code prefix not derived from service name 'user-management'")
	}
}

func TestGenerate_ErrorTypeBasePopulatesTypeURI(t *testing.T) {
	// AC5: error_type_base populates the type URI.
	g := &Generator{}
	ctx := basicContext()
	ctx.ServiceName = "hyperfleet-api"
	ctx.ErrorTypeBase = "https://api.hyperfleet.io/errors/"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	errorsContent := findFileContent(t, files, "internal/api/errors.go")

	if !strings.Contains(errorsContent, `"https://api.hyperfleet.io/errors/not-found"`) {
		t.Error("NotFound should use error_type_base for type URI")
	}
	if !strings.Contains(errorsContent, `"https://api.hyperfleet.io/errors/bad-request"`) {
		t.Error("BadRequest should use error_type_base for type URI")
	}
	if !strings.Contains(errorsContent, `"https://api.hyperfleet.io/errors/internal-error"`) {
		t.Error("InternalError should use error_type_base for type URI")
	}
}

func TestGenerate_ErrorTypeBaseAbsentUsesAboutBlank(t *testing.T) {
	// When error_type_base is not set, type uses about:blank per RFC 9457.
	g := &Generator{}
	ctx := basicContext()
	ctx.ServiceName = "test-svc"
	ctx.ErrorTypeBase = ""

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	errorsContent := findFileContent(t, files, "internal/api/errors.go")

	if !strings.Contains(errorsContent, `"about:blank"`) {
		t.Error("errors.go should use about:blank when error_type_base is not set")
	}
}

func TestGenerate_HandlersUseHandleError(t *testing.T) {
	// AC6: All handlers use handleError instead of raw http.Error.
	g := &Generator{}
	ctx := basicContext()
	ctx.ServiceName = "test-svc"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_users.go")

	// Must NOT contain http.Error.
	if strings.Contains(handler, "http.Error(") {
		t.Error("handler still contains http.Error() calls; should use handleError()")
	}

	// Must contain handleError calls for each error case.
	expectedCalls := []string{
		"handleError(w, r, BadRequest(",
		"handleError(w, r, InternalError(",
		"handleError(w, r, NotFound(",
	}
	for _, call := range expectedCalls {
		if !strings.Contains(handler, call) {
			t.Errorf("handler missing expected call: %s", call)
		}
	}
}

func TestGenerate_ErrorsFileCompiles(t *testing.T) {
	// AC8+AC9: Generated errors.go compiles together with handlers.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{Name: "widgets", Entity: "Widget", Operations: []types.Operation{
				types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList,
			}},
		},
		OutputNamespace: "internal/api",
		ServiceName:     "test-svc",
		ErrorTypeBase:   "https://example.com/errors/",
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
		t.Fatalf("generated code with errors.go does not compile:\n%s\n%s", err, output)
	}
}

func TestGenerate_HandleErrorPopulatesInstance(t *testing.T) {
	// handleError should populate instance from request path.
	g := &Generator{}
	ctx := basicContext()
	ctx.ServiceName = "test-svc"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	errorsContent := findFileContent(t, files, "internal/api/errors.go")

	if !strings.Contains(errorsContent, "svcErr.Instance = r.URL.Path") {
		t.Error("handleError should populate instance from request path")
	}
}

func TestGenerate_HandleErrorPopulatesTimestamp(t *testing.T) {
	// handleError should populate timestamp.
	g := &Generator{}
	ctx := basicContext()
	ctx.ServiceName = "test-svc"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	errorsContent := findFileContent(t, files, "internal/api/errors.go")

	if !strings.Contains(errorsContent, "time.Now().UTC().Format(time.RFC3339)") {
		t.Error("handleError should populate timestamp with UTC RFC3339")
	}
}

func TestGenerate_HandleErrorPopulatesTraceID(t *testing.T) {
	// handleError should populate trace_id from W3C Trace Context header
	// (OpenTelemetry propagation format: version-traceId-parentId-flags).
	g := &Generator{}
	ctx := basicContext()
	ctx.ServiceName = "test-svc"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	errorsContent := findFileContent(t, files, "internal/api/errors.go")

	if !strings.Contains(errorsContent, `r.Header.Get("Traceparent")`) {
		t.Error("handleError should extract trace ID from Traceparent header")
	}
	if !strings.Contains(errorsContent, "svcErr.TraceID = parts[1]") {
		t.Error("handleError should set TraceID from traceparent parts")
	}
}

func TestGenerate_ValidationErrorsArray(t *testing.T) {
	// AC7: Validation errors include per-field details array.
	g := &Generator{}
	ctx := basicContext()
	ctx.ServiceName = "test-svc"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	errorsContent := findFileContent(t, files, "internal/api/errors.go")

	// Validation constructor accepts []ValidationError.
	if !strings.Contains(errorsContent, "func Validation(errors []ValidationError)") {
		t.Error("Validation constructor should accept []ValidationError")
	}
	// ServiceError has validation_errors JSON tag.
	if !strings.Contains(errorsContent, `json:"validation_errors,omitempty"`) {
		t.Error("ServiceError should have validation_errors JSON tag")
	}
}

func TestGenerate_SlotErrorsUseHandleError(t *testing.T) {
	// Slot error responses should use handleError, not raw http.Error.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{Name: "widgets", Entity: "Widget", Operations: []types.Operation{types.OpCreate}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Collection: "widgets", Gate: []string{"my-gate"}},
		},
		OutputNamespace: "internal/api",
		ModuleName:      "testmod",
		SlotsPackage:    "internal/slots",
		ServiceName:     "test-svc",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	if strings.Contains(handler, "http.Error(") {
		t.Error("handler with slots still uses http.Error; should use handleError")
	}
	if !strings.Contains(handler, "handleError(w, r, InternalError(slotErr.Error()))") {
		t.Error("slot error should use handleError + InternalError")
	}
}

func TestGenerate_SlotRejectionIncludesCode(t *testing.T) {
	// Round 2 finding: slot rejection errors must include Code field via
	// errorForStatus, not an inline ServiceError{} without Code.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{Name: "widgets", Entity: "Widget", Operations: []types.Operation{types.OpCreate}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Collection: "widgets", Gate: []string{"my-gate"}},
		},
		OutputNamespace: "internal/api",
		ModuleName:      "testmod",
		SlotsPackage:    "internal/slots",
		ServiceName:     "test-svc",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	// Slot rejection must use errorForStatus (which sets Code), not inline ServiceError{}.
	if !strings.Contains(handler, "handleError(w, r, errorForStatus(sc, slotResult.ErrorMessage))") {
		t.Error("slot rejection should use errorForStatus to include Code field")
	}
	if strings.Contains(handler, "&ServiceError{Type:") {
		t.Error("handler should not contain inline ServiceError{} construction — use error constructors")
	}

	// Verify errors.go contains errorForStatus function.
	errorsContent := findFileContent(t, files, "internal/api/errors.go")
	if !strings.Contains(errorsContent, "func errorForStatus(status int, detail string) *ServiceError") {
		t.Error("errors.go should contain errorForStatus function")
	}
	// Verify errorForStatus maps status codes to categories.
	for _, cat := range []string{`category = "VAL"`, `category = "AUT"`, `category = "AUZ"`, `category = "NTF"`, `category = "CNF"`} {
		if !strings.Contains(errorsContent, cat) {
			t.Errorf("errorForStatus missing category mapping: %s", cat)
		}
	}
}

func TestGenerate_ErrorForStatusCompilesWithHandlers(t *testing.T) {
	// Verify errorForStatus compiles together with handlers and slots.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{Name: "widgets", Entity: "Widget", Operations: []types.Operation{
				types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList,
			}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Collection: "widgets", Gate: []string{"my-gate"}},
		},
		OutputNamespace: "internal/api",
		ModuleName:      "testmod",
		SlotsPackage:    "internal/slots",
		ServiceName:     "test-svc",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmpDir := t.TempDir()

	goMod := "module testmod\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}

	// Write stub slots package for slot interface types.
	slotsDir := filepath.Join(tmpDir, "internal", "slots")
	os.MkdirAll(slotsDir, 0755)
	slotsStub := `package slots
import "context"
type BeforeCreateSlot interface { Evaluate(ctx context.Context, req *BeforeCreateRequest) (*SlotResult, error) }
type BeforeCreateRequest struct { Input *CreateRequest; Caller *Identity; Entity string }
type CreateRequest struct { Entity string; Fields map[string]string }
type Identity struct { UserID string; Role string; Attributes map[string]string }
type SlotResult struct { Ok bool; Halt bool; StatusCode int32; ErrorMessage string }
`
	if err := os.WriteFile(filepath.Join(slotsDir, "slots.go"), []byte(slotsStub), 0644); err != nil {
		t.Fatalf("writing slots stub: %v", err)
	}

	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		dst := filepath.Join(tmpDir, f.Path)
		os.MkdirAll(filepath.Dir(dst), 0755)
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated code with errorForStatus does not compile:\n%s\n%s", err, output)
	}
}

func TestGenerate_ReadHandlerDistinguishesNotFoundFromInternalError(t *testing.T) {
	// Round 2 finding: Read handler must distinguish not-found (404) from
	// infrastructure errors (500) using errors.Is(err, ErrNotFound).
	g := &Generator{}
	ctx := basicContext()
	ctx.ServiceName = "test-svc"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_users.go")

	// Handler must import "errors" for errors.Is.
	if !strings.Contains(handler, `"errors"`) {
		t.Error("handler with Read operation must import \"errors\"")
	}
	// Handler must check errors.Is(err, ErrNotFound).
	if !strings.Contains(handler, "errors.Is(err, ErrNotFound)") {
		t.Error("Read handler must use errors.Is(err, ErrNotFound) to distinguish not-found from internal errors")
	}
	// Not-found path must use NotFound constructor.
	if !strings.Contains(handler, "handleError(w, r, NotFound(") {
		t.Error("Read handler must use NotFound() for not-found errors")
	}
	// Other errors must use InternalError constructor.
	if !strings.Contains(handler, "handleError(w, r, InternalError(err.Error()))") {
		t.Error("Read handler must use InternalError() for non-not-found errors")
	}
}

func TestGenerate_ErrNotFoundDefinedInRouter(t *testing.T) {
	// ErrNotFound sentinel must be defined in router.go alongside Storage interface.
	g := &Generator{}
	ctx := basicContext()
	ctx.ServiceName = "test-svc"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	router := findFileContent(t, files, "internal/api/router.go")

	if !strings.Contains(router, `var ErrNotFound = errors.New("entity not found")`) {
		t.Error("router.go must define ErrNotFound sentinel error")
	}
	if !strings.Contains(router, `"errors"`) {
		t.Error("router.go must import \"errors\" for ErrNotFound")
	}
	// Storage interface doc should reference ErrNotFound.
	if !strings.Contains(router, "Get must return ErrNotFound") {
		t.Error("Storage interface doc should document that Get must return ErrNotFound")
	}
}

func TestGenerate_DeleteOnlyImportsErrors(t *testing.T) {
	// Delete-only handler should import "errors" because Delete checks
	// errors.Is(err, ErrNotFound) to distinguish not-found from internal errors.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{Name: "widgets", Entity: "Widget", Operations: []types.Operation{types.OpDelete}},
		},
		OutputNamespace: "internal/api",
		ServiceName:     "test-svc",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	// Delete handler returns ErrNotFound for non-existent entities, requiring errors.Is.
	if !strings.Contains(handler, `"errors"`) {
		t.Error("delete-only handler must import \"errors\" for ErrNotFound check")
	}
	if !strings.Contains(handler, `errors.Is(err, ErrNotFound)`) {
		t.Error("delete handler must check errors.Is(err, ErrNotFound)")
	}
}

func TestGenerate_ErrorForStatusWithTypeBase(t *testing.T) {
	// When error_type_base is set, errorForStatus must use statusToSlug
	// to produce the Type URI.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{Name: "widgets", Entity: "Widget", Operations: []types.Operation{types.OpCreate}},
		},
		OutputNamespace: "internal/api",
		ServiceName:     "test-svc",
		ErrorTypeBase:   "https://example.com/errors/",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	errorsContent := findFileContent(t, files, "internal/api/errors.go")

	// errorForStatus should reference statusToSlug for Type URI.
	if !strings.Contains(errorsContent, "statusToSlug(status)") {
		t.Error("errorForStatus with error_type_base should use statusToSlug for Type URI")
	}
	// statusToSlug helper should be present.
	if !strings.Contains(errorsContent, "func statusToSlug(status int) string") {
		t.Error("errors.go with error_type_base should contain statusToSlug helper")
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// --- Task 024: Response Envelope Format with Pagination ---

func envelopeContext() gen.Context {
	return gen.Context{
		Conventions: types.Convention{
			Layout:         "flat",
			ErrorHandling:  "problem-details-rfc",
			ResponseFormat: "envelope",
		},
		Entities: []types.Entity{
			{
				Name: "Widget",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString},
					{Name: "color", Type: types.FieldTypeString},
				},
			},
		},
		Collections: []types.Collection{
			{
				Name:       "widgets",
				Entity:     "Widget",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpList},
			},
		},
		OutputNamespace: "internal/api",
		BasePath:        "/api/v1",
		ServiceName:     "widget-service",
	}
}

func TestGenerate_ResponseFormatConvention(t *testing.T) {
	// AC1: ResponseFormat added to Convention struct
	conv := types.Convention{ResponseFormat: "envelope"}
	if conv.ResponseFormat != "envelope" {
		t.Errorf("expected ResponseFormat 'envelope', got %q", conv.ResponseFormat)
	}
}

func TestGenerate_EnvelopeSingleResourceCreate(t *testing.T) {
	// AC2: Single resource responses include id, kind, href when envelope is enabled.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	// Create handler must generate a UUID for the entity ID.
	if !strings.Contains(handler, `uuid.New().String()`) {
		t.Error("Create handler must generate a UUID when envelope is enabled")
	}

	// Create handler must use presentEntity to wrap the response.
	if !strings.Contains(handler, `presentEntity(`) {
		t.Error("Create handler must use presentEntity for envelope response")
	}

	// Verify kind is the entity name.
	if !strings.Contains(handler, `"Widget"`) {
		t.Error("Create handler must pass entity name as kind to presentEntity")
	}

	// Verify href includes the base path.
	if !strings.Contains(handler, `"/api/v1/widgets"`) {
		t.Error("Create handler must include base_path + collection path in href")
	}
}

func TestGenerate_EnvelopeSingleResourceRead(t *testing.T) {
	// AC2: Single resource Read includes id, kind, href.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	// Read handler must use presentEntity.
	if !strings.Contains(handler, `presentEntity(widget,`) {
		t.Error("Read handler must use presentEntity for envelope response")
	}

	// Read handler must use the path ID for the envelope.
	if !strings.Contains(handler, `"/api/v1/widgets"+"/"+id`) {
		t.Error("Read handler must construct href from base_path + collection_path + id")
	}
}

func TestGenerate_EnvelopeSingleResourceUpdate(t *testing.T) {
	// AC2: Single resource Update includes id, kind, href.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	// Update handler must use presentEntity with id from path.
	updateSection := extractMethodBody(handler, "func (h *WidgetsHandler) Update")
	if updateSection == "" {
		t.Fatal("could not find Update method in handler")
	}
	if !strings.Contains(updateSection, `presentEntity(`) {
		t.Error("Update handler must use presentEntity for envelope response")
	}
}

func TestGenerate_EnvelopeListResponse(t *testing.T) {
	// AC3: List responses include kind, page, size, total, items envelope.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	// List handler must construct envelope with all required fields.
	if !strings.Contains(handler, `"kind":  "WidgetList"`) {
		t.Error("list response must include kind with entity name + 'List'")
	}
	if !strings.Contains(handler, `"page":  page`) {
		t.Error("list response must include page")
	}
	if !strings.Contains(handler, `"size":  actualSize`) {
		t.Error("list response must include actual size")
	}
	if !strings.Contains(handler, `"total": listResult.Total`) {
		t.Error("list response must include total from ListResult")
	}
	if !strings.Contains(handler, `"items": presentedItems`) {
		t.Error("list response must include presented items")
	}
	// Each list item must be converted to API type and run through presentEntity for id/kind/href metadata.
	if !strings.Contains(handler, `presentEntity(apiItem,`) {
		t.Error("list items must be run through presentEntity for envelope metadata")
	}
}

func TestGenerate_ListQueryParametersParsing(t *testing.T) {
	// AC4: List query parameters parsed and validated: page, size, orderBy, fields.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	// page and size parsing.
	if !strings.Contains(handler, `r.URL.Query().Get("page")`) {
		t.Error("list handler must parse page query parameter")
	}
	if !strings.Contains(handler, `r.URL.Query().Get("size")`) {
		t.Error("list handler must parse size query parameter")
	}

	// orderBy parsing.
	if !strings.Contains(handler, `r.URL.Query().Get("orderBy")`) {
		t.Error("list handler must parse orderBy query parameter")
	}
	if !strings.Contains(handler, `strings.Split(orderByStr, ",")`) {
		t.Error("list handler must split orderBy by comma")
	}

	// fields parsing.
	if !strings.Contains(handler, `r.URL.Query().Get("fields")`) {
		t.Error("list handler must parse fields query parameter")
	}

	// search parsing.
	if !strings.Contains(handler, `r.URL.Query().Get("search")`) {
		t.Error("list handler must parse search query parameter")
	}

	// ListOptions construction.
	if !strings.Contains(handler, `opts := ListOptions{Page: page, Size: size, OrderBy: orderBy, Fields: fields, Search: searchExpr}`) {
		t.Error("list handler must construct ListOptions from parsed parameters")
	}
}

func TestGenerate_SizeCappedAt65500(t *testing.T) {
	// AC5: size capped at 65500.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	if !strings.Contains(handler, `size > 65500`) {
		t.Error("list handler must cap size at 65500")
	}
	if !strings.Contains(handler, `size = 65500`) {
		t.Error("list handler must clamp size to 65500 when exceeded")
	}
}

func TestGenerate_OrderByFieldValidation(t *testing.T) {
	// AC6: orderBy field names validated against entity fields.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	// Valid fields set must include entity field names.
	if !strings.Contains(handler, `"name": true`) {
		t.Error("validFields must include entity field 'name'")
	}
	if !strings.Contains(handler, `"color": true`) {
		t.Error("validFields must include entity field 'color'")
	}

	// Validation check on orderBy field names.
	if !strings.Contains(handler, `validFields[fieldName]`) {
		t.Error("orderBy must validate field names against validFields")
	}

	// Direction validation.
	if !strings.Contains(handler, `d != "asc" && d != "desc"`) {
		t.Error("orderBy must validate direction is 'asc' or 'desc'")
	}

	// Invalid field produces 400 error.
	if !strings.Contains(handler, `BadRequest("invalid orderBy field: "`) {
		t.Error("invalid orderBy field must return BadRequest error")
	}
}

func TestGenerate_StorageInterfaceListOptionsAndResult(t *testing.T) {
	// AC7: Storage interface includes ListOptions and ListResult types.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	router := findFileContent(t, files, "internal/api/router.go")

	// ListOptions type.
	if !strings.Contains(router, "type ListOptions struct") {
		t.Error("router.go must define ListOptions struct")
	}
	if !strings.Contains(router, "Page    int") {
		t.Error("ListOptions must have Page int field")
	}
	if !strings.Contains(router, "Size    int") {
		t.Error("ListOptions must have Size int field")
	}
	if !strings.Contains(router, "OrderBy []OrderByField") {
		t.Error("ListOptions must have OrderBy []OrderByField field")
	}
	if !strings.Contains(router, "Fields  []string") {
		t.Error("ListOptions must have Fields []string field")
	}

	// ListResult type.
	if !strings.Contains(router, "type ListResult struct") {
		t.Error("router.go must define ListResult struct")
	}

	// OrderByField type.
	if !strings.Contains(router, "type OrderByField struct") {
		t.Error("router.go must define OrderByField struct")
	}

	// Storage interface List method uses new types.
	if !strings.Contains(router, "List(ctx context.Context, entity string, scopeField string, scopeValue string, opts ListOptions) (ListResult, error)") {
		t.Error("Storage.List must use ListOptions parameter and return ListResult")
	}
}

func TestGenerate_BareResponseFormatPreservesBehavior(t *testing.T) {
	// AC8: response_format: bare (or unset) preserves current behavior.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{Name: "items", Entity: "Item", Operations: []types.Operation{
				types.OpCreate, types.OpRead, types.OpList,
			}},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_items.go")

	// Bare mode: no presentEntity calls.
	if strings.Contains(handler, "presentEntity(") {
		t.Error("bare mode must NOT use presentEntity")
	}

	// Bare mode: no UUID generation.
	if strings.Contains(handler, "uuid.New()") {
		t.Error("bare mode must NOT generate UUIDs")
	}

	// Bare mode: list should return items directly, not in an envelope.
	if strings.Contains(handler, `"kind":`) {
		t.Error("bare mode list must NOT include envelope fields")
	}

	// Bare mode: list should encode listResult.Items directly.
	if !strings.Contains(handler, "json.NewEncoder(w).Encode(listResult.Items)") {
		t.Error("bare mode list must encode listResult.Items directly")
	}

	// Bare mode: should NOT import reflect (no actualSize computation).
	if strings.Contains(handler, `"reflect"`) {
		t.Error("bare mode list must NOT import reflect")
	}
}

func TestGenerate_EntityStructIncludesID(t *testing.T) {
	// Entity structs include an ID field for envelope support.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	router := findFileContent(t, files, "internal/api/router.go")

	// After go/format, struct fields may be tab-aligned. Check both parts.
	if !strings.Contains(router, "ID") || !strings.Contains(router, `json:"id,omitempty"`) {
		t.Error("entity struct must include ID string field with json:\"id,omitempty\" tag")
	}
}

func TestGenerate_PresentEntityHelper(t *testing.T) {
	// When envelope is enabled, router.go includes presentEntity helper.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	router := findFileContent(t, files, "internal/api/router.go")

	if !strings.Contains(router, "func presentEntity(entity any, kind, id, href string) map[string]any") {
		t.Error("router.go must include presentEntity helper when envelope is enabled")
	}
}

func TestGenerate_PresentEntityNotInBareMode(t *testing.T) {
	// When envelope is NOT set, router.go should NOT include presentEntity.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	router := findFileContent(t, files, "internal/api/router.go")
	if strings.Contains(router, "func presentEntity") {
		t.Error("bare mode router.go must NOT include presentEntity helper")
	}
}

func TestGenerate_EnvelopeCreateUUIDGoModRequires(t *testing.T) {
	// When envelope + create op, wiring must include uuid dependency.
	g := &Generator{}
	ctx := envelopeContext()
	_, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wiring.GoModRequires == nil {
		t.Fatal("wiring.GoModRequires should not be nil when envelope + create")
	}
	if _, ok := wiring.GoModRequires["github.com/google/uuid"]; !ok {
		t.Error("wiring.GoModRequires must include github.com/google/uuid when envelope + create")
	}
}

func TestGenerate_BareCreateNoUUIDGoModRequires(t *testing.T) {
	// When bare mode + create op, wiring should NOT require uuid.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpCreate}},
		},
		OutputNamespace: "internal/api",
	}
	_, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wiring.GoModRequires != nil {
		if _, ok := wiring.GoModRequires["github.com/google/uuid"]; ok {
			t.Error("bare mode must NOT require github.com/google/uuid")
		}
	}
}

// extractMethodBody extracts the body of a method starting with the given signature prefix.
func extractMethodBody(source, sigPrefix string) string {
	idx := strings.Index(source, sigPrefix)
	if idx == -1 {
		return ""
	}
	// Find the next function definition or end of file.
	rest := source[idx:]
	nextFunc := strings.Index(rest[1:], "\nfunc ")
	if nextFunc == -1 {
		return rest
	}
	return rest[:nextFunc+1]
}

// --- Task 024 Round 1 Revision: Findings 1-4 ---

func TestGenerate_EnvelopeScopedCollectionHrefResolved(t *testing.T) {
	// Finding 1: href for scoped collections must resolve path parameters using
	// r.PathValue() at runtime, not embed template literals like {org_id}.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{
			Layout:         "flat",
			ErrorHandling:  "problem-details-rfc",
			ResponseFormat: "envelope",
		},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
			}},
		},
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpCreate, types.OpRead}},
			{Name: "org-users", Entity: "User",
				Scope:      map[string]string{"org_id": "Organization"},
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpList},
			},
		},
		OutputNamespace: "internal/api",
		BasePath:        "/api/v1",
		ServiceName:     "user-service",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_org_users.go")

	// The generated handler must NOT contain unresolved {org_id} template literals.
	if strings.Contains(handler, `{org_id}`) {
		t.Error("scoped collection handler must NOT contain unresolved {org_id} template literals in href")
	}

	// Instead, it must resolve the path parameter using r.PathValue("org_id").
	if !strings.Contains(handler, `r.PathValue("org_id")`) {
		t.Error("scoped collection handler must use r.PathValue(\"org_id\") to resolve href path parameters")
	}

	// Create method must produce a resolved href.
	createSection := extractMethodBody(handler, "func (h *OrgUsersHandler) Create(")
	if createSection == "" {
		t.Fatal("could not find Create method in org-users handler")
	}
	if strings.Contains(createSection, `{org_id}`) {
		t.Error("Create method must not contain unresolved {org_id} in presentEntity call")
	}
	if !strings.Contains(createSection, `r.PathValue("org_id")`) {
		t.Error("Create method must resolve org_id via r.PathValue for href construction")
	}

	// Read method must produce a resolved href.
	readSection := extractMethodBody(handler, "func (h *OrgUsersHandler) Read(")
	if readSection == "" {
		t.Fatal("could not find Read method in org-users handler")
	}
	if strings.Contains(readSection, `{org_id}`) {
		t.Error("Read method must not contain unresolved {org_id} in presentEntity call")
	}

	// Update method must produce a resolved href.
	updateSection := extractMethodBody(handler, "func (h *OrgUsersHandler) Update(")
	if updateSection == "" {
		t.Fatal("could not find Update method in org-users handler")
	}
	if strings.Contains(updateSection, `{org_id}`) {
		t.Error("Update method must not contain unresolved {org_id} in presentEntity call")
	}
}

func TestGenerate_EnvelopeListItemsPresented(t *testing.T) {
	// Finding 2: List response items must be run through presentEntity()
	// to include id, kind, and href metadata — matching single-resource responses.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")
	listSection := extractMethodBody(handler, "func (h *WidgetsHandler) List(")
	if listSection == "" {
		t.Fatal("could not find List method in widgets handler")
	}

	// List must iterate items and present each one.
	if !strings.Contains(listSection, "presentedItems") {
		t.Error("list handler must build presentedItems array with presented entities")
	}

	// Each item must be converted to API type and wrapped through presentEntity.
	if !strings.Contains(listSection, `presentEntity(apiItem,`) {
		t.Error("list handler must call presentEntity on each item")
	}

	// The envelope must use presentedItems, not raw listResult.Items.
	if strings.Contains(listSection, `"items": listResult.Items`) {
		t.Error("list envelope must use presented items, not raw listResult.Items")
	}

	// Items must reflect the entity kind.
	if !strings.Contains(listSection, `"Widget"`) {
		t.Error("list handler must pass entity name as kind to presentEntity for each item")
	}
}

func TestGenerate_EnvelopeListItemsHrefForScoped(t *testing.T) {
	// Finding 2 extension: List items for scoped collections must have resolved hrefs.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{
			Layout:         "flat",
			ErrorHandling:  "problem-details-rfc",
			ResponseFormat: "envelope",
		},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
			}},
		},
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{Name: "org-users", Entity: "User",
				Scope:      map[string]string{"org_id": "Organization"},
				Operations: []types.Operation{types.OpList},
			},
		},
		OutputNamespace: "internal/api",
		ServiceName:     "user-service",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_org_users.go")
	listSection := extractMethodBody(handler, "func (h *OrgUsersHandler) List(")
	if listSection == "" {
		t.Fatal("could not find List method in org-users handler")
	}

	// The hrefBase in the list handler must be resolved using r.PathValue,
	// not contain literal {org_id}.
	if strings.Contains(listSection, `{org_id}`) {
		t.Error("list handler hrefBase must not contain unresolved {org_id} template literal")
	}
	if !strings.Contains(listSection, `r.PathValue("org_id")`) {
		t.Error("list handler must resolve org_id via r.PathValue for item hrefs")
	}
}

func TestGenerate_OpenAPIListOrderByAndFieldsParams(t *testing.T) {
	// Finding 3: OpenAPI spec must declare orderBy and fields query parameters
	// for list operations.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiJSON := findFileContent(t, files, "internal/api/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(openapiJSON), &spec); err != nil {
		t.Fatalf("failed to parse openapi.json: %v", err)
	}

	paths := spec["paths"].(map[string]any)
	widgetsPath := paths["/api/v1/widgets"].(map[string]any)
	getOp := widgetsPath["get"].(map[string]any)
	params := getOp["parameters"].([]any)

	// Collect parameter names.
	paramNames := make(map[string]bool)
	for _, p := range params {
		pm := p.(map[string]any)
		paramNames[pm["name"].(string)] = true
	}

	if !paramNames["orderBy"] {
		t.Error("OpenAPI list operation must include 'orderBy' query parameter")
	}
	if !paramNames["fields"] {
		t.Error("OpenAPI list operation must include 'fields' query parameter")
	}
	if !paramNames["search"] {
		t.Error("OpenAPI list operation must include 'search' query parameter")
	}
	if !paramNames["page"] {
		t.Error("OpenAPI list operation must include 'page' query parameter")
	}
	if !paramNames["size"] {
		t.Error("OpenAPI list operation must include 'size' query parameter")
	}
}

func TestGenerate_OpenAPIListResponseEnvelopeSchema(t *testing.T) {
	// Finding 4: When response_format is envelope, the OpenAPI list response
	// schema must be an object with kind/page/size/total/items — not a plain array.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiJSON := findFileContent(t, files, "internal/api/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(openapiJSON), &spec); err != nil {
		t.Fatalf("failed to parse openapi.json: %v", err)
	}

	paths := spec["paths"].(map[string]any)
	widgetsPath := paths["/api/v1/widgets"].(map[string]any)
	getOp := widgetsPath["get"].(map[string]any)
	responses := getOp["responses"].(map[string]any)
	ok200 := responses["200"].(map[string]any)
	content := ok200["content"].(map[string]any)
	jsonMedia := content["application/json"].(map[string]any)
	schema := jsonMedia["schema"].(map[string]any)

	// Schema must be an object, not an array.
	if schema["type"] != "object" {
		t.Errorf("envelope list response schema type must be 'object', got %q", schema["type"])
	}

	// Must have envelope properties.
	props := schema["properties"].(map[string]any)
	for _, field := range []string{"kind", "page", "size", "total", "items"} {
		if _, ok := props[field]; !ok {
			t.Errorf("envelope list response schema must include property %q", field)
		}
	}

	// Items property must be an array of entity refs.
	itemsProp := props["items"].(map[string]any)
	if itemsProp["type"] != "array" {
		t.Errorf("envelope items property must be type 'array', got %q", itemsProp["type"])
	}
}

func TestGenerate_OpenAPIBareListResponseIsArray(t *testing.T) {
	// When response_format is bare, the OpenAPI list response schema must
	// remain a plain array (no envelope).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpList}},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiJSON := findFileContent(t, files, "internal/api/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(openapiJSON), &spec); err != nil {
		t.Fatalf("failed to parse openapi.json: %v", err)
	}

	paths := spec["paths"].(map[string]any)
	itemsPath := paths["/items"].(map[string]any)
	getOp := itemsPath["get"].(map[string]any)
	responses := getOp["responses"].(map[string]any)
	ok200 := responses["200"].(map[string]any)
	content := ok200["content"].(map[string]any)
	jsonMedia := content["application/json"].(map[string]any)
	schema := jsonMedia["schema"].(map[string]any)

	// Bare mode: list response schema must be a plain array.
	if schema["type"] != "array" {
		t.Errorf("bare list response schema type must be 'array', got %q", schema["type"])
	}
}

func TestResolvedHrefExpr_FlatPath(t *testing.T) {
	// For flat paths (no parameters), resolvedHrefExpr produces a simple literal.
	expr := resolvedHrefExpr("/api/v1/widgets", "widget.ID")
	if strings.Contains(expr, "PathValue") {
		t.Error("flat path href should not use PathValue")
	}
	if !strings.Contains(expr, `"/api/v1/widgets"`) {
		t.Errorf("flat path href should contain the literal path, got: %s", expr)
	}
	if !strings.Contains(expr, "widget.ID") {
		t.Errorf("flat path href should contain the id expression, got: %s", expr)
	}
}

func TestResolvedHrefExpr_ScopedPath(t *testing.T) {
	// For scoped paths, resolvedHrefExpr substitutes {param} with r.PathValue().
	expr := resolvedHrefExpr("/organizations/{org_id}/users", "user.ID")
	if strings.Contains(expr, "{org_id}") {
		t.Errorf("scoped path href must not contain literal {org_id}, got: %s", expr)
	}
	if !strings.Contains(expr, `r.PathValue("org_id")`) {
		t.Errorf("scoped path href must use r.PathValue(\"org_id\"), got: %s", expr)
	}
	if !strings.Contains(expr, "user.ID") {
		t.Errorf("scoped path href must contain the id expression, got: %s", expr)
	}
}

func TestResolvedHrefExpr_MultiLevel(t *testing.T) {
	// For multi-level scoped paths, all parameters must be resolved.
	expr := resolvedHrefExpr("/clusters/{cluster_id}/nodepools/{nodepool_id}/statuses", "status.ID")
	if strings.Contains(expr, "{cluster_id}") || strings.Contains(expr, "{nodepool_id}") {
		t.Errorf("multi-level path must not contain unresolved params, got: %s", expr)
	}
	if !strings.Contains(expr, `r.PathValue("cluster_id")`) {
		t.Errorf("must resolve cluster_id, got: %s", expr)
	}
	if !strings.Contains(expr, `r.PathValue("nodepool_id")`) {
		t.Errorf("must resolve nodepool_id, got: %s", expr)
	}
}

func TestResolvedHrefBaseExpr_FlatPath(t *testing.T) {
	expr := resolvedHrefBaseExpr("/api/v1/widgets")
	if strings.Contains(expr, "PathValue") {
		t.Error("flat path hrefBase should not use PathValue")
	}
}

func TestResolvedHrefBaseExpr_ScopedPath(t *testing.T) {
	expr := resolvedHrefBaseExpr("/organizations/{org_id}/users")
	if strings.Contains(expr, "{org_id}") {
		t.Errorf("scoped path hrefBase must not contain literal {org_id}, got: %s", expr)
	}
	if !strings.Contains(expr, `r.PathValue("org_id")`) {
		t.Errorf("scoped path hrefBase must use r.PathValue, got: %s", expr)
	}
}

// --- Task 024 Round 2 Revision: Findings 1-3 ---

func TestGenerate_EnvelopeFieldsIDAlwaysIncluded(t *testing.T) {
	// Round 2 Finding 1: The fields query parameter must ensure "id" is always
	// included in the field list even if the user does not list it.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")
	listBody := extractMethodBody(handler, "func (h *WidgetsHandler) List")
	if listBody == "" {
		t.Fatal("could not find List method in handler")
	}

	// The fields parsing code must check for "id" and inject it if missing.
	if !strings.Contains(listBody, `hasID`) {
		t.Error("List handler must track whether 'id' is in the fields list")
	}
	if !strings.Contains(listBody, `fields = append(fields, "id")`) {
		t.Error("List handler must append 'id' to fields when not listed by user")
	}
}

func TestGenerate_EnvelopeUpdateSetsEntityIDFromPath(t *testing.T) {
	// Round 2 Finding 2: The Update handler must set entity.ID = id from the
	// path parameter before passing the entity to storage. Otherwise the stored
	// entity has an empty or client-provided ID that may not match the URL.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")
	updateBody := extractMethodBody(handler, "func (h *WidgetsHandler) Update")
	if updateBody == "" {
		t.Fatal("could not find Update method in handler")
	}

	// Must set entity ID from path before storage call.
	if !strings.Contains(updateBody, "widget.ID = id") {
		t.Error("Update handler must set entity.ID = id from the path parameter")
	}

	// The ID assignment must come before the store.Replace call.
	idAssignIdx := strings.Index(updateBody, "widget.ID = id")
	storeCallIdx := strings.Index(updateBody, "h.store.Replace")
	if idAssignIdx == -1 || storeCallIdx == -1 {
		t.Fatal("could not find ID assignment or store.Replace call")
	}
	if idAssignIdx > storeCallIdx {
		t.Error("entity.ID = id must come before h.store.Replace call")
	}
}

func TestGenerate_OpenAPISingleResourceEnvelopeSchema(t *testing.T) {
	// Round 2 Finding 3: When response_format is envelope, single-resource
	// response schemas (create, read, update) must reflect the envelope format
	// with id, kind, href metadata — not just the bare entity ref.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiJSON := findFileContent(t, files, "internal/api/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(openapiJSON), &spec); err != nil {
		t.Fatalf("failed to parse openapi.json: %v", err)
	}

	paths := spec["paths"].(map[string]any)

	// Check create (POST /api/v1/widgets → 201 response).
	widgetsPath := paths["/api/v1/widgets"].(map[string]any)
	postOp := widgetsPath["post"].(map[string]any)
	postResponses := postOp["responses"].(map[string]any)
	created := postResponses["201"].(map[string]any)
	createdContent := created["content"].(map[string]any)
	createdMedia := createdContent["application/json"].(map[string]any)
	createdSchema := createdMedia["schema"].(map[string]any)
	assertEnvelopeResourceSchema(t, createdSchema, "create")

	// Check read (GET /api/v1/widgets/{id} → 200 response).
	itemPath := paths["/api/v1/widgets/{id}"].(map[string]any)
	getOp := itemPath["get"].(map[string]any)
	getResponses := getOp["responses"].(map[string]any)
	ok200 := getResponses["200"].(map[string]any)
	okContent := ok200["content"].(map[string]any)
	okMedia := okContent["application/json"].(map[string]any)
	okSchema := okMedia["schema"].(map[string]any)
	assertEnvelopeResourceSchema(t, okSchema, "read")

	// Check update (PUT /api/v1/widgets/{id} → 200 response).
	putOp := itemPath["put"].(map[string]any)
	putResponses := putOp["responses"].(map[string]any)
	updated := putResponses["200"].(map[string]any)
	updatedContent := updated["content"].(map[string]any)
	updatedMedia := updatedContent["application/json"].(map[string]any)
	updatedSchema := updatedMedia["schema"].(map[string]any)
	assertEnvelopeResourceSchema(t, updatedSchema, "update")
}

func TestGenerate_OpenAPISingleResourceBareSchema(t *testing.T) {
	// When response_format is bare or unset, single-resource response schemas
	// must remain plain entity refs (no allOf envelope).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate}},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiJSON := findFileContent(t, files, "internal/api/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(openapiJSON), &spec); err != nil {
		t.Fatalf("failed to parse openapi.json: %v", err)
	}

	paths := spec["paths"].(map[string]any)

	// Create response: must use plain $ref.
	itemsPath := paths["/items"].(map[string]any)
	postOp := itemsPath["post"].(map[string]any)
	postResponses := postOp["responses"].(map[string]any)
	created := postResponses["201"].(map[string]any)
	createdContent := created["content"].(map[string]any)
	createdMedia := createdContent["application/json"].(map[string]any)
	createdSchema := createdMedia["schema"].(map[string]any)
	if _, hasAllOf := createdSchema["allOf"]; hasAllOf {
		t.Error("bare mode create response schema should not use allOf envelope")
	}
	if createdSchema["$ref"] != "#/components/schemas/Item" {
		t.Errorf("bare mode create response schema should use direct $ref, got %v", createdSchema)
	}
}

func TestGenerate_OpenAPIUpsertEnvelopeSchema(t *testing.T) {
	// Upsert response schema must also use envelope when response_format is envelope.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{
			Layout:         "flat",
			ResponseFormat: "envelope",
		},
		Entities: []types.Entity{
			{
				Name: "Resource",
				Fields: []types.Field{
					{Name: "name", Type: types.FieldTypeString},
					{Name: "type", Type: types.FieldTypeString},
				},
			},
		},
		Collections: []types.Collection{
			{
				Name:      "resources",
				Entity:    "Resource",
				Operations: []types.Operation{types.OpUpsert},
				UpsertKey: []string{"name", "type"},
			},
		},
		OutputNamespace: "internal/api",
		BasePath:        "/api/v1",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiJSON := findFileContent(t, files, "internal/api/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(openapiJSON), &spec); err != nil {
		t.Fatalf("failed to parse openapi.json: %v", err)
	}

	paths := spec["paths"].(map[string]any)
	resourcesPath := paths["/api/v1/resources"].(map[string]any)
	putOp := resourcesPath["put"].(map[string]any)
	putResponses := putOp["responses"].(map[string]any)
	upserted := putResponses["200"].(map[string]any)
	upsertedContent := upserted["content"].(map[string]any)
	upsertedMedia := upsertedContent["application/json"].(map[string]any)
	upsertedSchema := upsertedMedia["schema"].(map[string]any)
	assertEnvelopeResourceSchema(t, upsertedSchema, "upsert")
}

func TestGenerate_FieldsQueryParameterValidation(t *testing.T) {
	// Round 3 Finding 1: The fields query parameter must validate field names
	// against the entity's fields, same as orderBy validation. Unvalidated
	// field names in ListOptions.Fields reach the storage layer SQL SELECT.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")
	listBody := extractMethodBody(handler, "func (h *WidgetsHandler) List")
	if listBody == "" {
		t.Fatal("could not find List method in handler")
	}

	// Fields must be validated against validFields — same map used by orderBy.
	if !strings.Contains(listBody, `validFields[f]`) {
		t.Error("fields query parameter must validate each field name against validFields")
	}

	// Invalid field names must be rejected with a BadRequest error.
	if !strings.Contains(listBody, `BadRequest("invalid fields value: "`) {
		t.Error("invalid fields value must return BadRequest error")
	}

	// "id" must be accepted without validFields check (it's the implicit primary key,
	// not an entity-declared field).
	if !strings.Contains(listBody, `f != "id" && !validFields[f]`) {
		t.Error("fields validation must allow 'id' even though it's not in validFields")
	}
}

func TestGenerate_OpenAPIListItemsEnvelopeSchema(t *testing.T) {
	// Round 3 Finding 2: When response_format is envelope, the OpenAPI list
	// response items array must use the presented entity schema (allOf with
	// id/kind/href), not the bare entity ref. The handler runs each list item
	// through presentEntity(), so the spec must match.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiJSON := findFileContent(t, files, "internal/api/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(openapiJSON), &spec); err != nil {
		t.Fatalf("failed to parse openapi.json: %v", err)
	}

	paths := spec["paths"].(map[string]any)
	widgetsPath := paths["/api/v1/widgets"].(map[string]any)
	getOp := widgetsPath["get"].(map[string]any)
	responses := getOp["responses"].(map[string]any)
	ok200 := responses["200"].(map[string]any)
	content := ok200["content"].(map[string]any)
	jsonMedia := content["application/json"].(map[string]any)
	schema := jsonMedia["schema"].(map[string]any)

	// The envelope list response is an object with items property.
	props := schema["properties"].(map[string]any)
	itemsProp := props["items"].(map[string]any)
	if itemsProp["type"] != "array" {
		t.Fatalf("envelope items property must be type 'array', got %q", itemsProp["type"])
	}

	// The items array element schema must use allOf with envelope metadata,
	// not a bare $ref — because each item is run through presentEntity().
	itemsSchema := itemsProp["items"].(map[string]any)
	assertEnvelopeResourceSchema(t, itemsSchema, "list items")
}

// assertEnvelopeResourceSchema verifies that a schema uses allOf to compose the
// entity ref with envelope metadata (id, kind, href).
func assertEnvelopeResourceSchema(t *testing.T, schema map[string]any, opName string) {
	t.Helper()
	allOf, ok := schema["allOf"].([]any)
	if !ok || len(allOf) < 2 {
		t.Errorf("%s response schema must use allOf with entity ref and envelope properties, got %v", opName, schema)
		return
	}

	// First element should be the entity $ref.
	refPart := allOf[0].(map[string]any)
	if _, hasRef := refPart["$ref"]; !hasRef {
		t.Errorf("%s response schema allOf[0] must be entity $ref, got %v", opName, refPart)
	}

	// Second element should have properties for id, kind, href.
	propsPart := allOf[1].(map[string]any)
	props, ok := propsPart["properties"].(map[string]any)
	if !ok {
		t.Errorf("%s response schema allOf[1] must have properties, got %v", opName, propsPart)
		return
	}
	for _, field := range []string{"id", "kind", "href"} {
		if _, exists := props[field]; !exists {
			t.Errorf("%s response envelope schema must include %q property", opName, field)
		}
	}
}

func TestGenerate_FieldsSparseFieldsetFiltersResponse(t *testing.T) {
	// Round 4 Finding: The fields query parameter must filter the response map
	// to only include requested fields plus metadata (id, kind, href). Without
	// filtering, non-selected fields appear with zero values (empty strings, 0,
	// etc.) violating sparse fieldset semantics.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")
	listBody := extractMethodBody(handler, "func (h *WidgetsHandler) List")
	if listBody == "" {
		t.Fatal("could not find List method in handler")
	}

	// The list handler must filter presented items when fields is non-empty.
	// It should build an allowed set including metadata keys and requested fields,
	// then delete non-allowed keys from the presented map.
	if !strings.Contains(listBody, `if len(fields) > 0`) {
		t.Error("List handler must check if fields is non-empty to trigger sparse filtering")
	}

	// Metadata keys (id, kind, href) must always be retained regardless of
	// the fields selection.
	if !strings.Contains(listBody, `"id": true, "kind": true, "href": true`) {
		t.Error("sparse fieldset filter must always allow metadata keys: id, kind, href")
	}

	// Non-selected fields must be removed from the presented entity map.
	if !strings.Contains(listBody, `delete(presentedItems[i], k)`) {
		t.Error("sparse fieldset filter must delete non-allowed keys from presented items")
	}
}

func TestGenerate_PatchOperation(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString, Unique: true},
				{Name: "spec", Type: types.FieldTypeJsonb},
				{Name: "labels", Type: types.FieldTypeJsonb, Optional: true},
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
				{Name: "status", Type: types.FieldTypeJsonb, Computed: true, FilledBy: "status-agg"},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "clusters",
				Entity:     "Cluster",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpPatch},
				Patchable:  []string{"spec", "labels"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_clusters.go")

	// Verify patch request struct is generated with pointer fields.
	if !strings.Contains(handler, "type ClustersPatchRequest struct") {
		t.Error("handler missing ClustersPatchRequest struct")
	}
	// spec is jsonb → *json.RawMessage (pointer for nil = not provided).
	if !strings.Contains(handler, "*json.RawMessage") {
		t.Error("patch request missing Spec field as *json.RawMessage")
	}
	// labels is jsonb → *json.RawMessage (pointer for nil = not provided).
	if !strings.Contains(handler, "Labels") {
		t.Error("patch request missing Labels field")
	}

	// Verify Patch method exists.
	if !strings.Contains(handler, "func (h *ClustersHandler) Patch(") {
		t.Error("handler missing Patch method")
	}

	// Verify get-then-merge: fetches existing, converts via JSON, decodes patch, applies.
	if !strings.Contains(handler, "h.store.Get(r.Context()") {
		t.Error("Patch handler must fetch existing entity via store.Get")
	}
	if !strings.Contains(handler, "json.Marshal(existing)") {
		t.Error("Patch handler must marshal Get result for cross-package type conversion")
	}
	if !strings.Contains(handler, "json.Unmarshal(existingData") {
		t.Error("Patch handler must unmarshal Get result into API-package entity type")
	}
	if !strings.Contains(handler, "var patch ClustersPatchRequest") {
		t.Error("Patch handler must decode into patch request struct")
	}
	if !strings.Contains(handler, "h.store.Replace(r.Context()") {
		t.Error("Patch handler must save merged entity via store.Replace")
	}

	// Verify apply-non-nil logic for patchable fields (pointer dereference for
	// non-optional fields, direct pointer assignment for optional fields).
	if !strings.Contains(handler, "if patch.Spec != nil") {
		t.Error("Patch handler must check patch.Spec != nil before applying")
	}
	if !strings.Contains(handler, "*patch.Spec") {
		t.Error("Patch handler must dereference pointer field Spec (non-optional)")
	}
	if !strings.Contains(handler, "if patch.Labels != nil") {
		t.Error("Patch handler must check patch.Labels != nil before applying")
	}
	// Labels is optional → entity field is already a pointer → assign directly.
	if !strings.Contains(handler, "cluster.Labels = patch.Labels") {
		t.Error("Patch handler must assign optional pointer field Labels directly (not dereference)")
	}

	// Verify route is PATCH method on /{id} path.
	found := false
	for _, route := range wiring.Routes {
		if strings.Contains(route, "PATCH") && strings.Contains(route, "{id}") && strings.Contains(route, "Patch") {
			found = true
			break
		}
	}
	if !found {
		t.Error("wiring must include PATCH /{path}/{id} route")
	}
}

func TestGenerate_PatchOperationWithPointerFields(t *testing.T) {
	// Verify that ALL patchable fields get pointer types, including reference types.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "count", Type: types.FieldTypeInt32},
				{Name: "score", Type: types.FieldTypeDouble},
				{Name: "active", Type: types.FieldTypeBool},
				{Name: "metadata", Type: types.FieldTypeJsonb},
				{Name: "payload", Type: types.FieldTypeBytes},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "widgets",
				Entity:     "Widget",
				Operations: []types.Operation{types.OpRead, types.OpPatch},
				Patchable:  []string{"name", "count", "score", "active", "metadata", "payload"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")

	// Pointer fields for value types.
	if !strings.Contains(handler, "*string") {
		t.Error("patch request missing *string pointer field")
	}
	if !strings.Contains(handler, "*int32") {
		t.Error("patch request missing *int32 pointer field")
	}
	if !strings.Contains(handler, "*float64") {
		t.Error("patch request missing *float64 pointer field")
	}
	if !strings.Contains(handler, "*bool") {
		t.Error("patch request missing *bool pointer field")
	}
	// Pointer fields for reference types (spec mandates *json.RawMessage, *[]byte).
	if !strings.Contains(handler, "*json.RawMessage") {
		t.Error("patch request missing *json.RawMessage pointer field for jsonb")
	}
	if !strings.Contains(handler, "*[]byte") {
		t.Error("patch request missing *[]byte pointer field for bytes")
	}

	// Verify pointer dereference in apply logic for all types.
	if !strings.Contains(handler, "*patch.Name") {
		t.Error("Patch handler must dereference pointer field Name")
	}
	if !strings.Contains(handler, "*patch.Count") {
		t.Error("Patch handler must dereference pointer field Count")
	}
	if !strings.Contains(handler, "*patch.Metadata") {
		t.Error("Patch handler must dereference pointer field Metadata")
	}
	if !strings.Contains(handler, "*patch.Payload") {
		t.Error("Patch handler must dereference pointer field Payload")
	}
}

func TestGenerate_PatchOpenAPISchema(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString, Unique: true},
				{Name: "spec", Type: types.FieldTypeJsonb},
				{Name: "labels", Type: types.FieldTypeJsonb, Optional: true},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "clusters",
				Entity:     "Cluster",
				Operations: []types.Operation{types.OpRead, types.OpPatch},
				Patchable:  []string{"spec", "labels"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiContent := findFileContent(t, files, "internal/api/openapi.json")

	var spec map[string]any
	if err := json.Unmarshal([]byte(openapiContent), &spec); err != nil {
		t.Fatalf("openapi spec is not valid JSON: %v", err)
	}

	// Verify patch operation exists on /{path}/{id}.
	paths, _ := spec["paths"].(map[string]any)
	itemPath, ok := paths["/clusters/{id}"].(map[string]any)
	if !ok {
		t.Fatal("missing /clusters/{id} path in OpenAPI spec")
	}
	patchOp, ok := itemPath["patch"].(map[string]any)
	if !ok {
		t.Fatal("missing patch operation on /clusters/{id}")
	}
	if opID, _ := patchOp["operationId"].(string); opID != "patchClusters" {
		t.Errorf("expected operationId 'patchClusters', got %q", opID)
	}

	// Verify patch request body references the patch schema.
	reqBody, _ := patchOp["requestBody"].(map[string]any)
	if reqBody == nil {
		t.Fatal("patch operation missing requestBody")
	}
	reqContent, _ := reqBody["content"].(map[string]any)
	jsonSchema, _ := reqContent["application/json"].(map[string]any)
	schema, _ := jsonSchema["schema"].(map[string]any)
	ref, _ := schema["$ref"].(string)
	if ref != "#/components/schemas/ClustersPatchRequest" {
		t.Errorf("expected patch request body ref to ClustersPatchRequest, got %q", ref)
	}

	// Verify the patch schema exists in components/schemas.
	components, _ := spec["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	patchSchema, ok := schemas["ClustersPatchRequest"].(map[string]any)
	if !ok {
		t.Fatal("missing ClustersPatchRequest in OpenAPI schemas")
	}
	props, _ := patchSchema["properties"].(map[string]any)
	if _, ok := props["spec"]; !ok {
		t.Error("ClustersPatchRequest schema missing 'spec' property")
	}
	if _, ok := props["labels"]; !ok {
		t.Error("ClustersPatchRequest schema missing 'labels' property")
	}
	// Non-patchable fields must not be in the patch schema.
	if _, ok := props["name"]; ok {
		t.Error("ClustersPatchRequest schema must NOT include non-patchable field 'name'")
	}
}

func TestGenerate_PatchCompilesAsPackage(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "count", Type: types.FieldTypeInt32},
				{Name: "price", Type: types.FieldTypeDouble},
				{Name: "active", Type: types.FieldTypeBool},
				{Name: "data", Type: types.FieldTypeJsonb, Optional: true},
				{Name: "created_at", Type: types.FieldTypeTimestamp},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "widgets",
				Entity:     "Widget",
				Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpPatch},
				Patchable:  []string{"name", "count", "price", "active", "data"},
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
		t.Fatalf("generated code with patch does not compile:\n%s\n%s", err, output)
	}
}

func TestGenerate_PatchOnlyCompilesAsPackage(t *testing.T) {
	// Verify that a collection with only patch+read operations compiles.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Config", Fields: []types.Field{
				{Name: "value", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "configs",
				Entity:     "Config",
				Operations: []types.Operation{types.OpRead, types.OpPatch},
				Patchable:  []string{"value"},
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
		t.Fatalf("generated code with patch-only does not compile:\n%s\n%s", err, output)
	}
}

func TestGenerate_PatchWithEnvelopeCompilesAsPackage(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions:     types.Convention{Layout: "flat", ResponseFormat: "envelope"},
		OutputNamespace: "internal/api",
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "data", Type: types.FieldTypeJsonb, Optional: true},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "widgets",
				Entity:     "Widget",
				Operations: []types.Operation{types.OpRead, types.OpPatch},
				Patchable:  []string{"name", "data"},
			},
		},
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
		t.Fatalf("generated code with patch+envelope does not compile:\n%s\n%s", err, output)
	}
}

func TestGenerate_PatchWithTimestampFieldCompilesAsPackage(t *testing.T) {
	// When a patchable field is a timestamp, the patch request struct contains
	// *time.Time, so the handler file must import "time". This test verifies
	// that the time import is correctly included even without create/update/upsert
	// (which would otherwise trigger the time import for computed field zeroing).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Event", Fields: []types.Field{
				{Name: "title", Type: types.FieldTypeString},
				{Name: "scheduled_at", Type: types.FieldTypeTimestamp},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "events",
				Entity:     "Event",
				Operations: []types.Operation{types.OpRead, types.OpPatch},
				Patchable:  []string{"scheduled_at"},
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
		t.Fatalf("generated code with patchable timestamp does not compile:\n%s\n%s", err, output)
	}

	// Also verify the handler contains *time.Time in the patch request struct.
	handler := findFileContent(t, files, "internal/api/handler_events.go")
	if !strings.Contains(handler, "*time.Time") {
		t.Error("patch request struct missing *time.Time pointer field for timestamp patchable field")
	}
}

func TestGenerate_PatchOpenAPIIncludes404Response(t *testing.T) {
	// The patch handler performs a Get before merging. If the entity does not
	// exist, it returns 404. The OpenAPI spec must declare this response.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				{Name: "data", Type: types.FieldTypeJsonb, Optional: true},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "widgets",
				Entity:     "Widget",
				Operations: []types.Operation{types.OpRead, types.OpPatch},
				Patchable:  []string{"name", "data"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	openapiContent := findFileContent(t, files, "internal/api/openapi.json")

	var spec map[string]any
	if err := json.Unmarshal([]byte(openapiContent), &spec); err != nil {
		t.Fatalf("openapi spec is not valid JSON: %v", err)
	}

	paths, _ := spec["paths"].(map[string]any)
	itemPath, ok := paths["/widgets/{id}"].(map[string]any)
	if !ok {
		t.Fatal("missing /widgets/{id} path in OpenAPI spec")
	}
	patchOp, ok := itemPath["patch"].(map[string]any)
	if !ok {
		t.Fatal("missing patch operation on /widgets/{id}")
	}
	responses, _ := patchOp["responses"].(map[string]any)
	if _, ok := responses["404"]; !ok {
		t.Error("patch OpenAPI spec must include 404 response (handler performs Get before merge)")
	}
}

func TestGenerate_PatchEntityNamedExistingOrPatchCompiles(t *testing.T) {
	// Checklist item 188: generatePatchMethod introduces hardcoded local
	// variable names "existing" and "patch" which must be guarded in
	// handlerScopeIdentifiers. An entity named "Existing" or "Patch" whose
	// strings.ToLower produces "existing" or "patch" would collide with
	// those locals without the guard.
	g := &Generator{}

	collNames := map[string]string{"Existing": "existings", "Patch": "patches"}
	for _, name := range []string{"Existing", "Patch"} {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{
				Conventions: types.Convention{Layout: "flat"},
				Entities: []types.Entity{
					{Name: name, Fields: []types.Field{
						{Name: "label", Type: types.FieldTypeString},
					}},
				},
				Collections: []types.Collection{
					{
						Name:       collNames[name],
						Entity:     name,
						Operations: []types.Operation{types.OpRead, types.OpPatch},
						Patchable:  []string{"label"},
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
				t.Fatalf("entity named %q with patch operation does not compile:\n%s\n%s", name, err, output)
			}
		})
	}
}

func TestGenerate_UpsertEntityNamedCreatedCompiles(t *testing.T) {
	// Checklist item 188: generateUpsertMethod introduces hardcoded local
	// variable names "created" and "upsertKey" which must be guarded in
	// handlerScopeIdentifiers. An entity named "Created" whose
	// strings.ToLower produces "created" would collide with the return-value
	// variable in the upsert handler without the guard.
	g := &Generator{}

	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Created", Fields: []types.Field{
				{Name: "label", Type: types.FieldTypeString},
				{Name: "key_field", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "createds",
				Entity:     "Created",
				Operations: []types.Operation{types.OpUpsert},
				UpsertKey:  []string{"key_field"},
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
		t.Fatalf("entity named 'Created' with upsert operation does not compile:\n%s\n%s", err, output)
	}
}

// --- Validation Middleware Tests ---

func TestGenerate_ValidationMiddleware_Generated(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.Conventions.RequestValidation = "openapi-schema"

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have validation.go in addition to handler, router, errors, openapi.json.
	var foundValidation bool
	for _, f := range files {
		if f.Path == "internal/api/validation.go" {
			foundValidation = true
			content := string(f.Bytes())
			if !strings.Contains(content, "//go:embed openapi.json") {
				t.Error("validation.go missing //go:embed openapi.json")
			}
			if !strings.Contains(content, "NewValidationMiddleware") {
				t.Error("validation.go missing NewValidationMiddleware function")
			}
			if !strings.Contains(content, "openapi3filter") {
				t.Error("validation.go missing openapi3filter import")
			}
			if !strings.Contains(content, "parseValidationErrors") {
				t.Error("validation.go missing parseValidationErrors function")
			}
			if !strings.Contains(content, "ValidateRequestBody") {
				t.Error("validation.go missing ValidateRequestBody call")
			}
			if !strings.Contains(content, "handleError") {
				t.Error("validation.go missing handleError call for validation failures")
			}
			if !strings.Contains(content, "Validation(valErrors)") {
				t.Error("validation.go missing Validation() error constructor call")
			}
		}
	}
	if !foundValidation {
		t.Fatal("expected validation.go to be generated when RequestValidation is set")
	}

	if wiring == nil {
		t.Fatal("expected wiring, got nil")
	}
	if len(wiring.Middlewares) != 1 {
		t.Fatalf("expected 1 middleware in wiring, got %d", len(wiring.Middlewares))
	}
	ms := wiring.Middlewares[0]
	if ms.WrapExpr != "%s(%s)" {
		t.Errorf("unexpected WrapExpr: %s", ms.WrapExpr)
	}
	if ms.ConstructorIndex < 0 || ms.ConstructorIndex >= len(wiring.Constructors) {
		t.Fatalf("middleware ConstructorIndex %d out of range [0, %d)", ms.ConstructorIndex, len(wiring.Constructors))
	}
	if !strings.Contains(wiring.Constructors[ms.ConstructorIndex], "NewValidationMiddleware") {
		t.Errorf("middleware constructor does not reference NewValidationMiddleware: %s", wiring.Constructors[ms.ConstructorIndex])
	}

	if wiring.GoModRequires == nil {
		t.Fatal("expected GoModRequires, got nil")
	}
	if _, ok := wiring.GoModRequires["github.com/getkin/kin-openapi"]; !ok {
		t.Error("GoModRequires missing kin-openapi dependency")
	}
}

func TestGenerate_ValidationMiddleware_NotGeneratedWhenNotSet(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		if f.Path == "internal/api/validation.go" {
			t.Error("validation.go should not be generated when RequestValidation is empty")
		}
	}

	if wiring != nil && len(wiring.Middlewares) > 0 {
		t.Error("no middlewares should be in wiring when RequestValidation is empty")
	}
}

func TestGenerate_ValidationMiddleware_NotGeneratedForReadOnly(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{
			Layout:            "flat",
			RequestValidation: "openapi-schema",
		},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{
				{Name: "label", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "widgets",
				Entity:     "Widget",
				Operations: []types.Operation{types.OpRead, types.OpList, types.OpDelete},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		if f.Path == "internal/api/validation.go" {
			t.Error("validation.go should not be generated when no operations accept request bodies")
		}
	}

	if wiring != nil && len(wiring.Middlewares) > 0 {
		t.Error("no middlewares should be in wiring when no operations accept request bodies")
	}
}

func TestGenerate_ValidationMiddleware_MethodFilter(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.Conventions.RequestValidation = "openapi-schema"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var content string
	for _, f := range files {
		if f.Path == "internal/api/validation.go" {
			content = string(f.Bytes())
			break
		}
	}
	if content == "" {
		t.Fatal("validation.go not found")
	}

	if !strings.Contains(content, "http.MethodPost") {
		t.Error("validation.go missing http.MethodPost check")
	}
	if !strings.Contains(content, "http.MethodPut") {
		t.Error("validation.go missing http.MethodPut check")
	}
	if !strings.Contains(content, "http.MethodPatch") {
		t.Error("validation.go missing http.MethodPatch check")
	}
}

func TestGenerate_ValidationMiddleware_ErrorFormat(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.Conventions.RequestValidation = "openapi-schema"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var validationContent string
	for _, f := range files {
		if f.Path == "internal/api/validation.go" {
			validationContent = string(f.Bytes())
			break
		}
	}
	if validationContent == "" {
		t.Fatal("validation.go not found")
	}

	if !strings.Contains(validationContent, "openapi3filter.RequestError") {
		t.Error("parseValidationErrors should unwrap openapi3filter.RequestError")
	}
	if !strings.Contains(validationContent, "openapi3.MultiError") {
		t.Error("parseValidationErrors should handle openapi3.MultiError")
	}
	if !strings.Contains(validationContent, "openapi3.SchemaError") {
		t.Error("addSchemaErrors should extract field info from openapi3.SchemaError")
	}
	if !strings.Contains(validationContent, "JSONPointer()") {
		t.Error("addSchemaErrors should use JSONPointer() for field path")
	}
}

func TestGenerate_ValidationMiddleware_BodyRestore(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.Conventions.RequestValidation = "openapi-schema"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var content string
	for _, f := range files {
		if f.Path == "internal/api/validation.go" {
			content = string(f.Bytes())
			break
		}
	}
	if content == "" {
		t.Fatal("validation.go not found")
	}

	if strings.Count(content, "io.NopCloser(bytes.NewReader(bodyBytes))") < 2 {
		t.Error("validation middleware should restore body at least twice (before validation and after)")
	}
	if !strings.Contains(content, "io.ReadAll(r.Body)") {
		t.Error("validation middleware should buffer the body with io.ReadAll")
	}
}

func TestGenerate_ValidationMiddleware_WithEnvelope(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.Conventions.RequestValidation = "openapi-schema"
	ctx.Conventions.ResponseFormat = "envelope"
	ctx.ModuleName = "github.com/test/svc"

	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var foundValidation bool
	for _, f := range files {
		if f.Path == "internal/api/validation.go" {
			foundValidation = true
		}
	}
	if !foundValidation {
		t.Fatal("validation.go should be generated with envelope format")
	}

	if wiring.GoModRequires == nil {
		t.Fatal("expected GoModRequires")
	}
	if _, ok := wiring.GoModRequires["github.com/google/uuid"]; !ok {
		t.Error("missing uuid in GoModRequires")
	}
	if _, ok := wiring.GoModRequires["github.com/getkin/kin-openapi"]; !ok {
		t.Error("missing kin-openapi in GoModRequires")
	}
}

func TestGenerate_ValidationMiddleware_AuthBypass(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.Conventions.RequestValidation = "openapi-schema"

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var content string
	for _, f := range files {
		if f.Path == "internal/api/validation.go" {
			content = string(f.Bytes())
			break
		}
	}
	if content == "" {
		t.Fatal("validation.go not found")
	}

	if !strings.Contains(content, "NoopAuthenticationFunc") {
		t.Error("validation middleware should use NoopAuthenticationFunc to skip auth during validation")
	}
}

func TestGenerate_ScopedReadVerifiesScope(t *testing.T) {
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpRead},
				Scope:      map[string]string{"org_id": "Organization"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_users.go")
	// Read handler for scoped collection must verify scope field via
	// map[string]any lookup so the original storage value (with metadata)
	// is preserved for the response.
	if !strings.Contains(handler, `scopeVal, _ := scopeMap["org_id"].(string)`) {
		t.Error("scoped Read handler must extract scope field from map[string]any")
	}
	if !strings.Contains(handler, `scopeVal != r.PathValue("org_id")`) {
		t.Error("scoped Read handler must verify scope value matches URL path parameter")
	}
	// Must return 404 on scope mismatch.
	if !strings.Contains(handler, `NotFound("User", id)`) {
		t.Error("scoped Read handler must return NotFound on scope mismatch")
	}
	// Must convert via JSON roundtrip to map for scope field lookup.
	if !strings.Contains(handler, "json.Marshal(existing)") {
		t.Error("scoped Read handler must marshal Get result for scope check")
	}
	// After scope check, must convert storage type to API type via JSON
	// unmarshal of scopeData, stripping storage metadata for consistent responses.
	if !strings.Contains(handler, "json.Unmarshal(scopeData, &") {
		t.Error("scoped Read handler must unmarshal scopeData into API type")
	}
}

func TestGenerate_ScopedUpdateVerifiesScope(t *testing.T) {
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpUpdate},
				Scope:      map[string]string{"org_id": "Organization"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_users.go")
	// Update handler for scoped collection must pre-fetch and verify scope
	// using map[string]any approach (consistent with Read and Delete handlers).
	if !strings.Contains(handler, `scopeVal != r.PathValue("org_id")`) {
		t.Error("scoped Update handler must verify existing entity scope field matches URL path parameter")
	}
	// Must fetch existing entity before body decode to check scope.
	if !strings.Contains(handler, `h.store.Get(r.Context(), "User", id)`) {
		t.Error("scoped Update handler must call store.Get to verify scope before Replace")
	}
}

func TestGenerate_ScopedPatchVerifiesScope(t *testing.T) {
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpPatch},
				Scope:      map[string]string{"org_id": "Organization"},
				Patchable:  []string{"email"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_users.go")
	// Patch handler already does store.Get + JSON roundtrip; scope check
	// must be added after the unmarshal.
	if !strings.Contains(handler, `user.OrgID != r.PathValue("org_id")`) {
		t.Error("scoped Patch handler must verify entity scope field matches URL path parameter")
	}
}

func TestGenerate_ScopedDeleteVerifiesScope(t *testing.T) {
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
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "users",
				Entity:     "User",
				Operations: []types.Operation{types.OpDelete},
				Scope:      map[string]string{"org_id": "Organization"},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_users.go")
	// Delete handler for scoped collection must fetch and verify scope
	// before performing the delete using map[string]any approach.
	if !strings.Contains(handler, `scopeVal != r.PathValue("org_id")`) {
		t.Error("scoped Delete handler must verify entity scope field matches URL path parameter")
	}
	if !strings.Contains(handler, `h.store.Get(r.Context(), "User", id)`) {
		t.Error("scoped Delete handler must call store.Get to verify scope before Delete")
	}
	// Verify encoding/json is imported even when delete is the only operation.
	if !strings.Contains(handler, `"encoding/json"`) {
		t.Error("scoped delete-only collection must import encoding/json for scope verification")
	}
}

func TestGenerate_UnscopedReadNoScopeCheck(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpRead, types.OpDelete}},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_items.go")
	// Unscoped collections must NOT have scope verification code.
	if strings.Contains(handler, "scopeData") || strings.Contains(handler, "scopeMap") || strings.Contains(handler, "scopeVal") {
		t.Error("unscoped collection handler must not contain scope verification code")
	}
	// Read fetches into 'existing' and converts to API type via JSON roundtrip.
	if !strings.Contains(handler, `existing, err := h.store.Get(r.Context(), "Item", id)`) {
		t.Error("unscoped Read must fetch into 'existing' variable")
	}
	if !strings.Contains(handler, "json.Marshal(existing)") {
		t.Error("unscoped Read must marshal 'existing' for API type conversion")
	}
}

func TestGenerate_ListStorageAPIConversionChecksErrors(t *testing.T) {
	// Round 10 finding: List handler's storage→API JSON roundtrip must check
	// marshal and unmarshal errors, matching the Read handler's error handling.
	// Previously, List used `itemData, _ := json.Marshal(item)` (blank
	// identifier) and `json.Unmarshal(itemData, &apiItem)` (uncaptured return),
	// while Read properly checked both errors and returned InternalError.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := findFileContent(t, files, "internal/api/handler_widgets.go")
	listSection := extractMethodBody(handler, "func (h *WidgetsHandler) List(")
	if listSection == "" {
		t.Fatal("could not find List method in widgets handler")
	}

	// json.Marshal error must be captured (not discarded via blank identifier).
	if strings.Contains(listSection, "itemData, _ := json.Marshal") {
		t.Error("List handler must not discard json.Marshal error via blank identifier; must check err")
	}
	if !strings.Contains(listSection, "itemData, err := json.Marshal(item)") {
		t.Error("List handler must capture json.Marshal error into err variable")
	}

	// json.Unmarshal error must be captured and checked.
	if !strings.Contains(listSection, "if err := json.Unmarshal(itemData, &apiItem); err != nil") {
		t.Error("List handler must check json.Unmarshal error")
	}

	// Both errors must produce InternalError responses.
	// Count InternalError calls in the list section — there should be at least
	// two: one for marshal failure and one for unmarshal failure.
	marshalErrIdx := strings.Index(listSection, "itemData, err := json.Marshal(item)")
	if marshalErrIdx == -1 {
		t.Fatal("could not find json.Marshal call in List method")
	}
	afterMarshal := listSection[marshalErrIdx:]
	if !strings.Contains(afterMarshal, `InternalError("internal error")`) {
		t.Error("List handler must return InternalError on json.Marshal/Unmarshal failure")
	}
}
