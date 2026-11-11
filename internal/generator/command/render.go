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
	w.f("\t\"context\"\n\t\"crypto/rand\"\n\t\"encoding/hex\"\n")
	if m.usesTime() {
		w.f("\t\"time\"\n")
	}
	w.f("\n")
	if m.usesAudit() {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/audit\"\n")
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
			w.f("var %s = &Refusal{command: %q, name: %q, status: %d, code: %q}\n\n", r.goName, c.decl.Name, r.name, r.status, r.code)
		}
	}
	w.f("%s%s", refusalHelpers, entityHelpers)
	return w.Bytes()
}

// EntityFile gives a Go file of the package pkg that holds the repository declarations on the command runtime of the
// named entities, as the command contract holds them (ST-SUP-RP10-LC02): for each entity its row type, its loaded
// type, its command.Entity declaration with the statements of ST-SUP-RP10-LC23 and ST-SUP-RP10-LC24, and its
// repository accessor, with the helpers that they use. Each statement takes the tenant column tenantKey as $1. It is
// for a generator that declares commands of its own, such as the rest-api generator for generated CRUD
// (ST-SUP-RC20-OC37), so the entity code has one source. It refuses an entity that a command cannot write.
func EntityFile(pkg string, names []string, entities []types.Entity, tenantKey string) ([]byte, error) {
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
		p, err := planEntity(e, byName, tenantKey)
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
	w.f("\t\"crypto/rand\"\n\t\"encoding/hex\"\n")
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
	w.f("%s", entityHelpers)
	source, err := format.Source(w.Bytes())
	if err != nil {
		return nil, fmt.Errorf("command: formatting the entity declarations: %w", err)
	}
	return source, nil
}

// refusalHelpers are the refusal helpers of the contract package, and entityHelpers the helpers of the entity
// declarations. They are the same for every declaration; ST-SUP-Q74 asks whether they move into the runtime library
// (ST-STD-RUNTIME-R05).
const refusalHelpers = `// Refusal is a declared refusal of a command (ST-SUP-RC32-OC09). Only the values of this package exist: a fill
// returns one of them, and the handler answers it with its status and code. Its fields are not exported, so a fill
// cannot make one of its own.
type Refusal struct {
	command, name, code string
	status              int
}

// Error names the command and the refusal, and no value of a request or a row.
func (r *Refusal) Error() string { return "command " + r.command + ": refused: " + r.name }

// Command is the command that declares the refusal.
func (r *Refusal) Command() string { return r.command }

// Name is the declared name of the refusal.
func (r *Refusal) Name() string { return r.name }

// Status is the HTTP status of the refusal.
func (r *Refusal) Status() int { return r.status }

// Code is the fixed code of the refusal.
func (r *Refusal) Code() string { return r.code }

`

const entityHelpers = `// mustEntity gives a checked repository declaration. The generator checked each declaration, so it does not fail.
func mustEntity[T any](e *command.Entity[T], err error) *command.Entity[T] {
	if err != nil {
		panic(err)
	}
	return e
}

// NewID gives a new random UUID, the key of a new row and the ID of a request.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ValidUUID tells whether id is a UUID in its text form, the key type of every entity.
func ValidUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// clonePointer gives a copy of the value that p points to.
func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// stateOf gives the value that p points to for an audit state, or nil when p is nil.
func stateOf[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// equalPointer tells whether two pointers point to equal values or are both nil.
func equalPointer[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
`

// renderHandlers writes the handlers package: the command declarations with their permission and resource, the
// entities of the runtime, and one HTTP handler and one gRPC handler for each command.
func renderHandlers(m *model) []byte {
	w := &writer{}
	w.f("// Package %s holds the generated handlers of the commands of the service (ST-SUP-RC32-OC02): one HTTP handler\n", handlersFolder)
	w.f("// and one gRPC handler for each declared command, each bound to the one fill that the command names.\n")
	w.f("package %s\n\nimport (\n\t\"context\"\n\t\"net/http\"\n", handlersFolder)
	if m.hasLockTimeout() {
		w.f("\t\"time\"\n")
	}
	w.f("\n")
	if m.hasRelations() {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/authz\"\n")
	}
	w.f("\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n\n\tcommands %q\n", m.contractImport)
	if m.probeImport != "" {
		w.f("\tprobes %q\n", m.probeImport)
	}
	w.f(")\n\n")
	w.f("// subjectType is the subject type of the schema that a verified identity maps to.\nconst subjectType = %q\n\n", m.subjectType)
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

// renderCommandHandlers writes the command declaration and the handlers of one command.
func renderCommandHandlers(w *writer, m *model, c *command) {
	lower := strings.ToLower(c.goName[:1]) + c.goName[1:]
	probe := m.probeType(c)
	w.f("// %sCommand is the declaration of the command %s (ST-SUP-RP10-LC31, ST-SUP-RC32-OC10).\n", lower, c.decl.Name)
	w.f("var %sCommand = mustCommand(command.NewCommand(command.CommandSpec[%s]{\n", lower, probe)
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
			w.f("{Name: %q, MustLand: true, Idempotent: true, Effects: []string{", d)
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
			w.f("mustRelation(%q)", r)
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
	token := fmt.Sprintf("SELECT authz_token FROM %s WHERE %s = $1 AND id = $2::%s FOR SHARE", r.table, m.tenantKey, r.keyCast)
	w.f("\tPermission: %q,\n\tResource: &command.Resource{Type: %q, Token: %q, ValidID: commands.ValidUUID},\n}))\n\n", c.decl.Permission, r.resourceType, token)
	spec := fmt.Sprintf("spec[*commands.%s, *commands.%s, %s]", c.request.name, c.response.name, probe)
	w.f("// %sSpec is what varies with the declaration of %s.\nfunc %sSpec(fill commands.%sFill, options Options) %s {\n", lower, c.decl.Name, lower, c.goName, spec)
	w.f("\treturn %s{\n\t\tname: %q,\n\t\tcommand: %sCommand,\n\t\tresourceType: %q,\n", spec, c.decl.Name, lower, r.resourceType)
	status := "http.StatusOK"
	if c.creates {
		status = "http.StatusCreated"
	}
	w.f("\t\tstatus: %s,\n", status)
	w.f("\t\tnewRequest: func() *commands.%s { return &commands.%s{} },\n", c.request.name, c.request.name)
	w.f("\t\tnewResponse: func() *commands.%s { return &commands.%s{} },\n", c.response.name, c.response.name)
	w.f("\t\tpath: func(r *http.Request, request *commands.%s) error {\n", c.request.name)
	for _, p := range c.params {
		w.f("\t\t\tif err := setPath(&request.%s, r.PathValue(%q)); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n", p.field.goName, p.name)
	}
	w.f("\t\t\treturn nil\n\t\t},\n")
	w.f("\t\tresource: func(request *commands.%s) string { return request.%s },\n", c.request.name, c.member.goName)
	w.f("\t\tfill: func(ctx context.Context, request *commands.%s, h *command.Handle[%s]) (*commands.%s, error) {\n\t\t\treturn fill(ctx, request, h)\n\t\t},\n", c.request.name, probe, c.response.name)
	w.f("\t\thasFill: fill != nil,\n")
	w.f("\t\trefusals: []*commands.Refusal{")
	for i, ref := range c.refusals {
		if i > 0 {
			w.f(", ")
		}
		w.f("commands.%s", ref.goName)
	}
	w.f("},\n\t\toptions: options,\n\t}\n}\n\n")
	w.f("// %s gives the HTTP handler of the command %s at %s: it calls the one fill %s through Run.\n", c.goName, c.decl.Name, c.pattern, c.decl.Fill[0])
	w.f("func %s(rt *command.Runtime, fill commands.%sFill, options Options) (http.Handler, error) {\n\treturn serveHTTP(rt, %sSpec(fill, options))\n}\n\n", c.goName, c.goName, lower)
	w.f("// %sRPC gives the gRPC handler of the command %s: the same fill, handle, transaction, permission, audit and\n// idempotency as its HTTP handler, with the idempotency key from the idempotency-key request metadata.\n", c.goName, c.decl.Name)
	w.f("func %sRPC(rt *command.Runtime, fill commands.%sFill, options Options) (func(context.Context, *commands.%s) (*commands.%s, error), error) {\n\treturn serveRPC(rt, %sSpec(fill, options))\n}\n\n", c.goName, c.goName, c.request.name, c.response.name, lower)
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
