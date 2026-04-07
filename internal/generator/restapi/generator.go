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

		handlerFile, err := generateHandler(ctx.OutputNamespace, entity, eb)
		if err != nil {
			return nil, nil, fmt.Errorf("generating handler for %s: %w", eb.Entity, err)
		}
		files = append(files, handlerFile)

		lower := strings.ToLower(entity.Name)
		wiring.Constructors = append(wiring.Constructors,
			fmt.Sprintf("api.New%sHandler(store)", entity.Name))

		basePath := entityBasePath(eb, exposeMap)
		for _, op := range eb.Operations {
			switch op {
			case types.OpCreate:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"POST %s\", %sHandler.create)", basePath, lower))
			case types.OpRead:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"GET %s/{id}\", %sHandler.read)", basePath, lower))
			case types.OpUpdate:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"PUT %s/{id}\", %sHandler.update)", basePath, lower))
			case types.OpDelete:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"DELETE %s/{id}\", %sHandler.delete)", basePath, lower))
			case types.OpList:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"GET %s\", %sHandler.list)", basePath, lower))
			case types.OpUpsert:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"PUT %s\", %sHandler.upsert)", basePath, lower))
			}
		}
	}

	// Generate router file.
	routerFile, err := generateRouter(ctx.OutputNamespace, ctx.Entities, ctx.Expose, exposeMap)
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
// Each operation is a separate method with http.HandlerFunc signature, registered
// individually in the router via Go 1.22 method+pattern routes.
func generateHandler(ns string, entity types.Entity, eb types.ExposeBlock) (gen.File, error) {
	var buf bytes.Buffer

	lower := strings.ToLower(entity.Name)
	handlerType := entity.Name + "Handler"

	// Determine whether encoding/json is needed. It is used by all operations
	// except delete (which only sends status codes, no JSON body).
	needJSON := false
	for _, op := range eb.Operations {
		if op != types.OpDelete {
			needJSON = true
			break
		}
	}

	// Determine whether time package is needed (computed timestamp fields
	// require time.Time{} zero value in write operations).
	needTime := false
	hasWriteOp := false
	for _, op := range eb.Operations {
		if op == types.OpCreate || op == types.OpUpdate || op == types.OpUpsert {
			hasWriteOp = true
			break
		}
	}
	if hasWriteOp {
		for _, f := range entity.Fields {
			if f.Computed && f.Type == types.FieldTypeTimestamp {
				needTime = true
				break
			}
		}
	}

	fmt.Fprintf(&buf, "package api\n\n")
	fmt.Fprintf(&buf, "import (\n")
	if needJSON {
		fmt.Fprintf(&buf, "\t\"encoding/json\"\n")
	}
	fmt.Fprintf(&buf, "\t\"net/http\"\n")
	if needTime {
		fmt.Fprintf(&buf, "\t\"time\"\n")
	}
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

	// Parent verification helper for nested routing.
	if eb.Parent != "" {
		parentIDVar := strings.ToLower(eb.Parent) + "ID"
		parentIDParam := strings.ToLower(eb.Parent) + "_id"
		fmt.Fprintf(&buf, "// checkParent verifies the parent %s exists.\n", eb.Parent)
		fmt.Fprintf(&buf, "func (h *%s) checkParent(w http.ResponseWriter, r *http.Request) bool {\n", handlerType)
		fmt.Fprintf(&buf, "\t%s := r.PathValue(%q)\n", parentIDVar, parentIDParam)
		fmt.Fprintf(&buf, "\tif %s == \"\" {\n", parentIDVar)
		fmt.Fprintf(&buf, "\t\thttp.Error(w, \"missing %s\", http.StatusBadRequest)\n", parentIDParam)
		fmt.Fprintf(&buf, "\t\treturn false\n")
		fmt.Fprintf(&buf, "\t}\n")
		fmt.Fprintf(&buf, "\tif !h.store.Exists(%q, %s) {\n", eb.Parent, parentIDVar)
		fmt.Fprintf(&buf, "\t\thttp.Error(w, %q, http.StatusNotFound)\n", eb.Parent+" not found")
		fmt.Fprintf(&buf, "\t\treturn false\n")
		fmt.Fprintf(&buf, "\t}\n")
		fmt.Fprintf(&buf, "\treturn true\n")
		fmt.Fprintf(&buf, "}\n\n")
	}

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

func emitParentCheck(buf *bytes.Buffer, eb types.ExposeBlock) {
	if eb.Parent != "" {
		fmt.Fprintf(buf, "\tif !h.checkParent(w, r) {\n")
		fmt.Fprintf(buf, "\t\treturn\n")
		fmt.Fprintf(buf, "\t}\n")
	}
}

func generateCreateMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock) {
	lower := strings.ToLower(entity.Name)
	fmt.Fprintf(buf, "func (h *%sHandler) create(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	emitClearComputedFields(buf, lower, entity)
	if eb.Parent != "" {
		parentIDParam := strings.ToLower(eb.Parent) + "_id"
		fmt.Fprintf(buf, "\t%s.%s = r.PathValue(%q)\n", lower, parentRefFieldName(entity, eb.Parent), parentIDParam)
	}
	fmt.Fprintf(buf, "\tif err := h.store.Create(%q, %s); err != nil {\n", entity.Name, lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tw.WriteHeader(http.StatusCreated)\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
}

func generateReadMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock) {
	lower := strings.ToLower(entity.Name)
	fmt.Fprintf(buf, "func (h *%sHandler) read(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tid := r.PathValue(\"id\")\n")
	fmt.Fprintf(buf, "\t%s, err := h.store.Read(%q, id)\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err != nil {\n")
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusNotFound)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
}

func generateUpdateMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock) {
	lower := strings.ToLower(entity.Name)
	fmt.Fprintf(buf, "func (h *%sHandler) update(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tid := r.PathValue(\"id\")\n")
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	emitClearComputedFields(buf, lower, entity)
	fmt.Fprintf(buf, "\tif err := h.store.Update(%q, id, %s); err != nil {\n", entity.Name, lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
}

func generateDeleteMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock) {
	fmt.Fprintf(buf, "func (h *%sHandler) delete(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tid := r.PathValue(\"id\")\n")
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
	emitParentCheck(buf, eb)

	// Scope filtering: when a parent is set the scope value comes from the
	// parent's path parameter (already present in the route pattern). Without
	// a parent, scope is passed as a query parameter.
	if eb.Scope != "" && eb.Parent != "" {
		parentIDParam := strings.ToLower(eb.Parent) + "_id"
		fmt.Fprintf(buf, "\tscopeValue := r.PathValue(%q)\n", parentIDParam)
		fmt.Fprintf(buf, "\t%s, err := h.store.List(%q, %q, scopeValue)\n", lower, entity.Name, eb.Scope)
	} else if eb.Scope != "" {
		fmt.Fprintf(buf, "\tscopeValue := r.URL.Query().Get(%q)\n", eb.Scope)
		fmt.Fprintf(buf, "\t%s, err := h.store.List(%q, %q, scopeValue)\n", lower, entity.Name, eb.Scope)
	} else if eb.Parent != "" {
		parentIDVar := strings.ToLower(eb.Parent) + "ID"
		parentIDParam := strings.ToLower(eb.Parent) + "_id"
		parentField := parentRefFieldName(entity, eb.Parent)
		fmt.Fprintf(buf, "\t%s := r.PathValue(%q)\n", parentIDVar, parentIDParam)
		fmt.Fprintf(buf, "\t%s, err := h.store.List(%q, %q, %s)\n", lower, entity.Name, parentField, parentIDVar)
	} else {
		fmt.Fprintf(buf, "\t%s, err := h.store.List(%q, \"\", \"\")\n", lower, entity.Name)
	}

	fmt.Fprintf(buf, "\tif err != nil {\n")
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
}

func generateUpsertMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock) {
	lower := strings.ToLower(entity.Name)
	fmt.Fprintf(buf, "func (h *%sHandler) upsert(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	emitClearComputedFields(buf, lower, entity)
	if eb.Parent != "" {
		parentIDParam := strings.ToLower(eb.Parent) + "_id"
		fmt.Fprintf(buf, "\t%s.%s = r.PathValue(%q)\n", lower, parentRefFieldName(entity, eb.Parent), parentIDParam)
	}

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
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tw.WriteHeader(http.StatusOK)\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
}

// generateRouter produces the router.go file with entity type definitions,
// the Storage interface, Go 1.22 method+pattern route registration, and
// helper functions.
func generateRouter(ns string, entities []types.Entity, expose []types.ExposeBlock, exposeMap map[string]types.ExposeBlock) (gen.File, error) {
	var buf bytes.Buffer

	// Build entity map and determine needed imports from entity field types.
	entityMap := make(map[string]types.Entity, len(entities))
	needTime := false
	needJSON := false
	for _, e := range entities {
		entityMap[e.Name] = e
	}
	for _, eb := range expose {
		if entity, ok := entityMap[eb.Entity]; ok {
			for _, f := range entity.Fields {
				if f.Type == types.FieldTypeTimestamp {
					needTime = true
				}
				if f.Type == types.FieldTypeJsonb {
					needJSON = true
				}
			}
		}
	}

	fmt.Fprintf(&buf, "package api\n\n")
	fmt.Fprintf(&buf, "import (\n")
	if needJSON {
		fmt.Fprintf(&buf, "\t\"encoding/json\"\n")
	}
	fmt.Fprintf(&buf, "\t\"net/http\"\n")
	if needTime {
		fmt.Fprintf(&buf, "\t\"time\"\n")
	}
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

	// Entity types with fields from the entity definitions.
	for _, eb := range expose {
		entity := entityMap[eb.Entity]
		fmt.Fprintf(&buf, "// %s represents the %s entity.\n", eb.Entity, eb.Entity)
		fmt.Fprintf(&buf, "type %s struct {\n", eb.Entity)
		for _, f := range entity.Fields {
			goName := toPascalCase(f.Name)
			goType := fieldTypeToGo(f.Type)
			fmt.Fprintf(&buf, "\t%s %s `json:%q`\n", goName, goType, f.Name)
		}
		fmt.Fprintf(&buf, "}\n\n")
	}

	// NewRouter function with Go 1.22 method+pattern routes.
	fmt.Fprintf(&buf, "// NewRouter creates an http.Handler with all routes registered.\n")
	fmt.Fprintf(&buf, "func NewRouter(auth func(http.Handler) http.Handler, store Storage) http.Handler {\n")
	fmt.Fprintf(&buf, "\tmux := http.NewServeMux()\n\n")

	for _, eb := range expose {
		basePath := entityBasePath(eb, exposeMap)
		lower := strings.ToLower(eb.Entity)
		fmt.Fprintf(&buf, "\t%sHandler := New%sHandler(store)\n", lower, eb.Entity)

		for _, op := range eb.Operations {
			switch op {
			case types.OpCreate:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"POST %s\", %sHandler.create)\n", basePath, lower)
			case types.OpRead:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"GET %s/{id}\", %sHandler.read)\n", basePath, lower)
			case types.OpUpdate:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"PUT %s/{id}\", %sHandler.update)\n", basePath, lower)
			case types.OpDelete:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"DELETE %s/{id}\", %sHandler.delete)\n", basePath, lower)
			case types.OpList:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"GET %s\", %sHandler.list)\n", basePath, lower)
			case types.OpUpsert:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"PUT %s\", %sHandler.upsert)\n", basePath, lower)
			}
		}
		fmt.Fprintf(&buf, "\n")
	}

	fmt.Fprintf(&buf, "\treturn auth(mux)\n")
	fmt.Fprintf(&buf, "}\n")

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

		// Extract parent path parameters from the URL template.
		parentParams := pathParamsToOpenAPI(extractPathParams(basePath))

		collectionOps := make(map[string]openAPIOperation)
		itemOps := make(map[string]openAPIOperation)

		for _, op := range eb.Operations {
			tag := eb.Entity
			ref := "#/components/schemas/" + eb.Entity

			switch op {
			case types.OpList:
				listOp := openAPIOperation{
					Summary:     "List " + eb.Entity + " entities",
					OperationID: "list" + eb.Entity,
					Tags:        []string{tag},
					Parameters:  append([]openAPIParam{}, parentParams...),
					Responses: map[string]openAPIResponse{
						"200": {Description: "Successful response", Content: jsonContent(openAPISchema{
							Type:  "array",
							Items: &openAPISchema{Ref: ref},
						})},
					},
				}
				if len(listOp.Parameters) == 0 {
					listOp.Parameters = nil
				}
				collectionOps["get"] = listOp
			case types.OpCreate:
				createOp := openAPIOperation{
					Summary:     "Create " + eb.Entity,
					OperationID: "create" + eb.Entity,
					Tags:        []string{tag},
					Parameters:  append([]openAPIParam{}, parentParams...),
					RequestBody: &openAPIRequestBody{
						Required: true,
						Content:  jsonContent(openAPISchema{Ref: ref}),
					},
					Responses: map[string]openAPIResponse{
						"201": {Description: "Created"},
					},
				}
				if len(createOp.Parameters) == 0 {
					createOp.Parameters = nil
				}
				collectionOps["post"] = createOp
			case types.OpRead:
				itemOps["get"] = openAPIOperation{
					Summary:     "Read " + eb.Entity,
					OperationID: "read" + eb.Entity,
					Tags:        []string{tag},
					Parameters: append(append([]openAPIParam{}, parentParams...), openAPIParam{
						Name:     "id",
						In:       "path",
						Required: true,
						Schema:   openAPISchema{Type: "string"},
					}),
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
					Parameters: append(append([]openAPIParam{}, parentParams...), openAPIParam{
						Name:     "id",
						In:       "path",
						Required: true,
						Schema:   openAPISchema{Type: "string"},
					}),
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
					Parameters: append(append([]openAPIParam{}, parentParams...), openAPIParam{
						Name:     "id",
						In:       "path",
						Required: true,
						Schema:   openAPISchema{Type: "string"},
					}),
					Responses: map[string]openAPIResponse{
						"204": {Description: "Deleted"},
					},
				}
			case types.OpUpsert:
				upsertOp := openAPIOperation{
					Summary:     "Upsert " + eb.Entity,
					OperationID: "upsert" + eb.Entity,
					Tags:        []string{tag},
					Parameters:  append([]openAPIParam{}, parentParams...),
					RequestBody: &openAPIRequestBody{
						Required: true,
						Content:  jsonContent(openAPISchema{Ref: ref}),
					},
					Responses: map[string]openAPIResponse{
						"200": {Description: "Upserted"},
					},
				}
				if len(upsertOp.Parameters) == 0 {
					upsertOp.Parameters = nil
				}
				collectionOps["put"] = upsertOp
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

// extractPathParams returns the list of {param} placeholders in a URL template,
// excluding {id} which is handled separately by item-level operations.
func extractPathParams(path string) []string {
	var params []string
	for _, segment := range strings.Split(path, "/") {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			name := segment[1 : len(segment)-1]
			if name != "id" {
				params = append(params, name)
			}
		}
	}
	return params
}

// pathParamsToOpenAPI converts path parameter names to OpenAPI parameter objects.
func pathParamsToOpenAPI(names []string) []openAPIParam {
	params := make([]openAPIParam, len(names))
	for i, name := range names {
		params[i] = openAPIParam{
			Name:     name,
			In:       "path",
			Required: true,
			Schema:   openAPISchema{Type: "string"},
		}
	}
	return params
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
	if f.Computed {
		s.ReadOnly = true
	}
	return s
}

// emitClearComputedFields emits code that zeroes computed fields using their
// zero values. This is simpler: we know the Go types from the field definitions.
func emitClearComputedFields(buf *bytes.Buffer, varName string, entity types.Entity) {
	for _, f := range entity.Fields {
		if f.Computed {
			goName := toPascalCase(f.Name)
			goType := fieldTypeToGo(f.Type)
			switch goType {
			case "string":
				fmt.Fprintf(buf, "\t%s.%s = \"\"\n", varName, goName)
			case "bool":
				fmt.Fprintf(buf, "\t%s.%s = false\n", varName, goName)
			case "int32", "int64", "float32", "float64":
				fmt.Fprintf(buf, "\t%s.%s = 0\n", varName, goName)
			case "time.Time":
				fmt.Fprintf(buf, "\t%s.%s = time.Time{}\n", varName, goName)
			default:
				// For reference types ([]byte, json.RawMessage), use nil.
				fmt.Fprintf(buf, "\t%s.%s = nil\n", varName, goName)
			}
		}
	}
}

// parentRefFieldName finds the Go field name for the ref field that points to
// the parent entity. Falls back to Parent+"ID" if no matching ref is found.
func parentRefFieldName(entity types.Entity, parent string) string {
	for _, f := range entity.Fields {
		if f.Type == types.FieldTypeRef && f.To == parent {
			return toPascalCase(f.Name)
		}
	}
	return parent + "ID"
}

func jsonContent(schema openAPISchema) map[string]openAPIMediaType {
	return map[string]openAPIMediaType{
		"application/json": {Schema: schema},
	}
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

// OpenAPI types for JSON marshaling.

type openAPISpec struct {
	OpenAPI    string                     `json:"openapi"`
	Info       openAPIInfo                `json:"info"`
	Paths      map[string]openAPIPathItem `json:"paths"`
	Components openAPIComponents          `json:"components"`
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
	Summary     string                     `json:"summary"`
	OperationID string                     `json:"operationId"`
	Tags        []string                   `json:"tags,omitempty"`
	Parameters  []openAPIParam             `json:"parameters,omitempty"`
	RequestBody *openAPIRequestBody        `json:"requestBody,omitempty"`
	Responses   map[string]openAPIResponse `json:"responses"`
}

type openAPIParam struct {
	Name     string        `json:"name"`
	In       string        `json:"in"`
	Required bool          `json:"required"`
	Schema   openAPISchema `json:"schema"`
}

type openAPIRequestBody struct {
	Required bool                       `json:"required"`
	Content  map[string]openAPIMediaType `json:"content"`
}

type openAPIMediaType struct {
	Schema openAPISchema `json:"schema"`
}

type openAPIResponse struct {
	Description string                     `json:"description"`
	Content     map[string]openAPIMediaType `json:"content,omitempty"`
}

type openAPIComponents struct {
	Schemas map[string]openAPISchema `json:"schemas"`
}

type openAPISchema struct {
	Type       string                   `json:"type,omitempty"`
	Format     string                   `json:"format,omitempty"`
	Properties map[string]openAPISchema `json:"properties,omitempty"`
	Items      *openAPISchema           `json:"items,omitempty"`
	Ref        string                   `json:"$ref,omitempty"`
	Enum       []string                 `json:"enum,omitempty"`
	ReadOnly   bool                     `json:"readOnly,omitempty"`
}
