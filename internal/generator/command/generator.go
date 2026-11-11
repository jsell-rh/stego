// Package command generates the per-service code of the command capability (ST-SUP-RC32) on the command runtime
// github.com/jsell-rh/stego/registry/runtime/command (ST-SUP-RP10). For each declared command it emits the command slot contract (the typed
// request and response messages, the fill type and the command handle), the typed refusals, the repository
// declarations of the entities that the command writes, the command declaration with its permission and resource,
// the probe wiring through the domain probes (ST-SUP-RC34), and one HTTP handler and one gRPC handler that call the
// one fill through Run. The handle, Run, the idempotency wrapper, the audit and outbox writes and the decisions live
// in the runtime library; the generated code holds what varies with the declaration (ST-STD-RUNTIME-R05).
package command

import (
	"bytes"
	"fmt"
	"go/format"
	"path"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// Generator is the generator of the command component.
type Generator struct{}

// The slot that each command fill implements, and the components whose output the generated code uses.
const (
	commandSlot    = "command.command"
	probeComponent = "domain-probe"
	runtimeModule  = "github.com/jsell-rh/stego/registry/runtime"
	protobufModule = "google.golang.org/protobuf"
	protobufPin    = "v1.36.12"
	grpcModule     = "google.golang.org/grpc"
	grpcPin        = "v1.83.2"
)

// MinimumGoVersion is the Go version of the runtime module that the generated code imports.
func (*Generator) MinimumGoVersion() string { return "1.26.11" }

// ConsumedSlots is empty: a command fill is bound by the command's fill, not by a slot binding.
func (*Generator) ConsumedSlots() []string { return []string{} }

// InputFiles names the proto files of the command messages, which the compiler reads for the generator.
func (*Generator) InputFiles(config map[string]any) ([]gen.InputFile, error) {
	if _, set := config["proto_files"]; !set {
		// A service that declares no command has no command messages (ST-SUP-RC20-OC39).
		return nil, nil
	}
	items, err := protoSources(config)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.File
	}
	return gen.SourceInputs(names), nil
}

// ValidateContext checks the declared commands before any code is rendered.
func (*Generator) ValidateContext(ctx gen.Context) error {
	_, err := plan(ctx)
	return err
}

// Generate emits the command contract package and its handlers package in the component's output namespace.
func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	m, err := plan(ctx)
	if err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, &gen.Wiring{}, nil
	}
	files, err := m.messageFiles()
	if err != nil {
		return nil, nil, err
	}
	contract, err := format.Source(renderContract(m))
	if err != nil {
		return nil, nil, fmt.Errorf("command: formatting the contract package: %w", err)
	}
	handlers, err := format.Source(renderHandlers(m))
	if err != nil {
		return nil, nil, fmt.Errorf("command: formatting the handlers package: %w", err)
	}
	bound, err := format.Source(renderFills(m))
	if err != nil {
		return nil, nil, fmt.Errorf("command: formatting the fill binding: %w", err)
	}
	files = append(files,
		gen.File{Path: path.Join(ctx.OutputNamespace, "commands.go"), Content: contract},
		gen.File{Path: path.Join(ctx.OutputNamespace, handlersFolder, "fills.go"), Content: bound},
		gen.File{Path: path.Join(ctx.OutputNamespace, handlersFolder, "handlers.go"), Content: handlers},
	)
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	wiring := &gen.Wiring{GoModRequires: map[string]string{protobufModule: protobufPin, grpcModule: grpcPin}}
	return files, wiring, nil
}

// handlersFolder is the folder of the handlers package under the output namespace. Its code holds the runtime; the
// contract package, which a fill imports, does not.
const handlersFolder = "handlers"

// model is what the generator renders.
type model struct {
	ctx            gen.Context
	pkg            string // the contract package name
	contractImport string
	handlersImport string
	probeImport    string // the domain probes package, or empty when the service has none
	tenantKey      string
	subjectType    string
	basePath       string
	messages       *messages
	entities       []*entity // the entities that a command writes, in declaration order
	commands       []*command
	names          map[string]string
	// destinations are the declared outbox destinations by name, with their delivery (ST-SUP-RC32-OC12).
	destinations map[string]delivery
}

// delivery is the declared delivery of one outbox destination.
type delivery struct{ mustLand, idempotent bool }

// declaredDeliveries reads the destinations that the components declare, and refuses one that leaves must_land or
// idempotent out, naming the destination and the property (ST-SUP-RC32-OC12).
func declaredDeliveries(destinations []gen.Destination) (map[string]delivery, error) {
	out := map[string]delivery{}
	for _, d := range destinations {
		for property, value := range map[string]*bool{"must_land": d.MustLand, "idempotent": d.Idempotent} {
			if value == nil {
				return nil, fmt.Errorf("command: the destination %s of the component %s does not declare %s; the setting destinations declares must_land and idempotent for each destination (ST-SUP-RC32-OC12)", d.Name, d.Component, property)
			}
		}
		out[d.Name] = delivery{mustLand: *d.MustLand, idempotent: *d.Idempotent}
	}
	return out, nil
}

// command is one checked command.
type command struct {
	decl      types.Command
	goName    string
	writes    []*entity
	resource  *resource
	request   *message
	response  *message
	params    []param
	member    *messageField // the resource member, or nil for a command without a permission
	refusals  []refusal
	creates   bool
	pattern   string
	relations []string
	effects   []string
	// effectNames are the declared effect names of each effect destination, which Stage holds the fill to
	// (ST-SUP-RP10-LC07).
	effectNames map[string][]string
	retry       int
	lock        string // a Go duration expression, or empty for the default
}

// resource is the resource of a command's permission: its type and the table of the entity that has it.
type resource struct {
	resourceType string
	table        string
	keyCast      string
}

// param is one path parameter of a command's route and the request field that holds it.
type param struct {
	name  string
	field *messageField
}

// refusal is one declared refusal of a command.
type refusal struct {
	name, goName, code string
	status             int
}

// plan checks the context and builds the model. It refuses, naming the command, each declaration that the
// generated code or the runtime's checks would refuse, so the emitted code never fails at start. It gives no model
// for a service that declares no command: the component is then in the service only because its archetype holds it,
// and it emits nothing (ST-SUP-RC20-OC39).
func plan(ctx gen.Context) (*model, error) {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return nil, err
	}
	for key := range ctx.ComponentConfig {
		if key != "proto_files" {
			return nil, fmt.Errorf("command: the component has no setting %s", key)
		}
	}
	m := &model{
		ctx:            ctx,
		pkg:            path.Base(ctx.OutputNamespace),
		contractImport: path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace),
		basePath:       strings.TrimSuffix(ctx.BasePath, "/"),
		names:          map[string]string{},
	}
	m.handlersImport = path.Join(m.contractImport, handlersFolder)
	if ns := ctx.PeerNamespaces[probeComponent]; ns != "" {
		m.probeImport = path.Join(ctx.ModuleName, ctx.OutDirName, ns)
	}
	if len(ctx.Commands) == 0 {
		if _, set := ctx.ComponentConfig["proto_files"]; set {
			return nil, fmt.Errorf("command: the setting proto_files holds the messages of the declared commands, and the declaration declares no command; remove the setting or declare a command")
		}
		return nil, nil
	}
	if ctx.Tenancy == nil || ctx.Tenancy.Key != "tenant_id" {
		return nil, fmt.Errorf("command: a command runs in the transaction of one tenant, so the service needs pooled tenancy with the tenant column tenant_id")
	}
	m.tenantKey = ctx.Tenancy.Key
	if ctx.Authorization == nil || ctx.Authorization.SubjectFromIdentity == "" {
		return nil, fmt.Errorf("command: a command decides its permission before the fill, so the service needs authorization with subject_from_identity")
	}
	m.subjectType = ctx.Authorization.SubjectFromIdentity
	deliveries, err := declaredDeliveries(ctx.Destinations)
	if err != nil {
		return nil, err
	}
	m.destinations = deliveries
	msgs, err := compileMessages(ctx)
	if err != nil {
		return nil, err
	}
	m.messages = msgs
	byName := map[string]types.Entity{}
	for _, e := range ctx.Entities {
		byName[e.Name] = e
	}
	planned := map[string]*entity{}
	for _, c := range ctx.Commands {
		cmd, err := m.planCommand(c, byName, planned)
		if err != nil {
			return nil, err
		}
		m.commands = append(m.commands, cmd)
	}
	for _, e := range ctx.Entities {
		if p := planned[e.Name]; p != nil {
			m.entities = append(m.entities, p)
		}
	}
	if err := m.takeNames(); err != nil {
		return nil, err
	}
	return m, nil
}

// planCommand checks one command.
func (m *model) planCommand(c gen.Command, byName map[string]types.Entity, planned map[string]*entity) (*command, error) {
	decl := c.Command
	if !operationName.MatchString(decl.Name) {
		return nil, fmt.Errorf("command: the command name %q is not a lower-case name of letters, digits and hyphens", decl.Name)
	}
	cmd := &command{decl: decl, goName: mixedCase(decl.Name)}
	// One handler calls the one fill that the command names, and that fill must implement the command slot
	// (ST-SUP-RC32-OC02).
	if len(decl.Fill) != 1 {
		return nil, fmt.Errorf("command: the command %s names %d fills; it needs exactly one fill of the slot %s", decl.Name, len(decl.Fill), commandSlot)
	}
	if c.FillSlot != commandSlot {
		found := c.FillSlot
		if found == "" {
			found = "no slot"
		}
		return nil, fmt.Errorf("command: the command %s names the fill %s, which implements %s, not the command slot %s", decl.Name, decl.Fill[0], found, commandSlot)
	}
	if !fillName.MatchString(decl.Fill[0]) {
		return nil, fmt.Errorf("command: the fill %q of the command %s is not a valid fill folder name", decl.Fill[0], decl.Name)
	}
	if len(decl.Writes) == 0 {
		return nil, fmt.Errorf("command: the command %s writes no entity", decl.Name)
	}
	for _, name := range decl.Writes {
		e, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("command: the command %s writes the entity %s, which the declaration does not declare", decl.Name, name)
		}
		p := planned[name]
		if p == nil {
			var err error
			if p, err = planEntity(e, byName, m.tenantKey); err != nil {
				return nil, fmt.Errorf("command: the command %s writes %s: %w", decl.Name, name, err)
			}
			planned[name] = p
		}
		cmd.writes = append(cmd.writes, p)
	}
	if err := m.planRoute(cmd); err != nil {
		return nil, err
	}
	if err := m.planPermission(cmd, byName); err != nil {
		return nil, err
	}
	for _, r := range decl.Refusals {
		if !refusalName.MatchString(r.Name) || !refusalCode.MatchString(r.Code) || r.Code == "internal" || r.Code == "conflict" || !refusalStatus[r.Status] {
			return nil, fmt.Errorf("command: the command %s declares the refusal %s: %d %s, which is not a lower snake case name with a status of 400, 403, 404, 409, 412 or 422 and a code other than internal and conflict", decl.Name, r.Name, r.Status, r.Code)
		}
		cmd.refusals = append(cmd.refusals, refusal{name: r.Name, goName: cmd.goName + mixedCase(r.Name), code: r.Code, status: r.Status})
	}
	for _, text := range decl.RelationshipsWritten {
		if !relationText.MatchString(text) {
			return nil, fmt.Errorf("command: the command %s writes the relation %q, which is not resource_type#relation", decl.Name, text)
		}
		cmd.relations = append(cmd.relations, text)
	}
	seen, named := map[string]bool{}, map[string]bool{}
	for _, effect := range decl.Effects {
		if !effectName.MatchString(effect.Name) || named[effect.Name] {
			return nil, fmt.Errorf("command: the command %s declares the effect %q, whose name is missing, not a lower-case name or given twice", decl.Name, effect.String())
		}
		named[effect.Name] = true
		if effect.Destination == "" || effect.Destination == "authz" || effect.Destination == "audit" || effect.Destination == "audit-sink" {
			return nil, fmt.Errorf("command: the command %s stages for the destination %q, which is not an effect destination", decl.Name, effect.Destination)
		}
		if _, ok := m.destinations[effect.Destination]; !ok {
			return nil, fmt.Errorf("command: the command %s stages for the destination %q, which no component declares in its setting destinations (ST-SUP-RC32-OC12)", decl.Name, effect.Destination)
		}
		if !seen[effect.Destination] {
			seen[effect.Destination] = true
			cmd.effects = append(cmd.effects, effect.Destination)
		}
		if cmd.effectNames == nil {
			cmd.effectNames = map[string][]string{}
		}
		cmd.effectNames[effect.Destination] = append(cmd.effectNames[effect.Destination], effect.Name)
	}
	retry, lock, err := retryAndLock(decl)
	if err != nil {
		return nil, err
	}
	cmd.retry, cmd.lock = retry, lock
	return cmd, nil
}

// planRoute checks the command's route, finds its messages and binds each path parameter to a string field of the
// request message (ST-SUP-RC32-OC11).
func (m *model) planRoute(cmd *command) error {
	decl := cmd.decl
	verb, routePath, ok := strings.Cut(decl.Route, " ")
	if !ok || verb != "POST" || !strings.HasPrefix(routePath, "/") || strings.ContainsAny(routePath, " \t\"`\\") {
		return fmt.Errorf("command: the command %s has the route %q; a command route is POST and a path", decl.Name, decl.Route)
	}
	cmd.pattern = "POST " + m.basePath + routePath
	var err error
	if cmd.request, err = m.messages.find(cmd.goName + "Request"); err != nil {
		return fmt.Errorf("command: the command %s: %w", decl.Name, err)
	}
	if cmd.response, err = m.messages.find(cmd.goName + "Response"); err != nil {
		return fmt.Errorf("command: the command %s: %w", decl.Name, err)
	}
	segments := strings.Split(strings.Trim(routePath, "/"), "/")
	for _, segment := range segments {
		if !strings.HasPrefix(segment, "{") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
		f := cmd.request.field(name)
		if f == nil || !f.isString() {
			return fmt.Errorf("command: the route of %s has the path parameter %s, which is not a string field of the message %s", decl.Name, name, cmd.request.name)
		}
		cmd.params = append(cmd.params, param{name: name, field: f})
	}
	// A POST to a collection path creates a resource and answers 201; a POST to one resource answers 200.
	last := segments[len(segments)-1]
	cmd.creates = !strings.HasPrefix(last, "{") && !strings.Contains(last, ":")
	return nil
}

// planPermission checks the command's permission and finds its resource: the entity whose resource type the
// command names, and the request field that the resource member names (ST-SUP-RC32-OC06, ST-SUP-RP10-LC31).
func (m *model) planPermission(cmd *command, byName map[string]types.Entity) error {
	decl := cmd.decl
	if decl.Permission == "" || decl.ResourceType == "" || decl.ResourceMember == "" {
		return fmt.Errorf("command: the command %s needs its permission, its resource type and its resource member, which the authorization of the service makes required", decl.Name)
	}
	if !identifier.MatchString(decl.Permission) || !identifier.MatchString(decl.ResourceType) {
		return fmt.Errorf("command: the command %s has the permission %q on %q, which are not names of the schema", decl.Name, decl.Permission, decl.ResourceType)
	}
	var owner *types.Entity
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if e := byName[name]; e.ResourceType == decl.ResourceType {
			if owner != nil {
				return fmt.Errorf("command: the resource type %s of the command %s is the resource type of two entities, %s and %s", decl.ResourceType, decl.Name, owner.Name, e.Name)
			}
			e := e
			owner = &e
		}
	}
	if owner == nil {
		return fmt.Errorf("command: no entity has the resource type %s of the command %s, so its row token cannot be read", decl.ResourceType, decl.Name)
	}
	if owner.Tenant != "" && owner.Tenant != "owned" {
		return fmt.Errorf("command: the resource of the command %s is the entity %s, which is not owned by a tenant", decl.Name, owner.Name)
	}
	cmd.resource = &resource{resourceType: decl.ResourceType, table: tableName(owner.Name), keyCast: "uuid"}
	// The runtime checks the token statement at start against one form of plain identifiers (ST-SUP-RP10-LC31).
	if !sqlName.MatchString(cmd.resource.table) {
		return fmt.Errorf("command: the resource table %q of the command %s is not a plain SQL name", cmd.resource.table, decl.Name)
	}
	f := cmd.request.field(decl.ResourceMember)
	if f == nil || !f.isString() {
		return fmt.Errorf("command: the resource member %s of the command %s is not a string field of the message %s", decl.ResourceMember, decl.Name, cmd.request.name)
	}
	cmd.member = f
	return nil
}

// takeNames refuses two exported names of the contract package that are the same.
func (m *model) takeNames() error {
	take := func(owner string, names ...string) error {
		for _, name := range names {
			if other, ok := m.names[name]; ok {
				return fmt.Errorf("command: %s and %s both need the Go name %s", other, owner, name)
			}
			m.names[name] = owner
		}
		return nil
	}
	if err := take("the refusal type", "Refusal"); err != nil {
		return err
	}
	for _, name := range m.messages.goNames() {
		if err := take("the message "+name, name); err != nil {
			return err
		}
	}
	for _, e := range m.entities {
		if err := take("the entity "+e.decl.Name, e.goName, e.goName+"Entity", e.goName+"Repository", "Loaded"+e.goName); err != nil {
			return err
		}
	}
	for _, c := range m.commands {
		if err := take("the command "+c.decl.Name, c.goName+"Handle", c.goName+"Fill"); err != nil {
			return err
		}
		for _, r := range c.refusals {
			if err := take("the refusal "+r.name+" of "+c.decl.Name, r.goName); err != nil {
				return err
			}
		}
	}
	return nil
}

// writer is a source buffer.
type writer struct{ bytes.Buffer }

func (w *writer) f(format string, args ...any) { fmt.Fprintf(&w.Buffer, format, args...) }
