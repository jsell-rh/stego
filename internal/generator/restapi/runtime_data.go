package restapi

import (
	"bytes"
	"fmt"
	"go/format"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/command"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/query"
	"github.com/jsell-rh/stego/internal/types"
)

// The runtime data path, the one data path of the rest-api component (ST-SUP-RC20-OC39): each declared collection operation is
// one generated command on github.com/jsell-rh/stego/registry/runtime/command (create, update, patch, delete) or one query on
// github.com/jsell-rh/stego/registry/runtime/query (read, list), and each handler calls that command's or query's Run. The handlers open no
// database connection and hold no store; the bound before slots run in the command's fill, before its write, and
// the after slots run after the command committed, so tenancy, idempotency, audit and authorization are applied once,
// by the runtime (ST-SUP-RC20-OC37).

// commandComponent and queryComponent are the components whose generated commands and queries the handlers run.
const (
	commandComponent = "command"
	queryComponent   = "query"
)

// dataFolder is the folder, under the component's namespace, of the entity declarations of the command and the
// query runtime.
const dataFolder = "data"

// runtimeSlots are the slots that the handlers on the runtime data path call.
var runtimeSlots = map[string]bool{
	"before_create": true, "before_patch": true, "before_delete": true, "validate": true,
	"after_create": true, "after_patch": true, "after_delete": true, "on_entity_changed": true,
}

// eventTypePattern is an event type that is both an outbox message kind and a Kafka topic name that Kafka does not
// keep for itself (ST-SUP-RP01-LC30).
var eventTypePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// runtimeFieldTypes are the field types that a handler converts between the API type and the command runtime's row
// type.
var runtimeFieldTypes = map[types.FieldType]bool{
	types.FieldTypeString: true, types.FieldTypeEnum: true, types.FieldTypeRef: true, types.FieldTypeInt32: true,
	types.FieldTypeInt64: true, types.FieldTypeFloat: true, types.FieldTypeDouble: true, types.FieldTypeBool: true,
	types.FieldTypeTimestamp: true,
	// A jsonb field is json.RawMessage in the API type and []byte in the row type: the handler converts it.
	types.FieldTypeJsonb: true,
}

// runtimeCollection is one collection on the runtime data path.
type runtimeCollection struct {
	decl     types.Collection
	entity   types.Entity
	pascal   string // the Go name of the collection, such as Orders
	camel    string // its unexported Go name, such as orders
	lower    string // the variable of one entity value, such as order
	hrefBase string
	slots    []collectionSlotParam
	ops      map[types.Operation]bool
	// scope is the field of the parent of a scoped collection, whose value is the path parameter of the same name,
	// and empty for a collection with no parent (ST-SUP-RC20-OC24).
	scope string
	// implicit are the implicit fields of the collection and their values, by field name (ST-SUP-RC20-OC23).
	implicit [][2]string
	// refs are the ref fields of the entity whose target has a resource type: a write that sets one loads the row it
	// names, on which the command runtime decides the reference (ST-SUP-RP10-LC05).
	refs []types.Field
}

// permission gives the permission that the operation decides, and empty for a public operation.
func (c *runtimeCollection) permission(op types.Operation) string {
	p := c.entity.Permissions[op]
	if p == types.PermissionPublic {
		return ""
	}
	return p
}

// hrefExpr gives the expression of the href of the API value v: the path of the collection with the scope field of v
// in place of its path parameter.
func (c *runtimeCollection) hrefExpr(v string) string {
	base := c.hrefBase
	if c.scope != "" {
		base = strings.Replace(base, "{"+c.scope+"}", "\"+"+v+"."+toPascalCase(c.scope)+"+\"", 1)
	}
	return fmt.Sprintf("\"%s\" + \"/\" + %s.ID", base, v)
}

// searchKind gives the kind of the values of a field that a list search may name, or empty for a field that a
// search does not take: a pii or secret field, whose match would tell its value (ST-SUP-RP11-LC20), a ref, and a
// field of a jsonb, bytes or timestamp type.
func searchKind(f types.Field) string {
	if f.Sensitivity != "" {
		return ""
	}
	switch f.Type {
	case types.FieldTypeString, types.FieldTypeEnum:
		return "KindText"
	case types.FieldTypeInt32:
		return "KindInt32"
	case types.FieldTypeInt64:
		return "KindInt64"
	case types.FieldTypeFloat, types.FieldTypeDouble:
		return "KindFloat"
	case types.FieldTypeBool:
		return "KindBool"
	}
	return ""
}

// patchFields gives the collection's patchable fields in the entity's order.
func (c *runtimeCollection) patchFields() []types.Field {
	set := map[string]bool{}
	for _, name := range c.decl.Patchable {
		set[name] = true
	}
	var fields []types.Field
	for _, f := range c.entity.Fields {
		if set[f.Name] {
			fields = append(fields, f)
		}
	}
	return fields
}

// validateRuntimeData refuses, with the reason, a service that the handlers on the runtime data path cannot serve.
// The command and the query runtime run in the transaction of one tenant and decide each permission for one
// principal, so the service needs pooled tenancy and authorization; the forms that the path does not build yet are
// refused, so no declared behaviour is lost without a word.
func validateRuntimeData(ctx gen.Context, entities map[string]types.Entity) error {
	// The runtime data path is the one data path (ST-SUP-RC20-OC39): the handlers run the generated commands and
	// queries, so a service with collections needs the command and the query components.
	if ctx.PeerNamespaces[commandComponent] == "" || ctx.PeerNamespaces[queryComponent] == "" {
		return fmt.Errorf("rest-api: the service declares collections, and rest-api serves them only on the runtime data path, which needs the command and the query components; add the components command and query to the service (ST-SUP-RC20-OC39)")
	}
	if ctx.Tenancy == nil || ctx.Tenancy.Key != "tenant_id" {
		return fmt.Errorf("rest-api: on the runtime data path each operation runs in the transaction of one tenant, so the service needs pooled tenancy with the tenant column tenant_id")
	}
	if ctx.Authorization == nil || ctx.Authorization.SubjectFromIdentity == "" {
		return fmt.Errorf("rest-api: on the runtime data path each operation decides its permission for the verified principal, so the service needs authorization with subject_from_identity")
	}
	for _, b := range ctx.SlotBindings {
		if b.Slot == "before_upsert" || b.Slot == "after_upsert" {
			return fmt.Errorf("rest-api: collection %s binds the slot %s, which only the removed store path called (ST-SUP-RC20-OC39); bind validate and on_entity_changed, which an upsert calls", b.Collection, b.Slot)
		}
		if !runtimeSlots[b.Slot] {
			return fmt.Errorf("rest-api: collection %s binds the slot %s, which the runtime data path does not call yet", b.Collection, b.Slot)
		}
	}
	if changeEvents(ctx) {
		if ctx.PeerNamespaces["outbox"] == "" {
			return fmt.Errorf("rest-api: the change events of the collections go to Kafka through the outbox, so the service needs the outbox component (ST-SUP-RC20-OC40)")
		}
		for _, c := range ctx.Collections {
			for _, action := range []string{eventCreated, eventUpdated, eventDeleted} {
				if name := eventType(ctx.ServiceName, c.Entity, action); !eventTypePattern.MatchString(name) {
					return fmt.Errorf("rest-api: the event type %q of the entity %s is not an outbox kind and a Kafka topic name: [a-zA-Z0-9._-], at most 128 characters, starting with a letter or a digit (ST-SUP-RC20-OC40)", name, c.Entity)
				}
			}
		}
	}
	for _, c := range ctx.Collections {
		e := entities[c.Entity]
		if e.ResourceType == "" {
			return fmt.Errorf("rest-api: collection %s reads the entity %s, which has no resource type; on the runtime data path every operation is decided", c.Name, e.Name)
		}
		if err := validateRuntimeScope(ctx, c, e, entities); err != nil {
			return err
		}
		for _, op := range c.Operations {
			switch op {
			case types.OpUpsert:
				if err := validateRuntimeUpsert(c, e, entities); err != nil {
					return err
				}
			case types.OpUpdate, types.OpPatch, types.OpDelete:
				// Every write of a row decides a permission on it before the fill; a write with none would run
				// undecided (ST-SUP-RC29-OC01).
				if p := e.Permissions[op]; p == "" || p == types.PermissionPublic {
					return fmt.Errorf("rest-api: collection %s declares %s with the permission %q; on the runtime data path a write of a row decides a permission of %s on it", c.Name, op, p, e.ResourceType)
				}
			case types.OpCreate:
				// A top-level create is decided on the service-level resource of the tenant provisioning
				// (ST-SUP-RC29-OC01, ST-SUP-RC27-OC09), whose resource type the declaration does not name until the
				// tenant provisioning is built (ST-DECL-UC10); until then it must be public.
				if e.Permissions[op] != types.PermissionPublic {
					return fmt.Errorf("rest-api: collection %s declares create with the permission %q; on the runtime data path a create of a top-level entity is decided on the service-level resource of the tenant provisioning (ST-SUP-RC29-OC01), which the declaration cannot name yet (ST-DECL-UC10), so it must be public with a reason", c.Name, e.Permissions[op])
				}
				// A command that declares no permission may not write a field-sourced relationship: the client would
				// choose the subject (ST-SUP-Q118).
				for _, r := range e.Relationships {
					if r.SubjectSource == types.SubjectSourceField {
						return fmt.Errorf("rest-api: collection %s declares a public create of %s, whose relationship %s takes its subject from the field %s; a command that declares no permission may not write a field-sourced relationship, since the client would choose the subject (ST-SUP-Q118): take the subject from the caller, or write the relationship in a declared command with a permission", c.Name, e.Name, r.Relation, r.SubjectField)
					}
				}
			}
		}
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeMoney {
				return fmt.Errorf("rest-api: entity %s has the field %s of type money, which the runtime data path does not serve yet: the storage form (a bigint of minor units and a char(3) currency) and the API form {amount, currency} of a money field of a data entity are decided (ST-DECL-Q13) but not built", e.Name, f.Name)
			}
			if !runtimeFieldTypes[f.Type] {
				return fmt.Errorf("rest-api: entity %s has the field %s of type %s, which the runtime data path does not serve yet", e.Name, f.Name, f.Type)
			}
		}
	}
	return nil
}

// validateRuntimeScope refuses the forms of a scoped collection, of implicit fields and of a path prefix that the
// handlers on the runtime data path do not serve: a scope of more than one field or more than one level, a scope
// field that is not a required ref to the parent or that a patch may change, an implicit field that is not a text
// or enum field, and a path parameter that is not the scope field (ST-SUP-RC20-OC23, ST-SUP-RC20-OC24).
func validateRuntimeScope(ctx gen.Context, c types.Collection, e types.Entity, entities map[string]types.Entity) error {
	field := func(name string) (types.Field, bool) {
		for _, f := range e.Fields {
			if f.Name == name {
				return f, true
			}
		}
		return types.Field{}, false
	}
	scope := ""
	if len(c.Scope) > 0 {
		if len(c.Scope) != 1 {
			return fmt.Errorf("rest-api: collection %s is scoped by more than one field, which the runtime data path does not serve", c.Name)
		}
		scope = c.ScopeField()
		f, ok := field(scope)
		if !ok || f.Type != types.FieldTypeRef || f.To != c.ParentEntity() || f.Optional {
			return fmt.Errorf("rest-api: collection %s is scoped by %s, which is not a required ref field of %s to %s", c.Name, scope, e.Name, c.ParentEntity())
		}
		if slices.Contains(c.Patchable, scope) {
			return fmt.Errorf("rest-api: collection %s lets a patch change its scope field %s, which moves a row to another parent", c.Name, scope)
		}
	}
	for name, value := range c.Implicit {
		if slices.Contains(c.Patchable, name) {
			return fmt.Errorf("rest-api: collection %s lets a patch change its implicit field %s, which moves a row to another collection", c.Name, name)
		}
		f, ok := field(name)
		if !ok || (f.Type != types.FieldTypeString && f.Type != types.FieldTypeEnum) || (f.Type == types.FieldTypeEnum && !slices.Contains(f.Values, value)) {
			return fmt.Errorf("rest-api: collection %s has the implicit field %s, which is not a text or enum field that takes its value", c.Name, name)
		}
	}
	// The path of a collection follows the first collection of each entity, as the router does.
	collectionMap := map[string]types.Collection{}
	for _, other := range ctx.Collections {
		if _, ok := collectionMap[other.Entity]; !ok {
			collectionMap[other.Entity] = other
		}
	}
	if parent, ok := collectionMap[c.ParentEntity()]; ok && scope != "" && len(parent.Scope) > 0 {
		return fmt.Errorf("rest-api: collection %s is scoped under the scoped collection %s, and the runtime data path serves one level of scope", c.Name, parent.Name)
	}
	base, err := collectionBasePath(c, collectionMap)
	if err != nil {
		return err
	}
	for _, param := range extractPathParams(base) {
		if param != scope {
			return fmt.Errorf("rest-api: collection %s has the path parameter %s, which is not its scope field; the runtime data path checks only the scope field", c.Name, param)
		}
	}
	return nil
}

// validateRuntimeUpsert refuses an upsert that the handlers on the runtime data path cannot serve: one with no
// upsert key or a key field that is not a required field of the entity, one with optimistic concurrency on an entity
// with no required integer generation (ST-SUP-RC20-OC14), one whose update of a stored row decides no
// permission, and one whose insert can decide nothing: an entity with no required reference to an entity with a
// resource type needs a public create, with its reason (ST-SUP-RC20-OC13). An optional reference that the body
// leaves out decides nothing.
func validateRuntimeUpsert(c types.Collection, e types.Entity, entities map[string]types.Entity) error {
	if len(c.UpsertKey) == 0 {
		return fmt.Errorf("rest-api: collection %s declares upsert with no upsert_key", c.Name)
	}
	for _, key := range c.UpsertKey {
		ok := false
		for _, f := range e.Fields {
			ok = ok || (f.Name == key && !f.Optional && f.Type != types.FieldTypeJsonb && f.Type != types.FieldTypeBytes)
		}
		if !ok {
			return fmt.Errorf("rest-api: collection %s has the upsert key field %s, which is not a required field of %s of a plain type", c.Name, key, e.Name)
		}
	}
	if c.Concurrency == types.ConcurrencyOptimistic {
		// The upsert compares the generation of an If-Match header with the stored one (ST-SUP-RC20-OC14), so the
		// entity needs a required generation counter.
		ok := false
		for _, f := range e.Fields {
			ok = ok || (f.Name == "generation" && !f.Optional && (f.Type == types.FieldTypeInt32 || f.Type == types.FieldTypeInt64))
		}
		if !ok {
			return fmt.Errorf("rest-api: collection %s declares optimistic concurrency on upsert, and %s has no required field generation of type int32 or int64 for the If-Match precondition to compare (ST-SUP-RC20-OC14)", c.Name, e.Name)
		}
		// An If-Match on a key with no stored row is decided on a required reference before its 412, so the header
		// tells no caller that a row exists; a public insert decides nothing.
		decided := false
		for _, f := range e.Fields {
			ref, has := e.References[f.Name]
			decided = decided || (f.Type == types.FieldTypeRef && !f.Optional && has && ref.Permission != "" && entities[f.To].ResourceType != "")
		}
		if !decided {
			return fmt.Errorf("rest-api: collection %s declares optimistic concurrency on upsert of %s, which has no required reference with a permission: an If-Match on a key with no stored row could not be decided, and its answer would tell that no row exists (ST-SUP-RC20-OC14)", c.Name, e.Name)
		}
	}
	if p := e.Permissions[types.OpUpsert]; p == "" || p == types.PermissionPublic {
		return fmt.Errorf("rest-api: collection %s declares upsert with the permission %q; on the runtime data path an upsert of a stored row decides a permission of %s on it", c.Name, p, e.ResourceType)
	}
	referred := false
	for _, f := range e.Fields {
		referred = referred || (f.Type == types.FieldTypeRef && !f.Optional && entities[f.To].ResourceType != "")
	}
	if !referred && e.Permissions[types.OpCreate] != types.PermissionPublic {
		return fmt.Errorf("rest-api: collection %s declares upsert of %s, which has no required reference to an entity with a resource type, so the insert of a new row can be decided on nothing: declare create public with a reason", c.Name, e.Name)
	}
	return nil
}

// RuntimeService is the variable of main.go that holds the runtime wiring of the service on the runtime data path,
// which the compiler builds; its method CollectionRuntime gives the Runtime of the collection handlers.
const RuntimeService = "service"

// runtimeWiring adds to wiring one constructor per collection, which takes the Runtime of the runtime wiring and the
// operators of the collection's bound slots, and one literal route per operation (ST-SUP-RC20-OC33).
func runtimeWiring(ctx gen.Context, collections []*runtimeCollection, wiring *gen.Wiring) error {
	if wiring.ConstructorCollections == nil {
		wiring.ConstructorCollections = map[int]string{}
	}
	if wiring.ConstructorDeps == nil {
		wiring.ConstructorDeps = map[int][]string{}
	}
	for _, c := range collections {
		index := len(wiring.Constructors)
		wiring.Constructors = append(wiring.Constructors,
			fmt.Sprintf("%s.New%sHandler(%s.CollectionRuntime())", path.Base(ctx.OutputNamespace), c.pascal, RuntimeService))
		wiring.ConstructorCollections[index] = c.decl.Name
		wiring.ConstructorDeps[index] = []string{RuntimeService}
		for _, op := range c.decl.Operations {
			route, err := collectionHTTPRoute(op, c.hrefBase)
			if err != nil {
				return err
			}
			wiring.Routes = append(wiring.Routes, fmt.Sprintf("mux.HandleFunc(%q, %sHandler.%s)", route.pattern(), c.camel, route.handler))
		}
	}
	return nil
}

// runtimeCollections gives the collections of the context on the runtime data path.
func runtimeCollections(ctx gen.Context, entities map[string]types.Entity, collectionMap map[string]types.Collection) ([]*runtimeCollection, error) {
	var out []*runtimeCollection
	for _, c := range ctx.Collections {
		base, err := collectionBasePath(c, collectionMap)
		if err != nil {
			return nil, err
		}
		e := entities[c.Entity]
		rc := &runtimeCollection{decl: c, entity: e, pascal: collectionToPascalCase(c.Name), camel: collectionToCamelCase(c.Name),
			lower: safeVarName(strings.ToLower(e.Name)), hrefBase: ctx.BasePath + base,
			slots: collectCollectionSlotParams(c.Name, ctx.SlotBindings), ops: map[types.Operation]bool{}}
		for _, op := range c.Operations {
			rc.ops[op] = true
		}
		rc.scope = c.ScopeField()
		for _, name := range slices.Sorted(maps.Keys(c.Implicit)) {
			rc.implicit = append(rc.implicit, [2]string{name, c.Implicit[name]})
		}
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeRef && entities[f.To].ResourceType != "" {
				rc.refs = append(rc.refs, f)
			}
		}
		out = append(out, rc)
	}
	return out, nil
}

// generateRuntimeHandlers gives the files of the handlers on the runtime data path: the entity declarations of the
// command and the query runtime in the data package, the runtime helpers, and one handler file per collection.
func generateRuntimeHandlers(ctx gen.Context, collections []*runtimeCollection, slotsImportPath string) ([]gen.File, error) {
	var names []string
	seen := map[string]bool{}
	for _, c := range collections {
		if !seen[c.entity.Name] {
			seen[c.entity.Name] = true
			names = append(names, c.entity.Name)
		}
	}
	// The rows that a ref field names are loaded by the writes, so their entities are declared too.
	for _, c := range collections {
		for _, f := range c.refs {
			if !seen[f.To] {
				seen[f.To] = true
				names = append(names, f.To)
			}
		}
	}
	relations, err := command.SchemaRelations(ctx)
	if err != nil {
		return nil, fmt.Errorf("rest-api: %w", err)
	}
	entityFile, err := command.EntityFile(dataFolder, names, ctx.Entities, ctx.Tenancy.Key, relations)
	if err != nil {
		return nil, fmt.Errorf("rest-api: %w", err)
	}
	viewFile, err := query.ViewFile(dataFolder, names, ctx.Entities)
	if err != nil {
		return nil, fmt.Errorf("rest-api: %w", err)
	}
	dataImport := path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, dataFolder)
	files := []gen.File{
		{Path: path.Join(ctx.OutputNamespace, dataFolder, "entities.go"), Content: entityFile},
		{Path: path.Join(ctx.OutputNamespace, dataFolder, "views.go"), Content: viewFile},
	}
	hasSlots := false
	for _, c := range collections {
		hasSlots = hasSlots || len(c.slots) > 0
	}
	keys := idempotencyKeys(ctx)
	runtimeFile, err := renderRuntimeHelpers(ctx, names, dataImport, slotsImportPath, hasSlots, keys)
	if err != nil {
		return nil, err
	}
	files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "runtime.go"), Content: runtimeFile})
	for _, c := range collections {
		source, err := renderRuntimeHandler(ctx, c, dataImport, slotsImportPath, keys)
		if err != nil {
			return nil, fmt.Errorf("rest-api: collection %s: %w", c.decl.Name, err)
		}
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, collectionToSnakeCase(c.decl.Name)+"_handler.go"), Content: source})
	}
	return files, nil
}

// renderRuntimeHelpers writes runtime.go: the Runtime that the handlers run on and the answers of the errors of Run.
// The bind, the outcome map and the outcome row of a write are the command runtime's (ST-SUP-RP10-LC34).
func renderRuntimeHelpers(ctx gen.Context, names []string, dataImport, slotsImportPath string, hasSlots bool, keys *idempotencyHeader) ([]byte, error) {
	var b bytes.Buffer
	pkg := path.Base(ctx.OutputNamespace)
	fmt.Fprintf(&b, "package %s\n\nimport (\n\t\"context\"\n\t\"encoding/json\"\n\t\"errors\"\n\t\"log/slog\"\n\t\"net\"\n\t\"net/http\"\n\t\"strings\"\n\t\"sync\"\n\t\"sync/atomic\"\n\t\"time\"\n\n", pkg)
	fmt.Fprintf(&b, "\t\"go.opentelemetry.io/otel/attribute\"\n\t\"go.opentelemetry.io/otel/metric\"\n\n")
	fmt.Fprintf(&b, "\t\"github.com/jsell-rh/stego/registry/runtime/audit\"\n\t\"github.com/jsell-rh/stego/registry/runtime/authn\"\n\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n")
	fmt.Fprintf(&b, "\t\"github.com/jsell-rh/stego/registry/runtime/idempotency\"\n\t\"github.com/jsell-rh/stego/registry/runtime/outbox\"\n\t\"github.com/jsell-rh/stego/registry/runtime/probe\"\n\t\"github.com/jsell-rh/stego/registry/runtime/query\"\n\n")
	fmt.Fprintf(&b, "\tdata %q\n", dataImport)
	if hasSlots {
		fmt.Fprintf(&b, "\tslots %q\n", slotsImportPath)
	}
	fmt.Fprintf(&b, ")\n\n")
	fmt.Fprintf(&b, "// subjectType is the subject type of the schema that a verified identity maps to.\nconst subjectType = %q\n\n", ctx.Authorization.SubjectFromIdentity)
	missing, busy, reuse := "about:blank", "about:blank", "about:blank"
	if keys != nil {
		missing, busy, reuse = keys.missing, keys.busy, keys.reuse
	}
	fmt.Fprintf(&b, "// The problem types of the idempotency key (ST-SUP-RC30-OC02).\nconst (\n\tkeyMissingType = %q\n\tkeyBusyType = %q\n\tkeyReusedType = %q\n)\n\n", missing, busy, reuse)
	fmt.Fprintf(&b, "// Entities are the entities that the generated commands write, for the Entities of the command runtime's Config.\nfunc Entities() []command.Declared {\n\treturn []command.Declared{")
	for i, name := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "data.%sEntity", name)
	}
	b.WriteString("}\n}\n\n")
	b.WriteString(runtimeHelpers)
	b.WriteString(listOrderHelper)
	if hasSlots {
		b.WriteString(slotCallerHelper)
	}
	if changeEvents(ctx) {
		b.WriteString(changeEventHelper)
	}
	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("rest-api: formatting runtime.go: %w\n%s", err, b.Bytes())
	}
	return formatted, nil
}

// changeEventHelper builds the payload of a change event (ST-SUP-RC20-OC40).
const changeEventHelper = `// changeEvent gives the payload of a change event for the Kafka destination (ST-SUP-RC20-OC40): the entity, the
// action and the ID of the row, and no field value, so no pii or secret field leaves the service in an event; a
// consumer reads the row through the API, which decides its permission.
func changeEvent(entity, action, id string) json.RawMessage {
	payload, err := json.Marshal(struct {
		Entity string ` + "`json:\"entity\"`" + `
		Action string ` + "`json:\"action\"`" + `
		ID     string ` + "`json:\"id\"`" + `
	}{entity, action, id})
	if err != nil {
		panic(err)
	}
	return payload
}

`

// listOrderHelper reads the orderBy and the fields parameters of a list (ST-SUP-RC20-OC38, ST-SUP-RC20-OC12).
const listOrderHelper = `// errOrder is the error of an orderBy that names a field that is not declared, or a direction that is not asc or
// desc.
var errOrder = errors.New("orderBy names a field that is not declared or a direction that is not asc or desc")

// listOrder reads the orderBy parameter of a list: a comma-separated list of declared fields, each with asc or desc,
// asc when it names none. With no orderBy the order is created_time descending (ST-SUP-RC20-OC11). The key is the last
// tie-breaker of every order, which the query runtime adds (ST-SUP-RP11-LC18).
func listOrder(text string, fields map[string]bool) ([]query.OrderBy, error) {
	if strings.TrimSpace(text) == "" {
		return []query.OrderBy{{Field: "created_time", Descending: true}}, nil
	}
	var order []query.OrderBy
	for _, part := range strings.Split(text, ",") {
		words := strings.Fields(part)
		if len(words) == 0 || len(words) > 2 || !fields[words[0]] {
			return nil, errOrder
		}
		o := query.OrderBy{Field: words[0]}
		if len(words) == 2 {
			switch strings.ToLower(words[1]) {
			case "asc":
			case "desc":
				o.Descending = true
			default:
				return nil, errOrder
			}
		}
		order = append(order, o)
	}
	return order, nil
}

// isBadCursor tells whether err is a cursor of another order, which a list answers with 400.
func isBadCursor(err error) bool { return errors.Is(err, query.ErrBadCursor) }

// errFields is the error of a fields parameter that names a field that is not declared.
var errFields = errors.New("fields names a field that is not declared")

// listFields reads the fields parameter of a list: a comma-separated list of declared fields. It gives nil when the
// parameter names none, and else the selected fields with id, which a selection always keeps (ST-SUP-RC20-OC12).
func listFields(text string, fields map[string]bool) (map[string]bool, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	selected := map[string]bool{"id": true}
	for _, name := range strings.Split(text, ",") {
		name = strings.TrimSpace(name)
		if name == "" || name == "id" {
			continue
		}
		if !fields[name] {
			return nil, errFields
		}
		selected[name] = true
	}
	return selected, nil
}

// selectFields gives the members of the presented value v that the selection names, with id, kind and href, and v
// itself when there is no selection (ST-SUP-RC20-OC12).
func selectFields(v any, selected map[string]bool) any {
	if selected == nil {
		return v
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return v
	}
	for name := range members {
		if !selected[name] && name != "kind" && name != "href" {
			delete(members, name)
		}
	}
	return members
}

`

// runtimeHelpers are the parts of runtime.go after its constants. They are the same for every declaration; ST-SUP-Q74
// and ST-SUP-Q84 ask whether such code moves into the runtime library (ST-STD-RUNTIME-R05).
const runtimeHelpers = `// Runtime is what the handlers of the collections run on (ST-SUP-RC20-OC37): the command runtime for each write, the
// query runtime for each read, and the probes that bind the probe call of each read. The handlers open no database
// connection and hold no store.
type Runtime struct {
	Commands *command.Runtime
	Queries  *query.Runtime
	Probes   command.Probes
	// Retention is how long a completed idempotency key is kept; zero is the runtime's default.
	Retention time.Duration
	// AfterSlots are the after slots of the collections, which the outbox worker runs (ST-SUP-RC20-OC34). Nil: the
	// handlers register none, and a staged after slot waits in the outbox.
	AfterSlots *AfterSlots
}

// MaxRequestBody is the most bytes of a request body that a handler reads.
const MaxRequestBody = 1 << 20

// noProbe is the typed probe of a collection operation, which emits no domain observation.
func noProbe(*probe.Probe) struct{} { return struct{}{} }

// mustCommand gives a checked command declaration. The generator checked each declaration, so it does not fail.
func mustCommand(c *command.Command[struct{}], err error) *command.Command[struct{}] {
	if err != nil {
		panic(err)
	}
	return c
}

// mustQuery gives a checked query declaration.
func mustQuery(q *query.Query, err error) *query.Query {
	if err != nil {
		panic(err)
	}
	return q
}

// slotRefusal is the refusal of a before slot fill: the handler answers it with the slot's status, and Run rolls the
// command back, so nothing of it is written (ST-SUP-RC20-OC30).
type slotRefusal struct {
	status int
	detail string
}

func (r *slotRefusal) Error() string { return "a slot fill refused the request" }

// haltHeader marks the response of a command that a before slot halted with no write. The key row stores it, so a
// replay of the request is known as halted too; writeResult takes it off before the response leaves the service.
const haltHeader = "Stego-Slot-Halted"

// slotHalt is the result of a before slot fill that accepted the request and stops it with a status: the command
// writes nothing and answers the status, and no after slot runs.
func slotHalt(status int) (idempotency.Response, error) {
	if status <= 0 {
		status = http.StatusOK
	}
	return idempotency.Response{Status: status, Header: http.Header{haltHeader: {"true"}}}, nil
}

// halted tells whether a before slot halted the command of result with no write, also in a replay.
func halted(result command.Result) bool { return result.Response.Header.Get(haltHeader) != "" }

// afterSlotFailures counts the after slots that failed after their command committed.
var afterSlotFailures atomic.Int64

// AfterSlotFailures gives how many after slots failed after their command committed.
func AfterSlotFailures() int64 { return afterSlotFailures.Load() }

// AfterSlotsDestination is the outbox destination of the after slots: a command stages each bound after slot in its
// transaction, so the slot runs only after the command committed, and the outbox worker runs it and retries a
// failure (ST-SUP-RC20-OC34).
const AfterSlotsDestination = "rest-api.after-slots"

// errAfterSlot is the fixed error of a failed after slot: the outbox runs it again, and no error text of the fill
// reaches the queue or the log.
var errAfterSlot = errors.New("an after slot failed")

// AfterSlots runs the after slots that the outbox worker delivers, one function for each collection, and makes
// each failure and each retry visible as a metric and a log line that name the slot and hold no error text.
type AfterSlots struct {
	mu          sync.Mutex
	collections map[string]func(ctx context.Context, slot string, payload []byte) error
	failures    metric.Int64Counter
	retries     metric.Int64Counter
}

// NewAfterSlots gives the after slots of the collections, with no metric until Meter sets one.
func NewAfterSlots() *AfterSlots {
	return &AfterSlots{collections: map[string]func(context.Context, string, []byte) error{}}
}

// Meter records the failure and the retry metrics of the after slots on meter, before the worker runs. A nil meter,
// such as the telemetry runtime's with no collector set, records no metric; the log lines stay.
func (a *AfterSlots) Meter(meter metric.MeterProvider) error {
	if meter == nil {
		return nil
	}
	m := meter.Meter("stego/rest-api")
	failures, err := m.Int64Counter("rest_api.after_slot.failures", metric.WithDescription("after slots that failed, by slot"))
	if err != nil {
		return err
	}
	retries, err := m.Int64Counter("rest_api.after_slot.retries", metric.WithDescription("after slots that run again, by slot"))
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures, a.retries = failures, retries
	return nil
}

// add registers the after slots of one collection.
func (a *AfterSlots) add(collection string, deliver func(ctx context.Context, slot string, payload []byte) error) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.collections[collection] = deliver
}

// Deliver runs one after slot that the outbox worker delivers on AfterSlotsDestination, which the wiring registers
// as idempotent, as a fill runs at least once, and as must land, so a message is retried and never parked for its
// retries. The message kind is <collection>.<slot>. A run that is not the first counts as a retry, and a failure is
// counted and logged and refused as transient, so the outbox runs it again.
func (a *AfterSlots) Deliver(ctx context.Context, d outbox.Delivery) error {
	collection, slot, _ := strings.Cut(d.Kind, ".")
	a.mu.Lock()
	run, failures, retries := a.collections[collection], a.failures, a.retries
	a.mu.Unlock()
	attr := metric.WithAttributes(attribute.String("slot", d.Kind))
	if d.Attempts > 1 {
		if retries != nil {
			retries.Add(ctx, 1, attr)
		}
		slog.WarnContext(ctx, "an after slot runs again through the outbox", "slot", d.Kind)
	}
	if run == nil || run(ctx, slot, d.Payload) != nil {
		afterSlotFailed(ctx, failures, d.Kind)
		return errAfterSlot
	}
	return nil
}

// afterSlotFailed records an after slot that failed after its command committed: a metric and a log line that name
// the slot and hold no error text (ST-SUP-RC20-OC34).
func afterSlotFailed(ctx context.Context, failures metric.Int64Counter, slot string) {
	afterSlotFailures.Add(1)
	if failures != nil {
		failures.Add(ctx, 1, metric.WithAttributes(attribute.String("slot", slot)))
	}
	slog.ErrorContext(ctx, "an after slot failed after its command committed; the outbox runs it again", "slot", slot)
}

// invalidRequest is a request body that does not decode into the collection's type.
type invalidRequest struct{ problem *ServiceError }

func (e *invalidRequest) Error() string { return "the request is not valid" }

// preconditionFailed is the answer of an upsert with optimistic concurrency whose If-Match precondition does not
// hold: 412 for a stale or malformed generation or a row that does not exist, 428 for a stored row and no If-Match
// (ST-SUP-RC20-OC14). The command writes nothing.
type preconditionFailed struct {
	status int
	detail string
}

func (e *preconditionFailed) Error() string { return "the precondition of the request does not hold" }

// ifMatchGeneration reads the If-Match header of an upsert with optimistic concurrency: one strong entity tag that
// holds the decimal generation the client last read, such as "3". It gives the generation, whether the request
// sends the header, and whether the header has that form; a weak tag, *, a list or any other text is not valid.
func ifMatchGeneration(r *http.Request) (int64, bool, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		return 0, false, false
	}
	if len(values) != 1 {
		return 0, true, false
	}
	tag := strings.TrimSpace(values[0])
	if len(tag) < 3 || len(tag) > 21 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return 0, true, false
	}
	var generation int64
	for _, digit := range tag[1 : len(tag)-1] {
		if digit < '0' || digit > '9' || generation > (1<<62)/10 {
			return 0, true, false
		}
		generation = generation*10 + int64(digit-'0')
	}
	return generation, true, true
}

// requestKey is the idempotency key of a write that takes no key from the client: a PUT, a DELETE, or a create or
// a patch without the header when the declaration does not require it. Run takes every write inside the
// idempotency wrapper, so the request gets a key of its own that no retry sends again (ST-SUP-Q86).
func requestKey(requestID string) string { return "request:" + requestID }

// commandRequest gives what Run knows of one write.
func (rt *Runtime) commandRequest(r *http.Request, b command.Bound, key, fingerprint, requestID string, body []byte, resource string) command.Request {
	return command.Request{Tenant: b.Tenant, Principal: b.Principal, Key: key, Fingerprint: fingerprint, Body: body,
		Resource: resource, RequestID: requestID, ActorIP: remoteIP(r.RemoteAddr), UserAgent: r.UserAgent(),
		Retention: rt.Retention}
}

// boundQuery gives the bound call of a read for the verified tenant and principal, with its probe call.
func (rt *Runtime) boundQuery(ctx context.Context, b command.Bound, q *query.Query) query.Bound[struct{}] {
	call := rt.Probes.Bind(ctx, b.Tenant.ID(), b.Principal.Subject().String(), q.Name())
	return query.Bound[struct{}]{Tenant: b.Tenant, Principal: b.Principal, Call: call, Probe: noProbe, RequestID: command.NewID()}
}

// answerWrite answers an error of Run and writes the audit row of the denied or failed write in its own short
// transaction (ST-SUP-RC28-OC03): a slot refusal is the slot's status and outcome denied, a deny is 404, the same
// answer as a missing row, a refused field value 400 naming the field (ST-SUP-Q118), a conflict 409 and every other
// error 500. A row that is not there is 404 and outcome
// denied for an operation that decides a permission, as a deny is, so the answer never tells that a row exists or
// once existed (ST-SUP-RC20-OC37, ST-SUP-RP10-LC31), and 404 not_found otherwise. No error text reaches the answer or
// the audit row.
func (rt *Runtime) answerWrite(w http.ResponseWriter, r *http.Request, req command.Request, action, resourceType, kind string, decided bool, err error) {
	var refused *slotRefusal
	var invalid *invalidRequest
	var precondition *preconditionFailed
	switch {
	case errors.As(err, &invalid):
		handleError(w, r, invalid.problem)
		return
	case errors.As(err, &precondition):
		_ = rt.Commands.RecordFailure(r.Context(), req, action, resourceType, command.Outcome{Status: precondition.status, Code: "precondition_failed", Audit: audit.OutcomeFailed})
		handleError(w, r, errorForStatus(precondition.status, precondition.detail))
		return
	case errors.As(err, &refused):
		status := refused.status
		if status <= 0 {
			status = http.StatusForbidden
		}
		_ = rt.Commands.RecordFailure(r.Context(), req, action, resourceType, command.Outcome{Status: status, Code: "slot_refused", Audit: audit.OutcomeDenied})
		handleError(w, r, errorForStatus(status, refused.detail))
		return
	case errors.Is(err, command.ErrNotFound) && decided:
		_ = rt.Commands.RecordFailure(r.Context(), req, action, resourceType, command.Outcome{Status: http.StatusNotFound, Code: "denied", Audit: audit.OutcomeDenied})
		handleError(w, r, NotFound(kind, req.Resource))
		return
	case errors.Is(err, command.ErrNotFound):
		_ = rt.Commands.RecordFailure(r.Context(), req, action, resourceType, command.Outcome{Status: http.StatusNotFound, Code: "not_found", Audit: audit.OutcomeFailed})
		handleError(w, r, NotFound(kind, req.Resource))
		return
	}
	o := command.OutcomeOf(err)
	_ = rt.Commands.RecordFailure(r.Context(), req, action, resourceType, o)
	switch o.Status {
	case http.StatusBadRequest:
		// The runtime refused a field's value (command.ErrInvalid): the answer names the field and the fixed reason.
		handleError(w, r, Validation([]ValidationError{{Field: o.Field, Message: o.Reason}}))
	case http.StatusNotFound:
		handleError(w, r, NotFound(kind, req.Resource))
	case http.StatusConflict:
		handleError(w, r, Conflict("the "+kind+" changed during the request"))
	default:
		handleError(w, r, InternalError("internal error"))
	}
}

// answerKey answers a result of the idempotency wrapper that did not call the fill: a key in progress is 409 and a
// key used with another request 422 (ST-SUP-RC30-OC02).
func answerKey(w http.ResponseWriter, r *http.Request, result command.Result) {
	problem := errorForStatus(result.Response.Status, "the idempotency key is in progress")
	problem.Type = keyBusyType
	if result.Problem == idempotency.ProblemReused {
		problem.Detail, problem.Type = "the idempotency key was used with another request", keyReusedType
	}
	handleError(w, r, problem)
}

// keyMissing answers a create or a patch that needs the Idempotency-Key header and has none, or a malformed one.
func keyMissing(w http.ResponseWriter, r *http.Request) {
	problem := BadRequest("the Idempotency-Key header is absent or malformed")
	problem.Type = keyMissingType
	handleError(w, r, problem)
}

// canonicalID gives the lower-case form of a UUID, the form that the rows and the relations hold, so that an
// upper-case UUID in a path or a body names the same row (ST-SUP-RC20-OC24). Another text stays as it is.
func canonicalID(id string) string {
	if command.ValidUUID(id) {
		return strings.ToLower(id)
	}
	return id
}

// answerRead answers an error of a read: a row that is not there or that the caller may not read is 404, and every
// other error 500 (ST-SUP-RP11-LC06).
func answerRead(w http.ResponseWriter, r *http.Request, kind, id string, err error) {
	if errors.Is(err, query.ErrNotFound) {
		handleError(w, r, NotFound(kind, id))
		return
	}
	handleError(w, r, InternalError("internal error"))
}

// writeResult writes the response that Run gives: the fill's response or the stored one of a replay.
func writeResult(w http.ResponseWriter, result command.Result) {
	for name, values := range result.Response.Header {
		w.Header()[name] = values
	}
	w.Header().Del(haltHeader)
	w.WriteHeader(result.Response.Status)
	_, _ = w.Write(result.Response.Body)
}

// jsonResponse is the response of a write with a JSON body.
func jsonResponse(status int, body []byte) idempotency.Response {
	return idempotency.Response{Status: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}
}

// callerName is the name of the verified caller that the created_by and updated_by fields hold: the email, or the
// user ID when the identity has none.
func callerName(ctx context.Context) string {
	identity, _ := authn.FromContext(ctx)
	if email := identity.Email(); email != "" {
		return email
	}
	return identity.UserID()
}

// ptrOf gives a pointer to a copy of v.
func ptrOf[T any](v T) *T { return &v }

// remoteIP is the host part of a remote address.
func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}

`

// slotCallerHelper maps the verified identity to the caller of a slot request.
const slotCallerHelper = `
// callerIdentity is the caller of a slot request: the verified identity of the request.
func callerIdentity(ctx context.Context) *slots.Identity {
	identity, _ := authn.FromContext(ctx)
	role := ""
	if roles := identity.Roles(); len(roles) > 0 {
		role = roles[0]
	}
	return &slots.Identity{UserID: identity.UserID(), Role: role, Attributes: identity.Attributes()}
}
`

// emitRuntimeSlotCheck writes the handling of a before slot's result inside a generated command's fill: an error is
// the fill's error, a refusal is a slotRefusal, which rolls the command back before any write, and a halt answers
// its status with no write.
func emitRuntimeSlotCheck(buf *bytes.Buffer, fieldName string) {
	fmt.Fprintf(buf, "\t\tslotResult, slotErr := h.%s.Evaluate(ctx, slotReq)\n", fieldName)
	fmt.Fprintf(buf, "\t\tif slotErr != nil {\n\t\t\treturn idempotency.Response{}, slotErr\n\t\t}\n")
	fmt.Fprintf(buf, "\t\tif !slotResult.Ok {\n\t\t\treturn idempotency.Response{}, &slotRefusal{status: int(slotResult.StatusCode), detail: slotResult.ErrorMessage}\n\t\t}\n")
	fmt.Fprintf(buf, "\t\tif slotResult.Halt {\n\t\t\treturn slotHalt(int(slotResult.StatusCode))\n\t\t}\n")
	fmt.Fprintf(buf, "\t}\n")
}

// emitRuntimeAfterFailure is the answer of a failed after slot on the runtime data path: the command committed and
// its response is stored, so the failure is logged with no error text and the handler answers the committed
// response (ST-SUP-Q89).
func emitRuntimeAfterFailure(buf *bytes.Buffer, param collectionSlotParam) {
	fmt.Fprintf(buf, "\t\t\t%s\n", afterCallMarker)
}

// afterCallMarker marks the call of an after slot that the runtime data path replaces with the staging of the slot
// in the command's transaction (ST-SUP-RC20-OC34).
const afterCallMarker = "/* stego: after slot */"

// afterCall matches the call of an after slot that the shared emitters write, with the marker of a failure.
var afterCall = regexp.MustCompile(`(?m)^(\t*)if _, slotErr := h\.(\w+)\.Evaluate\(r\.Context\(\), (.+?)\); slotErr != nil \{\n\t*` + regexp.QuoteMeta(afterCallMarker) + `\n\t*\}\n`)

// runtimeHandlerWriter writes one handler file.
type runtimeHandlerWriter struct {
	// upsertRead is the Read statement of the lookup of an upsert by its natural key.
	upsertRead string
	bytes.Buffer
	c          *runtimeCollection
	ctx        gen.Context
	keys       *idempotencyHeader
	envelope   bool
	slotsAlias string
	// events tells whether each write stages its change event for the Kafka destination (ST-SUP-RC20-OC40).
	events bool
}

func (w *runtimeHandlerWriter) f(format string, args ...any) { fmt.Fprintf(&w.Buffer, format, args...) }

// renderRuntimeHandler writes the handler file of one collection: its command and query declarations, its handler
// type, and one method per declared operation that calls Run.
func renderRuntimeHandler(ctx gen.Context, c *runtimeCollection, dataImport, slotsImportPath string, keys *idempotencyHeader) ([]byte, error) {
	w := &runtimeHandlerWriter{c: c, ctx: ctx, keys: keys, envelope: ctx.Conventions.ResponseFormat == "envelope",
		events: changeEvents(ctx)}
	if c.ops[types.OpUpsert] {
		read, err := command.KeyListRead(c.entity.Name, ctx.Entities, ctx.Tenancy.Key, c.decl.UpsertLookup())
		if err != nil {
			return nil, err
		}
		w.upsertRead = read
	}
	if len(c.slots) > 0 {
		w.slotsAlias = "slots"
	}
	e := c.entity
	hasWrite := c.ops[types.OpCreate] || c.ops[types.OpUpdate] || c.ops[types.OpPatch] || c.ops[types.OpDelete] || c.ops[types.OpUpsert]
	hasBody := c.ops[types.OpCreate] || c.ops[types.OpUpdate] || c.ops[types.OpPatch] || c.ops[types.OpUpsert]
	needTime, needFmt := false, false
	for _, f := range e.Fields {
		if f.Type == types.FieldTypeTimestamp && (hasBody || f.Sensitivity == "secret") {
			needTime = true
		}
	}
	if len(c.slots) > 0 && needsFmtForSlotFields(e) {
		needFmt = true
	}
	needUUID := w.envelope && (c.ops[types.OpCreate] || c.ops[types.OpUpsert])
	w.f("package %s\n\nimport (\n\t\"context\"\n\t\"encoding/json\"\n", path.Base(ctx.OutputNamespace))
	if c.ops[types.OpList] {
		w.f("\t\"errors\"\n")
	}
	if needFmt {
		w.f("\t\"fmt\"\n")
	}
	if hasBody {
		w.f("\t\"io\"\n")
	}
	w.f("\t\"net/http\"\n")
	if c.ops[types.OpList] {
		w.f("\t\"strconv\"\n")
	}
	if needTime {
		w.f("\t\"time\"\n")
	}
	w.f("\n")
	if needUUID {
		w.f("\t\"github.com/google/uuid\"\n\n")
	}
	// Each handler binds the verified identity through command.BindIdentity.
	w.f("\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n")
	if hasWrite {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/idempotency\"\n")
	}
	if hasWrite && w.events {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox\"\n")
	}
	if c.ops[types.OpRead] || c.ops[types.OpList] {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/query\"\n")
	}
	w.f("\n\tdata %q\n", dataImport)
	if len(c.slots) > 0 {
		w.f("\tslots %q\n", slotsImportPath)
	}
	w.f(")\n\n")
	w.declarations()
	w.f("// %sHandler serves the collection %s on the command and the query runtime (ST-SUP-RC20-OC37): each\n", c.pascal, c.decl.Name)
	w.f("// operation calls the Run of its generated command or query, and the handler holds no store.\n")
	w.f("type %sHandler struct {\n\trt *Runtime\n", c.pascal)
	for _, sp := range c.slots {
		w.f("\t%s slots.%s\n", sp.FieldName, sp.InterfaceType)
	}
	w.f("}\n\n")
	w.f("// New%sHandler gives the handler of %s on rt, with the operators of its bound slots.\n", c.pascal, c.decl.Name)
	w.f("func New%sHandler(rt *Runtime", c.pascal)
	for _, sp := range c.slots {
		w.f(", %s slots.%s", sp.FieldName, sp.InterfaceType)
	}
	w.f(") *%sHandler {\n\th := &%sHandler{rt: rt", c.pascal, c.pascal)
	for _, sp := range c.slots {
		w.f(", %s: %s", sp.FieldName, sp.FieldName)
	}
	w.f("}\n")
	var deliver runtimeHandlerWriter
	deliver.c = c
	if deliver.deliverAfter() {
		w.f("\t// The outbox worker runs the after slots of the collection (ST-SUP-RC20-OC34).\n\trt.AfterSlots.add(%q, h.deliverAfter)\n", c.decl.Name)
	}
	w.f("\treturn h\n}\n\n")
	w.Write(deliver.Bytes())
	w.helpers()
	for _, op := range c.decl.Operations {
		switch op {
		case types.OpCreate:
			w.create()
		case types.OpRead:
			w.read()
		case types.OpList:
			w.list()
		case types.OpUpdate:
			w.update()
		case types.OpPatch:
			w.patch()
		case types.OpDelete:
			w.remove()
		case types.OpUpsert:
			w.upsert()
		}
	}
	formatted, err := format.Source(w.Bytes())
	if err != nil {
		return nil, fmt.Errorf("formatting the handler: %w\n%s", err, w.Bytes())
	}
	return formatted, nil
}

// commandVar is the variable of an operation's command or query declaration.
func (w *runtimeHandlerWriter) commandVar(op types.Operation) string {
	switch op {
	case types.OpRead, types.OpList:
		return w.c.camel + collectionToPascalCase(string(op)) + "Query"
	}
	return w.c.camel + collectionToPascalCase(string(op)) + "Command"
}

// declarations writes the command of each write operation and the query of each read operation. A command is named
// by its operation, which its audit rows carry as the action (ST-SUP-RC28-OC02); an operation that decides a
// permission decides it on the row of the request's ID before the fill (ST-SUP-RP10-LC31).
func (w *runtimeHandlerWriter) declarations() {
	c, e := w.c, w.c.entity
	table := commandTableName(e.Name)
	for _, op := range c.decl.Operations {
		switch op {
		case types.OpRead:
			w.f("// %s is the query of a read of %s (ST-SUP-RP11).\nvar %s = mustQuery(query.NewQuery(%q, data.Entity%sView()))\n\n", w.commandVar(op), c.decl.Name, w.commandVar(op), string(op), e.Name)
		case types.OpList:
			w.f("// %s is the query of a list of %s, on the list permission of %s (ST-SUP-RP11-LC07).\nvar %s = mustQuery(query.NewListQuery(%q, data.Entity%sView(), %q))\n\n", w.commandVar(op), c.decl.Name, e.Name, w.commandVar(op), string(op), e.Name, e.Permissions[types.OpList])
		default:
			w.f("// %s is the generated command of a %s of %s (ST-SUP-RC20-OC37).\n", w.commandVar(op), op, c.decl.Name)
			writes := []string{"data." + e.Name + "Entity"}
			for _, f := range c.refs {
				if name := "data." + f.To + "Entity"; op != types.OpDelete && !slices.Contains(writes, name) {
					writes = append(writes, name)
				}
			}
			w.f("var %s = mustCommand(command.NewCommand(command.CommandSpec[struct{}]{\n\tName: %q,\n\tWrites: []command.Declared{%s},\n\tProbe: noProbe,\n", w.commandVar(op), string(op), strings.Join(writes, ", "))
			var destinations []string
			if effects := w.afterEffects(op); len(effects) > 0 {
				destinations = append(destinations, fmt.Sprintf("{Name: AfterSlotsDestination, Idempotent: true, MustLand: true, Effects: []string{%s}}", quoteAll(effects)))
			}
			if w.events {
				// The change events of the write go to Kafka, idempotent by the event ID and not must land, so an event
				// whose attempts end is parked as on every other destination (ST-SUP-RP01-LC31).
				destinations = append(destinations, fmt.Sprintf("{Name: kafkaoutbox.DestinationName, Idempotent: true, Effects: []string{%s}}", quoteAll(w.eventTypes(op))))
			}
			if len(destinations) > 0 {
				w.f("\tDestinations: []command.Destination{%s},\n", strings.Join(destinations, ", "))
			}
			// A create and an upsert have no row to decide on before the fill: an upsert decides its permission on
			// the stored row in the fill.
			if p := c.permission(op); p != "" && op != types.OpCreate && op != types.OpUpsert {
				// The table is named with its schema, so no relation of the same name earlier on the search path
				// stands in for it.
				token := fmt.Sprintf("SELECT authz_token FROM public.%s WHERE tenant_id = $1 AND id = $2::uuid FOR SHARE", table)
				w.f("\tPermission: %q,\n\tResource: &command.Resource{Type: %q, Token: %q, ValidID: command.ValidUUID},\n", p, e.ResourceType, token)
			}
			w.f("}))\n\n")
		}
	}
	if c.ops[types.OpUpsert] {
		w.f("// %sUpsertKey finds the row of an upsert by %s: its natural key, with its scope and implicit fields\n// (ST-SUP-RC20-OC13).\nvar %sUpsertKey = command.Must(command.NewList(data.%sEntity, command.ListSpec{Name: \"upsert-key\", Read: %q}))\n\n", c.camel, strings.Join(c.decl.UpsertLookup(), ", "), c.camel, e.Name, w.upsertRead)
	}
	if c.ops[types.OpList] {
		w.f("// %sSearchFields are the fields that a list search may name, with the kind of their values: the declared\n// fields that are not pii, secret or implicit, of a text, number or bool type (ST-SUP-RP11-LC20).\nvar %sSearchFields = map[string]query.FieldKind{", c.camel, c.camel)
		implicit := map[string]bool{}
		for _, kv := range c.implicit {
			implicit[kv[0]] = true
		}
		for _, f := range e.Fields {
			// An implicit field has one value in the collection, which the list filters on already.
			if kind := searchKind(f); kind != "" && !implicit[f.Name] {
				w.f("%q: query.%s, ", f.Name, kind)
			}
		}
		w.f("}\n\n")
	}
	if fields := w.fingerprintFields(types.OpCreate); c.ops[types.OpCreate] {
		w.f("// %sCreateFields are the body fields that the fingerprint of a create covers (ST-SUP-RC30-OC03).\nvar %sCreateFields = []string{%s}\n\n", c.camel, c.camel, quoteAll(fields))
	}
	if fields := w.fingerprintFields(types.OpPatch); c.ops[types.OpPatch] {
		w.f("// %sPatchFields are the body fields that the fingerprint of a patch covers (ST-SUP-RC30-OC03).\nvar %sPatchFields = []string{%s}\n\n", c.camel, c.camel, quoteAll(fields))
	}
}

// fingerprintFields are the body fields that the client gives for an operation: for patch the patchable fields, and
// for create the entity's fields that are not server-managed.
func (w *runtimeHandlerWriter) fingerprintFields(op types.Operation) []string {
	var fields []string
	if op == types.OpPatch {
		for _, f := range w.c.patchFields() {
			fields = append(fields, f.Name)
		}
		return fields
	}
	for _, f := range w.c.entity.Fields {
		if !isServerManaged(f) {
			fields = append(fields, f.Name)
		}
	}
	return fields
}

func quoteAll(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}

// commandTableName is the table of an entity on the runtime data path, as the PostgreSQL adapter creates it
// (ST-SUP-RC01-OC41).
func commandTableName(entity string) string { return postgresadapter.TableName(entity) }

// helpers writes the conversions of the collection's entity: from a stored row to the API type, from the
// read-only value to it, the redaction of its secret fields and its presentation.
func (w *runtimeHandlerWriter) helpers() {
	c, e := w.c, w.c.entity
	w.f("// %sFromView gives the API value of a read-only %s. A secret field is never read (ST-DECL-EN03).\n", c.camel, e.Name)
	w.f("func %sFromView(v data.%sView) %s {\n\treturn %s{ID: v.ID", c.camel, e.Name, e.Name, e.Name)
	for _, f := range e.Fields {
		if f.Sensitivity == "secret" {
			continue
		}
		name := toPascalCase(f.Name)
		expr := "v." + name
		if f.Type == types.FieldTypeFloat {
			expr = "float32(" + expr + ")"
		}
		switch {
		case f.Optional && f.Type == types.FieldTypeJsonb:
			expr = c.camel + "JSON(" + expr + ")"
		case f.Optional:
			expr = "ptrOf(" + expr + ")"
		}
		w.f(", %s: %s", name, expr)
	}
	w.f("}\n}\n\n")
	w.converters()
	w.f("// %sPresented gives the response value of one %s: its secret fields are cleared, so no response and no stored\n// response holds them.\n", c.camel, e.Name)
	w.f("func %sPresented(v %s) any {\n", c.camel, e.Name)
	for _, f := range e.Fields {
		if f.Sensitivity != "secret" {
			continue
		}
		if f.Optional {
			w.f("\tv.%s = nil\n", toPascalCase(f.Name))
		} else {
			w.f("\tv.%s = %s\n", toPascalCase(f.Name), zeroOf(fieldTypeToGo(f.Type)))
		}
	}
	if w.envelope {
		w.f("\treturn presentEntity(v, %q, v.ID, %s)\n}\n\n", e.Name, c.hrefExpr("v"))
	} else {
		w.f("\treturn v\n}\n\n")
	}
	if c.ops[types.OpCreate] || c.ops[types.OpUpdate] || c.ops[types.OpUpsert] {
		w.f("// decode%s gives the %s of a create or an update body. A kind member that is not %s is refused, and the\n// client values of the server-managed fields are cleared (ST-SUP-RC20-OC17, ST-SUP-RC20-OC19).\n", c.pascal, e.Name, e.Name)
		w.f("func decode%s(bodyBytes []byte) (%s, error) {\n", c.pascal, e.Name)
		w.f("\tvar rawMap map[string]json.RawMessage\n\tif err := json.Unmarshal(bodyBytes, &rawMap); err != nil {\n\t\treturn %s{}, &invalidRequest{problem: BadRequest(\"the body is not a JSON object\")}\n\t}\n", e.Name)
		w.f("\tif kindRaw, ok := rawMap[\"kind\"]; ok {\n\t\tvar kind string\n\t\tif err := json.Unmarshal(kindRaw, &kind); err != nil || kind != %q {\n\t\t\treturn %s{}, &invalidRequest{problem: BadRequest(\"kind must be %s\")}\n\t\t}\n\t}\n", e.Name, e.Name, e.Name)
		w.f("\tvar %s %s\n\tif err := json.Unmarshal(bodyBytes, &%s); err != nil {\n\t\treturn %s{}, &invalidRequest{problem: BadRequest(\"the body does not decode into %s\")}\n\t}\n", c.lower, e.Name, c.lower, e.Name, e.Name)
		w.f("\t%s.ID = \"\"\n", c.lower)
		emitClearServerManagedFields(&w.Buffer, c.lower, e)
		w.canonicalRefs(c.lower, e.Fields, false)
		w.f("\treturn %s, nil\n}\n\n", c.lower)
	}
	if c.ops[types.OpPatch] {
		w.f("// %sPatchRequest holds the patchable fields of %s; a nil field is not changed (ST-SUP-RC20-OC15).\ntype %sPatchRequest struct {\n", c.pascal, c.decl.Name, c.pascal)
		for _, f := range c.patchFields() {
			w.f("\t%s *%s `json:%q`\n", toPascalCase(f.Name), fieldTypeToGo(f.Type), f.Name)
		}
		w.f("}\n\n")
		w.f("// decode%sPatch gives the patch of a patch body. A kind member that is not %s is refused.\n", c.pascal, e.Name)
		w.f("func decode%sPatch(bodyBytes []byte) (%sPatchRequest, error) {\n", c.pascal, c.pascal)
		w.f("\tvar rawMap map[string]json.RawMessage\n\tif err := json.Unmarshal(bodyBytes, &rawMap); err != nil {\n\t\treturn %sPatchRequest{}, &invalidRequest{problem: BadRequest(\"the body is not a JSON object\")}\n\t}\n", c.pascal)
		w.f("\tif kindRaw, ok := rawMap[\"kind\"]; ok {\n\t\tvar kind string\n\t\tif err := json.Unmarshal(kindRaw, &kind); err != nil || kind != %q {\n\t\t\treturn %sPatchRequest{}, &invalidRequest{problem: BadRequest(\"kind must be %s\")}\n\t\t}\n\t}\n", e.Name, c.pascal, e.Name)
		w.f("\tvar patch %sPatchRequest\n\tif err := json.Unmarshal(bodyBytes, &patch); err != nil {\n\t\treturn %sPatchRequest{}, &invalidRequest{problem: BadRequest(\"the body does not decode into the patch of %s\")}\n\t}\n", c.pascal, c.pascal, e.Name)
		w.canonicalRefs("patch", c.patchFields(), true)
		w.f("\treturn patch, nil\n}\n\n")
	}
}

// canonicalRefs writes, in the decode of a body, the lower-case form of each ref field of the decoded value v. So
// the before and validate slots, the scope check and the stored row all see the one form of a UUID, whatever case
// the request sent (ST-SUP-RC20-OC24). Each field of a patch is a pointer, and pointers tells so.
func (w *runtimeHandlerWriter) canonicalRefs(v string, fields []types.Field, pointers bool) {
	for _, f := range fields {
		if f.Type != types.FieldTypeRef {
			continue
		}
		name := v + "." + toPascalCase(f.Name)
		if pointers || f.Optional {
			w.f("\tif %s != nil {\n\t\t*%s = canonicalID(*%s)\n\t}\n", name, name, name)
		} else {
			w.f("\t%s = canonicalID(%s)\n", name, name)
		}
	}
}

// hasJSON tells whether the collection's entity has a jsonb field. Its API type holds the field as json.RawMessage
// (a pointer to one when the field is optional), and the command runtime's row type as []byte, so a struct
// conversion between the two does not compile and the handler converts field by field.
func (w *runtimeHandlerWriter) hasJSON() bool {
	for _, f := range w.c.entity.Fields {
		if f.Type == types.FieldTypeJsonb {
			return true
		}
	}
	return false
}

// toData gives the expression of the command runtime's row value of the API value expr.
func (w *runtimeHandlerWriter) toData(expr string) string {
	if w.hasJSON() {
		return w.c.camel + "ToData(" + expr + ")"
	}
	return "data." + w.c.entity.Name + "(" + expr + ")"
}

// fromData gives the expression of the API value of the command runtime's row value expr.
func (w *runtimeHandlerWriter) fromData(expr string) string {
	if w.hasJSON() {
		return w.c.camel + "FromData(" + expr + ")"
	}
	return w.c.entity.Name + "(" + expr + ")"
}

// converters writes, for an entity with a jsonb field, the field by field conversions between the API value and the
// command runtime's row value. An optional jsonb field that is not set is nil in both, so it is NULL in its column
// and absent from a response.
func (w *runtimeHandlerWriter) converters() {
	if !w.hasJSON() {
		return
	}
	c, e := w.c, w.c.entity
	for _, f := range e.Fields {
		if f.Type == types.FieldTypeJsonb && f.Optional {
			w.f("// %sJSON gives the API value of an optional jsonb column: nil when the column is NULL.\n", c.camel)
			w.f("func %sJSON(v []byte) *json.RawMessage {\n\tif v == nil {\n\t\treturn nil\n\t}\n\tr := json.RawMessage(v)\n\treturn &r\n}\n\n", c.camel)
			w.f("// %sJSONBytes gives the column value of an optional jsonb field: nil, which is NULL, when it is not set.\n", c.camel)
			w.f("func %sJSONBytes(v *json.RawMessage) []byte {\n\tif v == nil {\n\t\treturn nil\n\t}\n\treturn []byte(*v)\n}\n\n", c.camel)
			break
		}
	}
	field := func(f types.Field, to bool) string {
		name := toPascalCase(f.Name)
		switch {
		case f.Type != types.FieldTypeJsonb:
			return "v." + name
		case f.Optional && to:
			return c.camel + "JSONBytes(v." + name + ")"
		case f.Optional:
			return c.camel + "JSON(v." + name + ")"
		case to:
			return "[]byte(v." + name + ")"
		}
		return "json.RawMessage(v." + name + ")"
	}
	w.f("// %sToData gives the command runtime's row value of an API value of %s.\n", c.camel, e.Name)
	w.f("func %sToData(v %s) data.%s {\n\treturn data.%s{ID: v.ID", c.camel, e.Name, e.Name, e.Name)
	for _, f := range e.Fields {
		w.f(", %s: %s", toPascalCase(f.Name), field(f, true))
	}
	w.f("}\n}\n\n")
	w.f("// %sFromData gives the API value of a command runtime's row value of %s.\n", c.camel, e.Name)
	w.f("func %sFromData(v data.%s) %s {\n\treturn %s{ID: v.ID", c.camel, e.Name, e.Name, e.Name)
	for _, f := range e.Fields {
		w.f(", %s: %s", toPascalCase(f.Name), field(f, false))
	}
	w.f("}\n}\n\n")
}

// zeroOf is the zero value of a Go type of a field.
func zeroOf(goType string) string {
	switch goType {
	case "string":
		return `""`
	case "bool":
		return "false"
	case "time.Time":
		return "time.Time{}"
	case "int32", "int64", "float32", "float64":
		return "0"
	}
	return "nil"
}

// defaultLiteral gives the Go literal of a field's default: strconv.Quote of a string default of a string field, and
// the decimal or boolean text of an integer or boolean default of an integer or boolean field. Any other default is
// refused, so no default text reaches the generated code as code.
func defaultLiteral(f types.Field) (string, error) {
	switch value := f.Default.(type) {
	case string:
		if f.Type == types.FieldTypeString {
			return strconv.Quote(value), nil
		}
	case bool:
		if f.Type == types.FieldTypeBool {
			return strconv.FormatBool(value), nil
		}
	case int:
		if f.Type == types.FieldTypeInt32 || f.Type == types.FieldTypeInt64 {
			return strconv.FormatInt(int64(value), 10), nil
		}
	case int64:
		if f.Type == types.FieldTypeInt32 || f.Type == types.FieldTypeInt64 {
			return strconv.FormatInt(value, 10), nil
		}
	case uint64:
		if f.Type == types.FieldTypeInt64 {
			return strconv.FormatUint(value, 10), nil
		}
	}
	return "", fmt.Errorf("the field %s of type %s has the default %#v, which is not a string, an integer or a boolean of its type", f.Name, f.Type, f.Default)
}

// generationDefaults refuses an entity whose generation field has a default with no Go literal (defaultLiteral),
// because the handlers write that default into the generated code.
func generationDefaults(entities []types.Entity) error {
	for _, e := range entities {
		for _, f := range e.Fields {
			if f.Name == "generation" && f.Default != nil {
				if _, err := defaultLiteral(f); err != nil {
					return fmt.Errorf("entity %s: %w", e.Name, err)
				}
			}
		}
	}
	return nil
}

// populateCreate writes the server values of a new row: the created_by and updated_by fields from the verified
// caller, each timestamp field at the time of the change and the generation from its default (ST-SUP-RC20-OC17).
func (w *runtimeHandlerWriter) populateCreate() {
	c := w.c
	for _, f := range c.entity.Fields {
		name := toPascalCase(f.Name)
		switch {
		case f.Name == "created_by" || f.Name == "updated_by":
			w.setField(f, "callerName(r.Context())")
		case f.Type == types.FieldTypeTimestamp:
			w.setField(f, "time.Now().UTC()")
		case f.Name == "generation" && f.Default != nil:
			// ValidateContext refused a default that has no Go literal (generationDefaults).
			literal, _ := defaultLiteral(f)
			w.f("\t\t%s.%s = %s\n", c.lower, name, literal)
		}
	}
}

// clearServer writes the zero value into each server-managed field of the requested value, so that a slot that runs
// before the branch of an upsert is known sees none (ST-SUP-RC20-OC13, ST-SUP-RC20-OC17).
func (w *runtimeHandlerWriter) clearServer() {
	c := w.c
	first := true
	for _, f := range c.entity.Fields {
		if !isServerManaged(f) {
			continue
		}
		if first {
			w.f("\t\tvar serverBlank %s\n", c.entity.Name)
			first = false
		}
		name := toPascalCase(f.Name)
		w.f("\t\t%s.%s = serverBlank.%s\n", c.lower, name, name)
	}
}

// populateUpdate writes the server values of a changed row: the create-only values from the stored row, the
// generation raised by one, updated_by from the verified caller and updated_time at the time of the change
// (ST-SUP-RC20-OC16, ST-SUP-RC20-OC18).
func (w *runtimeHandlerWriter) populateUpdate() {
	c := w.c
	for _, f := range c.entity.Fields {
		if !isServerManaged(f) {
			continue
		}
		name := toPascalCase(f.Name)
		switch {
		case f.Name == "updated_by":
			w.setField(f, "callerName(r.Context())")
		case f.Name == "updated_time" && f.Type == types.FieldTypeTimestamp:
			w.setField(f, "time.Now().UTC()")
		case f.Name == "generation":
			if f.Optional {
				w.f("\t\tif stored.%s != nil {\n\t\t\t%s.%s = ptrOf(*stored.%s + 1)\n\t\t}\n", name, c.lower, name, name)
			} else {
				w.f("\t\t%s.%s = stored.%s + 1\n", c.lower, name, name)
			}
		default:
			w.f("\t\t%s.%s = stored.%s\n", c.lower, name, name)
		}
	}
}

func (w *runtimeHandlerWriter) setField(f types.Field, value string) {
	if f.Optional {
		w.f("\t\t%s.%s = ptrOf(%s)\n", w.c.lower, toPascalCase(f.Name), value)
		return
	}
	w.f("\t\t%s.%s = %s\n", w.c.lower, toPascalCase(f.Name), value)
}

// before writes the before slots of an operation inside its command's fill.
func (w *runtimeHandlerWriter) before(op types.Operation, patchFields []types.Field) {
	before, _ := slotsForOp(op, w.c.slots)
	for _, sp := range before {
		switch sp.SlotName {
		case "before_delete":
			emitBeforeDeleteSlot(&w.Buffer, w.slotsAlias, sp, w.c.entity.Name, emitRuntimeSlotCheck)
		case "before_patch":
			emitBeforePatchSlot(&w.Buffer, w.slotsAlias, sp, w.c.lower, w.c.entity, patchFields, emitRuntimeSlotCheck)
		default:
			emitBeforeSlot(&w.Buffer, w.slotsAlias, sp, w.c.lower, w.c.entity, emitRuntimeSlotCheck)
		}
	}
}

// after writes, inside the fill of an operation's command, the staging of each bound after slot in the command's
// transaction: the slot request is the slot's request of the operation, as JSON, and the effect is
// <collection>.<slot> on AfterSlotsDestination for the loaded value ref. So the slot runs only after the command
// committed, never after a replay or a before slot that halted the command, and the outbox worker runs it and
// retries a failure (ST-SUP-RC20-OC34).
func (w *runtimeHandlerWriter) after(op types.Operation, entityVar, ref string, patchFields []types.Field) {
	_, after := slotsForOp(op, w.c.slots)
	if len(after) == 0 {
		return
	}
	var buf bytes.Buffer
	for _, sp := range after {
		dispatchAfterSlot(&buf, w.slotsAlias, sp, entityVar, w.c.entity, op, patchFields, emitRuntimeAfterFailure)
	}
	text := afterCall.ReplaceAllStringFunc(buf.String(), func(call string) string {
		m := afterCall.FindStringSubmatch(call)
		indent, field, request := m[1], m[2], m[3]
		slot := ""
		for _, sp := range after {
			if sp.FieldName == field {
				slot = sp.SlotName
			}
		}
		return fmt.Sprintf("%[1]sslotPayload, err := json.Marshal(%[2]s)\n%[1]sif err != nil {\n%[1]s\treturn idempotency.Response{}, err\n%[1]s}\n"+
			"%[1]sif err := ch.Effects().Stage(ctx, AfterSlotsDestination, %[3]q, %[4]s, slotPayload); err != nil {\n%[1]s\treturn idempotency.Response{}, err\n%[1]s}\n",
			indent, request, w.c.decl.Name+"."+slot, ref)
	})
	if strings.Contains(text, afterCallMarker) {
		panic("rest-api: an after slot call was not replaced by its staging")
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		if line != "" {
			w.f("\t%s", line)
		}
	}
}

// changeEvents tells whether the service publishes the change events of its collections to Kafka: it has the
// kafka-producer component, whose events the Kafka destination of the outbox delivers (ST-SUP-RC20-OC40).
func changeEvents(ctx gen.Context) bool { return ctx.PeerNamespaces[kafkaComponent] != "" }

// kafkaComponent is the component whose change events go to Kafka.
const kafkaComponent = "kafka-producer"

// The actions of a change event: an upsert is created or updated by its branch, and a patch is updated.
const (
	eventCreated = "created"
	eventUpdated = "updated"
	eventDeleted = "deleted"
)

// eventType gives the event type of a change of the entity of the collection with action: the service name, the
// entity name in lower-case words joined by hyphens, and the action, joined by periods, such as
// user-management.org-setting.updated. It is the kind of the outbox message and, when the settings declare no
// topic, the Kafka topic (ST-SUP-RP01-LC30).
func (w *runtimeHandlerWriter) eventType(action string) string {
	return eventType(w.ctx.ServiceName, w.c.entity.Name, action)
}

func eventType(service, entity, action string) string {
	var b strings.Builder
	for i, r := range entity {
		if 'A' <= r && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return service + "." + b.String() + "." + action
}

// eventTypes gives the event types that the command of a write stages.
func (w *runtimeHandlerWriter) eventTypes(op types.Operation) []string {
	switch op {
	case types.OpCreate:
		return []string{w.eventType(eventCreated)}
	case types.OpUpsert:
		return []string{w.eventType(eventCreated), w.eventType(eventUpdated)}
	case types.OpDelete:
		return []string{w.eventType(eventDeleted)}
	}
	return []string{w.eventType(eventUpdated)}
}

// event writes, inside the fill of a write's command, after its write, the staging of its change event for the
// Kafka destination in the command's transaction, for the loaded value ref of the row with the ID id: so the event is
// sent only after the command committed, never after a replay or a before slot that halted the command, and never
// lost when the broker is down (ST-SUP-RC20-OC40).
func (w *runtimeHandlerWriter) event(action, ref, id string) {
	if !w.events {
		return
	}
	w.f("\t\tif err := ch.Effects().Stage(ctx, kafkaoutbox.DestinationName, %q, %s, changeEvent(%q, %q, %s)); err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n",
		w.eventType(action), ref, w.c.entity.Name, action, id)
}

// afterEffects gives the effect names that the command of an operation stages for its after slots.
func (w *runtimeHandlerWriter) afterEffects(op types.Operation) []string {
	_, after := slotsForOp(op, w.c.slots)
	var names []string
	for _, sp := range after {
		names = append(names, w.c.decl.Name+"."+sp.SlotName)
	}
	return names
}

// deliverAfter writes the function that runs the after slots of the collection that the outbox worker delivers:
// it decodes the slot request and calls the bound operator, which a nil guard skips.
func (w *runtimeHandlerWriter) deliverAfter() bool {
	var after []collectionSlotParam
	seen := map[string]bool{}
	for _, op := range w.c.decl.Operations {
		_, a := slotsForOp(op, w.c.slots)
		for _, sp := range a {
			if !seen[sp.SlotName] {
				seen[sp.SlotName] = true
				after = append(after, sp)
			}
		}
	}
	if len(after) == 0 {
		return false
	}
	w.f("// deliverAfter runs one after slot of %s that the outbox worker delivers (ST-SUP-RC20-OC34).\n", w.c.decl.Name)
	w.f("func (h *%sHandler) deliverAfter(ctx context.Context, slot string, payload []byte) error {\n\tswitch slot {\n", w.c.pascal)
	for _, sp := range after {
		w.f("\tcase %q:\n\t\tif h.%s == nil {\n\t\t\treturn nil\n\t\t}\n\t\tvar request slots.%s\n", sp.SlotName, sp.FieldName, sp.RequestType)
		w.f("\t\tif err := json.Unmarshal(payload, &request); err != nil {\n\t\t\treturn err\n\t\t}\n\t\t_, err := h.%s.Evaluate(ctx, &request)\n\t\treturn err\n", sp.FieldName)
	}
	w.f("\t}\n\treturn errAfterSlot\n}\n\n")
	return true
}

// key writes the read of the idempotency key of a create or a patch: required when the declaration requires it,
// and the request's own key when the client sends none (ST-SUP-RC30-OC01).
func (w *runtimeHandlerWriter) key(op types.Operation) {
	required := w.keys != nil && w.keys.required[op]
	w.f("\tkey, err := idempotency.KeyFrom(r.Header, %t)\n\tif err != nil {\n\t\tkeyMissing(w, r)\n\t\treturn\n\t}\n", required)
	w.f("\trequestID := command.NewID()\n\tif key == \"\" {\n\t\tkey = requestKey(requestID)\n\t}\n")
}

// start writes the binding of the verified caller and the read of the body.
func (w *runtimeHandlerWriter) start(body bool) {
	w.f("\tb, err := command.BindIdentity(r.Context(), subjectType)\n\tif err != nil {\n\t\thandleError(w, r, Unauthorized(\"the request is not authenticated\"))\n\t\treturn\n\t}\n")
	if w.c.scope != "" {
		// The parent of a scoped collection is the path parameter of its scope field (ST-SUP-RC20-OC24).
		w.f("\tscope := canonicalID(r.PathValue(%q))\n\tif !command.ValidUUID(scope) {\n\t\thandleError(w, r, NotFound(%q, scope))\n\t\treturn\n\t}\n", w.c.scope, w.c.decl.ParentEntity())
	}
	if body {
		w.f("\tbody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBody))\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the body cannot be read\"))\n\t\treturn\n\t}\n")
	}
}

// respond writes the response of a fill.
func (w *runtimeHandlerWriter) respond(status, value string) {
	w.f("\t\tresponse, err := json.Marshal(%sPresented(%s))\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", w.c.camel, value)
	w.f("\t\treturn jsonResponse(%s, response), nil\n", status)
}

// finish writes the answer of Run.
func (w *runtimeHandlerWriter) finish(op types.Operation, resource string) {
	w.f("\tif err != nil {\n\t\th.rt.answerWrite(w, r, req, %q, %q, %q, %t, err)\n\t\treturn\n\t}\n", string(op), w.c.entity.ResourceType, w.c.entity.Name, w.c.permission(op) != "")
	w.f("\tif result.Problem != \"\" {\n\t\tanswerKey(w, r, result)\n\t\treturn\n\t}\n")
	_ = resource
}

func (w *runtimeHandlerWriter) create() {
	c, e := w.c, w.c.entity
	w.f("// Create runs the generated command create of %s through Run: the before slots run in its fill, before the\n// write; the row, its audit row and the idempotency key commit together (ST-SUP-RC20-OC37).\n", c.decl.Name)
	w.f("func (h *%sHandler) Create(w http.ResponseWriter, r *http.Request) {\n", c.pascal)
	w.start(true)
	w.key(types.OpCreate)
	w.f("\tif _, err := decode%s(body); err != nil {\n\t\th.rt.answerWrite(w, r, command.Request{}, \"create\", %q, %q, false, err)\n\t\treturn\n\t}\n", c.pascal, e.ResourceType, e.Name)
	w.f("\tfingerprint, err := idempotency.FieldFingerprint(r.Method, r.URL.Path, body, %sCreateFields)\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the body is not a JSON object\"))\n\t\treturn\n\t}\n", c.camel)
	w.f("\treq := h.rt.commandRequest(r, b, key, fingerprint, requestID, body, \"\")\n")
	w.f("\tvar persisted %s\n", e.Name)
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, decode%s, func(ctx context.Context, %s %s, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpCreate), c.pascal, c.lower, e.Name)
	if w.envelope {
		w.f("\t\tid, err := uuid.NewV7()\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n\t\t%s.ID = id.String()\n", c.lower)
	} else {
		w.f("\t\t%s.ID = command.NewID()\n", c.lower)
	}
	w.populateCreate()
	w.forceScope("scope")
	w.before(types.OpCreate, nil)
	w.f("\t\trepo, err := data.%sRepository(ch)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", e.Name)
	w.parents("")
	w.f("\t\tcreated, err := repo.Create(ctx, %s, parents...)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", w.toData(c.lower))
	w.f("\t\tpersisted = %s\n", w.fromData("created.Value()"))
	w.after(types.OpCreate, "persisted", "created", nil)
	w.event(eventCreated, "created", "persisted.ID")
	w.respond("http.StatusCreated", "persisted")
	w.f("\t})\n")
	w.finish(types.OpCreate, "")
	w.f("\twriteResult(w, result)\n}\n\n")
}

func (w *runtimeHandlerWriter) update() {
	c, e := w.c, w.c.entity
	w.f("// Update runs the generated command update of %s through Run, on the row of the path's ID.\n", c.decl.Name)
	w.f("func (h *%sHandler) Update(w http.ResponseWriter, r *http.Request) {\n\tid := canonicalID(r.PathValue(\"id\"))\n", c.pascal)
	w.start(true)
	w.f("\tif _, err := decode%s(body); err != nil {\n\t\th.rt.answerWrite(w, r, command.Request{}, \"update\", %q, %q, false, err)\n\t\treturn\n\t}\n", c.pascal, e.ResourceType, e.Name)
	w.f("\trequestID := command.NewID()\n\tfingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, body)\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the body is not a JSON object\"))\n\t\treturn\n\t}\n")
	w.f("\treq := h.rt.commandRequest(r, b, requestKey(requestID), fingerprint, requestID, body, id)\n")
	w.f("\tvar persisted %s\n", e.Name)
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, decode%s, func(ctx context.Context, %s %s, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpUpdate), c.pascal, c.lower, e.Name)
	w.lockRow()
	w.f("\t\t%s.ID = id\n", c.lower)
	w.populateUpdate()
	w.forceScope("stored." + toPascalCase(c.scope))
	w.before(types.OpUpdate, nil)
	w.parents("stored")
	w.f("\t\tupdated, err := repo.Update(ctx, from, %s, parents...)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", w.toData(c.lower))
	w.f("\t\tpersisted = %s\n", w.fromData("updated.Value()"))
	w.after(types.OpUpdate, "persisted", "updated", nil)
	w.event(eventUpdated, "updated", "persisted.ID")
	w.respond("http.StatusOK", "persisted")
	w.f("\t})\n")
	w.finish(types.OpUpdate, "id")
	w.f("\twriteResult(w, result)\n}\n\n")
}

// storedValue gives the expression of the value of the implicit field name of the stored row, for an optional
// field the value it points to or the empty text.
func (w *runtimeHandlerWriter) storedValue(name string) string {
	for _, f := range w.c.entity.Fields {
		if f.Name == name && f.Optional {
			return fmt.Sprintf("command.StateOf(stored.%s)", toPascalCase(name))
		}
	}
	return "stored." + toPascalCase(name)
}

// forceScope writes the server values of the scope field and the implicit fields of a written value: the scope
// field from scope, and each implicit field from its declared value (ST-SUP-RC20-OC23, ST-SUP-RC20-OC24).
func (w *runtimeHandlerWriter) forceScope(scope string) {
	c := w.c
	if c.scope != "" {
		w.f("\t\t%s.%s = %s\n", c.lower, toPascalCase(c.scope), scope)
	}
	for _, kv := range c.implicit {
		for _, f := range c.entity.Fields {
			if f.Name == kv[0] {
				w.setField(f, fmt.Sprintf("%q", kv[1]))
			}
		}
	}
}

// parents writes the load of the row that each ref field of the written value names, which the command runtime
// decides the reference on (ST-SUP-RP10-LC05). With stored, the value of a write of a stored row, it loads only a
// ref that the write changes, since a reference that stays needs no parent.
func (w *runtimeHandlerWriter) parents(stored string) {
	c := w.c
	w.f("\t\tvar parents []command.Ref\n")
	// Each ref already holds the lower-case form of its UUID, which the decode of the body gave (canonicalRefs).
	for _, f := range c.refs {
		name := toPascalCase(f.Name)
		v := c.lower + "." + name
		var cond, id string
		switch {
		case f.Optional && stored == "":
			cond, id = fmt.Sprintf("%s != nil && *%s != \"\"", v, v), "*"+v
		case f.Optional:
			cond, id = fmt.Sprintf("%s != nil && *%s != \"\" && (%s.%s == nil || *%s.%s != *%s)", v, v, stored, name, stored, name, v), "*"+v
		case stored == "":
			cond, id = "true", v
		default:
			cond, id = fmt.Sprintf("%s != %s.%s", v, stored, name), v
		}
		if cond == "true" {
			w.f("\t\t{\n")
		} else {
			w.f("\t\tif %s {\n", cond)
		}
		w.f("\t\t\trefs, err := data.%sRepository(ch)\n\t\t\tif err != nil {\n\t\t\t\treturn idempotency.Response{}, err\n\t\t\t}\n", f.To)
		w.f("\t\t\tparent, err := refs.Get(ctx, %s)\n\t\t\tif err != nil {\n\t\t\t\treturn idempotency.Response{}, err\n\t\t\t}\n\t\t\tparents = append(parents, parent)\n\t\t}\n", id)
	}
}

// upsert writes the upsert of the collection: one generated command that finds the row of the natural key with no
// lock; a new row is created as a create is, and decided on the rows it refers to; a stored row is locked, decided on
// the upsert permission, and updated as an update is. It answers 201 for an insert and 200 for an update
// (ST-SUP-RC20-OC13). Two upserts of one new key that run at once meet the unique key: the later one runs again as
// a conflict and updates the row (ST-SUP-RP10-LC25).
func (w *runtimeHandlerWriter) upsert() {
	c, e := w.c, w.c.entity
	w.f("// Upsert runs the generated command upsert of %s through Run: it creates the row of the natural key %s, or\n// updates the stored one, and answers 201 or 200 (ST-SUP-RC20-OC13).\n", c.decl.Name, strings.Join(c.decl.UpsertKey, ", "))
	w.f("func (h *%sHandler) Upsert(w http.ResponseWriter, r *http.Request) {\n", c.pascal)
	w.start(true)
	w.f("\tif _, err := decode%s(body); err != nil {\n\t\th.rt.answerWrite(w, r, command.Request{}, \"upsert\", %q, %q, false, err)\n\t\treturn\n\t}\n", c.pascal, e.ResourceType, e.Name)
	w.f("\trequestID := command.NewID()\n\tfingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, body)\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the body is not a JSON object\"))\n\t\treturn\n\t}\n")
	w.f("\treq := h.rt.commandRequest(r, b, requestKey(requestID), fingerprint, requestID, body, \"\")\n")
	optimistic := c.decl.Concurrency == types.ConcurrencyOptimistic
	if optimistic {
		w.f("\tifMatch, ifMatchSent, ifMatchValid := ifMatchGeneration(r)\n")
	}
	w.f("\tvar persisted %s\n", e.Name)
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, decode%s, func(ctx context.Context, %s %s, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpUpsert), c.pascal, c.lower, e.Name)
	// The before slots decide the requested value before the lookup, on both branches (ST-SUP-RC20-OC13). They see
	// no server-managed field: the branch is not known yet, so a create-time value would be wrong on the update
	// branch, and a client value is never taken (ST-SUP-RC20-OC17).
	w.clearServer()
	w.forceScope("scope")
	w.before(types.OpUpsert, nil)
	w.f("\t\trepo, err := data.%sRepository(ch)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", e.Name)
	var keys []string
	for _, key := range c.decl.UpsertLookup() {
		keys = append(keys, c.lower+"."+toPascalCase(key))
	}
	w.f("\t\tfound, err := repo.List(ctx, %sUpsertKey, 1, %s)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", c.camel, strings.Join(keys, ", "))
	w.f("\t\tif len(found) == 0 {\n")
	if optimistic {
		// An If-Match names a stored row, and there is none. The insert would be decided on edit of each required
		// parent through its reference; that decision comes first, so a caller who may not write under the parent gets
		// 404, the answer of a stored row it may not upsert, and the header tells no caller that a row exists
		// (ST-SUP-RC20-OC14, ST-SUP-RC20-OC37).
		w.f("\t\tif ifMatchSent {\n")
		for _, f := range c.refs {
			ref, ok := e.References[f.Name]
			if f.Optional || !ok || ref.Permission == "" {
				continue
			}
			v := c.lower + "." + toPascalCase(f.Name)
			w.f("\t\t\t{\n\t\t\t\trefs, err := data.%sRepository(ch)\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn idempotency.Response{}, err\n\t\t\t\t}\n", f.To)
			w.f("\t\t\t\tparent, err := refs.Get(ctx, canonicalID(%s))\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn idempotency.Response{}, err\n\t\t\t\t}\n", v)
			w.f("\t\t\t\tallowed, err := ch.Authz().Evaluate(ctx, %q, parent, nil)\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn idempotency.Response{}, err\n\t\t\t\t}\n\t\t\t\tif !allowed {\n\t\t\t\t\treturn idempotency.Response{}, command.ErrNotFound\n\t\t\t\t}\n\t\t\t}\n", ref.Permission)
		}
		w.f("\t\t\treturn idempotency.Response{}, &preconditionFailed{status: http.StatusPreconditionFailed, detail: \"the If-Match precondition names a %s that does not exist\"}\n\t\t}\n", e.Name)
	}
	w.populateCreate()
	if w.envelope {
		w.f("\t\tid, err := uuid.NewV7()\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n\t\t%s.ID = id.String()\n", c.lower)
	} else {
		w.f("\t\t%s.ID = command.NewID()\n", c.lower)
	}
	w.parents("")
	w.f("\t\tcreated, err := repo.Create(ctx, %s, parents...)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", w.toData(c.lower))
	w.f("\t\tpersisted = %s\n", w.fromData("created.Value()"))
	w.after(types.OpUpsert, "persisted", "created", nil)
	w.event(eventCreated, "created", "persisted.ID")
	w.respond("http.StatusCreated", "persisted")
	w.f("\t\t}\n")
	w.f("\t\tfrom, err := repo.GetForUpdate(ctx, found[0].ID())\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n")
	w.f("\t\t// A stored row is decided on the upsert permission; a deny is not found (ST-SUP-RP11-LC06).\n")
	w.f("\t\tallowed, err := ch.Authz().Evaluate(ctx, %q, from, nil)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n\t\tif !allowed {\n\t\t\treturn idempotency.Response{}, command.ErrNotFound\n\t\t}\n", e.Permissions[types.OpUpsert])
	w.f("\t\tstored := %s\n\t\t%s.ID = stored.ID\n", w.fromData("from.Value()"), c.lower)
	w.notInCollection()
	if optimistic {
		// The generation that the client last read must be the stored one; the update raises it by one
		// (ST-SUP-RC20-OC14, ST-SUP-RC20-OC18).
		w.f("\t\tif !ifMatchSent {\n\t\t\treturn idempotency.Response{}, &preconditionFailed{status: http.StatusPreconditionRequired, detail: \"an upsert of a stored %s needs an If-Match precondition with its generation\"}\n\t\t}\n", e.Name)
		w.f("\t\tif !ifMatchValid || int64(stored.Generation) != ifMatch {\n\t\t\treturn idempotency.Response{}, &preconditionFailed{status: http.StatusPreconditionFailed, detail: \"the If-Match precondition does not name the stored generation of the %s\"}\n\t\t}\n", e.Name)
	}
	w.populateUpdate()
	w.forceScope("scope")
	w.parents("stored")
	w.f("\t\tupdated, err := repo.Update(ctx, from, %s, parents...)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", w.toData(c.lower))
	w.f("\t\tpersisted = %s\n", w.fromData("updated.Value()"))
	w.after(types.OpUpsert, "persisted", "updated", nil)
	w.event(eventUpdated, "updated", "persisted.ID")
	w.respond("http.StatusOK", "persisted")
	w.f("\t})\n")
	w.finish(types.OpUpsert, "")
	w.f("\twriteResult(w, result)\n}\n\n")
}

// lockRow writes the locked read of the row of the path's ID in a command's fill, as stored.
func (w *runtimeHandlerWriter) lockRow() {
	e := w.c.entity
	w.f("\t\trepo, err := data.%sRepository(ch)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", e.Name)
	w.f("\t\tfrom, err := repo.GetForDelete(ctx, id)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n")
	w.f("\t\tstored := %s\n\t\t_ = stored\n", w.fromData("from.Value()"))
	w.notInCollection()
}

// notInCollection writes the check of the stored row of a write: a row of another parent, or with other implicit
// values, is not found in this collection (ST-SUP-RC20-OC23, ST-SUP-RC20-OC24).
func (w *runtimeHandlerWriter) notInCollection() {
	var checks []string
	if w.c.scope != "" {
		checks = append(checks, fmt.Sprintf("stored.%s != scope", toPascalCase(w.c.scope)))
	}
	for _, kv := range w.c.implicit {
		checks = append(checks, fmt.Sprintf("%s != %q", w.storedValue(kv[0]), kv[1]))
	}
	if len(checks) > 0 {
		w.f("\t\tif %s {\n\t\t\treturn idempotency.Response{}, command.ErrNotFound\n\t\t}\n", strings.Join(checks, " || "))
	}
}

func (w *runtimeHandlerWriter) patch() {
	c, e := w.c, w.c.entity
	fields := c.patchFields()
	w.f("// Patch runs the generated command patch of %s through Run: it changes only the fields that the request\n// gives (ST-SUP-RC20-OC15).\n", c.decl.Name)
	w.f("func (h *%sHandler) Patch(w http.ResponseWriter, r *http.Request) {\n\tid := canonicalID(r.PathValue(\"id\"))\n", c.pascal)
	w.start(true)
	w.key(types.OpPatch)
	w.f("\tif _, err := decode%sPatch(body); err != nil {\n", c.pascal)
	w.f("\t\th.rt.answerWrite(w, r, command.Request{}, \"patch\", %q, %q, false, err)\n\t\treturn\n\t}\n", e.ResourceType, e.Name)
	w.f("\tfingerprint, err := idempotency.FieldFingerprint(r.Method, r.URL.Path, body, %sPatchFields)\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the body is not a JSON object\"))\n\t\treturn\n\t}\n", c.camel)
	w.f("\treq := h.rt.commandRequest(r, b, key, fingerprint, requestID, body, id)\n")
	w.f("\tvar persisted %s\n", e.Name)
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, decode%sPatch, func(ctx context.Context, patch %sPatchRequest, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpPatch), c.pascal, c.pascal)
	w.lockRow()
	w.f("\t\t%s := stored\n", c.lower)
	w.before(types.OpPatch, fields)
	for _, f := range fields {
		name := toPascalCase(f.Name)
		if f.Optional {
			w.f("\t\tif patch.%s != nil {\n\t\t\t%s.%s = ptrOf(*patch.%s)\n\t\t}\n", name, c.lower, name, name)
		} else {
			w.f("\t\tif patch.%s != nil {\n\t\t\t%s.%s = *patch.%s\n\t\t}\n", name, c.lower, name, name)
		}
	}
	w.forceScope("stored." + toPascalCase(c.scope))
	for _, f := range e.Fields {
		switch {
		case f.Name == "updated_by":
			w.setField(f, "callerName(r.Context())")
		case f.Name == "updated_time" && f.Type == types.FieldTypeTimestamp:
			w.setField(f, "time.Now().UTC()")
		}
	}
	w.parents("stored")
	w.f("\t\tupdated, err := repo.Update(ctx, from, %s, parents...)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", w.toData(c.lower))
	w.f("\t\tpersisted = %s\n", w.fromData("updated.Value()"))
	w.after(types.OpPatch, "persisted", "updated", fields)
	w.event(eventUpdated, "updated", "persisted.ID")
	w.respond("http.StatusOK", "persisted")
	w.f("\t})\n")
	w.finish(types.OpPatch, "id")
	w.f("\twriteResult(w, result)\n}\n\n")
}

func (w *runtimeHandlerWriter) remove() {
	c := w.c
	w.f("// Delete runs the generated command delete of %s through Run, and answers 204 with no body (ST-SUP-RC20-OC32).\n", c.decl.Name)
	w.f("func (h *%sHandler) Delete(w http.ResponseWriter, r *http.Request) {\n\tid := canonicalID(r.PathValue(\"id\"))\n", c.pascal)
	w.start(false)
	w.f("\trequestID := command.NewID()\n\tfingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, nil)\n\tif err != nil {\n\t\thandleError(w, r, InternalError(\"internal error\"))\n\t\treturn\n\t}\n")
	w.f("\treq := h.rt.commandRequest(r, b, requestKey(requestID), fingerprint, requestID, nil, id)\n")
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, func([]byte) (struct{}, error) { return struct{}{}, nil }, func(ctx context.Context, _ struct{}, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpDelete))
	w.lockRow()
	w.before(types.OpDelete, nil)
	w.f("\t\tif err := repo.Delete(ctx, from); err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n")
	w.after(types.OpDelete, "", "from", nil)
	w.event(eventDeleted, "from", "id")
	w.f("\t\treturn idempotency.Response{Status: http.StatusNoContent}, nil\n\t})\n")
	w.finish(types.OpDelete, "id")
	w.f("\twriteResult(w, result)\n}\n\n")
}

func (w *runtimeHandlerWriter) read() {
	c, e := w.c, w.c.entity
	w.f("// Read runs the query read of %s through query.Run: a row that is not there and a deny are both 404\n// (ST-SUP-RP11-LC06), and a read of a pii field has its audit row (ST-SUP-RC28-OC04).\n", c.decl.Name)
	w.f("func (h *%sHandler) Read(w http.ResponseWriter, r *http.Request) {\n\tid := canonicalID(r.PathValue(\"id\"))\n", c.pascal)
	w.start(false)
	w.f("\tif !command.ValidUUID(id) {\n\t\thandleError(w, r, NotFound(%q, id))\n\t\treturn\n\t}\n", e.Name)
	q := w.commandVar(types.OpRead)
	w.f("\tvalue, err := query.Run(r.Context(), h.rt.Queries, %s, h.rt.boundQuery(r.Context(), b, %s), id, func(ctx context.Context, id string, qh *query.Handle[struct{}]) (%s, error) {\n", q, q, e.Name)
	w.f("\t\trepo, err := query.Repo(qh, data.Entity%sView())\n\t\tif err != nil {\n\t\t\treturn %s{}, err\n\t\t}\n", e.Name, e.Name)
	w.f("\t\tloaded, err := repo.Get(ctx, id)\n\t\tif err != nil {\n\t\t\treturn %s{}, err\n\t\t}\n", e.Name)
	// A row of another parent, or with other implicit values, is not found in this collection (ST-SUP-RC20-OC23,
	// ST-SUP-RC20-OC24).
	var checks []string
	if c.scope != "" {
		checks = append(checks, fmt.Sprintf("loaded.Value().%s != scope", toPascalCase(c.scope)))
	}
	for _, kv := range c.implicit {
		checks = append(checks, fmt.Sprintf("loaded.Value().%s != %q", toPascalCase(kv[0]), kv[1]))
	}
	if len(checks) > 0 {
		w.f("\t\tif %s {\n\t\t\treturn %s{}, query.ErrNotFound\n\t\t}\n", strings.Join(checks, " || "), e.Name)
	}
	w.f("\t\treturn %sFromView(loaded.Value()), nil\n\t})\n", c.camel)
	w.f("\tif err != nil {\n\t\tanswerRead(w, r, %q, id, err)\n\t\treturn\n\t}\n", e.Name)
	w.f("\tw.Header().Set(\"Content-Type\", \"application/json\")\n\t_ = json.NewEncoder(w).Encode(%sPresented(value))\n}\n\n", c.camel)
}

func (w *runtimeHandlerWriter) list() {
	c, e := w.c, w.c.entity
	q := w.commandVar(types.OpList)
	w.f("// %sPage is one page of a list of %s.\ntype %sPage struct {\n\titems []%s\n\tnext  query.Cursor\n\tmore  bool\n}\n\n", c.camel, c.decl.Name, c.camel, e.Name)
	w.f("// List runs the query list of %s through query.Run: it pages by the sealed cursor of the query runtime, at\n// most pageSize values a page, and keeps only the rows that the caller may see (ST-SUP-RP11-LC07, ST-SUP-RP11-LC15).\n", c.decl.Name)
	w.f("func (h *%sHandler) List(w http.ResponseWriter, r *http.Request) {\n", c.pascal)
	w.start(false)
	w.f("\tlimit := 100\n\tif text := r.URL.Query().Get(\"pageSize\"); text != \"\" {\n\t\tn, err := strconv.Atoi(text)\n\t\tif err != nil || n < 1 {\n\t\t\thandleError(w, r, BadRequest(\"pageSize must be a whole number of at least 1\"))\n\t\t\treturn\n\t\t}\n\t\tlimit = min(n, query.MaxLimit)\n\t}\n")
	var fields []string
	// A pii or secret field orders no list: the order would tell its value (ST-SUP-RP11-LC20).
	for _, f := range e.Fields {
		if f.Sensitivity == "" {
			fields = append(fields, fmt.Sprintf("%q: true", f.Name))
		}
	}
	w.f("\torder, err := listOrder(r.URL.Query().Get(\"orderBy\"), map[string]bool{%s})\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(err.Error()))\n\t\treturn\n\t}\n", strings.Join(fields, ", "))
	// A field selection names declared fields; a secret field is cleared in each presented value (ST-SUP-RC20-OC12).
	var declared []string
	for _, f := range e.Fields {
		declared = append(declared, fmt.Sprintf("%q: true", f.Name))
	}
	w.f("\tfields, err := listFields(r.URL.Query().Get(\"fields\"), map[string]bool{%s})\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(err.Error()))\n\t\treturn\n\t}\n", strings.Join(declared, ", "))
	// The filter of the list: the conditions of the search, then the scope field and the implicit fields, which
	// the search may not name again (ST-SUP-RP11-LC20, ST-SUP-RC20-OC23, ST-SUP-RC20-OC24).
	own := len(c.implicit)
	if c.scope != "" {
		own++
	}
	w.f("\twhere, err := query.ParseSearch(r.URL.Query().Get(\"search\"), %sSearchFields, query.MaxFilter-%d)\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the search is not valid\"))\n\t\treturn\n\t}\n", c.camel, own)
	// A renamed field is named in the API by its first name and stored in the column of its declared name
	// (ST-GEN-UC12-R04): the order and the search name it by the API name, and the query runtime reads columns.
	var renamed []string
	for _, f := range e.Fields {
		if f.Column != "" {
			renamed = append(renamed, fmt.Sprintf("%q: %q", f.Name, f.Column))
		}
	}
	if len(renamed) > 0 {
		w.f("\tcolumns := map[string]string{%s}\n\tfor i := range order {\n\t\tif column, ok := columns[order[i].Field]; ok {\n\t\t\torder[i].Field = column\n\t\t}\n\t}\n\tfor i := range where {\n\t\tif column, ok := columns[where[i].Field]; ok {\n\t\t\twhere[i].Field = column\n\t\t}\n\t}\n", strings.Join(renamed, ", "))
	}
	if c.scope != "" {
		w.f("\twhere = append(where, query.Equal{Field: %q, Value: scope})\n", c.scope)
	}
	for _, kv := range c.implicit {
		w.f("\twhere = append(where, query.Equal{Field: %q, Value: %q})\n", kv[0], kv[1])
	}
	w.f("\tvar after query.Cursor\n\tif text := r.URL.Query().Get(\"cursor\"); text != \"\" {\n\t\tif after, err = h.rt.Queries.ParseCursor(%s, b.Tenant, b.Principal, text); err != nil {\n\t\t\thandleError(w, r, BadRequest(\"the cursor is not valid\"))\n\t\t\treturn\n\t\t}\n\t}\n", q)
	w.f("\tpage, err := query.Run(r.Context(), h.rt.Queries, %s, h.rt.boundQuery(r.Context(), b, %s), query.ListRequest{Limit: limit, After: after, Order: order, Where: where}, func(ctx context.Context, request query.ListRequest, qh *query.Handle[struct{}]) (%sPage, error) {\n", q, q, c.camel)
	w.f("\t\trepo, err := query.Repo(qh, data.Entity%sView())\n\t\tif err != nil {\n\t\t\treturn %sPage{}, err\n\t\t}\n", e.Name, c.camel)
	w.f("\t\tresult, err := repo.List(ctx, request)\n\t\tif err != nil {\n\t\t\treturn %sPage{}, err\n\t\t}\n", c.camel)
	w.f("\t\tpage := %sPage{next: result.Next, more: result.More}\n\t\tfor _, item := range result.Items {\n\t\t\tpage.items = append(page.items, %sFromView(item.Value()))\n\t\t}\n\t\treturn page, nil\n\t})\n", c.camel, c.camel)
	w.f("\tif isBadCursor(err) {\n\t\thandleError(w, r, BadRequest(\"the cursor is not valid\"))\n\t\treturn\n\t}\n")
	w.f("\tif errors.Is(err, query.ErrBadRequest) {\n\t\thandleError(w, r, BadRequest(\"the list request is not valid\"))\n\t\treturn\n\t}\n")
	w.f("\tif err != nil {\n\t\thandleError(w, r, InternalError(\"internal error\"))\n\t\treturn\n\t}\n")
	w.f("\titems := make([]any, 0, len(page.items))\n\tfor _, item := range page.items {\n\t\titems = append(items, selectFields(%sPresented(item), fields))\n\t}\n", c.camel)
	w.f("\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	if w.envelope {
		w.f("\tresult := map[string]any{\"kind\": %q, \"size\": len(items), \"items\": items}\n", e.Name+"List")
		w.f("\tif page.more {\n\t\tnext, err := h.rt.Queries.CursorText(%s, b.Tenant, b.Principal, page.next)\n\t\tif err != nil {\n\t\t\thandleError(w, r, InternalError(\"internal error\"))\n\t\t\treturn\n\t\t}\n\t\tresult[\"next\"] = next\n\t}\n", q)
		w.f("\t_ = json.NewEncoder(w).Encode(result)\n}\n\n")
	} else {
		w.f("\t_ = json.NewEncoder(w).Encode(items)\n}\n\n")
	}
}
