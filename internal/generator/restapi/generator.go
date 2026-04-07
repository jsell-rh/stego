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

	// Validate no duplicate expose blocks for the same entity. This must
	// happen before any map or iteration to avoid silent overwrites (map)
	// and duplicate type declarations (iteration).
	if err := validateExposeUniqueness(ctx.Expose); err != nil {
		return nil, nil, err
	}

	// Check for entity name collisions with generator-internal identifiers
	// and cross-entity derived identifier collisions.
	if err := checkEntityNameCollisions(ctx.Expose); err != nil {
		return nil, nil, err
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

	// Validate that every expose block has at least one operation. An empty
	// operations list produces unused imports and handler variables — Go
	// compile errors.
	if err := validateExposeOperations(ctx.Expose); err != nil {
		return nil, nil, err
	}

	// Validate that all parent cross-references resolve within the expose list.
	if err := validateParentReferences(ctx.Expose, exposeMap); err != nil {
		return nil, nil, err
	}

	// Validate that every entity with a parent declaration has exactly one
	// ref field pointing to the parent. This is a structural invariant of the
	// parent declaration itself — it must hold regardless of which operations
	// are exposed. Lazy validation inside operation methods would miss
	// read-only or delete-only entities.
	if err := validateParentRefFields(ctx.Expose, entityMap); err != nil {
		return nil, nil, err
	}

	// Validate that scope and upsert_key field-name references resolve to
	// actual entity fields. The generator is the first consumer that knows
	// both the expose block and the entity's field definitions.
	if err := validateFieldReferences(ctx.Expose, entityMap); err != nil {
		return nil, nil, err
	}

	// Validate that when scope and parent are both set, the scope field is
	// the entity's ref field pointing to the parent. Otherwise the generated
	// list handler extracts the parent ID from the URL and passes it as the
	// filter for a different field — semantically wrong.
	if err := validateScopeParentConsistency(ctx.Expose, entityMap); err != nil {
		return nil, nil, err
	}

	// Validate that no two entities produce the same route path. Collisions
	// cause runtime panics (Go 1.22 ServeMux), OpenAPI path overwrites, and
	// duplicate variable declarations.
	if err := validateRouteCollisions(ctx.Expose, exposeMap); err != nil {
		return nil, nil, err
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
			fmt.Sprintf("%s.New%sHandler(store)", path.Base(ctx.OutputNamespace), entity.Name))

		basePath, err := entityBasePath(eb, exposeMap)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving path for %s: %w", eb.Entity, err)
		}
		for _, op := range eb.Operations {
			switch op {
			case types.OpCreate:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"POST %s\", %sHandler.Create)", basePath, lower))
			case types.OpRead:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"GET %s/{id}\", %sHandler.Read)", basePath, lower))
			case types.OpUpdate:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"PUT %s/{id}\", %sHandler.Update)", basePath, lower))
			case types.OpDelete:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"DELETE %s/{id}\", %sHandler.Delete)", basePath, lower))
			case types.OpList:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"GET %s\", %sHandler.List)", basePath, lower))
			case types.OpUpsert:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"PUT %s\", %sHandler.Upsert)", basePath, lower))
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

	wiring.Imports = []string{ctx.OutputNamespace}

	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}

	return files, wiring, nil
}

// entityBasePath returns the URL path prefix for an entity's expose block.
// If PathPrefix is set, it is used directly. Otherwise, a default is derived
// from the entity name, prepended with the parent's path if nested.
// Returns an error if a circular parent reference is detected.
func entityBasePath(eb types.ExposeBlock, exposeMap map[string]types.ExposeBlock) (string, error) {
	return entityBasePathWithVisited(eb, exposeMap, map[string]bool{eb.Entity: true})
}

func entityBasePathWithVisited(eb types.ExposeBlock, exposeMap map[string]types.ExposeBlock, visited map[string]bool) (string, error) {
	if eb.PathPrefix != "" {
		return eb.PathPrefix, nil
	}
	base := "/" + strings.ToLower(eb.Entity) + "s"
	if eb.Parent != "" {
		if visited[eb.Parent] {
			return "", fmt.Errorf("circular parent reference detected: %s is an ancestor of itself", eb.Parent)
		}
		if parentEB, ok := exposeMap[eb.Parent]; ok {
			visited[eb.Parent] = true
			parentPath, err := entityBasePathWithVisited(parentEB, exposeMap, visited)
			if err != nil {
				return "", err
			}
			parentParam := "{" + strings.ToLower(eb.Parent) + "_id}"
			return parentPath + "/" + parentParam + base, nil
		}
	}
	return base, nil
}

// generateHandler produces a single Go handler file for an exposed entity.
// Each operation is a separate method with http.HandlerFunc signature, registered
// individually in the router via Go 1.22 method+pattern routes.
func generateHandler(ns string, entity types.Entity, eb types.ExposeBlock, exposeMap map[string]types.ExposeBlock) (gen.File, error) {
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

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))
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

	// Resolve ancestor parameter names from the actual route path. When
	// path_prefix is set, parameter names come from the prefix template;
	// otherwise they are derived from entity names via convention.
	var ancestorParams map[string]string
	var parentParamName string
	if eb.Parent != "" {
		var err error
		ancestorParams, err = resolveAncestorParams(eb, exposeMap)
		if err != nil {
			return gen.File{}, err
		}
		parentParamName = ancestorParams[eb.Parent]
	}

	// Ancestor verification helper for nested routing. Verifies the existence
	// of all ancestor entities in the URL hierarchy, not just the immediate parent.
	if eb.Parent != "" {
		ancestors, err := collectAncestors(eb, exposeMap)
		if err != nil {
			return gen.File{}, err
		}
		fmt.Fprintf(&buf, "// checkAncestors verifies that all ancestor entities in the URL hierarchy exist.\n")
		fmt.Fprintf(&buf, "func (h *%s) checkAncestors(w http.ResponseWriter, r *http.Request) bool {\n", handlerType)
		for _, anc := range ancestors {
			idParam := ancestorParams[anc]
			idVar := strings.ToLower(anc) + "ID"
			fmt.Fprintf(&buf, "\t%s := r.PathValue(%q)\n", idVar, idParam)
			fmt.Fprintf(&buf, "\tif %s == \"\" {\n", idVar)
			fmt.Fprintf(&buf, "\t\thttp.Error(w, \"missing %s\", http.StatusBadRequest)\n", idParam)
			fmt.Fprintf(&buf, "\t\treturn false\n")
			fmt.Fprintf(&buf, "\t}\n")
			fmt.Fprintf(&buf, "\tif !h.store.Exists(%q, %s) {\n", anc, idVar)
			fmt.Fprintf(&buf, "\t\thttp.Error(w, %q, http.StatusNotFound)\n", anc+" not found")
			fmt.Fprintf(&buf, "\t\treturn false\n")
			fmt.Fprintf(&buf, "\t}\n")
		}
		fmt.Fprintf(&buf, "\treturn true\n")
		fmt.Fprintf(&buf, "}\n\n")
	}

	// Generate operation methods.
	for _, op := range eb.Operations {
		var opErr error
		switch op {
		case types.OpCreate:
			opErr = generateCreateMethod(&buf, entity, eb, parentParamName)
		case types.OpRead:
			generateReadMethod(&buf, entity, eb)
		case types.OpUpdate:
			opErr = generateUpdateMethod(&buf, entity, eb, parentParamName)
		case types.OpDelete:
			generateDeleteMethod(&buf, entity, eb)
		case types.OpList:
			opErr = generateListMethod(&buf, entity, eb, parentParamName)
		case types.OpUpsert:
			opErr = generateUpsertMethod(&buf, entity, eb, parentParamName)
		}
		if opErr != nil {
			return gen.File{}, opErr
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
		fmt.Fprintf(buf, "\tif !h.checkAncestors(w, r) {\n")
		fmt.Fprintf(buf, "\t\treturn\n")
		fmt.Fprintf(buf, "\t}\n")
	}
}

func generateCreateMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock, parentParamName string) error {
	lower := safeVarName(strings.ToLower(entity.Name))
	fmt.Fprintf(buf, "func (h *%sHandler) Create(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	emitClearComputedFields(buf, lower, entity)
	if eb.Parent != "" {
		refField, err := parentRefFieldName(entity, eb.Parent)
		if err != nil {
			return err
		}
		fmt.Fprintf(buf, "\t%s.%s = r.PathValue(%q)\n", lower, refField, parentParamName)
	}
	fmt.Fprintf(buf, "\tif err := h.store.Create(%q, %s); err != nil {\n", entity.Name, lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tw.WriteHeader(http.StatusCreated)\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
	return nil
}

func generateReadMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock) {
	lower := safeVarName(strings.ToLower(entity.Name))
	fmt.Fprintf(buf, "func (h *%sHandler) Read(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
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

func generateUpdateMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock, parentParamName string) error {
	lower := safeVarName(strings.ToLower(entity.Name))
	fmt.Fprintf(buf, "func (h *%sHandler) Update(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tid := r.PathValue(\"id\")\n")
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	emitClearComputedFields(buf, lower, entity)
	if eb.Parent != "" {
		refField, err := parentRefFieldName(entity, eb.Parent)
		if err != nil {
			return err
		}
		fmt.Fprintf(buf, "\t%s.%s = r.PathValue(%q)\n", lower, refField, parentParamName)
	}
	fmt.Fprintf(buf, "\tif err := h.store.Update(%q, id, %s); err != nil {\n", entity.Name, lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
	return nil
}

func generateDeleteMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock) {
	fmt.Fprintf(buf, "func (h *%sHandler) Delete(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tid := r.PathValue(\"id\")\n")
	fmt.Fprintf(buf, "\tif err := h.store.Delete(%q, id); err != nil {\n", entity.Name)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tw.WriteHeader(http.StatusNoContent)\n")
	fmt.Fprintf(buf, "}\n\n")
}

func generateListMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock, parentParamName string) error {
	lower := safeVarName(strings.ToLower(entity.Name)) + "s"
	fmt.Fprintf(buf, "func (h *%sHandler) List(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	emitParentCheck(buf, eb)

	// Scope filtering: when a parent is set the scope value comes from the
	// parent's path parameter (already present in the route pattern). Without
	// a parent, scope is passed as a query parameter.
	if eb.Scope != "" && eb.Parent != "" {
		fmt.Fprintf(buf, "\tscopeValue := r.PathValue(%q)\n", parentParamName)
		fmt.Fprintf(buf, "\t%s, err := h.store.List(%q, %q, scopeValue)\n", lower, entity.Name, eb.Scope)
	} else if eb.Scope != "" {
		fmt.Fprintf(buf, "\tscopeValue := r.URL.Query().Get(%q)\n", eb.Scope)
		fmt.Fprintf(buf, "\t%s, err := h.store.List(%q, %q, scopeValue)\n", lower, entity.Name, eb.Scope)
	} else if eb.Parent != "" {
		parentIDVar := strings.ToLower(eb.Parent) + "ID"
		parentField, err := parentRefRawFieldName(entity, eb.Parent)
		if err != nil {
			return err
		}
		fmt.Fprintf(buf, "\t%s := r.PathValue(%q)\n", parentIDVar, parentParamName)
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
	return nil
}

func generateUpsertMethod(buf *bytes.Buffer, entity types.Entity, eb types.ExposeBlock, parentParamName string) error {
	lower := safeVarName(strings.ToLower(entity.Name))
	fmt.Fprintf(buf, "func (h *%sHandler) Upsert(w http.ResponseWriter, r *http.Request) {\n", entity.Name)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	emitClearComputedFields(buf, lower, entity)
	if eb.Parent != "" {
		refField, err := parentRefFieldName(entity, eb.Parent)
		if err != nil {
			return err
		}
		fmt.Fprintf(buf, "\t%s.%s = r.PathValue(%q)\n", lower, refField, parentParamName)
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
	return nil
}

// generateRouter produces the router.go file with entity type definitions,
// the Storage interface, Go 1.22 method+pattern route registration, and
// helper functions.
func generateRouter(ns string, entities []types.Entity, expose []types.ExposeBlock, exposeMap map[string]types.ExposeBlock) (_ gen.File, retErr error) {
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

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))
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
		basePath, err := entityBasePath(eb, exposeMap)
		if err != nil {
			return gen.File{}, err
		}
		lower := strings.ToLower(eb.Entity)
		fmt.Fprintf(&buf, "\t%sHandler := New%sHandler(store)\n", lower, eb.Entity)

		for _, op := range eb.Operations {
			switch op {
			case types.OpCreate:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"POST %s\", %sHandler.Create)\n", basePath, lower)
			case types.OpRead:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"GET %s/{id}\", %sHandler.Read)\n", basePath, lower)
			case types.OpUpdate:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"PUT %s/{id}\", %sHandler.Update)\n", basePath, lower)
			case types.OpDelete:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"DELETE %s/{id}\", %sHandler.Delete)\n", basePath, lower)
			case types.OpList:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"GET %s\", %sHandler.List)\n", basePath, lower)
			case types.OpUpsert:
				fmt.Fprintf(&buf, "\tmux.HandleFunc(\"PUT %s\", %sHandler.Upsert)\n", basePath, lower)
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

	// Generate schemas for exposed entities only. Non-exposed entities
	// (e.g. ref targets managed by other components) should not appear in
	// the OpenAPI spec — they have no corresponding path operations.
	for _, eb := range expose {
		e := entityMap[eb.Entity]
		schema := openAPISchema{
			Type:       "object",
			Properties: make(map[string]openAPISchema),
		}
		var required []string
		for _, f := range e.Fields {
			schema.Properties[f.Name] = fieldToOpenAPISchema(f)
			if !f.Optional && !f.Computed {
				required = append(required, f.Name)
			}
		}
		if len(required) > 0 {
			schema.Required = required
		}
		spec.Components.Schemas[e.Name] = schema
	}

	// Generate paths from expose blocks.
	for _, eb := range expose {
		basePath, err := entityBasePath(eb, exposeMap)
		if err != nil {
			return gen.File{}, err
		}
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
				listParams := append([]openAPIParam{}, parentParams...)
				// When scope is set without a parent, the scope value is passed
				// as a query parameter — declare it in the OpenAPI spec.
				if eb.Scope != "" && eb.Parent == "" {
					listParams = append(listParams, openAPIParam{
						Name:     eb.Scope,
						In:       "query",
						Required: false,
						Schema:   openAPISchema{Type: "string"},
					})
				}
				listOp := openAPIOperation{
					Summary:     "List " + eb.Entity + " entities",
					OperationID: "list" + eb.Entity,
					Tags:        []string{tag},
					Parameters:  listParams,
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
						"201": {Description: "Created", Content: jsonContent(openAPISchema{Ref: ref})},
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
						"200": {Description: "Updated", Content: jsonContent(openAPISchema{Ref: ref})},
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
						"200": {Description: "Upserted", Content: jsonContent(openAPISchema{Ref: ref})},
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
	case types.FieldTypeInt32:
		s.Type = "integer"
		s.Format = "int32"
	case types.FieldTypeInt64:
		s.Type = "integer"
		s.Format = "int64"
	case types.FieldTypeFloat:
		s.Type = "number"
		s.Format = "float"
	case types.FieldTypeDouble:
		s.Type = "number"
		s.Format = "double"
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
	// Propagate string constraint attributes.
	if f.MinLength != nil {
		s.MinLength = f.MinLength
	}
	if f.MaxLength != nil {
		s.MaxLength = f.MaxLength
	}
	if f.Pattern != "" {
		s.Pattern = f.Pattern
	}
	// Propagate numeric constraint attributes.
	if f.Min != nil {
		s.Minimum = f.Min
	}
	if f.Max != nil {
		s.Maximum = f.Max
	}
	if f.Default != nil {
		s.Default = f.Default
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

// collectAncestors walks the parent chain from the given expose block and returns
// all ancestor entity names in top-down order (grandparent before parent).
// Returns an error if a circular parent reference is detected.
func collectAncestors(eb types.ExposeBlock, exposeMap map[string]types.ExposeBlock) ([]string, error) {
	var ancestors []string
	visited := map[string]bool{eb.Entity: true}
	current := eb
	for current.Parent != "" {
		if visited[current.Parent] {
			return nil, fmt.Errorf("circular parent reference detected: %s is an ancestor of itself", current.Parent)
		}
		visited[current.Parent] = true
		ancestors = append(ancestors, current.Parent)
		parentEB, ok := exposeMap[current.Parent]
		if !ok {
			break
		}
		current = parentEB
	}
	// Reverse to get top-down order (grandparent first, parent last).
	for i, j := 0, len(ancestors)-1; i < j; i, j = i+1, j-1 {
		ancestors[i], ancestors[j] = ancestors[j], ancestors[i]
	}
	return ancestors, nil
}

// parentRefFieldName finds the Go field name for the ref field that points to
// the parent entity. Returns an error if no matching ref field is found or if
// multiple ref fields point to the same parent (ambiguous match).
func parentRefFieldName(entity types.Entity, parent string) (string, error) {
	var matches []string
	for _, f := range entity.Fields {
		if f.Type == types.FieldTypeRef && f.To == parent {
			matches = append(matches, f.Name)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("entity %q declares parent %q but has no field of type ref with to: %q", entity.Name, parent, parent)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("entity %q has multiple ref fields pointing to parent %q: %s; the parent relationship is ambiguous — use a single ref field per parent entity", entity.Name, parent, strings.Join(matches, ", "))
	}
	return toPascalCase(matches[0]), nil
}

// parentRefRawFieldName finds the raw YAML field name for the ref field that
// points to the parent entity. Returns an error if no matching ref field is
// found or if multiple ref fields point to the same parent (ambiguous match).
func parentRefRawFieldName(entity types.Entity, parent string) (string, error) {
	var matches []string
	for _, f := range entity.Fields {
		if f.Type == types.FieldTypeRef && f.To == parent {
			matches = append(matches, f.Name)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("entity %q declares parent %q but has no field of type ref with to: %q", entity.Name, parent, parent)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("entity %q has multiple ref fields pointing to parent %q: %s; the parent relationship is ambiguous — use a single ref field per parent entity", entity.Name, parent, strings.Join(matches, ", "))
	}
	return matches[0], nil
}

// goReservedWords is the set of Go keywords and predeclared identifiers that
// cannot be used as variable names in generated code.
var goReservedWords = map[string]bool{
	// Keywords
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
	// Predeclared identifiers
	"bool": true, "byte": true, "error": true, "int": true, "string": true,
	"true": true, "false": true, "nil": true, "len": true, "cap": true,
	"make": true, "new": true, "append": true, "copy": true, "delete": true,
	"print": true, "println": true, "complex": true, "real": true, "imag": true,
	"close": true, "panic": true, "recover": true,
}

// handlerScopeIdentifiers is the set of identifiers that are in scope inside
// generated handler method bodies. These include the method receiver, parameter
// names, and import aliases. A generated local variable whose name matches any
// of these will redeclare or shadow the identifier, causing a compile error.
var handlerScopeIdentifiers = map[string]bool{
	// Method receiver.
	"h": true,
	// Handler method parameters.
	"w": true,
	"r": true,
	// Import aliases used in handler files.
	"json": true, // encoding/json
	"http": true, // net/http
	"time": true, // time (conditional, but safer to always guard)
	"fmt":  true, // not currently imported, but guard for safety
}

// safeVarName returns the given name with a trailing underscore appended if it
// collides with a Go reserved word, predeclared identifier, or function-scoped
// identifier in generated handler methods. This prevents generated code from
// using keywords or shadowing in-scope names as variable names.
func safeVarName(name string) string {
	if goReservedWords[name] || handlerScopeIdentifiers[name] {
		return name + "_"
	}
	return name
}

// reservedTypeNames is the set of identifiers the rest-api generator defines
// unconditionally in the generated package. An entity whose name matches any
// of these produces a redeclaration compile error. The check must happen at
// generation time with a clear diagnostic — downstream compilation would point
// at generated code, giving the user no indication that their entity name is
// the problem.
var reservedTypeNames = map[string]bool{
	"Storage":   true, // type Storage interface { ... } in router.go
	"NewRouter": true, // func NewRouter(...) in router.go
}

// checkEntityNameCollisions verifies that no exposed entity name collides with
// a generator-internal identifier or with another entity's derived type names.
// Returns an error identifying the collision.
func checkEntityNameCollisions(expose []types.ExposeBlock) error {
	// Check against static reserved names.
	for _, eb := range expose {
		if reservedTypeNames[eb.Entity] {
			return fmt.Errorf("entity name %q collides with rest-api generator internal identifier %q; rename the entity to avoid a redeclaration error in generated code", eb.Entity, eb.Entity)
		}
	}

	// Check cross-entity derived identifier collisions. Each entity produces
	// derived names: "<Entity>Handler" (type) and "New<Entity>Handler" (constructor).
	// If another entity's direct name matches a derived name, it's a collision.
	derivedNames := make(map[string]string, len(expose)*2) // derived name -> source entity
	directNames := make(map[string]bool, len(expose))
	for _, eb := range expose {
		directNames[eb.Entity] = true
	}
	for _, eb := range expose {
		handlerName := eb.Entity + "Handler"
		ctorName := "New" + eb.Entity + "Handler"
		derivedNames[handlerName] = eb.Entity
		derivedNames[ctorName] = eb.Entity
	}
	for _, eb := range expose {
		if source, ok := derivedNames[eb.Entity]; ok {
			return fmt.Errorf("entity name %q collides with derived handler type name from entity %q; rename one of the entities to avoid a redeclaration error in generated code", eb.Entity, source)
		}
	}

	return nil
}

// validateExposeUniqueness checks that no entity appears more than once in the
// expose list. Duplicate entries cause duplicate type declarations and silent
// map overwrites.
func validateExposeUniqueness(expose []types.ExposeBlock) error {
	seen := make(map[string]int, len(expose))
	var dupes []string
	for _, eb := range expose {
		seen[eb.Entity]++
		if seen[eb.Entity] == 2 {
			dupes = append(dupes, eb.Entity)
		}
	}
	if len(dupes) > 0 {
		return fmt.Errorf("duplicate expose blocks for entities: %s; each entity may only appear once in the expose list", strings.Join(dupes, ", "))
	}
	return nil
}

// validateFieldReferences checks that scope and upsert_key field-name references
// in expose blocks resolve to actual fields on the referenced entity.
func validateFieldReferences(expose []types.ExposeBlock, entityMap map[string]types.Entity) error {
	var errs []string
	for _, eb := range expose {
		entity, ok := entityMap[eb.Entity]
		if !ok {
			continue // handled by the unknown entity check later
		}

		fieldSet := make(map[string]bool, len(entity.Fields))
		for _, f := range entity.Fields {
			fieldSet[f.Name] = true
		}

		if eb.Scope != "" && !fieldSet[eb.Scope] {
			errs = append(errs, fmt.Sprintf(
				"expose block for %q references scope field %q, but entity %q has no field with that name",
				eb.Entity, eb.Scope, eb.Entity))
		}

		for _, key := range eb.UpsertKey {
			if !fieldSet[key] {
				errs = append(errs, fmt.Sprintf(
					"expose block for %q references upsert_key field %q, but entity %q has no field with that name",
					eb.Entity, key, eb.Entity))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("unresolved field references:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// resolveAncestorParams returns a map from ancestor entity name to the actual
// path parameter name used in the route. When path_prefix is set, parameter
// names are extracted from the prefix and matched positionally with ancestors.
// When not set, convention-derived names (lowercase_entity + "_id") are used.
func resolveAncestorParams(eb types.ExposeBlock, exposeMap map[string]types.ExposeBlock) (map[string]string, error) {
	ancestors, err := collectAncestors(eb, exposeMap)
	if err != nil {
		return nil, err
	}
	if len(ancestors) == 0 {
		return nil, nil
	}

	basePath, err := entityBasePath(eb, exposeMap)
	if err != nil {
		return nil, err
	}

	params := extractPathParams(basePath) // excludes {id}

	if len(params) != len(ancestors) {
		return nil, fmt.Errorf(
			"path %q contains %d path parameters %v but entity %q has %d ancestors %v; each ancestor must have a corresponding path parameter",
			basePath, len(params), params, eb.Entity, len(ancestors), ancestors)
	}

	result := make(map[string]string, len(ancestors))
	for i, anc := range ancestors {
		result[anc] = params[i]
	}
	return result, nil
}

// validateParentReferences verifies that every expose block's parent field
// references an entity that is also in the expose list. A parent outside the
// expose list means the generator cannot produce a correct route — the parent's
// path segment and path parameter will be missing, causing every request to fail.
func validateParentReferences(expose []types.ExposeBlock, exposeMap map[string]types.ExposeBlock) error {
	var errs []string
	for _, eb := range expose {
		if eb.Parent != "" {
			if _, ok := exposeMap[eb.Parent]; !ok {
				errs = append(errs, fmt.Sprintf(
					"expose block for %q references parent %q, but %q is not in the expose list",
					eb.Entity, eb.Parent, eb.Parent))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("unresolved parent references:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
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
	Required   []string                 `json:"required,omitempty"`
	Items      *openAPISchema           `json:"items,omitempty"`
	Ref        string                   `json:"$ref,omitempty"`
	Enum       []string                 `json:"enum,omitempty"`
	ReadOnly   bool                     `json:"readOnly,omitempty"`
	MinLength  *int                     `json:"minLength,omitempty"`
	MaxLength  *int                     `json:"maxLength,omitempty"`
	Pattern    string                   `json:"pattern,omitempty"`
	Minimum    *float64                 `json:"minimum,omitempty"`
	Maximum    *float64                 `json:"maximum,omitempty"`
	Default    any                      `json:"default,omitempty"`
}

// validateExposeOperations checks that every expose block has at least one
// operation. An empty operations list produces an unused handler variable and
// an unused net/http import — both Go compile errors.
func validateExposeOperations(expose []types.ExposeBlock) error {
	var empty []string
	for _, eb := range expose {
		if len(eb.Operations) == 0 {
			empty = append(empty, eb.Entity)
		}
	}
	if len(empty) > 0 {
		return fmt.Errorf("expose blocks with no operations: %s; each expose block must have at least one operation",
			strings.Join(empty, ", "))
	}
	return nil
}

// validateRouteCollisions detects duplicate effective route paths between
// entities. Collisions cause Go 1.22 ServeMux runtime panics (duplicate
// pattern registrations), OpenAPI path map overwrites, and duplicate handler
// variable declarations.
func validateRouteCollisions(expose []types.ExposeBlock, exposeMap map[string]types.ExposeBlock) error {
	type pathOwner struct {
		entity string
		path   string
	}
	// Track collection and item paths separately.
	seen := make(map[string]pathOwner)
	var errs []string

	for _, eb := range expose {
		basePath, err := entityBasePath(eb, exposeMap)
		if err != nil {
			return err
		}
		collectionPath := basePath
		itemPath := basePath + "/{id}"

		for _, p := range []string{collectionPath, itemPath} {
			// Normalize to lowercase for case-insensitive collision detection.
			key := strings.ToLower(p)
			if existing, ok := seen[key]; ok && existing.entity != eb.Entity {
				errs = append(errs, fmt.Sprintf(
					"entities %q and %q both resolve to route path %q",
					existing.entity, eb.Entity, p))
			} else {
				seen[key] = pathOwner{entity: eb.Entity, path: p}
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("route path collisions:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// validateScopeParentConsistency checks that when both scope and parent are set
// on an expose block, the scope field is the entity's ref field pointing to the
// parent. The scope+parent code path extracts the parent's ID from the URL path
// parameter and uses it as the filter for the scope field. If the scope field is
// a different field, the generated code passes the wrong value — semantically
// broken at runtime.
func validateScopeParentConsistency(expose []types.ExposeBlock, entityMap map[string]types.Entity) error {
	var errs []string
	for _, eb := range expose {
		if eb.Scope == "" || eb.Parent == "" {
			continue
		}
		entity, ok := entityMap[eb.Entity]
		if !ok {
			continue
		}
		// Find the ref field pointing to the parent. validateParentRefFields
		// has already guaranteed exactly one exists if parent is set.
		refFieldName := ""
		for _, f := range entity.Fields {
			if f.Type == types.FieldTypeRef && f.To == eb.Parent {
				refFieldName = f.Name
				break
			}
		}
		if refFieldName == "" {
			// Already caught by validateParentRefFields.
			continue
		}
		if eb.Scope != refFieldName {
			errs = append(errs, fmt.Sprintf(
				"expose block for %q sets scope: %q with parent: %q, but %q is not the ref field to %q (which is %q); when both scope and parent are set, scope must name the entity's ref field to the parent",
				eb.Entity, eb.Scope, eb.Parent, eb.Scope, eb.Parent, refFieldName))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("scope/parent consistency errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// validateParentRefFields checks that every entity with a parent declaration has
// exactly one ref field pointing to the parent entity. This is a structural
// invariant of the parent declaration — the declaration implies a data
// relationship that requires a ref field, regardless of which operations are
// exposed. Without this upfront check, read-only or delete-only entities with
// parent silently pass generation but produce semantically hollow nesting (ancestor
// existence is verified but parent-child ownership is never enforced).
func validateParentRefFields(expose []types.ExposeBlock, entityMap map[string]types.Entity) error {
	var errs []string
	for _, eb := range expose {
		if eb.Parent == "" {
			continue
		}
		entity, ok := entityMap[eb.Entity]
		if !ok {
			continue // handled by the unknown entity check later
		}
		var matches []string
		for _, f := range entity.Fields {
			if f.Type == types.FieldTypeRef && f.To == eb.Parent {
				matches = append(matches, f.Name)
			}
		}
		if len(matches) == 0 {
			errs = append(errs, fmt.Sprintf(
				"entity %q declares parent %q but has no field of type ref with to: %q",
				eb.Entity, eb.Parent, eb.Parent))
		} else if len(matches) > 1 {
			errs = append(errs, fmt.Sprintf(
				"entity %q has multiple ref fields pointing to parent %q: %s; the parent relationship is ambiguous — use a single ref field per parent entity",
				eb.Entity, eb.Parent, strings.Join(matches, ", ")))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("parent ref field errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}
