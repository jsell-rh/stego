// Package restapi implements the rest-api component Generator. It produces
// HTTP handler files, route registration, middleware wiring, and an OpenAPI
// spec from the service declaration's entities and expose blocks.
package restapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"path"
	"slices"
	"strings"

	"github.com/stego-project/stego/internal/gen"
	"github.com/stego-project/stego/internal/types"
)

// Generator produces the rest-api component's generated code.
type Generator struct{}

// Generate produces HTTP handler files (one per exposed entity), a router file,
// and an OpenAPI spec. It returns wiring instructions for main.go assembly.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if len(ctx.Expose) == 0 {
		return nil, nil, nil
	}

	// Build entity lookup for field resolution.
	entityMap := make(map[string]types.Entity, len(ctx.Entities))
	for _, e := range ctx.Entities {
		entityMap[e.Name] = e
	}

	// Build parent lookup: entity name -> its ExposeBlock (for nested routing).
	exposeMap := make(map[string]types.ExposeBlock, len(ctx.Expose))
	for _, eb := range ctx.Expose {
		exposeMap[eb.Entity] = eb
	}

	var files []gen.File
	wiring := &gen.Wiring{}

	// Generate handler file per exposed entity.
	for _, eb := range ctx.Expose {
		entity, ok := entityMap[eb.Entity]
		if !ok {
			return nil, nil, fmt.Errorf("expose references unknown entity %q", eb.Entity)
		}

		handlerFile, err := generateHandler(ctx.OutputNamespace, entity, eb, exposeMap)
		if err != nil {
			return nil, nil, fmt.Errorf("generating handler for %s: %w", eb.Entity, err)
		}
		files = append(files, handlerFile)

		lower := strings.ToLower(entity.Name)
		wiring.Constructors = append(wiring.Constructors,
			fmt.Sprintf("api.New%sHandler(store)", entity.Name))
		wiring.Routes = append(wiring.Routes,
			fmt.Sprintf("mux.Handle(\"%s\", %sHandler)", entityBasePath(eb, exposeMap), lower))
	}

	// Generate router file.
	routerFile, err := generateRouter(ctx.OutputNamespace, ctx.Expose, exposeMap)
	if err != nil {
		return nil, nil, fmt.Errorf("generating router: %w", err)
	}
	files = append(files, routerFile)

	// Generate OpenAPI spec.
	openapiFile, err := generateOpenAPI(ctx.OutputNamespace, ctx.Entities, ctx.Expose, exposeMap)
	if err != nil {
		return nil, nil, fmt.Errorf("generating openapi spec: %w", err)
	}
	files = append(files, openapiFile)

	wiring.Imports = []string{"internal/api"}

	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}

	return files, wiring, nil
}

// entityBasePath returns the URL path prefix for an entity's expose block.
// If PathPrefix is set, it is used directly. Otherwise, a default is derived
// from the entity name, prepended with the parent's path if nested.
func entityBasePath(eb types.ExposeBlock, exposeMap map[string]types.ExposeBlock) string {
	if eb.PathPrefix != "" {
		return eb.PathPrefix
	}
	base := "/" + strings.ToLower(eb.Entity) + "s"
	if eb.Parent != "" {
		if parentEB, ok := exposeMap[eb.Parent]; ok {
			parentPath := entityBasePath(parentEB, exposeMap)
			parentParam := "{" + strings.ToLower(eb.Parent) + "_id}"
			return parentPath + "/" + parentParam + base
		}
	}
	return base
}

// generateHandler produces a single Go handler file for an exposed entity.
func generateHandler(ns string, entity types.Entity, eb types.ExposeBlock, _ map[string]types.ExposeBlock) (gen.File, error) {
	var buf bytes.Buffer

	lower := strings.ToLower(entity.Name)
	handlerType := entity.Name + "Handler"

	fmt.Fprintf(&buf, "package api\n\n")
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"encoding/json\"\n")
	fmt.Fprintf(&buf, "\t\"net/http\"\n")
	fmt.Fprintf(&buf, ")\n\n")

	// Handler struct.
	fmt.Fprintf(&buf, "// %s handles HTTP requests for %s entities.\n", handlerType, entity.Name)
	fmt.Fprintf(&buf, "type %s struct {\n", handlerType)
	fmt.Fprintf(&buf, "\tstore Storage\n")
	fmt.Fprintf(&buf, "}\n\n")

	// Constructor.
	fmt.Fprintf(&buf, "// New%sHandler creates a new %s.\n", entity.Name, handlerType)
	fmt.Fprintf(&buf, "func New%sHandler(store Storage) *%s {\n", entity.Name, handlerType)
	fmt.Fprintf(&buf, "\treturn &%s{store: store}\n", handlerType)
	fmt.Fprintf(&buf, "}\n\n")

	// ServeHTTP dispatcher.
	fmt.Fprintf(&buf, "// ServeHTTP dispatches to the appropriate operation handler.\n")
	fmt.Fprintf(&buf, "func (h *%s) ServeHTTP(w http.ResponseWriter, r *http.Request) {\n", handlerType)

	if eb.Parent != "" {
		// Nested routing: verify parent exists.
		parentLower := strings.ToLower(eb.Parent)
		parentIDParam := parentLower + "_id"
		fmt.Fprintf(&buf, "\t// Verify parent %s exists.\n", eb.Parent)
		fmt.Fprintf(&buf, "\t%s := extractPathParam(r, %q)\n", parentIDParam, parentIDParam)
		fmt.Fprintf(&buf, "\tif %s == \"\" {\n", parentIDParam)
		fmt.Fprintf(&buf, "\t\thttp.Error(w, \"missing %s\", http.StatusBadRequest)\n", parentIDParam)
		fmt.Fprintf(&buf, "\t\treturn\n")
		fmt.Fprintf(&buf, "\t}\n")
		fmt.Fprintf(&buf, "\tif !h.store.Exists(%q, %s) {\n", eb.Parent, parentIDParam)
		fmt.Fprintf(&buf, "\t\thttp.Error(w, \"%s not found\", http.StatusNotFound)\n", eb.Parent)
		fmt.Fprintf(&buf, "\t\treturn\n")
		fmt.Fprintf(&buf, "\t}\n\n")
	}

	fmt.Fprintf(&buf, "\tswitch r.Method {\n")

	for _, op := range eb.Operations {
		switch op {
		case types.OpCreate:
			fmt.Fprintf(&buf, "\tcase http.MethodPost:\n")
			fmt.Fprintf(&buf, "\t\th.create(w, r)\n")
		case types.OpRead:
			fmt.Fprintf(&buf, "\tcase http.MethodGet:\n")
			fmt.Fprintf(&buf, "\t\tid := extractID(r)\n")
			fmt.Fprintf(&buf, "\t\tif id != \"\" {\n")
			fmt.Fprintf(&buf, "\t\t\th.read(w, r, id)\n")
			fmt.Fprintf(&buf, "\t\t} else {\n")
			if hasOp(eb.Operations, types.OpList) {
				fmt.Fprintf(&buf, "\t\t\th.list(w, r)\n")
			} else {
				fmt.Fprintf(&buf, "\t\t\thttp.Error(w, \"id required\", http.StatusBadRequest)\n")
			}
			fmt.Fprintf(&buf, "\t\t}\n")
		case types.OpUpdate:
			fmt.Fprintf(&buf, "\tcase http.MethodPut:\n")
			fmt.Fprintf(&buf, "\t\th.update(w, r)\n")
		case types.OpDelete:
			fmt.Fprintf(&buf, "\tcase http.MethodDelete:\n")
			fmt.Fprintf(&buf, "\t\th.delete(w, r)\n")
		case types.OpList:
			// Handled via GET without ID above if read is also present.
			if !hasOp(eb.Operations, types.OpRead) {
				fmt.Fprintf(&buf, "\tcase http.MethodGet:\n")
				fmt.Fprintf(&buf, "\t\th.list(w, r)\n")
			}
		case types.OpUpsert:
			fmt.Fprintf(&buf, "\tcase http.MethodPut:\n")
			fmt.Fprintf(&buf, "\t\th.upsert(w, r)\n")
		}
	}

	fmt.Fprintf(&buf, "\tdefault:\n")
	fmt.Fprintf(&buf, "\t\thttp.Error(w, \"method not allowed\", http.StatusMethodNotAllowed)\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "}\n\n")

	// Generate operation methods.
	for _, op := range eb.Operations {
		switch op {
		case types.OpCreate:
			generateCreateMethod(&buf, entity, eb)
		case types.OpRead:
			generateReadMethod(&buf, entity, eb)
		case types.OpUpdate:
			generateUpdateMethod(&buf, entity, eb)
		case types.OpDelete:
			generateDeleteMethod(&buf, entity, eb)
		case types.OpList:
			generateListMethod(&buf, entity, eb)
		case types.OpUpsert:
			generateUpsertMethod(&buf, entity, eb)
		}
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting %s handler: %w", entity.Name, err)
	}

	return gen.File{
		Path:    path.Join(ns, "handler_"+lower+".go"),
		Content: formatted,
	}, nil
}

func generateCreateMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock) {
	lower := strings.ToLower(entity.Name)
	fmt.Fprintf(buf, "func (h *%sHandler) create(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	if eb.Parent != "" {
		parentIDParam := strings.ToLower(eb.Parent) + "_id"
		fmt.Fprintf(buf, "\t%s.%s = extractPathParam(r, %q)\n", lower, parentFieldName(eb.Parent), parentIDParam)
	}
	fmt.Fprintf(buf, "\tif err := h.store.Create(%q, %s); err != nil {\n", entity.Name, lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tw.WriteHeader(http.StatusCreated)\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
}

func generateReadMethod(buf *bytes.Buffer, entity types.Entity, _ types.ExposeBlock) {
	lower := strings.ToLower(entity.Name)
	fmt.Fprintf(buf, "func (h *%sHandler) read(w http.ResponseWriter, r *http.Request, id string) {\n", entity.Name)
	fmt.Fprintf(buf, "\t%s, err := h.store.Read(%q, id)\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err != nil {\n")
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusNotFound)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
}

func generateUpdateMethod(buf *bytes.Buffer, entity types.Entity, _ types.ExposeBlock) {
	lower := strings.ToLower(entity.Name)
	fmt.Fprintf(buf, "func (h *%sHandler) update(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	fmt.Fprintf(buf, "\tid := extractID(r)\n")
	fmt.Fprintf(buf, "\tif id == \"\" {\n")
	fmt.Fprintf(buf, "\t\thttp.Error(w, \"id required\", http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tif err := h.store.Update(%q, id, %s); err != nil {\n", entity.Name, lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
}

func generateDeleteMethod(buf *bytes.Buffer, entity types.Entity, _ types.ExposeBlock) {
	fmt.Fprintf(buf, "func (h *%sHandler) delete(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	fmt.Fprintf(buf, "\tid := extractID(r)\n")
	fmt.Fprintf(buf, "\tif id == \"\" {\n")
	fmt.Fprintf(buf, "\t\thttp.Error(w, \"id required\", http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tif err := h.store.Delete(%q, id); err != nil {\n", entity.Name)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tw.WriteHeader(http.StatusNoContent)\n")
	fmt.Fprintf(buf, "}\n\n")
}

func generateListMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock) {
	lower := strings.ToLower(entity.Name) + "s"
	fmt.Fprintf(buf, "func (h *%sHandler) list(w http.ResponseWriter, r *http.Request) {\n", entity.Name)

	// Scope filtering.
	if eb.Scope != "" {
		fmt.Fprintf(buf, "\tscopeValue := extractPathParam(r, %q)\n", eb.Scope)
		fmt.Fprintf(buf, "\t%s, err := h.store.List(%q, %q, scopeValue)\n", lower, entity.Name, eb.Scope)
	} else if eb.Parent != "" {
		parentIDParam := strings.ToLower(eb.Parent) + "_id"
		parentField := parentFieldName(eb.Parent)
		fmt.Fprintf(buf, "\t%s := extractPathParam(r, %q)\n", parentIDParam, parentIDParam)
		fmt.Fprintf(buf, "\t%s, err := h.store.List(%q, %q, %s)\n", lower, entity.Name, parentField, parentIDParam)
	} else {
		fmt.Fprintf(buf, "\t%s, err := h.store.List(%q, \"\", \"\")\n", lower, entity.Name)
	}

	fmt.Fprintf(buf, "\tif err != nil {\n")
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
}

func generateUpsertMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock) {
	lower := strings.ToLower(entity.Name)
	fmt.Fprintf(buf, "func (h *%sHandler) upsert(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")

	if len(eb.UpsertKey) > 0 {
		keyFields := make([]string, len(eb.UpsertKey))
		for i, k := range eb.UpsertKey {
			keyFields[i] = fmt.Sprintf("%q", k)
		}
		fmt.Fprintf(buf, "\tupsertKey := []string{%s}\n", strings.Join(keyFields, ", "))
	} else {
		fmt.Fprintf(buf, "\tupsertKey := []string{}\n")
	}

	concurrency := string(eb.Concurrency)
	if concurrency == "" {
		concurrency = "none"
	}
	fmt.Fprintf(buf, "\tif err := h.store.Upsert(%q, %s, upsertKey, %q); err != nil {\n", entity.Name, lower, concurrency)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tw.WriteHeader(http.StatusOK)\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
}

// generateRouter produces the router.go file with route registration and helper functions.
func generateRouter(ns string, expose []types.ExposeBlock, exposeMap map[string]types.ExposeBlock) (gen.File, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package api\n\n")
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"net/http\"\n")
	fmt.Fprintf(&buf, "\t\"strings\"\n")
	fmt.Fprintf(&buf, ")\n\n")

	// Storage interface used by all handlers.
	fmt.Fprintf(&buf, "// Storage is the interface that handlers use to interact with the data store.\n")
	fmt.Fprintf(&buf, "type Storage interface {\n")
	fmt.Fprintf(&buf, "\tCreate(entity string, value any) error\n")
	fmt.Fprintf(&buf, "\tRead(entity string, id string) (any, error)\n")
	fmt.Fprintf(&buf, "\tUpdate(entity string, id string, value any) error\n")
	fmt.Fprintf(&buf, "\tDelete(entity string, id string) error\n")
	fmt.Fprintf(&buf, "\tList(entity string, scopeField string, scopeValue string) (any, error)\n")
	fmt.Fprintf(&buf, "\tUpsert(entity string, value any, upsertKey []string, concurrency string) error\n")
	fmt.Fprintf(&buf, "\tExists(entity string, id string) bool\n")
	fmt.Fprintf(&buf, "}\n\n")

	// Entity types (minimal struct for each exposed entity).
	for _, eb := range expose {
		fmt.Fprintf(&buf, "// %s represents the %s entity.\n", eb.Entity, eb.Entity)
		fmt.Fprintf(&buf, "type %s struct{}\n\n", eb.Entity)
	}

	// NewRouter function.
	fmt.Fprintf(&buf, "// NewRouter creates an http.Handler with all routes registered.\n")
	fmt.Fprintf(&buf, "func NewRouter(auth func(http.Handler) http.Handler, store Storage) http.Handler {\n")
	fmt.Fprintf(&buf, "\tmux := http.NewServeMux()\n\n")

	for _, eb := range expose {
		basePath := entityBasePath(eb, exposeMap)
		lower := strings.ToLower(eb.Entity)
		fmt.Fprintf(&buf, "\t%sHandler := New%sHandler(store)\n", lower, eb.Entity)
		fmt.Fprintf(&buf, "\tmux.Handle(\"%s/\", http.StripPrefix(\"%s\", %sHandler))\n\n",
			basePath, basePath, lower)
	}

	fmt.Fprintf(&buf, "\treturn auth(mux)\n")
	fmt.Fprintf(&buf, "}\n\n")

	// Helper functions.
	fmt.Fprintf(&buf, "// extractID extracts the trailing path segment as the resource ID.\n")
	fmt.Fprintf(&buf, "func extractID(r *http.Request) string {\n")
	fmt.Fprintf(&buf, "\tp := strings.TrimPrefix(r.URL.Path, \"/\")\n")
	fmt.Fprintf(&buf, "\tif p == \"\" {\n")
	fmt.Fprintf(&buf, "\t\treturn \"\"\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\tparts := strings.Split(p, \"/\")\n")
	fmt.Fprintf(&buf, "\treturn parts[len(parts)-1]\n")
	fmt.Fprintf(&buf, "}\n\n")

	fmt.Fprintf(&buf, "// extractPathParam extracts a named path parameter from the request URL.\n")
	fmt.Fprintf(&buf, "func extractPathParam(r *http.Request, name string) string {\n")
	fmt.Fprintf(&buf, "\treturn r.PathValue(name)\n")
	fmt.Fprintf(&buf, "}\n\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting router: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "router.go"),
		Content: formatted,
	}, nil
}

// generateOpenAPI produces an OpenAPI 3.0 spec as JSON.
func generateOpenAPI(ns string, entities []types.Entity, expose []types.ExposeBlock, exposeMap map[string]types.ExposeBlock) (gen.File, error) {
	entityMap := make(map[string]types.Entity, len(entities))
	for _, e := range entities {
		entityMap[e.Name] = e
	}

	spec := openAPISpec{
		OpenAPI: "3.0.3",
		Info: openAPIInfo{
			Title:   "Generated API",
			Version: "1.0.0",
		},
		Paths:      make(map[string]openAPIPathItem),
		Components: openAPIComponents{Schemas: make(map[string]openAPISchema)},
	}

	// Generate schemas for each entity.
	for _, e := range entities {
		schema := openAPISchema{
			Type:       "object",
			Properties: make(map[string]openAPISchema),
		}
		for _, f := range e.Fields {
			schema.Properties[f.Name] = fieldToOpenAPISchema(f)
		}
		spec.Components.Schemas[e.Name] = schema
	}

	// Generate paths from expose blocks.
	for _, eb := range expose {
		basePath := entityBasePath(eb, exposeMap)
		collectionPath := basePath
		itemPath := basePath + "/{id}"

		collectionOps := make(map[string]openAPIOperation)
		itemOps := make(map[string]openAPIOperation)

		for _, op := range eb.Operations {
			tag := eb.Entity
			ref := "#/components/schemas/" + eb.Entity

			switch op {
			case types.OpList:
				collectionOps["get"] = openAPIOperation{
					Summary:     "List " + eb.Entity + " entities",
					OperationID: "list" + eb.Entity,
					Tags:        []string{tag},
					Responses: map[string]openAPIResponse{
						"200": {Description: "Successful response", Content: jsonContent(openAPISchema{
							Type:  "array",
							Items: &openAPISchema{Ref: ref},
						})},
					},
				}
			case types.OpCreate:
				collectionOps["post"] = openAPIOperation{
					Summary:     "Create " + eb.Entity,
					OperationID: "create" + eb.Entity,
					Tags:        []string{tag},
					RequestBody: &openAPIRequestBody{
						Required: true,
						Content:  jsonContent(openAPISchema{Ref: ref}),
					},
					Responses: map[string]openAPIResponse{
						"201": {Description: "Created"},
					},
				}
			case types.OpRead:
				itemOps["get"] = openAPIOperation{
					Summary:     "Read " + eb.Entity,
					OperationID: "read" + eb.Entity,
					Tags:        []string{tag},
					Parameters: []openAPIParam{{
						Name:     "id",
						In:       "path",
						Required: true,
						Schema:   openAPISchema{Type: "string"},
					}},
					Responses: map[string]openAPIResponse{
						"200": {Description: "Successful response", Content: jsonContent(openAPISchema{Ref: ref})},
						"404": {Description: "Not found"},
					},
				}
			case types.OpUpdate:
				itemOps["put"] = openAPIOperation{
					Summary:     "Update " + eb.Entity,
					OperationID: "update" + eb.Entity,
					Tags:        []string{tag},
					Parameters: []openAPIParam{{
						Name:     "id",
						In:       "path",
						Required: true,
						Schema:   openAPISchema{Type: "string"},
					}},
					RequestBody: &openAPIRequestBody{
						Required: true,
						Content:  jsonContent(openAPISchema{Ref: ref}),
					},
					Responses: map[string]openAPIResponse{
						"200": {Description: "Updated"},
					},
				}
			case types.OpDelete:
				itemOps["delete"] = openAPIOperation{
					Summary:     "Delete " + eb.Entity,
					OperationID: "delete" + eb.Entity,
					Tags:        []string{tag},
					Parameters: []openAPIParam{{
						Name:     "id",
						In:       "path",
						Required: true,
						Schema:   openAPISchema{Type: "string"},
					}},
					Responses: map[string]openAPIResponse{
						"204": {Description: "Deleted"},
					},
				}
			case types.OpUpsert:
				collectionOps["put"] = openAPIOperation{
					Summary:     "Upsert " + eb.Entity,
					OperationID: "upsert" + eb.Entity,
					Tags:        []string{tag},
					RequestBody: &openAPIRequestBody{
						Required: true,
						Content:  jsonContent(openAPISchema{Ref: ref}),
					},
					Responses: map[string]openAPIResponse{
						"200": {Description: "Upserted"},
					},
				}
			}
		}

		if len(collectionOps) > 0 {
			spec.Paths[collectionPath] = openAPIPathItem{Operations: collectionOps}
		}
		if len(itemOps) > 0 {
			spec.Paths[itemPath] = openAPIPathItem{Operations: itemOps}
		}
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return gen.File{}, fmt.Errorf("marshaling openapi spec: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "openapi.json"),
		Content: data,
	}, nil
}

// fieldToOpenAPISchema converts a types.Field to an OpenAPI schema.
func fieldToOpenAPISchema(f types.Field) openAPISchema {
	s := openAPISchema{}
	switch f.Type {
	case types.FieldTypeString:
		s.Type = "string"
	case types.FieldTypeInt32, types.FieldTypeInt64:
		s.Type = "integer"
	case types.FieldTypeFloat, types.FieldTypeDouble:
		s.Type = "number"
	case types.FieldTypeBool:
		s.Type = "boolean"
	case types.FieldTypeBytes:
		s.Type = "string"
		s.Format = "byte"
	case types.FieldTypeTimestamp:
		s.Type = "string"
		s.Format = "date-time"
	case types.FieldTypeEnum:
		s.Type = "string"
		s.Enum = f.Values
	case types.FieldTypeRef:
		s.Type = "string"
	case types.FieldTypeJsonb:
		s.Type = "object"
	}
	return s
}

func hasOp(ops []types.Operation, target types.Operation) bool {
	return slices.Contains(ops, target)
}

func parentFieldName(parent string) string {
	return parent + "ID"
}

func jsonContent(schema openAPISchema) map[string]openAPIMediaType {
	return map[string]openAPIMediaType{
		"application/json": {Schema: schema},
	}
}

// OpenAPI types for JSON marshaling.

type openAPISpec struct {
	OpenAPI    string                       `json:"openapi"`
	Info       openAPIInfo                  `json:"info"`
	Paths      map[string]openAPIPathItem   `json:"paths"`
	Components openAPIComponents            `json:"components"`
}

type openAPIInfo struct {
	Title   string `json:"title"`
	Version string `json:"version"`
}

type openAPIPathItem struct {
	Operations map[string]openAPIOperation `json:"-"`
}

// MarshalJSON flattens the Operations map into the path item object.
func (p openAPIPathItem) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.Operations)
}

type openAPIOperation struct {
	Summary     string                       `json:"summary"`
	OperationID string                       `json:"operationId"`
	Tags        []string                     `json:"tags,omitempty"`
	Parameters  []openAPIParam               `json:"parameters,omitempty"`
	RequestBody *openAPIRequestBody          `json:"requestBody,omitempty"`
	Responses   map[string]openAPIResponse   `json:"responses"`
}

type openAPIParam struct {
	Name     string        `json:"name"`
	In       string        `json:"in"`
	Required bool          `json:"required"`
	Schema   openAPISchema `json:"schema"`
}

type openAPIRequestBody struct {
	Required bool                          `json:"required"`
	Content  map[string]openAPIMediaType   `json:"content"`
}

type openAPIMediaType struct {
	Schema openAPISchema `json:"schema"`
}

type openAPIResponse struct {
	Description string                        `json:"description"`
	Content     map[string]openAPIMediaType   `json:"content,omitempty"`
}

type openAPIComponents struct {
	Schemas map[string]openAPISchema `json:"schemas"`
}

type openAPISchema struct {
	Type       string                    `json:"type,omitempty"`
	Format     string                    `json:"format,omitempty"`
	Properties map[string]openAPISchema  `json:"properties,omitempty"`
	Items      *openAPISchema            `json:"items,omitempty"`
	Ref        string                    `json:"$ref,omitempty"`
	Enum       []string                  `json:"enum,omitempty"`
}
