// Package restapi implements the rest-api component Generator. It produces
// HTTP handler files, route registration, middleware wiring, and an OpenAPI
// spec from the service declaration's entities and collections.
package restapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"path"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// Generator produces the rest-api component's generated code.
type Generator struct{}

// Generate produces HTTP handler files (one per collection), a router file,
// and an OpenAPI spec. It returns wiring instructions for main.go assembly.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if len(ctx.Collections) == 0 {
		return nil, nil, nil
	}

	// Validate base_path if provided.
	if ctx.BasePath != "" && !strings.HasPrefix(ctx.BasePath, "/") {
		return nil, nil, fmt.Errorf("base_path must start with '/', got %q", ctx.BasePath)
	}

	// Validate collection names are unique. Collection names drive handler
	// type names, file names, and wiring variable names.
	if err := validateCollectionNameUniqueness(ctx.Collections); err != nil {
		return nil, nil, err
	}

	// Check for collection-derived identifier collisions with generator-internal
	// identifiers, entity struct names, and cross-collection derived names.
	if err := checkCollectionNameCollisions(ctx.Collections, ctx.Entities); err != nil {
		return nil, nil, err
	}

	// Check for collections whose derived PascalCase identifiers collide.
	// Two collections like "org-users" and "org_users" both produce
	// "OrgUsers", causing colliding handler types, file paths, and
	// variable declarations.
	if err := validateCollectionDerivedUniqueness(ctx.Collections); err != nil {
		return nil, nil, err
	}

	// Build entity lookup for field resolution.
	entityMap := make(map[string]types.Entity, len(ctx.Entities))
	for _, e := range ctx.Entities {
		entityMap[e.Name] = e
	}

	// Build parent lookup: entity name → its first collection (for nested routing).
	// When multiple collections reference the same entity, the first one
	// provides the path for parent resolution.
	collectionMap := make(map[string]types.Collection, len(ctx.Collections))
	for _, eb := range ctx.Collections {
		if _, exists := collectionMap[eb.Entity]; !exists {
			collectionMap[eb.Entity] = eb
		}
	}

	// Validate scope cardinality: multi-field scopes are not yet supported.
	// ScopeField() and ParentEntity() iterate the map and return the first
	// element, which is non-deterministic for maps with more than one entry.
	// This check must run before any validation or generation that calls
	// ScopeField() or ParentEntity().
	if err := validateScopeCardinality(ctx.Collections); err != nil {
		return nil, nil, err
	}

	// Validate that every collection has at least one operation. An empty
	// operations list produces unused imports and handler variables — Go
	// compile errors.
	if err := validateCollectionOperations(ctx.Collections); err != nil {
		return nil, nil, err
	}

	// Validate that no collection contains duplicate operations. Duplicate
	// operations produce duplicate method declarations (compile error),
	// duplicate route registrations (runtime panic), and duplicate OpenAPI
	// operation entries (silent overwrite).
	if err := validateOperationUniqueness(ctx.Collections); err != nil {
		return nil, nil, err
	}

	// Validate that all parent cross-references resolve within the collections list.
	if err := validateParentReferences(ctx.Collections, collectionMap); err != nil {
		return nil, nil, err
	}

	// Validate that every entity with a parent declaration has exactly one
	// ref field pointing to the parent. This is a structural invariant of the
	// parent declaration itself — it must hold regardless of which operations
	// are exposed. Lazy validation inside operation methods would miss
	// read-only or delete-only entities.
	if err := validateParentRefFields(ctx.Collections, entityMap); err != nil {
		return nil, nil, err
	}

	// Validate that scope and upsert_key field-name references resolve to
	// actual entity fields. The generator is the first consumer that knows
	// both the collection and the entity's field definitions.
	if err := validateFieldReferences(ctx.Collections, entityMap); err != nil {
		return nil, nil, err
	}

	// Validate that when scope and parent are both set, the scope field is
	// the entity's ref field pointing to the parent. Otherwise the generated
	// list handler extracts the parent ID from the URL and passes it as the
	// filter for a different field — semantically wrong.
	if err := validateScopeParentConsistency(ctx.Collections, entityMap); err != nil {
		return nil, nil, err
	}

	// Validate that no two collections produce the same route path. Collisions
	// cause runtime panics (Go 1.22 ServeMux) and OpenAPI path overwrites.
	if err := validateRouteCollisions(ctx.Collections, collectionMap); err != nil {
		return nil, nil, err
	}

	var files []gen.File
	wiring := &gen.Wiring{}

	// Compute the slots import path. Handlers that have slot bindings need
	// to import the slots package to reference slot interface types.
	// With go.mod at the project root, generated packages live under the
	// output directory, so the import path must include the OutDirName prefix.
	slotsImportPath := ""
	if ctx.SlotsPackage != "" && ctx.ModuleName != "" {
		if ctx.OutDirName != "" {
			slotsImportPath = ctx.ModuleName + "/" + ctx.OutDirName + "/" + ctx.SlotsPackage
		} else {
			slotsImportPath = ctx.ModuleName + "/" + ctx.SlotsPackage
		}
	}

	// Generate handler file per collection.
	for _, eb := range ctx.Collections {
		entity, ok := entityMap[eb.Entity]
		if !ok {
			return nil, nil, fmt.Errorf("collection %q references unknown entity %q", eb.Name, eb.Entity)
		}

		collPascal := collectionToPascalCase(eb.Name)
		collCamel := collectionToCamelCase(eb.Name)

		slotParams := collectCollectionSlotParams(eb.Name, ctx.SlotBindings)

		handlerFile, err := generateHandler(ctx.OutputNamespace, entity, eb, collectionMap, slotParams, slotsImportPath, ctx.AuthPackage)
		if err != nil {
			return nil, nil, fmt.Errorf("generating handler for collection %s: %w", eb.Name, err)
		}
		files = append(files, handlerFile)

		constructorIdx := len(wiring.Constructors)
		wiring.Constructors = append(wiring.Constructors,
			fmt.Sprintf("%s.New%sHandler(store)", path.Base(ctx.OutputNamespace), collPascal))
		if wiring.ConstructorCollections == nil {
			wiring.ConstructorCollections = make(map[int]string)
		}
		wiring.ConstructorCollections[constructorIdx] = eb.Name
		if wiring.ConstructorDeps == nil {
			wiring.ConstructorDeps = make(map[int][]string)
		}
		wiring.ConstructorDeps[constructorIdx] = []string{"store"}

		collPath, err := collectionBasePath(eb, collectionMap)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving path for collection %s: %w", eb.Name, err)
		}
		basePath := ctx.BasePath + collPath
		for _, op := range eb.Operations {
			switch op {
			case types.OpCreate:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"POST %s\", %sHandler.Create)", basePath, collCamel))
			case types.OpRead:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"GET %s/{id}\", %sHandler.Read)", basePath, collCamel))
			case types.OpUpdate:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"PUT %s/{id}\", %sHandler.Update)", basePath, collCamel))
			case types.OpDelete:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"DELETE %s/{id}\", %sHandler.Delete)", basePath, collCamel))
			case types.OpList:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"GET %s\", %sHandler.List)", basePath, collCamel))
			case types.OpUpsert:
				wiring.Routes = append(wiring.Routes,
					fmt.Sprintf("mux.HandleFunc(\"PUT %s\", %sHandler.Upsert)", basePath, collCamel))
			}
		}
	}

	// Generate router file.
	routerFile, err := generateRouter(ctx.OutputNamespace, ctx.Entities, ctx.Collections)
	if err != nil {
		return nil, nil, fmt.Errorf("generating router: %w", err)
	}
	files = append(files, routerFile)

	// Generate OpenAPI spec.
	openapiFile, err := generateOpenAPI(ctx.OutputNamespace, ctx.Entities, ctx.Collections, collectionMap, ctx.BasePath)
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

// collectionBasePath returns the URL path prefix for a collection.
// If PathPrefix is set, it is used directly. Otherwise, a default is derived
// from the entity name, prepended with the parent's path if nested.
// Returns an error if a circular parent reference is detected.
func collectionBasePath(eb types.Collection, collectionMap map[string]types.Collection) (string, error) {
	return collectionBasePathWithVisited(eb, collectionMap, map[string]bool{eb.Entity: true})
}

func collectionBasePathWithVisited(eb types.Collection, collectionMap map[string]types.Collection, visited map[string]bool) (string, error) {
	if eb.PathPrefix != "" {
		return eb.PathPrefix, nil
	}
	base := "/" + strings.ToLower(eb.Entity) + "s"
	if eb.ParentEntity() != "" {
		if visited[eb.ParentEntity()] {
			return "", fmt.Errorf("circular parent reference detected: %s is an ancestor of itself", eb.ParentEntity())
		}
		if parentEB, ok := collectionMap[eb.ParentEntity()]; ok {
			visited[eb.ParentEntity()] = true
			parentPath, err := collectionBasePathWithVisited(parentEB, collectionMap, visited)
			if err != nil {
				return "", err
			}
			parentParam := "{" + eb.ScopeField() + "}"
			return parentPath + "/" + parentParam + base, nil
		}
	}
	return base, nil
}

// generateHandler produces a single Go handler file for a collection.
// Each operation is a separate method with http.HandlerFunc signature, registered
// individually in the router via Go 1.22 method+pattern routes.
func generateHandler(ns string, entity types.Entity, eb types.Collection, collectionMap map[string]types.Collection, slotParams []collectionSlotParam, slotsImportPath string, authImportPath string) (gen.File, error) {
	var buf bytes.Buffer

	collPascal := collectionToPascalCase(eb.Name)
	collSnake := collectionToSnakeCase(eb.Name)
	handlerType := collPascal + "Handler"

	// Determine whether encoding/json is needed. It is used by all operations
	// except delete (which only sends status codes, no JSON body).
	needJSON := false
	needStrconv := false
	needReflect := false
	for _, op := range eb.Operations {
		if op != types.OpDelete {
			needJSON = true
		}
		if op == types.OpList {
			needStrconv = true
			needReflect = true
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

	// Derive the slots import alias from the package path.
	slotsAlias := ""
	if len(slotParams) > 0 && slotsImportPath != "" {
		slotsAlias = path.Base(slotsImportPath)
	}

	// Determine whether fmt package is needed for slot request field conversion.
	// Before-slots populate a CreateRequest.Fields map[string]string from entity
	// fields; non-string-typed fields require fmt.Sprintf for conversion.
	needFmt := false
	needAuth := false
	if len(slotParams) > 0 {
		hasBeforeSlots := false
		for _, op := range eb.Operations {
			before, _ := slotsForOp(op, slotParams)
			if len(before) > 0 {
				hasBeforeSlots = true
				break
			}
		}
		if hasBeforeSlots {
			needFmt = needsFmtForSlotFields(entity)
			// Check if any before-slot has a Caller field that needs identity
			// extraction from the request context via auth middleware.
			if authImportPath != "" {
				for _, sp := range slotParams {
					if sp.HasCaller {
						needAuth = true
						break
					}
				}
			}
		}
	}

	// Derive the auth import alias from the package path.
	authAlias := ""
	if needAuth {
		authAlias = path.Base(authImportPath)
	}

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))
	fmt.Fprintf(&buf, "import (\n")
	if needJSON {
		fmt.Fprintf(&buf, "\t\"encoding/json\"\n")
	}
	if needFmt {
		fmt.Fprintf(&buf, "\t\"fmt\"\n")
	}
	fmt.Fprintf(&buf, "\t\"net/http\"\n")
	if needReflect {
		fmt.Fprintf(&buf, "\t\"reflect\"\n")
	}
	if needStrconv {
		fmt.Fprintf(&buf, "\t\"strconv\"\n")
	}
	if needTime {
		fmt.Fprintf(&buf, "\t\"time\"\n")
	}
	if slotsAlias != "" || authAlias != "" {
		fmt.Fprintf(&buf, "\n")
		if authAlias != "" {
			fmt.Fprintf(&buf, "\t%s %q\n", authAlias, authImportPath)
		}
		if slotsAlias != "" {
			fmt.Fprintf(&buf, "\t%s %q\n", slotsAlias, slotsImportPath)
		}
	}
	fmt.Fprintf(&buf, ")\n\n")

	// Handler struct.
	fmt.Fprintf(&buf, "// %s handles HTTP requests for %s entities.\n", handlerType, entity.Name)
	fmt.Fprintf(&buf, "type %s struct {\n", handlerType)
	fmt.Fprintf(&buf, "\tstore Storage\n")
	for _, sp := range slotParams {
		fmt.Fprintf(&buf, "\t%s %s.%s\n", sp.FieldName, slotsAlias, sp.InterfaceType)
	}
	fmt.Fprintf(&buf, "}\n\n")

	// Constructor.
	fmt.Fprintf(&buf, "// New%sHandler creates a new %s.\n", collPascal, handlerType)
	if len(slotParams) == 0 {
		fmt.Fprintf(&buf, "func New%sHandler(store Storage) *%s {\n", collPascal, handlerType)
		fmt.Fprintf(&buf, "\treturn &%s{store: store}\n", handlerType)
	} else {
		fmt.Fprintf(&buf, "func New%sHandler(store Storage", collPascal)
		for _, sp := range slotParams {
			fmt.Fprintf(&buf, ", %s %s.%s", sp.FieldName, slotsAlias, sp.InterfaceType)
		}
		fmt.Fprintf(&buf, ") *%s {\n", handlerType)
		fmt.Fprintf(&buf, "\treturn &%s{\n", handlerType)
		fmt.Fprintf(&buf, "\t\tstore: store,\n")
		for _, sp := range slotParams {
			fmt.Fprintf(&buf, "\t\t%s: %s,\n", sp.FieldName, sp.FieldName)
		}
		fmt.Fprintf(&buf, "\t}\n")
	}
	fmt.Fprintf(&buf, "}\n\n")

	// Resolve ancestor parameter names from the actual route path. When
	// path_prefix is set, parameter names come from the prefix template;
	// otherwise they are derived from entity names via convention.
	var ancestorParams map[string]string
	var parentParamName string
	if eb.ParentEntity() != "" {
		var err error
		ancestorParams, err = resolveAncestorParams(eb, collectionMap)
		if err != nil {
			return gen.File{}, err
		}
		parentParamName = ancestorParams[eb.ParentEntity()]
	}

	// Ancestor verification helper for nested routing. Verifies the existence
	// of all ancestor entities in the URL hierarchy, not just the immediate parent.
	if eb.ParentEntity() != "" {
		ancestors, err := collectAncestors(eb, collectionMap)
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
			fmt.Fprintf(&buf, "\t%sExists, %sErr := h.store.Exists(r.Context(), %q, %s)\n", idVar, idVar, anc, idVar)
			fmt.Fprintf(&buf, "\tif %sErr != nil {\n", idVar)
			fmt.Fprintf(&buf, "\t\thttp.Error(w, \"internal error\", http.StatusInternalServerError)\n")
			fmt.Fprintf(&buf, "\t\treturn false\n")
			fmt.Fprintf(&buf, "\t}\n")
			fmt.Fprintf(&buf, "\tif !%sExists {\n", idVar)
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
			opErr = generateCreateMethod(&buf, entity, eb, parentParamName, slotParams, slotsAlias, authAlias)
		case types.OpRead:
			generateReadMethod(&buf, entity, eb, slotParams, slotsAlias)
		case types.OpUpdate:
			opErr = generateUpdateMethod(&buf, entity, eb, parentParamName, slotParams, slotsAlias, authAlias)
		case types.OpDelete:
			generateDeleteMethod(&buf, entity, eb, slotParams, slotsAlias)
		case types.OpList:
			opErr = generateListMethod(&buf, entity, eb, parentParamName, slotParams, slotsAlias)
		case types.OpUpsert:
			opErr = generateUpsertMethod(&buf, entity, eb, parentParamName, slotParams, slotsAlias, authAlias)
		}
		if opErr != nil {
			return gen.File{}, opErr
		}
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting %s handler: %w", eb.Name, err)
	}

	return gen.File{
		Path:    path.Join(ns, "handler_"+collSnake+".go"),
		Content: formatted,
	}, nil
}

func emitParentCheck(buf *bytes.Buffer, eb types.Collection) {
	if eb.ParentEntity() != "" {
		fmt.Fprintf(buf, "\tif !h.checkAncestors(w, r) {\n")
		fmt.Fprintf(buf, "\t\treturn\n")
		fmt.Fprintf(buf, "\t}\n")
	}
}

func generateCreateMethod(buf *bytes.Buffer, entity types.Entity, eb types.Collection, parentParamName string, slotParams []collectionSlotParam, slotsAlias string, authAlias string) error {
	collPascal := collectionToPascalCase(eb.Name)
	lower := safeVarName(strings.ToLower(entity.Name))
	fmt.Fprintf(buf, "func (h *%sHandler) Create(w http.ResponseWriter, r *http.Request) {\n", collPascal)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	emitClearComputedFields(buf, lower, entity)
	if eb.ParentEntity() != "" {
		refField, err := parentRefFieldName(entity, eb.ParentEntity())
		if err != nil {
			return err
		}
		fmt.Fprintf(buf, "\t%s.%s = r.PathValue(%q)\n", lower, refField, parentParamName)
	}
	// Before-slots: gate and validate fire before create.
	before, after := slotsForOp(types.OpCreate, slotParams)
	for _, sp := range before {
		emitBeforeSlot(buf, slotsAlias, authAlias, sp, lower, entity)
	}
	fmt.Fprintf(buf, "\tif err := h.store.Create(r.Context(), %q, %s); err != nil {\n", entity.Name, lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	// After-slots: on_entity_changed fires after create.
	for _, sp := range after {
		emitAfterSlot(buf, slotsAlias, sp, entity.Name, types.OpCreate)
	}
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tw.WriteHeader(http.StatusCreated)\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
	return nil
}

func generateReadMethod(buf *bytes.Buffer, entity types.Entity, eb types.Collection, slotParams []collectionSlotParam, slotsAlias string) {
	collPascal := collectionToPascalCase(eb.Name)
	lower := safeVarName(strings.ToLower(entity.Name))
	fmt.Fprintf(buf, "func (h *%sHandler) Read(w http.ResponseWriter, r *http.Request) {\n", collPascal)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tid := r.PathValue(\"id\")\n")
	fmt.Fprintf(buf, "\t%s, err := h.store.Get(r.Context(), %q, id)\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err != nil {\n")
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusNotFound)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
	// Read has no before or after slot lifecycle points.
	_, _ = slotParams, slotsAlias
}

func generateUpdateMethod(buf *bytes.Buffer, entity types.Entity, eb types.Collection, parentParamName string, slotParams []collectionSlotParam, slotsAlias string, authAlias string) error {
	collPascal := collectionToPascalCase(eb.Name)
	lower := safeVarName(strings.ToLower(entity.Name))
	fmt.Fprintf(buf, "func (h *%sHandler) Update(w http.ResponseWriter, r *http.Request) {\n", collPascal)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tid := r.PathValue(\"id\")\n")
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	emitClearComputedFields(buf, lower, entity)
	if eb.ParentEntity() != "" {
		refField, err := parentRefFieldName(entity, eb.ParentEntity())
		if err != nil {
			return err
		}
		fmt.Fprintf(buf, "\t%s.%s = r.PathValue(%q)\n", lower, refField, parentParamName)
	}
	before, after := slotsForOp(types.OpUpdate, slotParams)
	for _, sp := range before {
		emitBeforeSlot(buf, slotsAlias, authAlias, sp, lower, entity)
	}
	fmt.Fprintf(buf, "\tif err := h.store.Replace(r.Context(), %q, id, %s); err != nil {\n", entity.Name, lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	for _, sp := range after {
		emitAfterSlot(buf, slotsAlias, sp, entity.Name, types.OpUpdate)
	}
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
	return nil
}

func generateDeleteMethod(buf *bytes.Buffer, entity types.Entity, eb types.Collection, slotParams []collectionSlotParam, slotsAlias string) {
	collPascal := collectionToPascalCase(eb.Name)
	fmt.Fprintf(buf, "func (h *%sHandler) Delete(w http.ResponseWriter, r *http.Request) {\n", collPascal)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tid := r.PathValue(\"id\")\n")
	fmt.Fprintf(buf, "\tif err := h.store.Delete(r.Context(), %q, id); err != nil {\n", entity.Name)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	_, after := slotsForOp(types.OpDelete, slotParams)
	for _, sp := range after {
		emitAfterSlot(buf, slotsAlias, sp, entity.Name, types.OpDelete)
	}
	fmt.Fprintf(buf, "\tw.WriteHeader(http.StatusNoContent)\n")
	fmt.Fprintf(buf, "}\n\n")
}

func generateListMethod(buf *bytes.Buffer, entity types.Entity, eb types.Collection, parentParamName string, slotParams []collectionSlotParam, slotsAlias string) error {
	collPascal := collectionToPascalCase(eb.Name)
	lower := safeVarName(strings.ToLower(entity.Name)) + "s"
	fmt.Fprintf(buf, "func (h *%sHandler) List(w http.ResponseWriter, r *http.Request) {\n", collPascal)
	emitParentCheck(buf, eb)

	// Parse page/size pagination parameters (spec-defined, 1-indexed).
	// Defaults: page=1, size=100. Max size=65500 (PostgreSQL parameter limit).
	fmt.Fprintf(buf, "\tpageStr := r.URL.Query().Get(\"page\")\n")
	fmt.Fprintf(buf, "\tsizeStr := r.URL.Query().Get(\"size\")\n")
	fmt.Fprintf(buf, "\tpage, _ := strconv.Atoi(pageStr)\n")
	fmt.Fprintf(buf, "\tif page < 1 {\n")
	fmt.Fprintf(buf, "\t\tpage = 1\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tsize, _ := strconv.Atoi(sizeStr)\n")
	fmt.Fprintf(buf, "\tif size < 1 {\n")
	fmt.Fprintf(buf, "\t\tsize = 100\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tif size > 65500 {\n")
	fmt.Fprintf(buf, "\t\tsize = 65500\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\toffset := (page - 1) * size\n")
	fmt.Fprintf(buf, "\tlimit := size\n")

	// Scope filtering: when a parent is set the scope value comes from the
	// parent's path parameter (already present in the route pattern). Without
	// a parent, scope is passed as a query parameter.
	if len(eb.Scope) > 0 && eb.ParentEntity() != "" {
		fmt.Fprintf(buf, "\tscopeValue := r.PathValue(%q)\n", parentParamName)
		fmt.Fprintf(buf, "\t%s, total, err := h.store.List(r.Context(), %q, %q, scopeValue, offset, limit)\n", lower, entity.Name, eb.ScopeField())
	} else if len(eb.Scope) > 0 {
		fmt.Fprintf(buf, "\tscopeValue := r.URL.Query().Get(%q)\n", eb.ScopeField())
		fmt.Fprintf(buf, "\t%s, total, err := h.store.List(r.Context(), %q, %q, scopeValue, offset, limit)\n", lower, entity.Name, eb.ScopeField())
	} else if eb.ParentEntity() != "" {
		parentIDVar := strings.ToLower(eb.ParentEntity()) + "ID"
		parentField, err := parentRefRawFieldName(entity, eb.ParentEntity())
		if err != nil {
			return err
		}
		fmt.Fprintf(buf, "\t%s := r.PathValue(%q)\n", parentIDVar, parentParamName)
		fmt.Fprintf(buf, "\t%s, total, err := h.store.List(r.Context(), %q, %q, %s, offset, limit)\n", lower, entity.Name, parentField, parentIDVar)
	} else {
		fmt.Fprintf(buf, "\t%s, total, err := h.store.List(r.Context(), %q, \"\", \"\", offset, limit)\n", lower, entity.Name)
	}

	fmt.Fprintf(buf, "\tif err != nil {\n")
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tactualSize := reflect.ValueOf(%s).Len()\n", lower)
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tresult := map[string]any{\n")
	fmt.Fprintf(buf, "\t\t\"kind\":  %q,\n", entity.Name+"List")
	fmt.Fprintf(buf, "\t\t\"page\":  page,\n")
	fmt.Fprintf(buf, "\t\t\"size\":  actualSize,\n")
	fmt.Fprintf(buf, "\t\t\"total\": total,\n")
	fmt.Fprintf(buf, "\t\t\"items\": %s,\n", lower)
	fmt.Fprintf(buf, "\t}\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(result)\n")
	fmt.Fprintf(buf, "}\n\n")
	// List has no before or after slot lifecycle points.
	_, _ = slotParams, slotsAlias
	return nil
}

func generateUpsertMethod(buf *bytes.Buffer, entity types.Entity, eb types.Collection, parentParamName string, slotParams []collectionSlotParam, slotsAlias string, authAlias string) error {
	collPascal := collectionToPascalCase(eb.Name)
	lower := safeVarName(strings.ToLower(entity.Name))
	fmt.Fprintf(buf, "func (h *%sHandler) Upsert(w http.ResponseWriter, r *http.Request) {\n", collPascal)
	emitParentCheck(buf, eb)
	fmt.Fprintf(buf, "\tvar %s %s\n", lower, entity.Name)
	fmt.Fprintf(buf, "\tif err := json.NewDecoder(r.Body).Decode(&%s); err != nil {\n", lower)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusBadRequest)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	emitClearComputedFields(buf, lower, entity)
	if eb.ParentEntity() != "" {
		refField, err := parentRefFieldName(entity, eb.ParentEntity())
		if err != nil {
			return err
		}
		fmt.Fprintf(buf, "\t%s.%s = r.PathValue(%q)\n", lower, refField, parentParamName)
	}

	before, after := slotsForOp(types.OpUpsert, slotParams)
	for _, sp := range before {
		emitBeforeSlot(buf, slotsAlias, authAlias, sp, lower, entity)
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
	fmt.Fprintf(buf, "\tif err := h.store.Upsert(r.Context(), %q, %s, upsertKey, %q); err != nil {\n", entity.Name, lower, concurrency)
	fmt.Fprintf(buf, "\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\treturn\n")
	fmt.Fprintf(buf, "\t}\n")
	for _, sp := range after {
		emitAfterSlot(buf, slotsAlias, sp, entity.Name, types.OpUpsert)
	}
	fmt.Fprintf(buf, "\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	fmt.Fprintf(buf, "\tw.WriteHeader(http.StatusOK)\n")
	fmt.Fprintf(buf, "\tjson.NewEncoder(w).Encode(%s)\n", lower)
	fmt.Fprintf(buf, "}\n\n")
	return nil
}

// generateRouter produces the router.go file with entity type definitions,
// the Storage interface, Go 1.22 method+pattern route registration, and
// helper functions.
func generateRouter(ns string, entities []types.Entity, collections []types.Collection) (_ gen.File, retErr error) {
	var buf bytes.Buffer

	// Build entity map and determine needed imports from entity field types.
	entityMap := make(map[string]types.Entity, len(entities))
	needTime := false
	needJSON := false
	for _, e := range entities {
		entityMap[e.Name] = e
	}
	for _, eb := range collections {
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
	// Always need context for the Storage interface.
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"context\"\n")
	if needJSON {
		fmt.Fprintf(&buf, "\t\"encoding/json\"\n")
	}
	if needTime {
		fmt.Fprintf(&buf, "\t\"time\"\n")
	}
	fmt.Fprintf(&buf, ")\n\n")

	// Storage interface used by all handlers.
	fmt.Fprintf(&buf, "// Storage is the interface that handlers use to interact with the data store.\n")
	fmt.Fprintf(&buf, "type Storage interface {\n")
	fmt.Fprintf(&buf, "\tCreate(ctx context.Context, entity string, value any) error\n")
	fmt.Fprintf(&buf, "\tGet(ctx context.Context, entity string, id string) (any, error)\n")
	fmt.Fprintf(&buf, "\tReplace(ctx context.Context, entity string, id string, value any) error\n")
	fmt.Fprintf(&buf, "\tDelete(ctx context.Context, entity string, id string) error\n")
	fmt.Fprintf(&buf, "\tList(ctx context.Context, entity string, scopeField string, scopeValue string, offset int, limit int) (any, int64, error)\n")
	fmt.Fprintf(&buf, "\tUpsert(ctx context.Context, entity string, value any, upsertKey []string, concurrency string) error\n")
	fmt.Fprintf(&buf, "\tExists(ctx context.Context, entity string, id string) (bool, error)\n")
	fmt.Fprintf(&buf, "}\n\n")

	// Entity types with fields from the entity definitions.
	// Deduplicate across collections: multiple collections may reference
	// the same entity, but the struct is emitted only once.
	emittedEntities := make(map[string]bool)
	for _, eb := range collections {
		if emittedEntities[eb.Entity] {
			continue
		}
		emittedEntities[eb.Entity] = true
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
func generateOpenAPI(ns string, entities []types.Entity, collections []types.Collection, collectionMap map[string]types.Collection, basePath string) (gen.File, error) {
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

	// Generate schemas only for entities that have collections. Deduplicate
	// across collections: multiple collections may reference the same entity.
	schemaEmitted := make(map[string]bool)
	for _, eb := range collections {
		if schemaEmitted[eb.Entity] {
			continue
		}
		schemaEmitted[eb.Entity] = true
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

	// Generate paths from collections.
	for _, eb := range collections {
		collPath, err := collectionBasePath(eb, collectionMap)
		if err != nil {
			return gen.File{}, err
		}
		fullPath := basePath + collPath
		collectionPath := fullPath
		itemPath := fullPath + "/{id}"

		// Extract parent path parameters from the URL template.
		parentParams := pathParamsToOpenAPI(extractPathParams(collPath))

		collectionOps := make(map[string]openAPIOperation)
		itemOps := make(map[string]openAPIOperation)

		collPascal := collectionToPascalCase(eb.Name)
		for _, op := range eb.Operations {
			tag := eb.Name
			ref := "#/components/schemas/" + eb.Entity

			switch op {
			case types.OpList:
				listParams := append([]openAPIParam{}, parentParams...)
				// Pagination query parameters (spec-defined).
				listParams = append(listParams, openAPIParam{
					Name:     "page",
					In:       "query",
					Required: false,
					Schema:   openAPISchema{Type: "integer", Default: 1},
				})
				listParams = append(listParams, openAPIParam{
					Name:     "size",
					In:       "query",
					Required: false,
					Schema:   openAPISchema{Type: "integer", Default: 100},
				})
				// When scope is set without a parent, the scope value is passed
				// as a query parameter — declare it in the OpenAPI spec.
				if len(eb.Scope) > 0 && eb.ParentEntity() == "" {
					listParams = append(listParams, openAPIParam{
						Name:     eb.ScopeField(),
						In:       "query",
						Required: false,
						Schema:   openAPISchema{Type: "string"},
					})
				}
				listOp := openAPIOperation{
					Summary:     "List " + eb.Entity + " entities via " + eb.Name,
					OperationID: "list" + collPascal,
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
					Summary:     "Create " + eb.Entity + " via " + eb.Name,
					OperationID: "create" + collPascal,
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
					Summary:     "Read " + eb.Entity + " via " + eb.Name,
					OperationID: "read" + collPascal,
					Tags: []string{tag},
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
					Summary:     "Update " + eb.Entity + " via " + eb.Name,
					OperationID: "update" + collPascal,
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
					Summary:     "Delete " + eb.Entity + " via " + eb.Name,
					OperationID: "delete" + collPascal,
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
					Summary:     "Upsert " + eb.Entity + " via " + eb.Name,
					OperationID: "upsert" + collPascal,
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

// collectAncestors walks the parent chain from the given collection and returns
// all ancestor entity names in top-down order (grandparent before parent).
// Returns an error if a circular parent reference is detected.
func collectAncestors(eb types.Collection, collectionMap map[string]types.Collection) ([]string, error) {
	var ancestors []string
	visited := map[string]bool{eb.Entity: true}
	current := eb
	for current.ParentEntity() != "" {
		if visited[current.ParentEntity()] {
			return nil, fmt.Errorf("circular parent reference detected: %s is an ancestor of itself", current.ParentEntity())
		}
		visited[current.ParentEntity()] = true
		ancestors = append(ancestors, current.ParentEntity())
		parentEB, ok := collectionMap[current.ParentEntity()]
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
	"json":    true, // encoding/json
	"http":    true, // net/http
	"reflect": true, // reflect (conditional for List actual item count)
	"strconv": true, // strconv (conditional for List pagination)
	"time":    true, // time (conditional, but safer to always guard)
	"fmt":     true, // not currently imported, but guard for safety
	// Generator-emitted local variables in Get/Replace/Delete method bodies.
	"id":  true, // id := r.PathValue("id")
	"err": true, // %s, err := h.store.Get(...) in Get method
	// Generator-emitted local variables in List method body.
	"page":       true, // page, _ := strconv.Atoi(pageStr)
	"size":       true, // size, _ := strconv.Atoi(sizeStr)
	"pageStr":    true, // pageStr := r.URL.Query().Get("page")
	"sizeStr":    true, // sizeStr := r.URL.Query().Get("size")
	"offset":     true, // offset := (page - 1) * size
	"limit":      true, // limit := size
	"total":      true, // total from h.store.List(...)
	"actualSize": true, // actualSize := reflect.ValueOf(<items>).Len()
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
	"Storage": true, // type Storage interface { ... } in router.go
}

// checkCollectionNameCollisions verifies that no collection-derived handler type
// name collides with a generator-internal identifier, an entity struct name, or
// another collection's derived type names.
func checkCollectionNameCollisions(collections []types.Collection, entities []types.Entity) error {
	// Build set of entity struct names (emitted in the same package).
	entityNames := make(map[string]bool, len(entities))
	for _, e := range entities {
		entityNames[e.Name] = true
	}

	for _, c := range collections {
		cp := collectionToPascalCase(c.Name)
		handlerName := cp + "Handler"
		ctorName := "New" + cp + "Handler"

		// Check handler type against reserved names.
		if reservedTypeNames[handlerName] {
			return fmt.Errorf("collection %q produces handler type %q which collides with rest-api generator internal identifier; rename the collection", c.Name, handlerName)
		}

		// Check PascalCase collection name against reserved names.
		if reservedTypeNames[cp] {
			return fmt.Errorf("collection %q produces type name %q which collides with rest-api generator internal identifier; rename the collection", c.Name, cp)
		}

		// Check handler type against entity struct names.
		if entityNames[handlerName] {
			return fmt.Errorf("collection %q produces handler type %q which collides with entity struct name %q; rename the collection or entity", c.Name, handlerName, handlerName)
		}

		// Check constructor against entity struct names.
		if entityNames[ctorName] {
			return fmt.Errorf("collection %q produces constructor %q which collides with entity struct name %q; rename the collection or entity", c.Name, ctorName, ctorName)
		}
	}

	// Check cross-collection derived identifier collisions.
	derivedNames := make(map[string]string, len(collections)*2)
	for _, c := range collections {
		cp := collectionToPascalCase(c.Name)
		handlerName := cp + "Handler"
		ctorName := "New" + cp + "Handler"
		if source, ok := derivedNames[handlerName]; ok {
			return fmt.Errorf("collections %q and %q both produce handler type %q; rename one to avoid a redeclaration error", source, c.Name, handlerName)
		}
		derivedNames[handlerName] = c.Name
		derivedNames[ctorName] = c.Name
	}

	return nil
}

// validateScopeCardinality rejects collections with more than one scope entry.
// Multi-field scopes are not yet supported — ScopeField() and ParentEntity()
// iterate the Scope map and return the first element, which is non-deterministic
// for maps with more than one entry. This check mirrors the validation in
// validate.go and ensures the invariant is enforced on the Reconcile() path
// (stego plan / stego apply), not just the Validate() path (stego validate).
func validateScopeCardinality(collections []types.Collection) error {
	var errs []string
	for _, c := range collections {
		if len(c.Scope) > 1 {
			errs = append(errs, fmt.Sprintf(
				"collection %q has %d scope entries but multi-field scopes are not yet supported — scope must contain exactly one entry",
				c.Name, len(c.Scope)))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("scope cardinality errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// validateCollectionNameUniqueness checks that no collection name appears more
// than once in the collections list.
func validateCollectionNameUniqueness(collections []types.Collection) error {
	seen := make(map[string]int, len(collections))
	var dupes []string
	for _, c := range collections {
		seen[c.Name]++
		if seen[c.Name] == 2 {
			dupes = append(dupes, c.Name)
		}
	}
	if len(dupes) > 0 {
		return fmt.Errorf("duplicate collection names: %s", strings.Join(dupes, ", "))
	}
	return nil
}

// validateFieldReferences checks that scope and upsert_key field-name references
// in collections resolve to actual fields on the referenced entity.
func validateFieldReferences(collections []types.Collection, entityMap map[string]types.Entity) error {
	var errs []string
	for _, eb := range collections {
		entity, ok := entityMap[eb.Entity]
		if !ok {
			continue // handled by the unknown entity check later
		}

		fieldSet := make(map[string]bool, len(entity.Fields))
		for _, f := range entity.Fields {
			fieldSet[f.Name] = true
		}

		if len(eb.Scope) > 0 && !fieldSet[eb.ScopeField()] {
			errs = append(errs, fmt.Sprintf(
				"collection %q references scope field %q, but entity %q has no field with that name",
				eb.Name, eb.ScopeField(), eb.Entity))
		}

		for _, key := range eb.UpsertKey {
			if !fieldSet[key] {
				errs = append(errs, fmt.Sprintf(
					"collection %q references upsert_key field %q, but entity %q has no field with that name",
					eb.Name, key, eb.Entity))
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
func resolveAncestorParams(eb types.Collection, collectionMap map[string]types.Collection) (map[string]string, error) {
	ancestors, err := collectAncestors(eb, collectionMap)
	if err != nil {
		return nil, err
	}
	if len(ancestors) == 0 {
		return nil, nil
	}

	basePath, err := collectionBasePath(eb, collectionMap)
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

// validateParentReferences verifies that every collection's parent field
// references an entity that has at least one collection in the collectionMap.
// A parent outside the collections means the generator cannot produce a correct
// route — the parent's path segment and path parameter will be missing.
func validateParentReferences(collections []types.Collection, collectionMap map[string]types.Collection) error {
	var errs []string
	for _, eb := range collections {
		if eb.ParentEntity() != "" {
			if _, ok := collectionMap[eb.ParentEntity()]; !ok {
				errs = append(errs, fmt.Sprintf(
					"collection %q references parent entity %q, but no collection exposes %q",
					eb.Name, eb.ParentEntity(), eb.ParentEntity()))
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
	Required bool                        `json:"required"`
	Content  map[string]openAPIMediaType `json:"content"`
}

type openAPIMediaType struct {
	Schema openAPISchema `json:"schema"`
}

type openAPIResponse struct {
	Description string                      `json:"description"`
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

// validateCollectionOperations checks that every collection has at least one
// operation. An empty operations list produces an unused handler variable and
// an unused net/http import — both Go compile errors.
func validateCollectionOperations(collections []types.Collection) error {
	var empty []string
	for _, eb := range collections {
		if len(eb.Operations) == 0 {
			empty = append(empty, eb.Name)
		}
	}
	if len(empty) > 0 {
		return fmt.Errorf("collections with no operations: %s; each collection must have at least one operation",
			strings.Join(empty, ", "))
	}
	return nil
}

// validateRouteCollisions detects duplicate effective route paths between
// collections. Collisions cause Go 1.22 ServeMux runtime panics (duplicate
// pattern registrations) and OpenAPI path map overwrites.
func validateRouteCollisions(collections []types.Collection, collectionMap map[string]types.Collection) error {
	type pathOwner struct {
		collection string
		path       string
	}
	// Track collection and item paths separately.
	seen := make(map[string]pathOwner)
	var errs []string

	for _, eb := range collections {
		basePath, err := collectionBasePath(eb, collectionMap)
		if err != nil {
			return err
		}
		collectionPath := basePath
		itemPath := basePath + "/{id}"

		for _, p := range []string{collectionPath, itemPath} {
			// Normalize to lowercase for case-insensitive collision detection.
			key := strings.ToLower(p)
			if existing, ok := seen[key]; ok && existing.collection != eb.Name {
				errs = append(errs, fmt.Sprintf(
					"collections %q and %q both resolve to route path %q",
					existing.collection, eb.Name, p))
			} else if !ok {
				seen[key] = pathOwner{collection: eb.Name, path: p}
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("route path collisions:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// validateScopeParentConsistency checks that when both scope and parent are set
// on a collection, the scope field is the entity's ref field pointing to the
// parent. The scope+parent code path extracts the parent's ID from the URL path
// parameter and uses it as the filter for the scope field. If the scope field is
// a different field, the generated code passes the wrong value — semantically
// broken at runtime.
func validateScopeParentConsistency(collections []types.Collection, entityMap map[string]types.Entity) error {
	var errs []string
	for _, eb := range collections {
		if len(eb.Scope) == 0 || eb.ParentEntity() == "" {
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
			if f.Type == types.FieldTypeRef && f.To == eb.ParentEntity() {
				refFieldName = f.Name
				break
			}
		}
		if refFieldName == "" {
			// Already caught by validateParentRefFields.
			continue
		}
		if eb.ScopeField() != refFieldName {
			errs = append(errs, fmt.Sprintf(
				"collection %q sets scope: %q with parent: %q, but %q is not the ref field to %q (which is %q); when both scope and parent are set, scope must name the entity's ref field to the parent",
				eb.Name, eb.ScopeField(), eb.ParentEntity(), eb.ScopeField(), eb.ParentEntity(), refFieldName))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("scope/parent consistency errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// validateCollectionDerivedUniqueness checks that no two collections produce
// the same PascalCase identifier. Two collections like "org-users" and
// "org_users" both produce "OrgUsers", causing colliding handler types, file
// paths, and variable declarations.
func validateCollectionDerivedUniqueness(collections []types.Collection) error {
	seen := make(map[string]string, len(collections)) // PascalCase → original name
	var errs []string
	for _, c := range collections {
		pascal := collectionToPascalCase(c.Name)
		if existing, ok := seen[pascal]; ok {
			errs = append(errs, fmt.Sprintf(
				"collections %q and %q both produce identifier %q; rename one to avoid colliding handler types and file paths in generated code",
				existing, c.Name, pascal))
		} else {
			seen[pascal] = c.Name
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("collection identifier collisions:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// validateOperationUniqueness checks that no collection contains duplicate
// operations. Duplicate operations produce duplicate method declarations (Go
// compile error), duplicate route registrations (Go 1.22 ServeMux runtime
// panic), and duplicate OpenAPI operation entries (silent overwrite).
func validateOperationUniqueness(collections []types.Collection) error {
	var errs []string
	for _, eb := range collections {
		seen := make(map[types.Operation]bool, len(eb.Operations))
		var dupes []string
		for _, op := range eb.Operations {
			if seen[op] {
				// Only report each duplicate once.
				already := false
				for _, d := range dupes {
					if d == string(op) {
						already = true
						break
					}
				}
				if !already {
					dupes = append(dupes, string(op))
				}
			}
			seen[op] = true
		}
		if len(dupes) > 0 {
			errs = append(errs, fmt.Sprintf(
				"collection %q has duplicate operations: %s",
				eb.Name, strings.Join(dupes, ", ")))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("duplicate operations in collections:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// collectionSlotParam describes a single slot operator parameter for a collection handler.
type collectionSlotParam struct {
	FieldName     string // handler struct field name (e.g., "beforeCreateGate")
	InterfaceType string // slot interface type without package qualifier (e.g., "BeforeCreateSlot")
	RequestType   string // slot request type without package qualifier (e.g., "BeforeCreateRequest")
	SlotName      string // raw slot name (e.g., "before_create")
	OperatorKind  string // "Gate", "Chain", or "FanOut"
	HasCaller     bool   // true when the request type has a Caller *Identity field (e.g., BeforeCreateRequest)
	HasEntityStr  bool   // true when the request type has an entity string field (e.g., ValidateRequest)
}

// slotRequestMeta describes which fields a slot's request type contains,
// derived from the slot proto definition. Each before-slot proto defines a
// distinct request message with different fields; emitBeforeSlot uses this
// metadata to populate only the fields that exist on the concrete type.
type slotRequestMeta struct {
	HasCaller    bool // request has Caller *Identity (e.g., before_create)
	HasEntityStr bool // request has entity string (e.g., validate)
}

// knownSlotRequestMeta maps slot names to their request type field metadata,
// derived from the proto definitions in registry/components/rest-api/slots/.
var knownSlotRequestMeta = map[string]slotRequestMeta{
	"before_create":    {HasCaller: true},
	"validate":         {HasEntityStr: true},
	"on_entity_changed": {}, // after-slot: Entity + Action, handled by emitAfterSlot
}

// slotPascal converts a snake_case slot name to PascalCase. Must match the
// assembler's snakeToPascal and the slot generator's service naming convention.
// Unlike toPascalCase, it does NOT treat "id" specially — slot names follow
// proto service naming, not Go field naming.
func slotPascal(s string) string {
	parts := strings.Split(s, "_")
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}

// slotCamel converts a snake_case slot name to camelCase.
func slotCamel(s string) string {
	pascal := slotPascal(s)
	if len(pascal) == 0 {
		return ""
	}
	return strings.ToLower(pascal[:1]) + pascal[1:]
}

// collectCollectionSlotParams collects slot binding parameters for a specific collection.
// The iteration order (bindings in order, gate before chain before fan-out per
// binding) must match buildSlotVarsByCollection in the assembler so that constructor
// parameter positions align with injected arguments.
func collectCollectionSlotParams(collectionName string, bindings []types.SlotDeclaration) []collectionSlotParam {
	var params []collectionSlotParam
	for _, sb := range bindings {
		if sb.Collection != collectionName {
			continue
		}
		sp := slotPascal(sb.Slot)
		sc := slotCamel(sb.Slot)
		ifaceType := sp + "Slot"
		reqType := sp + "Request"
		meta := knownSlotRequestMeta[sb.Slot]

		if len(sb.Gate) > 0 {
			params = append(params, collectionSlotParam{
				FieldName:     sc + "Gate",
				InterfaceType: ifaceType,
				RequestType:   reqType,
				SlotName:      sb.Slot,
				OperatorKind:  "Gate",
				HasCaller:     meta.HasCaller,
				HasEntityStr:  meta.HasEntityStr,
			})
		}
		if len(sb.Chain) > 0 {
			params = append(params, collectionSlotParam{
				FieldName:     sc + "Chain",
				InterfaceType: ifaceType,
				RequestType:   reqType,
				SlotName:      sb.Slot,
				OperatorKind:  "Chain",
				HasCaller:     meta.HasCaller,
				HasEntityStr:  meta.HasEntityStr,
			})
		}
		if len(sb.FanOut) > 0 {
			params = append(params, collectionSlotParam{
				FieldName:     sc + "FanOut",
				InterfaceType: ifaceType,
				RequestType:   reqType,
				SlotName:      sb.Slot,
				OperatorKind:  "FanOut",
				HasCaller:     meta.HasCaller,
				HasEntityStr:  meta.HasEntityStr,
			})
		}
	}
	return params
}

// slotBeforeOps maps slot name to operations where the slot fires BEFORE the
// main handler logic (body decode, store call).
var slotBeforeOps = map[string]map[types.Operation]bool{
	"before_create": {types.OpCreate: true},
	"validate":      {types.OpCreate: true, types.OpUpdate: true, types.OpUpsert: true},
}

// slotAfterOps maps slot name to operations where the slot fires AFTER the
// main handler logic (store call) but before the HTTP response is written.
var slotAfterOps = map[string]map[types.Operation]bool{
	"on_entity_changed": {
		types.OpCreate: true,
		types.OpUpdate: true,
		types.OpDelete: true,
		types.OpUpsert: true,
	},
}

// slotsForOp returns the slot params that fire before and after a given operation.
func slotsForOp(op types.Operation, params []collectionSlotParam) (before, after []collectionSlotParam) {
	for _, p := range params {
		if ops, ok := slotBeforeOps[p.SlotName]; ok && ops[op] {
			before = append(before, p)
		}
		if ops, ok := slotAfterOps[p.SlotName]; ok && ops[op] {
			after = append(after, p)
		}
	}
	return
}

// emitBeforeSlot emits code that calls a slot operator before the main operation.
// On error or non-Ok result, the handler returns an error response. The request
// is populated with the decoded entity variable so that fills can inspect the
// entity being processed. A nil guard wraps the invocation so that when no fills
// are wired, the handler degrades to passthrough semantics.
func emitBeforeSlot(buf *bytes.Buffer, slotsAlias string, authAlias string, param collectionSlotParam, entityVarName string, entity types.Entity) {
	fmt.Fprintf(buf, "\tif h.%s != nil {\n", param.FieldName)
	// Build populated request with entity data for the fill.
	// Fields are conditional on the slot's proto-defined request type
	// (checklist item 46: polymorphic struct literal emission).
	fmt.Fprintf(buf, "\t\tslotReq := &%s.%s{\n", slotsAlias, param.RequestType)
	fmt.Fprintf(buf, "\t\t\tInput: &%s.CreateRequest{\n", slotsAlias)
	fmt.Fprintf(buf, "\t\t\t\tEntity: %q,\n", entity.Name)
	fmt.Fprintf(buf, "\t\t\t\tFields: map[string]string{\n")
	for _, f := range entity.Fields {
		goName := toPascalCase(f.Name)
		fmt.Fprintf(buf, "\t\t\t\t\t%q: %s,\n", f.Name, fieldToStringExpr(entityVarName, goName, fieldTypeToGo(f.Type)))
	}
	fmt.Fprintf(buf, "\t\t\t\t},\n")
	fmt.Fprintf(buf, "\t\t\t},\n")
	if param.HasCaller {
		if authAlias != "" {
			// Extract authenticated caller identity from request context.
			// The auth middleware (e.g. jwt-auth) stores an Identity in the
			// context via context.WithValue; we retrieve it and map it to the
			// slots.Identity type for the fill.
			fmt.Fprintf(buf, "\t\t\tCaller: func() *%s.Identity {\n", slotsAlias)
			fmt.Fprintf(buf, "\t\t\t\tid := %s.IdentityFromContext(r.Context())\n", authAlias)
			fmt.Fprintf(buf, "\t\t\t\treturn &%s.Identity{UserID: id.UserID, Role: id.Role, Attributes: id.Attributes}\n", slotsAlias)
			fmt.Fprintf(buf, "\t\t\t}(),\n")
		} else {
			// No auth package available; provide a non-nil Identity to prevent
			// nil-dereference panics in fills.
			fmt.Fprintf(buf, "\t\t\tCaller: &%s.Identity{},\n", slotsAlias)
		}
	}
	if param.HasEntityStr {
		// Populate Entity string field for slots that need the entity name
		// (e.g., ValidateRequest has an entity string identifying which
		// entity is being validated).
		fmt.Fprintf(buf, "\t\t\tEntity: %q,\n", entity.Name)
	}
	fmt.Fprintf(buf, "\t\t}\n")
	fmt.Fprintf(buf, "\t\tslotResult, slotErr := h.%s.Evaluate(r.Context(), slotReq)\n", param.FieldName)
	fmt.Fprintf(buf, "\t\tif slotErr != nil {\n")
	fmt.Fprintf(buf, "\t\t\thttp.Error(w, slotErr.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\t\treturn\n")
	fmt.Fprintf(buf, "\t\t}\n")
	fmt.Fprintf(buf, "\t\tif !slotResult.Ok {\n")
	fmt.Fprintf(buf, "\t\t\tsc := http.StatusForbidden\n")
	fmt.Fprintf(buf, "\t\t\tif slotResult.StatusCode > 0 {\n")
	fmt.Fprintf(buf, "\t\t\t\tsc = int(slotResult.StatusCode)\n")
	fmt.Fprintf(buf, "\t\t\t}\n")
	fmt.Fprintf(buf, "\t\t\thttp.Error(w, slotResult.ErrorMessage, sc)\n")
	fmt.Fprintf(buf, "\t\t\treturn\n")
	fmt.Fprintf(buf, "\t\t}\n")
	// Short-circuit halt: chain step returned Ok but wants to stop further
	// processing (e.g. discard-stale-generation returning 204 no-op).
	fmt.Fprintf(buf, "\t\tif slotResult.Halt {\n")
	fmt.Fprintf(buf, "\t\t\tsc := http.StatusOK\n")
	fmt.Fprintf(buf, "\t\t\tif slotResult.StatusCode > 0 {\n")
	fmt.Fprintf(buf, "\t\t\t\tsc = int(slotResult.StatusCode)\n")
	fmt.Fprintf(buf, "\t\t\t}\n")
	fmt.Fprintf(buf, "\t\t\tw.WriteHeader(sc)\n")
	fmt.Fprintf(buf, "\t\t\treturn\n")
	fmt.Fprintf(buf, "\t\t}\n")
	fmt.Fprintf(buf, "\t}\n")
}

// emitAfterSlot emits code that calls a slot operator after the main operation,
// before the HTTP response is written. The request is populated with the entity
// name and the operation that triggered the slot. A nil guard wraps the invocation
// so that when no fills are wired, the handler degrades to passthrough semantics.
func emitAfterSlot(buf *bytes.Buffer, slotsAlias string, param collectionSlotParam, entityName string, op types.Operation) {
	fmt.Fprintf(buf, "\tif h.%s != nil {\n", param.FieldName)
	fmt.Fprintf(buf, "\t\tif _, slotErr := h.%s.Evaluate(r.Context(), &%s.%s{Entity: %q, Action: %q}); slotErr != nil {\n",
		param.FieldName, slotsAlias, param.RequestType, entityName, string(op))
	fmt.Fprintf(buf, "\t\t\thttp.Error(w, slotErr.Error(), http.StatusInternalServerError)\n")
	fmt.Fprintf(buf, "\t\t\treturn\n")
	fmt.Fprintf(buf, "\t\t}\n")
	fmt.Fprintf(buf, "\t}\n")
}

// fieldToStringExpr returns a Go expression that converts the given entity field
// to a string value for inclusion in a slot request's Fields map.
func fieldToStringExpr(varName, goFieldName, goType string) string {
	switch goType {
	case "string":
		return varName + "." + goFieldName
	case "[]byte", "json.RawMessage":
		return "string(" + varName + "." + goFieldName + ")"
	default:
		// For numeric, bool, time.Time, and other types, use fmt.Sprintf.
		return "fmt.Sprintf(\"%v\", " + varName + "." + goFieldName + ")"
	}
}

// collectionToPascalCase converts a kebab-case or snake_case collection name
// to PascalCase for Go type names. E.g., "org-users" → "OrgUsers".
func collectionToPascalCase(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_'
	})
	var b strings.Builder
	for _, p := range parts {
		if len(p) > 0 {
			b.WriteString(strings.ToUpper(p[:1]) + p[1:])
		}
	}
	return b.String()
}

// collectionToSnakeCase converts a kebab-case collection name to snake_case
// for file names. E.g., "org-users" → "org_users".
func collectionToSnakeCase(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}

// collectionToCamelCase converts a kebab-case collection name to camelCase
// for variable names. E.g., "org-users" → "orgUsers".
func collectionToCamelCase(name string) string {
	pascal := collectionToPascalCase(name)
	if len(pascal) == 0 {
		return ""
	}
	return strings.ToLower(pascal[:1]) + pascal[1:]
}

// needsFmtForSlotFields returns true if any entity field requires fmt.Sprintf
// for string conversion in slot request population.
func needsFmtForSlotFields(entity types.Entity) bool {
	for _, f := range entity.Fields {
		goType := fieldTypeToGo(f.Type)
		switch goType {
		case "string", "[]byte", "json.RawMessage":
			// These don't need fmt.
		default:
			return true
		}
	}
	return false
}

// validateParentRefFields checks that every entity with a parent declaration has
// exactly one ref field pointing to the parent entity. This is a structural
// invariant of the parent declaration — the declaration implies a data
// relationship that requires a ref field, regardless of which operations are
// exposed. Without this upfront check, read-only or delete-only entities with
// parent silently pass generation but produce semantically hollow nesting (ancestor
// existence is verified but parent-child ownership is never enforced).
func validateParentRefFields(collections []types.Collection, entityMap map[string]types.Entity) error {
	var errs []string
	for _, eb := range collections {
		if eb.ParentEntity() == "" {
			continue
		}
		entity, ok := entityMap[eb.Entity]
		if !ok {
			continue // handled by the unknown entity check later
		}
		var matches []string
		for _, f := range entity.Fields {
			if f.Type == types.FieldTypeRef && f.To == eb.ParentEntity() {
				matches = append(matches, f.Name)
			}
		}
		if len(matches) == 0 {
			errs = append(errs, fmt.Sprintf(
				"entity %q declares parent %q but has no field of type ref with to: %q",
				eb.Entity, eb.ParentEntity(), eb.ParentEntity()))
		} else if len(matches) > 1 {
			errs = append(errs, fmt.Sprintf(
				"entity %q has multiple ref fields pointing to parent %q: %s; the parent relationship is ambiguous — use a single ref field per parent entity",
				eb.Entity, eb.ParentEntity(), strings.Join(matches, ", ")))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("parent ref field errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}
