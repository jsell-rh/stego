package command

import (
	"fmt"
	"go/format"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

// probeType is the typed probe of a command: the domain probe of the command when the service has the
// domain-probe component (ST-SUP-RC34-OC04), and struct{} otherwise.
func (m *model) probeType(c *command) string {
	if m.probeImport == "" {
		return "struct{}"
	}
	return "probes." + c.goName + "Probe"
}

// usesTime tells whether an entity field is a timestamp.
func (m *model) usesTime() bool {
	for _, e := range m.entities {
		for _, f := range e.fields {
			if strings.Contains(f.goType, "time.Time") {
				return true
			}
		}
	}
	return false
}

func (m *model) usesBytes() bool {
	for _, e := range m.entities {
		for _, f := range e.fields {
			if f.slice {
				return true
			}
		}
	}
	return false
}

// usesJSON tells whether an entity has a jsonb field, whose audit state is its JSON value.
func (m *model) usesJSON() bool {
	for _, e := range m.entities {
		for _, f := range e.fields {
			if f.decl.Type == types.FieldTypeJsonb {
				return true
			}
		}
	}
	return false
}

func (m *model) usesAudit() bool {
	for _, e := range m.entities {
		for _, f := range e.fields {
			if f.sensitivity == "pii" || f.sensitivity == "secret" {
				return true
			}
		}
	}
	return false
}

// renderContract writes the command slot contract package, which a command fill imports (ST-SUP-RC32-OC01): the
// entity types and their repositories, the command handles, the fill types and the typed refusals. The request and
// the response messages are in the same package, in the files that protoc-gen-go writes.
func renderContract(m *model) []byte {
	w := &writer{}
	w.f("// Package %s is the command slot contract of the service (ST-SUP-RC32-OC01): for each declared command, the fill\n", m.pkg)
	w.f("// type, which takes the decoded request message and one command handle only, the command handle, the typed\n")
	w.f("// refusals and the repositories of the entities that the command writes. A command fill imports this package\n")
	w.f("// only; the handlers that call the fills are in the package %s.\n", m.handlersImport)
	w.f("package %s\n\nimport (\n", m.pkg)
	if m.usesBytes() {
		w.f("\t\"bytes\"\n")
	}
	w.f("\t\"context\"\n")
	if m.usesJSON() {
		w.f("\t\"encoding/json\"\n")
	}
	if m.usesTime() {
		w.f("\t\"time\"\n")
	}
	w.f("\n")
	if m.usesAudit() {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/audit\"\n")
	}
	if m.hasRelations() {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/authz\"\n")
	}
	w.f("\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n")
	if m.probeImport != "" {
		w.f("\n\tprobes %q\n", m.probeImport)
	}
	w.f(")\n\n")
	for _, e := range m.entities {
		renderEntity(w, e, m.tenantKey)
	}
	for _, c := range m.commands {
		w.f("// %sHandle is the command handle that the fill of %s gets: it reaches only the entities, relations and\n// effect destinations that the command declares.\n", c.goName, c.decl.Name)
		w.f("type %sHandle = command.Handle[%s]\n\n", c.goName, m.probeType(c))
		w.f("// %sFill is the command slot of %s: the fill gets the decoded request message and one command handle, and\n", c.goName, c.decl.Name)
		w.f("// nothing else that reaches storage, authorization, audit, the outbox or the network (ST-SUP-RC32-OC01). It\n// returns the response message, or an error: one of the declared refusals of the command, or any other error,\n// which the handler answers with 500 and the code internal.\n")
		w.f("type %sFill func(ctx context.Context, request *%s, h *%sHandle) (*%s, error)\n\n", c.goName, c.request.name, c.goName, c.response.name)
		for _, r := range c.refusals {
			w.f("// %s is the refusal %s of %s: the handler answers it with %d and the code %s.\n", r.goName, r.name, c.decl.Name, r.status, r.code)
			w.f("var %s = command.NewRefusal(%q, %q, %d, %q)\n\n", r.goName, c.decl.Name, r.name, r.status, r.code)
		}
		for _, h := range c.helpers {
			renderRelationHelper(w, c, h)
		}
	}
	w.f("%s", refusalHelpers)
	return w.Bytes()
}

// renderRelationHelper writes the typed helpers of one relation that the command declares (ST-SUP-RC32-OC13): a
// fill relates or unrelates a subject to the row of a loaded value through them, and never builds a resource ID or
// names a relation that the command does not declare. The tuple's resource is the tenant-qualified resource of the
// row in the handle's tenant (ST-SUP-RC29-OC22); the command runtime decides the command's permission on the row
// first, and the relay writes the tuple after the commit, in the revocation of the transaction (ST-SUP-RP10-LC05).
func renderRelationHelper(w *writer, c *command, h relationHelper) {
	for _, op := range []struct{ name, verb, call string }{{h.relate, "relates", "Relate"}, {h.unrelate, "unrelates", "Unrelate"}} {
		w.f("// %s %s the subject subjectType:subjectID to the row of on through the relation %s#%s that %s declares:\n", op.name, op.verb, h.resourceType, h.relation, c.decl.Name)
		w.f("// it enqueues the tuple for the relay in the command's transaction. on is a loaded %s of this handle.\n", h.entity.decl.Name)
		w.f("func %s(ctx context.Context, h *%sHandle, on Loaded%s, subjectType, subjectID string) error {\n", op.name, c.goName, h.entity.goName)
		w.f("\tif on == nil {\n\t\treturn command.ErrNoResource\n\t}\n")
		w.f("\tresource, err := authz.NewResource(h.Tenant(), %q, on.ID())\n\tif err != nil {\n\t\treturn err\n\t}\n", h.resourceType)
		w.f("\t_, err = h.Authz().%s(ctx, on, authz.Tuple{Resource: resource, Relation: %q, Subject: authz.Subject{Type: subjectType, ID: subjectID}})\n\treturn err\n}\n\n", op.call, h.relation)
	}
}

// EntityFile gives a Go file of the package pkg that holds the repository declarations on the command runtime of the
// named entities, as the command contract holds them (ST-SUP-RP10-LC02): for each entity its row type, its loaded
// type, its command.Entity declaration with the statements of ST-SUP-RP10-LC23 and ST-SUP-RP10-LC24, and its
// repository accessor, with the helpers that they use. Each statement takes the tenant column tenantKey as $1. It is
// for a generator that declares commands of its own, such as the rest-api generator for generated CRUD
// (ST-SUP-RC20-OC37), so the entity code has one source. It refuses an entity that a command cannot write.
func EntityFile(pkg string, names []string, entities []types.Entity, tenantKey string, relations map[string][]string) ([]byte, error) {
	byName := map[string]types.Entity{}
	for _, e := range entities {
		byName[e.Name] = e
	}
	m := &model{tenantKey: tenantKey}
	for _, name := range names {
		e, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("command: the entity %s is not declared", name)
		}
		p, err := planEntity(e, byName, tenantKey, relations)
		if err != nil {
			return nil, fmt.Errorf("command: the entity %s: %w", name, err)
		}
		m.entities = append(m.entities, p)
	}
	w := &writer{}
	w.f("package %s\n\nimport (\n", pkg)
	if m.usesBytes() {
		w.f("\t\"bytes\"\n")
	}
	if m.usesJSON() {
		w.f("\t\"encoding/json\"\n")
	}
	if m.usesTime() {
		w.f("\t\"time\"\n")
	}
	w.f("\n")
	if m.usesAudit() {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/audit\"\n")
	}
	w.f("\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n)\n\n")
	for _, e := range m.entities {
		renderEntity(w, e, tenantKey)
	}
	source, err := format.Source(w.Bytes())
	if err != nil {
		return nil, fmt.Errorf("command: formatting the entity declarations: %w", err)
	}
	return source, nil
}

// refusalHelpers name the refusal type of the runtime in the contract package (ST-SUP-RP10-LC33): a fill returns
// one of the declared values, which Serve knows by identity.
const refusalHelpers = `// Refusal is a declared refusal of a command (ST-SUP-RC32-OC09): the runtime's type. Only the values of this
// package are declared: a fill returns one of them, and the handler answers it with its status and code.
type Refusal = command.Refusal
`

// renderHandlers writes the handlers package: the command declarations with their permission and resource, the
// entities of the runtime, and one ServeSpec for each command, which command.Serve and command.ServeRPC serve
// (ST-SUP-RP10-LC33). Only what varies with the declaration is emitted (ST-STD-RUNTIME-R05).
func renderHandlers(m *model) []byte {
	w := &writer{}
	w.f("// Package %s holds the generated handlers of the commands of the service (ST-SUP-RC32-OC02): one HTTP handler\n", handlersFolder)
	w.f("// and one gRPC handler for each declared command, each bound to the one fill that the command names, served by\n// command.Serve and command.ServeRPC of the runtime library.\n")
	w.f("package %s\n\nimport (\n\t\"context\"\n\t\"net/http\"\n\t\"time\"\n\n", handlersFolder)
	if m.hasRelations() {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/authz\"\n")
	}
	w.f("\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n")
	if m.probeImport == "" {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/probe\"\n")
	}
	w.f("\n\tcommands %q\n", m.contractImport)
	if m.probeImport != "" {
		w.f("\tprobes %q\n", m.probeImport)
	}
	w.f(")\n\n")
	w.f("// subjectType is the subject type of the schema that a verified identity maps to.\nconst subjectType = %q\n\n", m.subjectType)
	w.f("// Options are the settings of the command handlers.\ntype Options struct {\n\t// Retention is how long a completed idempotency key is kept; zero is the runtime's default.\n\tRetention time.Duration\n}\n\n")
	w.f("// Entities are the entities that the commands write, for the Entities of the command runtime's Config.\nfunc Entities() []command.Declared {\n\treturn []command.Declared{")
	for i, e := range m.entities {
		if i > 0 {
			w.f(", ")
		}
		w.f("commands.%sEntity", e.goName)
	}
	w.f("}\n}\n\n")
	w.f("// Fills holds the one fill of each command.\ntype Fills struct {\n")
	for _, c := range m.commands {
		w.f("\t// %s is the fill %s of the command %s.\n\t%s commands.%sFill\n", c.goName, c.decl.Fill[0], c.decl.Name, c.goName, c.goName)
	}
	w.f("}\n\n")
	w.f("// Register registers the HTTP handler of each command on mux at the command's route.\nfunc Register(mux *http.ServeMux, rt *command.Runtime, fills Fills, options Options) error {\n")
	for _, c := range m.commands {
		w.f("\t{\n\t\th, err := %s(rt, fills.%s, options)\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n\t\tmux.Handle(%q, h)\n\t}\n", c.goName, c.goName, c.pattern)
	}
	w.f("\treturn nil\n}\n\n")
	if m.probeImport == "" {
		w.f("// noProbe is the typed probe of a service that has no domain probes.\nfunc noProbe(*probe.Probe) struct{} { return struct{}{} }\n\n")
	}
	for _, c := range m.commands {
		renderCommandHandlers(w, m, c)
	}
	return w.Bytes()
}

func (m *model) hasLockTimeout() bool {
	for _, c := range m.commands {
		if c.lock != "" {
			return true
		}
	}
	return false
}

func (m *model) hasRelations() bool {
	for _, c := range m.commands {
		if len(c.relations) > 0 {
			return true
		}
	}
	return false
}

// renderCommandHandlers writes the command declaration, the ServeSpec and the handlers of one command.
func renderCommandHandlers(w *writer, m *model, c *command) {
	lower := strings.ToLower(c.goName[:1]) + c.goName[1:]
	probe := m.probeType(c)
	w.f("// %sCommand is the declaration of the command %s (ST-SUP-RP10-LC31, ST-SUP-RC32-OC10).\n", lower, c.decl.Name)
	w.f("var %sCommand = command.Must(command.NewCommand(command.CommandSpec[%s]{\n", lower, probe)
	w.f("\tName: %q,\n\tWrites: []command.Declared{", c.decl.Name)
	for i, e := range c.writes {
		if i > 0 {
			w.f(", ")
		}
		w.f("commands.%sEntity", e.goName)
	}
	w.f("},\n")
	if len(c.effects) > 0 {
		w.f("\tDestinations: []command.Destination{")
		for i, d := range c.effects {
			if i > 0 {
				w.f(", ")
			}
			declared := m.destinations[d]
			w.f("{Name: %q, MustLand: %t, Idempotent: %t, Effects: []string{", d, declared.mustLand, declared.idempotent)
			for j, name := range c.effectNames[d] {
				if j > 0 {
					w.f(", ")
				}
				w.f("%q", name)
			}
			w.f("}}")
		}
		w.f("},\n")
	}
	if len(c.relations) > 0 {
		w.f("\tRelations: []authz.Relation{")
		for i, r := range c.relations {
			if i > 0 {
				w.f(", ")
			}
			w.f("command.Must(authz.ParseRelation(%q))", r)
		}
		w.f("},\n")
	}
	w.f("\tRetryOnConflict: %d,\n", c.retry)
	if c.lock != "" {
		w.f("\tLockTimeout: %s,\n", c.lock)
	}
	if m.probeImport != "" {
		w.f("\tProbe: probes.New%sProbe,\n", c.goName)
	} else {
		w.f("\tProbe: noProbe,\n")
	}
	r := c.resource
	// The table is named with its schema, so no relation of the same name earlier on the search path stands in for it.
	token := fmt.Sprintf("SELECT authz_token FROM public.%s WHERE %s = $1 AND id = $2::%s FOR SHARE", r.table, m.tenantKey, r.keyCast)
	w.f("\tPermission: %q,\n\tResource: &command.Resource{Type: %q, Token: %q, ValidID: command.ValidUUID},\n}))\n\n", c.decl.Permission, r.resourceType, token)
	spec := fmt.Sprintf("command.ServeSpec[*commands.%s, *commands.%s, %s]", c.request.name, c.response.name, probe)
	w.f("// %sSpec is what varies with the declaration of %s.\nfunc %sSpec(fill commands.%sFill, options Options) %s {\n", lower, c.decl.Name, lower, c.goName, spec)
	w.f("\tspec := %s{\n\t\tCommand: %sCommand,\n\t\tResourceType: %q,\n\t\tSubjectType: subjectType,\n", spec, lower, r.resourceType)
	status := "http.StatusOK"
	if c.creates {
		status = "http.StatusCreated"
	}
	w.f("\t\tStatus: %s,\n", status)
	w.f("\t\tNewRequest: func() *commands.%s { return &commands.%s{} },\n", c.request.name, c.request.name)
	w.f("\t\tNewResponse: func() *commands.%s { return &commands.%s{} },\n", c.response.name, c.response.name)
	if len(c.params) > 0 {
		w.f("\t\tPath: func(r *http.Request, request *commands.%s) error {\n", c.request.name)
		for _, p := range c.params {
			w.f("\t\t\tif err := command.SetPath(&request.%s, r.PathValue(%q)); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n", p.field.goName, p.name)
		}
		w.f("\t\t\treturn nil\n\t\t},\n")
	}
	w.f("\t\tResource: func(request *commands.%s) string { return request.%s },\n", c.request.name, c.member.goName)
	w.f("\t\tRefusals: []*command.Refusal{")
	for i, ref := range c.refusals {
		if i > 0 {
			w.f(", ")
		}
		w.f("commands.%s", ref.goName)
	}
	w.f("},\n\t\tKeyRequired: true,\n\t\tRetention: options.Retention,\n\t}\n")
	w.f("\tif fill != nil {\n\t\tspec.Fill = func(ctx context.Context, request *commands.%s, h *command.Handle[%s]) (*commands.%s, error) {\n\t\t\treturn fill(ctx, request, h)\n\t\t}\n\t}\n\treturn spec\n}\n\n", c.request.name, probe, c.response.name)
	w.f("// %s gives the HTTP handler of the command %s at %s: it calls the one fill %s through command.Serve.\n", c.goName, c.decl.Name, c.pattern, c.decl.Fill[0])
	w.f("func %s(rt *command.Runtime, fill commands.%sFill, options Options) (http.Handler, error) {\n\treturn command.Serve(rt, %sSpec(fill, options))\n}\n\n", c.goName, c.goName, lower)
	w.f("// %sRPC gives the gRPC handler of the command %s: the same fill, handle, transaction, permission, audit and\n// idempotency as its HTTP handler, with the idempotency key from the idempotency-key request metadata.\n", c.goName, c.decl.Name)
	w.f("func %sRPC(rt *command.Runtime, fill commands.%sFill, options Options) (func(context.Context, *commands.%s) (*commands.%s, error), error) {\n\treturn command.ServeRPC(rt, %sSpec(fill, options))\n}\n\n", c.goName, c.goName, c.request.name, c.response.name, lower)
}

// renderFills writes the binding of each command to the one fill that it names (ST-SUP-RC32-OC02): the fill folder
// fills/<fill> of the project is a Go package whose New gives the command's fill type, so a fill of another slot
// does not compile here.
func renderFills(m *model) []byte {
	w := &writer{}
	w.f("package %s\n\nimport (\n", handlersFolder)
	seen := map[string]bool{}
	for _, c := range m.commands {
		name := c.decl.Fill[0]
		if !seen[name] {
			seen[name] = true
			w.f("\t%s %q\n", fillAlias(name), m.ctx.ModuleName+"/fills/"+name)
		}
	}
	w.f(")\n\n// BoundFills gives the one fill that each command names, from its fill folder.\nfunc BoundFills() Fills {\n\treturn Fills{\n")
	for _, c := range m.commands {
		w.f("\t\t%s: %s.New(),\n", c.goName, fillAlias(c.decl.Fill[0]))
	}
	w.f("\t}\n}\n")
	return w.Bytes()
}

// fillAlias is the import alias of a fill package.
func fillAlias(name string) string {
	alias := mixedCase(name)
	return "fill" + alias
}

// KeyListRead gives the Read statement of a command.ListSpec that finds the row of the entity named name whose
// fields keys hold the values $3, $4 and on, in the tenant $1, at most $2 rows: the lookup of an upsert by its
// natural key (ST-SUP-RC20-OC13). It returns the columns of the entity's Scan, as each statement of EntityFile does.
func KeyListRead(name string, entities []types.Entity, tenantKey string, keys []string) (string, error) {
	byName := map[string]types.Entity{}
	for _, e := range entities {
		byName[e.Name] = e
	}
	decl, ok := byName[name]
	if !ok {
		return "", fmt.Errorf("command: the entity %s is not declared", name)
	}
	e, err := planEntity(decl, byName, tenantKey, nil)
	if err != nil {
		return "", fmt.Errorf("command: the entity %s: %w", name, err)
	}
	where := []string{fmt.Sprintf("%s = $1", quote(tenantKey))}
	for i, key := range keys {
		found := false
		for _, f := range e.fields {
			found = found || f.decl.Name == key
		}
		if !found {
			return "", fmt.Errorf("command: the entity %s has no field %s of its upsert key", name, key)
		}
		where = append(where, fmt.Sprintf("%s = $%d", quote(key), i+3))
	}
	return fmt.Sprintf("SELECT %s FROM public.%s WHERE %s ORDER BY \"id\" LIMIT $2", e.columns(), quote(e.table), strings.Join(where, " AND ")), nil
}
