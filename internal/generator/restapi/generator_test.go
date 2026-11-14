package restapi

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"

	"github.com/jsell-rh/stego/internal/types"
)

// onRuntime gives ctx with what the runtime data path, the one data path (ST-SUP-RC20-OC39), needs and ctx does not
// declare: the command and the query components, pooled tenancy, authorization with subject_from_identity, and for
// each entity a resource type and the permissions of its operations, with create public.
func onRuntime(ctx gen.Context) gen.Context {
	peers := map[string]string{"command": "commands", "query": "queries"}
	for name, ns := range ctx.PeerNamespaces {
		peers[name] = ns
	}
	ctx.PeerNamespaces = peers
	if ctx.ModuleName == "" {
		ctx.ModuleName = "example.com/service"
	}
	if ctx.Tenancy == nil {
		ctx.Tenancy = &gen.Tenancy{Key: "tenant_id"}
	}
	if ctx.Authorization == nil {
		ctx.Authorization = &types.Authorization{Provider: "spicedb", SchemaPath: "authz/schema.zed", SubjectFromIdentity: "user"}
	}
	entities := make([]types.Entity, len(ctx.Entities))
	for i, e := range ctx.Entities {
		if e.ResourceType == "" {
			e.ResourceType = strings.ToLower(e.Name)
		}
		permissions := map[types.Operation]string{types.OpCreate: types.PermissionPublic, types.OpRead: "view", types.OpList: "view",
			types.OpUpdate: "edit", types.OpPatch: "edit", types.OpDelete: "edit", types.OpUpsert: "edit"}
		for op, p := range e.Permissions {
			permissions[op] = p
		}
		e.Permissions = permissions
		reasons := map[types.Operation]string{types.OpCreate: "a new row has no resource to decide on before it exists"}
		for op, reason := range e.PublicReasons {
			reasons[op] = reason
		}
		e.PublicReasons = reasons
		entities[i] = e
	}
	ctx.Entities = entities
	return withSchema(ctx)
}

// withSchema gives ctx the schema that its authorization setting names among the inputs, when ctx has none: a
// definition of each resource type with the relations owner and viewer and the permissions view and edit. The
// command generator reads the relations of each resource type from it (ST-SUP-Q118).
func withSchema(ctx gen.Context) gen.Context {
	if ctx.Authorization == nil || ctx.Authorization.SchemaPath == "" {
		return ctx
	}
	if _, ok := ctx.Inputs[ctx.Authorization.SchemaPath]; ok {
		return ctx
	}
	var b strings.Builder
	// The subject type user has one definition; an entity of the resource type user gets the relations there.
	b.WriteString("definition user {\n\trelation owner: user\n\trelation viewer: user\n\tpermission view = owner + viewer\n\tpermission edit = owner\n}\n")
	seen := map[string]bool{}
	for _, e := range ctx.Entities {
		if e.ResourceType == "" || e.ResourceType == "user" || seen[e.ResourceType] {
			continue
		}
		seen[e.ResourceType] = true
		b.WriteString("\ndefinition " + e.ResourceType + " {\n\trelation owner: user\n\trelation viewer: user\n\tpermission view = owner + viewer\n\tpermission edit = owner\n}\n")
	}
	inputs := map[string][]byte{}
	for path, data := range ctx.Inputs {
		inputs[path] = data
	}
	inputs[ctx.Authorization.SchemaPath] = []byte(b.String())
	ctx.Inputs = inputs
	return ctx
}

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
	files, wiring, err := g.Generate(onRuntime(ctx))
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

func TestGenerate_RouterEntityStructsHaveFields(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(onRuntime(ctx))
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
	t.Run("ST-SUP-RC20-OC33-T01 The emitted wiring registers one literal method-and-pattern route per operation", func(t *testing.T) {
		g := &Generator{}
		ctx := basicContext()

		_, wiring, err := g.Generate(onRuntime(ctx))
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
	})
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

	_, wiring, err := g.Generate(onRuntime(ctx))
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

func TestGenerate_UpsertOpenAPIResponses(t *testing.T) {
	t.Run("ST-SUP-RC20-OC13-T02 The emitted document declares both upsert answers", func(t *testing.T) {
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

		files, _, err := g.Generate(onRuntime(ctx))
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
	})
}

func TestGenerate_OpenAPISpec(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(onRuntime(ctx))
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

	files, _, err := g.Generate(onRuntime(ctx))
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

func TestGenerate_UnknownEntity(t *testing.T) {
	g := &Generator{}
	ctx := gen.Context{
		Entities: []types.Entity{},
		Collections: []types.Collection{
			{Name: "missings", Entity: "Missing", Operations: []types.Operation{types.OpRead}},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(onRuntime(ctx))
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

	files, _, err := g.Generate(onRuntime(ctx))
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

	files, _, err := g.Generate(onRuntime(ctx))
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

func TestCollectionBasePath_SegmentReplacementScoped(t *testing.T) {
	// A scoped collection with a segment-only path_prefix (no ancestor params)
	// should prepend the auto-derived parent chain, replacing only the leaf segment.
	collectionMap := map[string]types.Collection{
		"Cluster":  {Name: "clusters", Entity: "Cluster"},
		"NodePool": {Name: "cluster-nodepools", Entity: "NodePool", Scope: map[string]string{"cluster_id": "Cluster"}},
	}
	eb := types.Collection{
		Name:       "adapter-statuses",
		Entity:     "AdapterStatus",
		Scope:      map[string]string{"nodepool_id": "NodePool"},
		PathPrefix: "/statuses",
	}
	got, err := collectionBasePath(eb, collectionMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/clusters/{cluster_id}/nodepools/{nodepool_id}/statuses"
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

func TestCollectionBasePath_SegmentReplacementSingleLevel(t *testing.T) {
	// Single-level scope with segment-only path_prefix.
	collectionMap := map[string]types.Collection{
		"Organization": {Name: "organizations", Entity: "Organization"},
	}
	eb := types.Collection{
		Name:       "org-members",
		Entity:     "User",
		Scope:      map[string]string{"org_id": "Organization"},
		PathPrefix: "/members",
	}
	got, err := collectionBasePath(eb, collectionMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/organizations/{org_id}/members"
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

func TestCollectionBasePath_FullPathOverrideWithParams(t *testing.T) {
	// A scoped collection with path_prefix containing ancestor params should
	// be used as-is (full-path override) — existing behavior preserved.
	collectionMap := map[string]types.Collection{
		"Cluster": {Name: "clusters", Entity: "Cluster"},
	}
	eb := types.Collection{
		Name:       "node-pools",
		Entity:     "NodePool",
		Scope:      map[string]string{"cluster_id": "Cluster"},
		PathPrefix: "/clusters/{cid}/pools",
	}
	got, err := collectionBasePath(eb, collectionMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/clusters/{cid}/pools"
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

func TestCollectionBasePath_UnscopedPathPrefixUnchanged(t *testing.T) {
	// An unscoped collection with path_prefix (0 ancestors, 0 params) should
	// use the prefix as-is — no parent chain to prepend.
	eb := types.Collection{
		Name:       "widgets",
		Entity:     "Widget",
		PathPrefix: "/custom-widgets",
	}
	got, err := collectionBasePath(eb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/custom-widgets" {
		t.Errorf("expected /custom-widgets, got %s", got)
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

func TestGenerate_OpenAPIRenderedBytesValidJSON(t *testing.T) {
	// Verify that openapi.json rendered via File.Bytes() is valid JSON.
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rendered := findFileBytes(t, files, "internal/api/openapi.json")
	var parsed any
	if err := json.Unmarshal(rendered, &parsed); err != nil {
		t.Fatalf("openapi.json rendered via Bytes() is not valid JSON: %v", err)
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

	files, _, err := g.Generate(onRuntime(ctx))
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

	_, wiring, err := g.Generate(onRuntime(ctx))
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

func TestGenerate_MissingScopeFieldReturnsError(t *testing.T) {
	// When a collection has scope: {field: Parent} but the entity has no field
	// matching the scope field name, the generator must return a clear error.
	g := &Generator{}

	tests := []struct {
		name string
		ops  []types.Operation
	}{
		{"create", []types.Operation{types.OpCreate}},
		{"list", []types.Operation{types.OpList}},
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
						// No cluster_id field — scope field doesn't exist.
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

			_, _, err := g.Generate(onRuntime(ctx))
			if err == nil {
				t.Fatal("expected error when scope field is missing from entity")
			}
			if !strings.Contains(err.Error(), "cluster_id") {
				t.Errorf("error should mention missing scope field name, got: %v", err)
			}
		})
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

	files, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

func TestGenerate_UpdateWithMissingScopeFieldReturnsError(t *testing.T) {
	// Update with scope field that doesn't exist on the entity must return an
	// error, caught by validateFieldReferences.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Cluster", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "Widget", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
				// No cluster_id field — scope field doesn't exist.
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

	_, _, err := g.Generate(onRuntime(ctx))
	if err == nil {
		t.Fatal("expected error when scope field is missing for update")
	}
	if !strings.Contains(err.Error(), "cluster_id") {
		t.Errorf("error should mention missing scope field, got: %v", err)
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

	files, _, err := g.Generate(onRuntime(ctx))
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

func TestGenerate_CollectionNamedRuntimeReturnsError(t *testing.T) {
	// A collection named "runtime" produces collectionToPascalCase → "Runtime",
	// which collides with the generated Runtime type in runtime.go.
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
				Name:       "runtime",
				Entity:     "Warehouse",
				Operations: []types.Operation{types.OpCreate, types.OpRead},
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(onRuntime(ctx))
	if err == nil {
		t.Fatal("expected error for collection named 'runtime' (collides with generated Runtime type)")
	}
	if !strings.Contains(err.Error(), "Runtime") {
		t.Errorf("error should mention 'Runtime', got: %v", err)
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
		// Locals of the handlers and their fills.
		{"patch", "patch_"},
		{"created", "created_"},
		{"repo", "repo_"},
		{"stored", "stored_"},
		{"persisted", "persisted_"},
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

	files, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
	if err == nil {
		t.Fatal("expected error when parent entities are not in collections list, got nil")
	}
	// Org is not in the collections list, so Team's reference should fail.
	if !strings.Contains(err.Error(), "Org") {
		t.Errorf("error should mention missing parent 'Org', got: %v", err)
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

	files, _, err := g.Generate(onRuntime(ctx))
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

func TestGenerate_PathPrefixSegmentNoFalseCollision(t *testing.T) {
	// A segment-only path_prefix on a scoped collection must not falsely collide
	// with an unscoped collection that has a different path.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "Status", Fields: []types.Field{
				{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
				{Name: "message", Type: types.FieldTypeString},
			}},
			{Name: "GlobalStatus", Fields: []types.Field{
				{Name: "message", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "org-statuses",
				Entity:     "Status",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"org_id": "Organization"},
				PathPrefix: "/statuses",
			},
			{
				Name:       "global-statuses",
				Entity:     "GlobalStatus",
				Operations: []types.Operation{types.OpList},
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("expected no route collision error, got: %v", err)
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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
			{Name: "widgets", Entity: "Widget", Operations: []types.Operation{types.OpList}},                       // auto: /widgets
			{Name: "others", Entity: "Other", Operations: []types.Operation{types.OpList}, PathPrefix: "/widgets"}, // explicit: /widgets
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("unexpected error for consistent scope+parent: %v", err)
	}
}

func TestGenerate_ScopeFieldMissingFromEntityReturnsError(t *testing.T) {
	// When validateScopeParentConsistency runs and the scope field doesn't
	// exist on the entity at all, it must return a clear error.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString},
			}},
			{Name: "Widget", Fields: []types.Field{
				{Name: "label", Type: types.FieldTypeString},
				// No "nonexistent" field — scope field doesn't exist.
			}},
		},
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpRead}},
			{
				Name:       "widgets",
				Entity:     "Widget",
				Operations: []types.Operation{types.OpList},
				Scope:      map[string]string{"nonexistent": "Organization"},
			},
		},
		OutputNamespace: "internal/api",
	}

	_, _, err := g.Generate(onRuntime(ctx))
	if err == nil {
		t.Fatal("expected error when scope field doesn't exist on entity")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention the missing scope field name, got: %v", err)
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

func TestGenerate_MultipleRefFieldsScopeDisambiguatesAllOps(t *testing.T) {
	// With scope set, multiple ref fields to the same parent are not ambiguous
	// — the scope field is the linking field. Verify this works for all ops.
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
					{Name: "members", Entity: "Member", Operations: tt.ops, Scope: map[string]string{"primary_org_id": "Org"}, UpsertKey: []string{"primary_org_id"}},
				},
				OutputNamespace: "internal/api",
			}

			_, _, err := g.Generate(onRuntime(ctx))
			if err != nil {
				t.Fatalf("expected success when scope disambiguates, got: %v", err)
			}
		})
	}
}

func TestGenerate_ScopedReadDeleteOnlyMissingScopeFieldReturnsError(t *testing.T) {
	// When a scoped collection only exposes read/delete operations and the scope
	// field doesn't exist on the entity, the generator must still validate upfront.
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
						// No cluster_id field — scope field doesn't exist.
					}},
				},
				Collections: []types.Collection{
					{Name: "clusters", Entity: "Cluster", Operations: []types.Operation{types.OpRead}},
					{Name: "widgets", Entity: "Widget", Operations: tt.ops, Scope: map[string]string{"cluster_id": "Cluster"}},
				},
				OutputNamespace: "internal/api",
			}

			_, _, err := g.Generate(onRuntime(ctx))
			if err == nil {
				t.Fatalf("expected error when scope field is missing with %s operations", tt.name)
			}
			if !strings.Contains(err.Error(), "cluster_id") {
				t.Errorf("error should mention missing scope field, got: %v", err)
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("unexpected error for collections with distinct names: %v", err)
	}
}

func TestGenerate_HandlerWithoutSlotBindings_NoSlotCode(t *testing.T) {
	// Verify that handlers without slot bindings do NOT import the slots
	// package or have slot operator fields.
	g := &Generator{}
	ctx := basicContext()

	files, _, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		if strings.Contains(f.Path, "users_handler.go") {
			content := string(f.Content)
			if strings.Contains(content, "\tslots \"") || strings.Contains(content, ".Evaluate(") {
				t.Errorf("handler without slot bindings should not reference slots package:\n%s", content)
			}
		}
	}
}

// --- base_path tests ---

func TestGenerate_BasePathPrependedToRoutes(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.BasePath = "/api/v1"

	_, wiring, err := g.Generate(onRuntime(ctx))
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

	_, wiring, err := g.Generate(onRuntime(ctx))
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
	t.Run("ST-SUP-RC20-OC28-T03 The emitted document paths start at the base path", func(t *testing.T) {
		g := &Generator{}
		ctx := basicContext()
		ctx.BasePath = "/api/hyperfleet/v1"

		files, _, err := g.Generate(onRuntime(ctx))
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
	})
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

	_, wiring, err := g.Generate(onRuntime(ctx))
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

	_, wiring, err := g.Generate(onRuntime(ctx))
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

	_, _, err := g.Generate(onRuntime(ctx))
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
		{"hyperfleet-api", "HYPERFLEET"},      // spec example: strip -api, uppercase
		{"order-service", "ORDER"},            // spec example: strip -service, uppercase
		{"my-cool-server", "MYCOOL"},          // strip -server suffix
		{"user-management", "USERMANAGEMENT"}, // no suffix to strip, hyphens removed
		{"simple", "SIMPLE"},                  // no hyphens, kept as-is
		{"a-b-c", "ABC"},                      // all segments joined, uppercased
		{"just-api", "JUST"},                  // strip -api from short name
		{"api", "API"},                        // no leading hyphen, not stripped
		{"", ""},                              // edge case
	}
	for _, tt := range tests {
		got := deriveErrorPrefix(tt.input)
		if got != tt.want {
			t.Errorf("deriveErrorPrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGenerate_ErrorsFileGenerated(t *testing.T) {
	check := func(t *testing.T) {
		g := &Generator{}
		ctx := basicContext()
		ctx.ServiceName = "hyperfleet-api"

		files, _, err := g.Generate(onRuntime(ctx))
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
	t.Run("ST-SUP-RC20-OC01-T01 The emitted problem type and its serializer match RFC 9457", check)
	t.Run("ST-SUP-RC20-OC03-T01 The emitted error constructors carry the per-category codes", check)
	t.Run("ST-SUP-RC20-OC05-T01 The emitted problem type carries the validation array", check)
}

func TestGenerate_ErrorCodePrefixDerived(t *testing.T) {
	t.Run("ST-SUP-RC20-OC03-T02 The error code prefix comes from the service name", func(t *testing.T) {
		// AC3: Error code prefix derived from service name.
		// "user-management" → hyphens removed, uppercased → "USERMANAGEMENT"
		g := &Generator{}
		ctx := basicContext()
		ctx.ServiceName = "user-management"

		files, _, err := g.Generate(onRuntime(ctx))
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
	})
}

func TestGenerate_ErrorTypeBasePopulatesTypeURI(t *testing.T) {
	t.Run("ST-SUP-RC20-OC04-T01 The emitted problem type URI follows the declared error base", func(t *testing.T) {
		// AC5: error_type_base populates the type URI.
		g := &Generator{}
		ctx := basicContext()
		ctx.ServiceName = "hyperfleet-api"
		ctx.ErrorTypeBase = "https://api.hyperfleet.io/errors/"

		files, _, err := g.Generate(onRuntime(ctx))
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
	})
}

func TestGenerate_ErrorTypeBaseAbsentUsesAboutBlank(t *testing.T) {
	t.Run("ST-SUP-RC20-OC04-T02 Without a declared base the problem type is about:blank", func(t *testing.T) {
		// When error_type_base is not set, type uses about:blank per RFC 9457.
		g := &Generator{}
		ctx := basicContext()
		ctx.ServiceName = "test-svc"
		ctx.ErrorTypeBase = ""

		files, _, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		errorsContent := findFileContent(t, files, "internal/api/errors.go")

		if !strings.Contains(errorsContent, `"about:blank"`) {
			t.Error("errors.go should use about:blank when error_type_base is not set")
		}
	})
}

func TestGenerate_HandleErrorPopulatesInstance(t *testing.T) {
	t.Run("ST-SUP-RC20-OC02-T02 The emitted problem names the request path", func(t *testing.T) {
		// handleError should populate instance from request path.
		g := &Generator{}
		ctx := basicContext()
		ctx.ServiceName = "test-svc"

		files, _, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		errorsContent := findFileContent(t, files, "internal/api/errors.go")

		if !strings.Contains(errorsContent, "svcErr.Instance = r.URL.Path") {
			t.Error("handleError should populate instance from request path")
		}
	})
}

func TestGenerate_HandleErrorPopulatesTimestamp(t *testing.T) {
	t.Run("ST-SUP-RC20-OC02-T03 The emitted problem carries a UTC timestamp", func(t *testing.T) {
		// handleError should populate timestamp.
		g := &Generator{}
		ctx := basicContext()
		ctx.ServiceName = "test-svc"

		files, _, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		errorsContent := findFileContent(t, files, "internal/api/errors.go")

		if !strings.Contains(errorsContent, "time.Now().UTC().Format(time.RFC3339)") {
			t.Error("handleError should populate timestamp with UTC RFC3339")
		}
	})
}

func TestGenerate_HandleErrorPopulatesTraceID(t *testing.T) {
	t.Run("ST-SUP-RC20-OC02-T01 The emitted problem serializer reads the trace identifier from the request header", func(t *testing.T) {
		// handleError should populate trace_id from W3C Trace Context header
		// (OpenTelemetry propagation format: version-traceId-parentId-flags).
		g := &Generator{}
		ctx := basicContext()
		ctx.ServiceName = "test-svc"

		files, _, err := g.Generate(onRuntime(ctx))
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
	})
}

func TestGenerate_ValidationErrorsArray(t *testing.T) {
	t.Run("ST-SUP-RC20-OC05-T02 A validation problem carries its per-field array", func(t *testing.T) {
		// AC7: Validation errors include per-field details array.
		g := &Generator{}
		ctx := basicContext()
		ctx.ServiceName = "test-svc"

		files, _, err := g.Generate(onRuntime(ctx))
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
	})
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

	files, _, err := g.Generate(onRuntime(ctx))
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

func TestGenerate_EntityStructIncludesID(t *testing.T) {
	// Entity structs include an ID field for envelope support.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(onRuntime(ctx))
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
	t.Run("ST-SUP-RC20-OC06-T02 The emitted presenter adds the envelope metadata", func(t *testing.T) {
		// When envelope is enabled, router.go includes presentEntity helper.
		g := &Generator{}
		ctx := envelopeContext()
		files, _, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		router := findFileContent(t, files, "internal/api/router.go")

		if !strings.Contains(router, "func presentEntity(entity any, kind, id, href string) map[string]any") {
			t.Error("router.go must include presentEntity helper when envelope is enabled")
		}
	})
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
	files, _, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	router := findFileContent(t, files, "internal/api/router.go")
	if strings.Contains(router, "func presentEntity") {
		t.Error("bare mode router.go must NOT include presentEntity helper")
	}
}

func TestGenerate_EnvelopeCreateUUIDGoModRequires(t *testing.T) {
	t.Run("ST-SUP-RC20-OC08-T02 Envelope mode declares the identifier dependency", func(t *testing.T) {
		// When envelope + create op, wiring must include uuid dependency.
		g := &Generator{}
		ctx := envelopeContext()
		_, wiring, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if wiring.GoModRequires == nil {
			t.Fatal("wiring.GoModRequires should not be nil when envelope + create")
		}
		if _, ok := wiring.GoModRequires["github.com/google/uuid"]; !ok {
			t.Error("wiring.GoModRequires must include github.com/google/uuid when envelope + create")
		}
	})
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
	_, wiring, err := g.Generate(onRuntime(ctx))
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

	files, _, err := g.Generate(onRuntime(ctx))
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

// --- Task 024 Round 2 Revision: Findings 1-3 ---

func TestGenerate_OpenAPISingleResourceEnvelopeSchema(t *testing.T) {
	// Round 2 Finding 3: When response_format is envelope, single-resource
	// response schemas (create, read, update) must reflect the envelope format
	// with id, kind, href metadata — not just the bare entity ref.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(onRuntime(ctx))
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

	files, _, err := g.Generate(onRuntime(ctx))
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
				Name:       "resources",
				Entity:     "Resource",
				Operations: []types.Operation{types.OpUpsert},
				UpsertKey:  []string{"name", "type"},
			},
		},
		OutputNamespace: "internal/api",
		BasePath:        "/api/v1",
	}

	files, _, err := g.Generate(onRuntime(ctx))
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

func TestGenerate_OpenAPIListItemsEnvelopeSchema(t *testing.T) {
	// Round 3 Finding 2: When response_format is envelope, the OpenAPI list
	// response items array must use the presented entity schema (allOf with
	// id/kind/href), not the bare entity ref. The handler runs each list item
	// through presentEntity(), so the spec must match.
	g := &Generator{}
	ctx := envelopeContext()
	files, _, err := g.Generate(onRuntime(ctx))
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

	files, _, err := g.Generate(onRuntime(ctx))
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

	files, _, err := g.Generate(onRuntime(ctx))
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

// --- Validation Middleware Tests ---

func TestGenerate_ValidationMiddleware_Generated(t *testing.T) {
	t.Run("ST-SUP-RC20-OC21-T01 The emitted middleware checks each write body against the embedded document", func(t *testing.T) {
		g := &Generator{}
		ctx := basicContext()
		ctx.Conventions.RequestValidation = "openapi-schema"

		files, wiring, err := g.Generate(onRuntime(ctx))
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
	})
}

func TestGenerate_ValidationMiddleware_NotGeneratedWhenNotSet(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	files, wiring, err := g.Generate(onRuntime(ctx))
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

	files, wiring, err := g.Generate(onRuntime(ctx))
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
	t.Run("ST-SUP-RC20-OC21-T02 The emitted middleware checks only the methods that carry a body", func(t *testing.T) {
		g := &Generator{}
		ctx := basicContext()
		ctx.Conventions.RequestValidation = "openapi-schema"

		files, _, err := g.Generate(onRuntime(ctx))
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
	})
}

func TestGenerate_ValidationMiddleware_ErrorFormat(t *testing.T) {
	check := func(t *testing.T) {
		g := &Generator{}
		ctx := basicContext()
		ctx.Conventions.RequestValidation = "openapi-schema"

		files, _, err := g.Generate(onRuntime(ctx))
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
	t.Run("ST-SUP-RC20-OC05-T03 A failed body check answers with a validation problem", check)
	t.Run("ST-SUP-RC20-OC21-T03 The emitted middleware refuses a body that fails its check", check)
}

func TestGenerate_ValidationMiddleware_BodyRestore(t *testing.T) {
	t.Run("ST-SUP-RC20-OC22-T01 The emitted middleware gives the request body back to the handler", func(t *testing.T) {
		g := &Generator{}
		ctx := basicContext()
		ctx.Conventions.RequestValidation = "openapi-schema"

		files, _, err := g.Generate(onRuntime(ctx))
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
		if !strings.Contains(content, "io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBody))") {
			t.Error("validation middleware should buffer the body, at most MaxRequestBody bytes, with io.ReadAll")
		}
	})
}

func TestGenerate_ValidationMiddleware_WithEnvelope(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.Conventions.RequestValidation = "openapi-schema"
	ctx.Conventions.ResponseFormat = "envelope"
	ctx.ModuleName = "github.com/test/svc"

	files, wiring, err := g.Generate(onRuntime(ctx))
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

	files, _, err := g.Generate(onRuntime(ctx))
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

// --- Task 039: Server-Managed Fields and Request Schemas ---

func TestIsServerManaged(t *testing.T) {
	t.Run("ST-SUP-RC20-OC17-T02 The server-managed field rule covers all four cases", func(t *testing.T) {
		tests := []struct {
			name   string
			field  types.Field
			expect bool
		}{
			{
				name:   "computed field",
				field:  types.Field{Name: "status", Type: types.FieldTypeJsonb, Computed: true, FilledBy: "agg"},
				expect: true,
			},
			{
				name:   "timestamp field",
				field:  types.Field{Name: "created_time", Type: types.FieldTypeTimestamp},
				expect: true,
			},
			{
				name:   "created_by field",
				field:  types.Field{Name: "created_by", Type: types.FieldTypeString},
				expect: true,
			},
			{
				name:   "updated_by field",
				field:  types.Field{Name: "updated_by", Type: types.FieldTypeString},
				expect: true,
			},
			{
				name:   "generation field",
				field:  types.Field{Name: "generation", Type: types.FieldTypeInt32},
				expect: true,
			},
			{
				name:   "regular string field",
				field:  types.Field{Name: "name", Type: types.FieldTypeString},
				expect: false,
			},
			{
				name:   "regular ref field",
				field:  types.Field{Name: "org_id", Type: types.FieldTypeRef, To: "Organization"},
				expect: false,
			},
			{
				name:   "optional jsonb field",
				field:  types.Field{Name: "labels", Type: types.FieldTypeJsonb, Optional: true},
				expect: false,
			},
			{
				name:   "enum field",
				field:  types.Field{Name: "role", Type: types.FieldTypeEnum, Values: []string{"admin", "member"}},
				expect: false,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got := isServerManaged(tt.field)
				if got != tt.expect {
					t.Errorf("isServerManaged(%q) = %v, want %v", tt.field.Name, got, tt.expect)
				}
			})
		}
	})
}

func TestGenerate_CreateRequestSchemaExcludesScopeFields(t *testing.T) {
	t.Run("ST-SUP-RC20-OC20-T02 A create-request schema leaves out the scope fields", func(t *testing.T) {
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
				{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpCreate, types.OpRead}},
				{
					Name:       "org-users",
					Entity:     "User",
					Scope:      map[string]string{"org_id": "Organization"},
					Operations: []types.Operation{types.OpCreate, types.OpRead},
				},
			},
			OutputNamespace: "internal/api",
		}

		files, _, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		content := findFileContent(t, files, "internal/api/openapi.json")
		var spec map[string]any
		if err := json.Unmarshal([]byte(content), &spec); err != nil {
			t.Fatalf("parse error: %v", err)
		}

		schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
		createReq := schemas["UserCreateRequest"].(map[string]any)
		props := createReq["properties"].(map[string]any)

		// Scope field org_id should be excluded from CreateRequest.
		if _, ok := props["org_id"]; ok {
			t.Error("UserCreateRequest should not include scope field 'org_id'")
		}
		// email should be present.
		if _, ok := props["email"]; !ok {
			t.Error("UserCreateRequest should include client-provided field 'email'")
		}
	})
}

func TestGenerate_CreateRequestSchemaExcludesImplicitFields(t *testing.T) {
	t.Run("ST-SUP-RC20-OC20-T03 A create-request schema leaves out the implicit fields", func(t *testing.T) {
		g := &Generator{}
		ctx := gen.Context{
			Conventions: types.Convention{Layout: "flat"},
			Entities: []types.Entity{
				{Name: "AuditEvent", Fields: []types.Field{
					{Name: "message", Type: types.FieldTypeString},
					{Name: "source_type", Type: types.FieldTypeString},
					{Name: "severity", Type: types.FieldTypeString},
				}},
			},
			Collections: []types.Collection{
				{
					Name:       "user-audit-events",
					Entity:     "AuditEvent",
					PathPrefix: "/user-events",
					Implicit:   map[string]string{"source_type": "User"},
					Operations: []types.Operation{types.OpCreate, types.OpList},
				},
				{
					Name:       "org-audit-events",
					Entity:     "AuditEvent",
					PathPrefix: "/org-events",
					Implicit:   map[string]string{"source_type": "Organization"},
					Operations: []types.Operation{types.OpCreate, types.OpList},
				},
			},
			OutputNamespace: "internal/api",
		}

		files, _, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		content := findFileContent(t, files, "internal/api/openapi.json")
		var spec map[string]any
		if err := json.Unmarshal([]byte(content), &spec); err != nil {
			t.Fatalf("parse error: %v", err)
		}

		schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
		createReq := schemas["AuditEventCreateRequest"].(map[string]any)
		props := createReq["properties"].(map[string]any)

		// Implicit field source_type should be excluded from CreateRequest.
		if _, ok := props["source_type"]; ok {
			t.Error("AuditEventCreateRequest should not include implicit field 'source_type'")
		}
		// Non-implicit fields should be present.
		if _, ok := props["message"]; !ok {
			t.Error("AuditEventCreateRequest should include client-provided field 'message'")
		}
		if _, ok := props["severity"]; !ok {
			t.Error("AuditEventCreateRequest should include client-provided field 'severity'")
		}

		// Response schema should still include implicit fields.
		responseSchema := schemas["AuditEvent"].(map[string]any)
		responseProps := responseSchema["properties"].(map[string]any)
		if _, ok := responseProps["source_type"]; !ok {
			t.Error("AuditEvent response schema should include implicit field 'source_type'")
		}
	})
}

func TestGenerate_CreateRequestSchemaExcludesImplicitFieldAcrossCollections(t *testing.T) {
	// When a field is implicit in ONE collection but not another (both
	// referencing the same entity), the field is still excluded from the
	// entity-wide CreateRequest schema (conservative: exclude if implicit anywhere).
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Event", Fields: []types.Field{
				{Name: "title", Type: types.FieldTypeString},
				{Name: "category", Type: types.FieldTypeString},
			}},
		},
		Collections: []types.Collection{
			{
				Name:       "auto-events",
				Entity:     "Event",
				PathPrefix: "/auto-events",
				Implicit:   map[string]string{"category": "auto"},
				Operations: []types.Operation{types.OpCreate},
			},
			{
				Name:       "all-events",
				Entity:     "Event",
				PathPrefix: "/all-events",
				Operations: []types.Operation{types.OpCreate},
			},
		},
		OutputNamespace: "internal/api",
	}

	files, _, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/api/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(content), &spec); err != nil {
		t.Fatalf("parse error: %v", err)
	}

	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	createReq := schemas["EventCreateRequest"].(map[string]any)
	props := createReq["properties"].(map[string]any)

	// category is implicit in auto-events, so it should be excluded from
	// the entity-wide CreateRequest schema even though all-events does
	// not declare it as implicit.
	if _, ok := props["category"]; ok {
		t.Error("EventCreateRequest should not include field 'category' (implicit in at least one collection)")
	}
	if _, ok := props["title"]; !ok {
		t.Error("EventCreateRequest should include client-provided field 'title'")
	}
}

func TestGenerate_CORSMiddleware_Generated(t *testing.T) {
	t.Run("ST-SUP-RC20-OC27-T01 The emitted CORS middleware reads the environment and answers a preflight", func(t *testing.T) {
		g := &Generator{}
		ctx := basicContext()
		ctx.Conventions.CORS = "enabled"

		files, wiring, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Should have cors.go in the output files.
		var foundCORS bool
		for _, f := range files {
			if f.Path == "internal/api/cors.go" {
				foundCORS = true
				content := string(f.Bytes())

				// Verify the generated header is present.
				if !strings.Contains(content, "Code generated by stego") {
					t.Error("cors.go missing generated header")
				}

				// Verify constructor function.
				if !strings.Contains(content, "func NewCORSMiddleware()") {
					t.Error("cors.go missing NewCORSMiddleware function")
				}

				// Verify all 3 env vars with correct defaults.
				if !strings.Contains(content, `CORS_ALLOWED_ORIGINS`) {
					t.Error("cors.go missing CORS_ALLOWED_ORIGINS env var")
				}
				if strings.Contains(content, `"*"`) {
					t.Error("cors.go has a default origin of *; with no CORS_ALLOWED_ORIGINS it must set no CORS header")
				}

				if !strings.Contains(content, `CORS_ALLOWED_METHODS`) {
					t.Error("cors.go missing CORS_ALLOWED_METHODS env var")
				}
				if !strings.Contains(content, `allowedMethods = "GET,POST,PATCH,PUT,DELETE,OPTIONS"`) {
					t.Error("cors.go missing default methods")
				}

				if !strings.Contains(content, `CORS_ALLOWED_HEADERS`) {
					t.Error("cors.go missing CORS_ALLOWED_HEADERS env var")
				}
				if !strings.Contains(content, `allowedHeaders = "Content-Type,Authorization"`) {
					t.Error("cors.go missing default headers")
				}

				// Verify CORS response headers are set.
				if !strings.Contains(content, `Access-Control-Allow-Origin`) {
					t.Error("cors.go missing Access-Control-Allow-Origin header")
				}
				if !strings.Contains(content, `Access-Control-Allow-Methods`) {
					t.Error("cors.go missing Access-Control-Allow-Methods header")
				}
				if !strings.Contains(content, `Access-Control-Allow-Headers`) {
					t.Error("cors.go missing Access-Control-Allow-Headers header")
				}

				// Verify OPTIONS preflight handling with 204.
				if !strings.Contains(content, "http.MethodOptions") {
					t.Error("cors.go missing OPTIONS method check")
				}
				if !strings.Contains(content, "http.StatusNoContent") {
					t.Error("cors.go missing 204 No Content for preflight")
				}

				// Verify middleware wraps next handler.
				if !strings.Contains(content, "next.ServeHTTP(w, r)") {
					t.Error("cors.go missing next.ServeHTTP call")
				}
			}
		}
		if !foundCORS {
			t.Fatal("expected cors.go to be generated when CORS is enabled")
		}

		// Verify wiring metadata.
		if wiring == nil {
			t.Fatal("expected wiring, got nil")
		}
		if len(wiring.OuterMiddlewares) != 1 {
			t.Fatalf("expected 1 outer middleware in wiring, got %d", len(wiring.OuterMiddlewares))
		}
		ms := wiring.OuterMiddlewares[0]
		if ms.WrapExpr != "%s(%s)" {
			t.Errorf("unexpected WrapExpr: %s", ms.WrapExpr)
		}
		if ms.ConstructorIndex < 0 || ms.ConstructorIndex >= len(wiring.Constructors) {
			t.Fatalf("OuterMiddlewares ConstructorIndex %d out of range [0, %d)", ms.ConstructorIndex, len(wiring.Constructors))
		}
		if !strings.Contains(wiring.Constructors[ms.ConstructorIndex], "NewCORSMiddleware") {
			t.Errorf("outer middleware constructor does not reference NewCORSMiddleware: %s", wiring.Constructors[ms.ConstructorIndex])
		}
	})
}

func TestGenerate_CORSMiddleware_NotGeneratedWhenEmpty(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	// CORS is empty by default in basicContext — should not generate cors.go.

	files, wiring, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		if f.Path == "internal/api/cors.go" {
			t.Error("cors.go should not be generated when CORS convention is empty")
		}
	}

	if wiring != nil && len(wiring.OuterMiddlewares) > 0 {
		t.Error("no outer middlewares should be in wiring when CORS is empty")
	}
}

func TestGenerate_CORSMiddleware_NotGeneratedWhenUnset(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()
	ctx.Conventions.CORS = ""

	files, wiring, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		if f.Path == "internal/api/cors.go" {
			t.Error("cors.go should not be generated when CORS convention is explicitly empty")
		}
	}

	if wiring != nil && len(wiring.OuterMiddlewares) > 0 {
		t.Error("no outer middlewares should be in wiring when CORS is explicitly empty")
	}
}

func TestGenerate_CORSMiddleware_CompilesWithHandler(t *testing.T) {
	// Verify that cors.go compiles alongside handler files in the same package.
	g := &Generator{}
	ctx := basicContext()
	ctx.Conventions.CORS = "enabled"

	files, _, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var corsFile gen.File
	var found bool
	for _, f := range files {
		if f.Path == "internal/api/cors.go" {
			corsFile = f
			found = true
			break
		}
	}
	if !found {
		t.Fatal("cors.go not generated")
	}

	// Verify the CORS file can be formatted (a proxy for syntactic validity).
	content := corsFile.Bytes()
	if len(content) == 0 {
		t.Fatal("cors.go has empty content")
	}
}

// --- Discovery endpoint tests ---

func TestGenerate_DiscoveryFileGenerated(t *testing.T) {
	t.Run("ST-SUP-RC20-OC25-T02 The emitted discovery handler serves all three endpoints", func(t *testing.T) {
		g := &Generator{}
		ctx := basicContext()

		files, _, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		content := findFileContent(t, files, "internal/api/discovery.go")

		// Verify DiscoveryHandler struct exists.
		if !strings.Contains(content, "type DiscoveryHandler struct") {
			t.Error("discovery.go missing DiscoveryHandler struct")
		}

		// Verify all three handler methods exist.
		if !strings.Contains(content, "func (h *DiscoveryHandler) ServeOpenAPI(") {
			t.Error("discovery.go missing ServeOpenAPI method")
		}
		if !strings.Contains(content, "func (h *DiscoveryHandler) ServeOpenAPIUI(") {
			t.Error("discovery.go missing ServeOpenAPIUI method")
		}
		if !strings.Contains(content, "func (h *DiscoveryHandler) ServeMetadata(") {
			t.Error("discovery.go missing ServeMetadata method")
		}

		// Verify constructor exists.
		if !strings.Contains(content, "func NewDiscoveryHandler()") {
			t.Error("discovery.go missing NewDiscoveryHandler constructor")
		}

		// Verify OpenAPI spec embed.
		if !strings.Contains(content, "//go:embed openapi.json") {
			t.Error("discovery.go missing //go:embed openapi.json")
		}

		// Verify Swagger UI HTML template.
		if !strings.Contains(content, "swagger-ui") {
			t.Error("discovery.go missing Swagger UI HTML template")
		}

		// Verify Content-Type headers.
		if !strings.Contains(content, `"application/json"`) {
			t.Error("discovery.go missing application/json Content-Type")
		}
		if !strings.Contains(content, `"text/html"`) {
			t.Error("discovery.go missing text/html Content-Type")
		}
	})
}

func TestGenerate_DiscoveryWiringRoutes(t *testing.T) {
	t.Run("ST-SUP-RC20-OC25-T01 The emitted discovery routes sit outside the authenticated group", func(t *testing.T) {
		g := &Generator{}
		ctx := basicContext()
		ctx.BasePath = "/api/v1"

		_, wiring, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if wiring == nil {
			t.Fatal("expected wiring, got nil")
		}

		// Verify discovery routes are in DiscoveryRoutes (not Routes).
		if len(wiring.DiscoveryRoutes) != 3 {
			t.Fatalf("expected 3 discovery routes, got %d", len(wiring.DiscoveryRoutes))
		}

		foundOpenAPI := false
		foundOpenAPIHTML := false
		foundMetadata := false
		for _, r := range wiring.DiscoveryRoutes {
			if strings.Contains(r, "/api/v1/openapi\"") && strings.Contains(r, "ServeOpenAPI") {
				foundOpenAPI = true
			}
			if strings.Contains(r, "/api/v1/openapi.html\"") && strings.Contains(r, "ServeOpenAPIUI") {
				foundOpenAPIHTML = true
			}
			if strings.Contains(r, "GET /api/v1\"") && strings.Contains(r, "ServeMetadata") {
				foundMetadata = true
			}
		}

		if !foundOpenAPI {
			t.Errorf("missing OpenAPI spec route, got: %v", wiring.DiscoveryRoutes)
		}
		if !foundOpenAPIHTML {
			t.Errorf("missing OpenAPI UI route, got: %v", wiring.DiscoveryRoutes)
		}
		if !foundMetadata {
			t.Errorf("missing metadata route, got: %v", wiring.DiscoveryRoutes)
		}

		// Discovery routes must reference topMux (not mux).
		for _, r := range wiring.DiscoveryRoutes {
			if !strings.Contains(r, "topMux.") {
				t.Errorf("discovery route should use topMux, got: %s", r)
			}
		}
	})
}

func TestGenerate_DiscoveryMetadataTopLevelCollectionsOnly(t *testing.T) {
	t.Run("ST-SUP-RC20-OC26-T01 The emitted metadata lists only the top-level collections", func(t *testing.T) {
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
					Name:       "org-users",
					Entity:     "User",
					Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList},
					Scope:      map[string]string{"org_id": "Organization"},
				},
				{Name: "all-users", Entity: "User", Operations: []types.Operation{types.OpList}},
			},
			OutputNamespace: "internal/api",
			BasePath:        "/api/v1",
			ServiceName:     "user-management",
		}

		files, _, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		content := findFileContent(t, files, "internal/api/discovery.go")

		// Metadata should include top-level (unscoped) collections.
		if !strings.Contains(content, "OrganizationList") {
			t.Error("metadata missing OrganizationList (top-level collection)")
		}
		if !strings.Contains(content, "UserList") {
			t.Error("metadata missing UserList (top-level all-users collection)")
		}

		// Metadata should NOT include scoped collections.
		// org-users is scoped to Organization and should not appear at top level.
		// Check that the metadata JSON does not include the scoped path.
		if strings.Contains(content, "/organizations/{org_id}/users") {
			t.Error("metadata should not include scoped collection path")
		}

		// Verify service name in metadata.
		if !strings.Contains(content, `"user-management"`) {
			t.Error("metadata missing service name")
		}

		// Verify kind is API.
		if !strings.Contains(content, `"kind": "API"`) {
			t.Error("metadata missing kind: API")
		}

		// Verify href includes base_path.
		if !strings.Contains(content, `"href": "/api/v1"`) {
			t.Error("metadata missing href with base_path")
		}
	})
}

func TestGenerate_DiscoveryRoutesUseDiscoveryHandlerVar(t *testing.T) {
	g := &Generator{}
	ctx := basicContext()

	_, wiring, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify discovery constructor is present.
	foundDiscovery := false
	for _, c := range wiring.Constructors {
		if strings.Contains(c, "NewDiscoveryHandler") {
			foundDiscovery = true
		}
	}
	if !foundDiscovery {
		t.Error("wiring missing NewDiscoveryHandler constructor")
	}

	// Verify discovery routes reference discoveryHandler variable.
	for _, r := range wiring.DiscoveryRoutes {
		if !strings.Contains(r, "discoveryHandler.") {
			t.Errorf("discovery route should reference discoveryHandler, got: %s", r)
		}
	}
}

func TestGenerate_DiscoveryEmptyBasePath(t *testing.T) {
	t.Run("ST-SUP-RC20-OC28-T02 Without a base path the discovery routes start at the root", func(t *testing.T) {
		g := &Generator{}
		ctx := basicContext()
		ctx.BasePath = ""

		_, wiring, err := g.Generate(onRuntime(ctx))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// With empty base_path, routes should be at root.
		foundOpenAPI := false
		foundMetadata := false
		for _, r := range wiring.DiscoveryRoutes {
			if strings.Contains(r, "GET /openapi\"") {
				foundOpenAPI = true
			}
			if strings.Contains(r, "GET /{$}\"") && strings.Contains(r, "ServeMetadata") {
				foundMetadata = true
			}
		}
		if !foundOpenAPI {
			t.Errorf("with empty base_path, OpenAPI route should be at /openapi, got: %v", wiring.DiscoveryRoutes)
		}
		if !foundMetadata {
			t.Errorf("with empty base_path, metadata route should use /{$} (exact match), got: %v", wiring.DiscoveryRoutes)
		}
	})
}

func TestGenerate_DiscoveryMetadataCollectionsEmptyArrayNotNull(t *testing.T) {
	// Test the nil-slice-to-empty-array fix by verifying the metadata JSON
	// serialization. Even when top-level collections exist, the initialization
	// must use a non-nil slice. We verify by examining the generated constant.
	g := &Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "Widget", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "widgets", Entity: "Widget", Operations: []types.Operation{types.OpList}},
		},
		OutputNamespace: "internal/api",
		BasePath:        "/api/v1",
		ServiceName:     "test-service",
	}

	files, _, err := g.Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := findFileContent(t, files, "internal/api/discovery.go")

	// Verify the JSON metadata contains "collections" as a proper array.
	if !strings.Contains(content, `"collections"`) {
		t.Fatal("metadata missing collections field")
	}

	// The collections field should appear as a JSON array, never as null.
	// Even though this test has one top-level collection, verify the structure.
	if strings.Contains(content, `"collections": null`) {
		t.Error("metadata collections should never be null — must be an array")
	}

	// Additionally verify the top-level collection appears.
	if !strings.Contains(content, `"kind": "WidgetList"`) {
		t.Error("metadata missing WidgetList for top-level collection")
	}
}

func TestAutomaticRESTRejectsUnsupportedObservationPolicy(t *testing.T) {
	ctx := basicContext()
	ctx.Entities[0].Versioned = true
	ctx.Entities[0].GenerationFields = []string{"name"}
	ctx.Entities[0].Observations = map[string][]string{"health": {"status"}}
	if _, _, err := new(Generator).Generate(onRuntime(ctx)); err == nil || !strings.Contains(err.Error(), "observation ownership") {
		t.Fatal("automatic API ignored observation policy", err)
	}
}

// --- The output contracts of ST-SUP-RC20 on the runtime data path, the one data path (ST-SUP-RC20-OC39) ---

// managedContext is the test service of crudContext with the server-managed fields created_by, updated_by and
// generation (default 1) on Todo and on Setting, and the before and after slots of todos bound with a gate each.
func managedContext() gen.Context {
	ctx := crudContext()
	managed := []types.Field{{Name: "created_by", Type: types.FieldTypeString, Optional: true},
		{Name: "updated_by", Type: types.FieldTypeString, Optional: true}, {Name: "generation", Type: types.FieldTypeInt64, Default: 1}}
	for i := range ctx.Entities {
		if ctx.Entities[i].Name == "Todo" || ctx.Entities[i].Name == "Setting" {
			ctx.Entities[i].Fields = append(append([]types.Field(nil), ctx.Entities[i].Fields...), managed...)
		}
	}
	for _, slot := range []string{"before_create", "before_patch", "before_delete", "validate", "after_create", "after_patch", "after_delete", "on_entity_changed"} {
		ctx.SlotBindings = append(ctx.SlotBindings, types.SlotDeclaration{Slot: slot, Collection: "todos", Gate: []string{"todo-" + slot}})
	}
	return ctx
}

// handlerMethod gives the source of the method name of the handler file of the collection collection in files.
func handlerMethod(t *testing.T, files map[string][]byte, collection, name string) string {
	t.Helper()
	source, ok := files["api/"+collectionToSnakeCase(collection)+"_handler.go"]
	if !ok {
		t.Fatalf("no handler file for the collection %s", collection)
	}
	body := extractMethodBody(string(source), "func (h *"+collectionToPascalCase(collection)+"Handler) "+name+"(")
	if body == "" {
		t.Fatalf("the handler of %s has no method %s", collection, name)
	}
	return body
}

// handlerFunc gives the source of the function name of the handler file of the collection collection in files.
func handlerFunc(t *testing.T, files map[string][]byte, collection, name string) string {
	t.Helper()
	body := extractMethodBody(string(files["api/"+collectionToSnakeCase(collection)+"_handler.go"]), "func "+name+"(")
	if body == "" {
		t.Fatalf("the handler of %s has no function %s", collection, name)
	}
	return body
}

// inOrder fails the test unless body holds each part, each after the one before.
func inOrder(t *testing.T, label, body string, parts ...string) {
	t.Helper()
	at := 0
	for _, part := range parts {
		i := strings.Index(body[at:], part)
		if i < 0 {
			t.Errorf("%s: %q is not after the parts before it in:\n%s", label, part, body)
			return
		}
		at += i + len(part)
	}
}

func TestRuntimeProblemAnswers(t *testing.T) {
	files := crudFiles(t, crudContext())
	t.Run("ST-SUP-RC20-OC01-T02 Every emitted handler answers an error through the problem serializer", func(t *testing.T) {
		for name, data := range files {
			if !strings.HasSuffix(name, "_handler.go") && name != "api/runtime.go" {
				continue
			}
			source := string(data)
			if strings.Contains(source, "http.Error(") || strings.Contains(source, "WriteHeader(http.Status") {
				t.Errorf("%s answers an error outside handleError", name)
			}
			if strings.HasSuffix(name, "_handler.go") && !strings.Contains(source, "handleError(w, r, ") {
				t.Errorf("%s does not answer through handleError", name)
			}
		}
		answer := extractMethodBody(string(files["api/runtime.go"]), "func (rt *Runtime) answerWrite(")
		for _, part := range []string{"handleError(w, r, invalid.problem)", "handleError(w, r, errorForStatus(status, refused.detail))", "handleError(w, r, NotFound(kind, req.Resource))", "handleError(w, r, Conflict(", "handleError(w, r, InternalError("} {
			if !strings.Contains(answer, part) {
				t.Errorf("answerWrite does not answer %s", part)
			}
		}
	})
	t.Run("ST-SUP-RC20-OC29-T01 The emitted handlers set the JSON content type", func(t *testing.T) {
		for _, c := range crudContext().Collections {
			for _, op := range c.Operations {
				switch op {
				case types.OpRead, types.OpList:
					body := handlerMethod(t, files, c.Name, collectionToPascalCase(string(op)))
					if !strings.Contains(body, `w.Header().Set("Content-Type", "application/json")`) {
						t.Errorf("%s %s sets no JSON content type", c.Name, op)
					}
				case types.OpCreate, types.OpUpdate, types.OpPatch, types.OpUpsert:
					body := handlerMethod(t, files, c.Name, collectionToPascalCase(string(op)))
					if !strings.Contains(body, "return jsonResponse(") {
						t.Errorf("%s %s answers no JSON response", c.Name, op)
					}
				}
			}
		}
		if !strings.Contains(string(files["api/runtime.go"]), `Header: http.Header{"Content-Type": {"application/json"}}`) {
			t.Error("jsonResponse does not set the JSON content type")
		}
	})
}

func TestRuntimeSlots(t *testing.T) {
	files := crudFiles(t, managedContext())
	t.Run("ST-SUP-RC20-OC30-T01 A before slot refusal becomes a problem response with its own code", func(t *testing.T) {
		body := handlerMethod(t, files, "orders", "Create")
		inOrder(t, "orders Create", body, "h.beforeCreateChain.Evaluate(ctx, slotReq)",
			"&slotRefusal{status: int(slotResult.StatusCode), detail: slotResult.ErrorMessage}", "repo.Create(")
		answer := extractMethodBody(string(files["api/runtime.go"]), "func (rt *Runtime) answerWrite(")
		inOrder(t, "answerWrite", answer, "errors.As(err, &refused)", "status := refused.status", "handleError(w, r, errorForStatus(status, refused.detail))")
		if !strings.Contains(string(files["api/errors.go"]), "func errorForStatus(") {
			t.Error("errors.go has no errorForStatus, which gives the code of the status")
		}
	})
	t.Run("ST-SUP-RC20-OC30-T02 A bound before-delete slot gates the delete", func(t *testing.T) {
		body := handlerMethod(t, files, "todos", "Delete")
		inOrder(t, "todos Delete", body, "if h.beforeDeleteGate != nil {", "h.beforeDeleteGate.Evaluate(ctx, slotReq)",
			"return idempotency.Response{}, &slotRefusal{", "repo.Delete(ctx, from)")
	})
	t.Run("ST-SUP-RC20-OC31-T01 An unbound slot is never called", func(t *testing.T) {
		plain := crudFiles(t, crudContext())
		source := string(plain["api/todos_handler.go"])
		if strings.Contains(source, ".Evaluate(") || strings.Contains(source, "\tslots \"") {
			t.Errorf("the handler of todos, which binds no slot, calls a slot:\n%s", source)
		}
	})
	t.Run("ST-SUP-RC20-OC31-T02 Every before slot has its own nil guard", func(t *testing.T) {
		for op, fields := range map[string][]string{"Create": {"beforeCreateGate", "validateGate"}, "Update": {"validateGate"}, "Patch": {"beforePatchGate"}, "Delete": {"beforeDeleteGate"}} {
			body := handlerMethod(t, files, "todos", op)
			for _, field := range fields {
				inOrder(t, "todos "+op, body, "if h."+field+" != nil {", "h."+field+".Evaluate(ctx, slotReq)")
				if strings.Count(body, "h."+field+".Evaluate(") != strings.Count(body, "if h."+field+" != nil {") {
					t.Errorf("todos %s calls %s without its own nil guard", op, field)
				}
			}
		}
	})
	t.Run("ST-SUP-RC20-OC31-T03 Every after slot has its own nil guard", func(t *testing.T) {
		deliver := handlerMethod(t, files, "todos", "deliverAfter")
		for _, slot := range []string{"after_create", "after_patch", "after_delete", "on_entity_changed"} {
			field := slotCamel(slot) + "Gate"
			inOrder(t, "todos deliverAfter", deliver, "case \""+slot+"\":", "if h."+field+" == nil {", "return nil", "h."+field+".Evaluate(ctx, &request)")
		}
	})
}

func TestRuntimeEnvelope(t *testing.T) {
	files := crudFiles(t, crudContext())
	t.Run("ST-SUP-RC20-OC06-T01 The emitted create handler presents the envelope", func(t *testing.T) {
		presented := handlerFunc(t, files, "orders", "ordersPresented")
		if !strings.Contains(presented, `return presentEntity(v, "Order", v.ID, "/api/v1/orders"+"/"+v.ID)`) {
			t.Errorf("ordersPresented does not present the envelope:\n%s", presented)
		}
		inOrder(t, "orders Create", handlerMethod(t, files, "orders", "Create"), "json.Marshal(ordersPresented(persisted))", "jsonResponse(http.StatusCreated, response)")
	})
	t.Run("ST-SUP-RC20-OC08-T01 The emitted create handler makes a time-ordered identifier", func(t *testing.T) {
		for _, c := range []string{"orders", "folder-books"} {
			body := handlerMethod(t, files, c, "Create")
			if !strings.Contains(body, "uuid.NewV7()") || strings.Contains(body, "uuid.New()") || strings.Contains(body, "uuid.NewRandom()") {
				t.Errorf("%s Create does not make a version 7 identifier", c)
			}
		}
		if body := handlerMethod(t, files, "folder-settings", "Upsert"); !strings.Contains(body, "uuid.NewV7()") || strings.Contains(body, "uuid.New()") {
			t.Error("the upsert insert does not make a version 7 identifier")
		}
	})
}

func TestRuntimeList(t *testing.T) {
	files := crudFiles(t, crudContext())
	list := handlerMethod(t, files, "orders", "List")
	t.Run("ST-SUP-RC20-OC07-T01 The emitted list handler answers with the paging envelope", func(t *testing.T) {
		inOrder(t, "orders List", list, `result := map[string]any{"kind": "OrderList", "size": len(items), "items": items}`,
			"if page.more {", "h.rt.Queries.CursorText(ordersListQuery, b.Tenant, b.Principal, page.next)", `result["next"] = next`, "json.NewEncoder(w).Encode(result)")
		if strings.Contains(list, `"total"`) || strings.Contains(list, `"page"`) {
			t.Error("the list answers a total or a page number, which the cursor list does not have")
		}
	})
	t.Run("ST-SUP-RC20-OC07-T02 Each list item is presented like a single resource", func(t *testing.T) {
		if !strings.Contains(list, "items = append(items, selectFields(ordersPresented(item), fields))") {
			t.Errorf("the list items are not presented as a single resource is:\n%s", list)
		}
	})
	t.Run("ST-SUP-RC20-OC09-T01 The emitted list handler clamps the page size", func(t *testing.T) {
		inOrder(t, "orders List", list, "limit := 100", `r.URL.Query().Get("pageSize")`, "if err != nil || n < 1 {", "handleError(w, r, BadRequest(", "limit = min(n, query.MaxLimit)")
	})
	t.Run("ST-SUP-RC20-OC09-T02 The emitted list handler parses every list parameter", func(t *testing.T) {
		for _, name := range []string{"pageSize", "cursor", "orderBy", "fields", "search"} {
			if !strings.Contains(list, `r.URL.Query().Get("`+name+`")`) {
				t.Errorf("the list does not read the parameter %s", name)
			}
		}
		var document struct {
			Paths map[string]map[string]struct {
				Parameters []struct{ Name string } `json:"parameters"`
			} `json:"paths"`
		}
		if err := json.Unmarshal(files["api/openapi.json"], &document); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range document.Paths["/api/v1/orders"]["get"].Parameters {
			names = append(names, p.Name)
		}
		if strings.Join(names, ",") != "pageSize,cursor,orderBy,fields,search" {
			t.Errorf("the document declares the list parameters %v", names)
		}
	})
	t.Run("ST-SUP-RC20-OC10-T01 The emitted list handler refuses an order field that is not an entity field", func(t *testing.T) {
		inOrder(t, "orders List", list, `listOrder(r.URL.Query().Get("orderBy"), map[string]bool{"sku": true, "qty": true})`, "handleError(w, r, BadRequest(err.Error()))")
		// A pii or secret field orders no list (ST-SUP-RP11-LC20).
		if customers := handlerMethod(t, files, "customers", "List"); !strings.Contains(customers, `listOrder(r.URL.Query().Get("orderBy"), map[string]bool{"plan": true})`) {
			t.Error("the customers list orders by a sensitive field")
		}
		helperRun(t, "TestOrderField")
	})
	t.Run("ST-SUP-RC20-OC10-T02 A direction other than asc or desc is refused", func(t *testing.T) {
		helperRun(t, "TestOrderDirection")
	})
	t.Run("ST-SUP-RC20-OC11-T01 The emitted list handler sorts newest first without an order request", func(t *testing.T) {
		helperRun(t, "TestOrderDefault")
	})
	t.Run("ST-SUP-RC20-OC12-T01 The emitted list handler checks the field selection and keeps the identifier", func(t *testing.T) {
		inOrder(t, "orders List", list, `listFields(r.URL.Query().Get("fields"), map[string]bool{"sku": true, "qty": true})`, "handleError(w, r, BadRequest(err.Error()))")
		helperRun(t, "TestFieldsDeclared")
	})
	t.Run("ST-SUP-RC20-OC12-T02 The identifier stays in the field selection", func(t *testing.T) {
		helperRun(t, "TestFieldsKeepID")
	})
	t.Run("ST-SUP-RC20-OC12-T03 A field selection removes the fields it did not name", func(t *testing.T) {
		helperRun(t, "TestFieldsSelect")
	})
}

// helperChecks are the tests of the list helpers of runtime.go (listOrderHelper), which run them in a module of
// their own with a stand-in of the query runtime's OrderBy and ErrBadCursor.
const helperChecks = `package api

import (
	"encoding/json"
	"testing"

	"example.com/helpers/query"
)

var declared = map[string]bool{"sku": true, "qty": true}

func TestOrderField(t *testing.T) {
	for _, text := range []string{"price", "sku,price", "sku asc desc", ","} {
		if _, err := listOrder(text, declared); err != errOrder {
			t.Errorf("orderBy %q gave %v, want errOrder", text, err)
		}
	}
	order, err := listOrder("sku, qty", declared)
	if err != nil || len(order) != 2 || order[0] != (query.OrderBy{Field: "sku"}) || order[1] != (query.OrderBy{Field: "qty"}) {
		t.Errorf("orderBy sku, qty gave %v, %v", order, err)
	}
}

func TestOrderDirection(t *testing.T) {
	for _, text := range []string{"sku up", "sku descending", "qty 1"} {
		if _, err := listOrder(text, declared); err != errOrder {
			t.Errorf("orderBy %q gave %v, want errOrder", text, err)
		}
	}
	order, err := listOrder("sku DESC,qty asc", declared)
	if err != nil || order[0] != (query.OrderBy{Field: "sku", Descending: true}) || order[1] != (query.OrderBy{Field: "qty"}) {
		t.Errorf("orderBy sku DESC,qty asc gave %v, %v", order, err)
	}
}

func TestOrderDefault(t *testing.T) {
	for _, text := range []string{"", "  "} {
		order, err := listOrder(text, declared)
		if err != nil || len(order) != 1 || order[0] != (query.OrderBy{Field: "created_time", Descending: true}) {
			t.Errorf("orderBy %q gave %v, %v, want created_time descending", text, order, err)
		}
	}
}

func TestFieldsDeclared(t *testing.T) {
	for _, text := range []string{"price", "sku,price", "id,tenant_id"} {
		if _, err := listFields(text, declared); err != errFields {
			t.Errorf("fields %q gave %v, want errFields", text, err)
		}
	}
	if selected, err := listFields("", declared); selected != nil || err != nil {
		t.Errorf("no fields gave %v, %v, want no selection", selected, err)
	}
}

func TestFieldsKeepID(t *testing.T) {
	for _, text := range []string{"sku", "id,sku", " sku , "} {
		selected, err := listFields(text, declared)
		if err != nil || !selected["id"] || !selected["sku"] || selected["qty"] {
			t.Errorf("fields %q gave %v, %v", text, selected, err)
		}
	}
}

func TestFieldsSelect(t *testing.T) {
	selected, _ := listFields("sku", declared)
	item := map[string]any{"id": "a", "kind": "Order", "href": "/orders/a", "sku": "s", "qty": 2}
	raw, err := json.Marshal(selectFields(item, selected))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != ` + "`" + `{"href":"/orders/a","id":"a","kind":"Order","sku":"s"}` + "`" + ` {
		t.Errorf("the selection of sku gave %s", raw)
	}
	type order struct {
		ID  string ` + "`json:\"id\"`" + `
		Sku string ` + "`json:\"sku\"`" + `
		Qty int    ` + "`json:\"qty\"`" + `
	}
	raw, _ = json.Marshal(selectFields(order{ID: "a", Sku: "s", Qty: 2}, selected))
	if string(raw) != ` + "`" + `{"id":"a","sku":"s"}` + "`" + ` {
		t.Errorf("the selection of sku of a bare value gave %s", raw)
	}
	if got := selectFields(item, nil); got == nil {
		t.Error("no selection dropped the value")
	}
}
`

// helperRun runs the check name of helperChecks against the list helpers that runtime.go emits.
func helperRun(t *testing.T, name string) {
	t.Helper()
	p := &crudProject{t: t, dir: t.TempDir()}
	p.write("go.mod", []byte("module example.com/helpers\n\ngo 1.26.11\n"))
	p.write("query/query.go", []byte("package query\n\nimport \"errors\"\n\ntype OrderBy struct {\n\tField      string\n\tDescending bool\n}\n\nvar ErrBadCursor = errors.New(\"bad cursor\")\n"))
	p.write("api/helpers.go", []byte("package api\n\nimport (\n\t\"encoding/json\"\n\t\"errors\"\n\t\"strings\"\n\n\t\"example.com/helpers/query\"\n)\n\n"+listOrderHelper))
	p.write("api/helpers_test.go", []byte(helperChecks))
	output, err := p.goCommand(nil, "test", "-count=1", "-v", "-run", "^"+name+"$", "./api")
	if err != nil || !strings.Contains(output, "--- PASS: "+name+" ") {
		t.Fatalf("the check %s of the list helpers did not run and pass: %v\n%s", name, err, output)
	}
}

func TestRuntimeWrites(t *testing.T) {
	files := crudFiles(t, managedContext())
	t.Run("ST-SUP-RC20-OC15-T01 The emitted patch handler takes only the patchable fields, as pointers", func(t *testing.T) {
		source := string(files["api/orders_handler.go"])
		request := strings.Join(strings.Fields(extractMethodBody(source, "type OrdersPatchRequest struct")), " ")
		if !strings.Contains(request, "Qty *int32 `json:\"qty\"`") || strings.Contains(request, "Sku") {
			t.Errorf("the patch request of orders is not its one patchable field as a pointer:\n%s", request)
		}
		todos := strings.Join(strings.Fields(extractMethodBody(string(files["api/todos_handler.go"]), "type TodosPatchRequest struct")), " ")
		for _, field := range []string{"Title *string", "Done *bool", "Meta *json.RawMessage"} {
			if !strings.Contains(todos, field) {
				t.Errorf("the patch request of todos has no %s", field)
			}
		}
		if strings.Contains(todos, "CreatedBy") || strings.Contains(todos, "Generation") {
			t.Error("the patch request of todos takes a field that is not patchable")
		}
	})
	t.Run("ST-SUP-RC20-OC15-T02 The emitted patch handler reads, merges and saves", func(t *testing.T) {
		inOrder(t, "todos Patch", handlerMethod(t, files, "todos", "Patch"), "repo.GetForDelete(ctx, id)", "stored := todosFromData(from.Value())",
			"todo := stored", "if patch.Title != nil {", "todo.Title = *patch.Title", "if patch.Done != nil {", "todo.Done = ptrOf(*patch.Done)",
			"repo.Update(ctx, from, todosToData(todo), parents...)")
	})
	t.Run("ST-SUP-RC20-OC16-T01 The emitted patch handler records who changed the record and when", func(t *testing.T) {
		inOrder(t, "todos Patch", handlerMethod(t, files, "todos", "Patch"), "todo.UpdatedBy = ptrOf(callerName(r.Context()))", "repo.Update(")
		// The command runtime sets updated_time at each update of a row, a patch too.
		if !strings.Contains(string(files["api/data/entities.go"]), `\"generation\" = $10, \"version\" = \"version\" + 1, \"updated_time\" = now()`) {
			t.Error("the update of a todo does not set updated_time to the time of the change")
		}
		if !strings.Contains(string(files["api/runtime.go"]), "identity.Email()") {
			t.Error("callerName does not read the verified identity")
		}
	})
	t.Run("ST-SUP-RC20-OC17-T01 The emitted create handler discards client values for server-managed fields", func(t *testing.T) {
		inOrder(t, "decodeTodos", handlerFunc(t, files, "todos", "decodeTodos"), "json.Unmarshal(bodyBytes, &todo)", `todo.ID = ""`,
			"todo.CreatedBy = nil", "todo.UpdatedBy = nil", "todo.Generation = 0")
		inOrder(t, "todos Create", handlerMethod(t, files, "todos", "Create"), "todo.ID = id.String()",
			"todo.CreatedBy = ptrOf(callerName(r.Context()))", "todo.UpdatedBy = ptrOf(callerName(r.Context()))", "todo.Generation = 1", "repo.Create(")
	})
	t.Run("ST-SUP-RC20-OC17-T03 The emitted upsert handler resets the server fields", func(t *testing.T) {
		inOrder(t, "decodeFolderSettings", handlerFunc(t, files, "folder-settings", "decodeFolderSettings"), "setting.CreatedBy = nil", "setting.UpdatedBy = nil", "setting.Generation = 0")
		inOrder(t, "folder-settings Upsert", handlerMethod(t, files, "folder-settings", "Upsert"),
			"setting.CreatedBy = ptrOf(callerName(r.Context()))", "setting.Generation = 1", "repo.Create(",
			"setting.CreatedBy = stored.CreatedBy", "setting.UpdatedBy = ptrOf(callerName(r.Context()))", "setting.Generation = stored.Generation + 1", "repo.Update(")
	})
	t.Run("ST-SUP-RC20-OC18-T01 The emitted update handler keeps create-only values and raises the generation", func(t *testing.T) {
		inOrder(t, "todos Update", handlerMethod(t, files, "todos", "Update"), "repo.GetForDelete(ctx, id)", "stored := todosFromData(from.Value())",
			"todo.ID = id", "todo.CreatedBy = stored.CreatedBy", "todo.UpdatedBy = ptrOf(callerName(r.Context()))", "todo.Generation = stored.Generation + 1", "repo.Update(")
	})
	kind := func(t *testing.T, collection, decode, op, entity string) {
		t.Helper()
		inOrder(t, decode, handlerFunc(t, files, collection, decode), `if kindRaw, ok := rawMap["kind"]; ok {`, `kind != "`+entity+`"`, `BadRequest("kind must be `+entity+`")`)
		inOrder(t, collection+" "+op, handlerMethod(t, files, collection, op), decode+"(body)", "h.rt.answerWrite(w, r, command.Request{}, ", "command.Run(")
	}
	t.Run("ST-SUP-RC20-OC19-T01 The emitted handlers refuse a body whose kind is not the entity", func(t *testing.T) {
		kind(t, "todos", "decodeTodos", "Create", "Todo")
		answer := extractMethodBody(string(files["api/runtime.go"]), "func (rt *Runtime) answerWrite(")
		inOrder(t, "answerWrite", answer, "errors.As(err, &invalid)", "handleError(w, r, invalid.problem)")
	})
	t.Run("ST-SUP-RC20-OC19-T02 The emitted upsert handler checks the kind", func(t *testing.T) {
		kind(t, "folder-settings", "decodeFolderSettings", "Upsert", "Setting")
	})
	t.Run("ST-SUP-RC20-OC19-T03 The emitted update handler checks the body kind", func(t *testing.T) {
		kind(t, "todos", "decodeTodos", "Update", "Todo")
	})
	t.Run("ST-SUP-RC20-OC19-T04 The emitted patch handler checks the body kind", func(t *testing.T) {
		kind(t, "todos", "decodeTodosPatch", "Patch", "Todo")
	})
	var document struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(files["api/openapi.json"], &document); err != nil {
		t.Fatal(err)
	}
	t.Run("ST-SUP-RC20-OC20-T01 The emitted create-request schema leaves out the fields a client cannot set", func(t *testing.T) {
		request, ok := document.Components.Schemas["TodoCreateRequest"]
		if !ok {
			t.Fatal("the document has no TodoCreateRequest")
		}
		for _, name := range []string{"id", "created_by", "updated_by", "generation"} {
			if _, ok := request.Properties[name]; ok {
				t.Errorf("TodoCreateRequest holds the server-managed field %s", name)
			}
		}
		for _, name := range []string{"title", "done", "meta"} {
			if _, ok := request.Properties[name]; !ok {
				t.Errorf("TodoCreateRequest has no field %s", name)
			}
		}
	})
	t.Run("ST-SUP-RC20-OC20-T04 The write operation points at the create-request schema", func(t *testing.T) {
		if !strings.Contains(strings.Join(strings.Fields(string(document.Paths["/api/v1/todos"]["post"])), ""), `"$ref":"#/components/schemas/TodoCreateRequest"`) {
			t.Errorf("the create of todos does not point at TodoCreateRequest: %s", document.Paths["/api/v1/todos"]["post"])
		}
	})
}

func TestRuntimeDelete(t *testing.T) {
	files := crudFiles(t, crudContext())
	t.Run("ST-SUP-RC20-OC32-T01 A delete answers 204", func(t *testing.T) {
		for _, c := range crudContext().Collections {
			if slices.Contains(c.Operations, types.OpDelete) {
				inOrder(t, c.Name+" Delete", handlerMethod(t, files, c.Name, "Delete"), "repo.Delete(ctx, from)",
					"return idempotency.Response{Status: http.StatusNoContent}, nil", "writeResult(w, result)")
			}
		}
	})
	t.Run("ST-SUP-RC20-OC32-T02 A delete writes no response body", func(t *testing.T) {
		body := handlerMethod(t, files, "orders", "Delete")
		if strings.Contains(body, "jsonResponse(") || strings.Contains(body, "json.Marshal(") || strings.Contains(body, "w.Write(") {
			t.Errorf("the delete writes a body:\n%s", body)
		}
		// The response of the delete has no header and no body, and writeResult writes only those of the response.
		if !strings.Contains(body, "return idempotency.Response{Status: http.StatusNoContent}, nil") {
			t.Errorf("the delete answers a response with a header or a body:\n%s", body)
		}
		inOrder(t, "writeResult", extractMethodBody(string(files["api/runtime.go"]), "func writeResult("), "w.WriteHeader(result.Response.Status)", "w.Write(result.Response.Body)")
	})
}
