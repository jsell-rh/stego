// Package query is the generator of the query component (ST-SUP-RC33). For each query of the declaration it emits
// the query slot contract, which the query's one fill implements, and one HTTP handler served through query.Serve of
// the runtime library (ST-SUP-RP11-LC17), with the gRPC handler of a query that a gRPC method serves through
// query.ServeRPC or query.ServeStream (ST-SUP-RP11-LC19); a stream query gets no HTTP handler (ST-SUP-RC33-OC08): the handler verifies the caller, opens one READ ONLY transaction of the
// caller's tenant through query.Run, decides a get query's permission on its resource before the fill and calls the
// fill once with a query handle. The handle, its authorization part, Run, Serve and the sealed list cursor live in
// github.com/jsell-rh/stego/registry/runtime/query; the generated code holds only what varies with the declaration (ST-STD-RUNTIME-R05).
package query

import (
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/capability"
	"github.com/jsell-rh/stego/internal/types"
)

// Slot is the slot that a query fill implements, as component.slot (ST-DECL-INV45).
const Slot = "query.query"

// probeComponent is the component whose typed probes the query handles carry (ST-SUP-RC34-OC04).
const probeComponent = "domain-probe"

// tenantColumn is the tenant column that the query runtime adds to each read (github.com/jsell-rh/stego/registry/runtime/query.TenantColumn).
const tenantColumn = "tenant_id"

// Generator is the generator of the query component.
type Generator struct{}

// MinimumGoVersion is the Go version of the runtime library that the generated code imports.
func (*Generator) MinimumGoVersion() string { return "1.26.11" }

// ValidateContext checks the declared queries before any code is rendered, with the checks of Generate.
func (*Generator) ValidateContext(ctx gen.Context) error {
	_, err := plan(ctx)
	return err
}

// GoModels describes the read-only value type of each entity that a query reads; the PostgreSQL adapter emits no
// entity model (ST-SUP-RC01-OC41). The domain-probe generator takes the loaded types of its probe methods from it, so
// a probe method takes a value that a query handle loaded (ST-SUP-RC34-OC01). A service that declares no query gets
// no model.
func (*Generator) GoModels(ctx gen.Context) ([]gen.GoModel, error) {
	m, err := plan(ctx)
	if err != nil || m == nil {
		return nil, err
	}
	models := make([]gen.GoModel, 0, len(m.entities))
	for _, e := range m.entities {
		models = append(models, e.value.GoModel())
	}
	return models, gen.ValidateGoModels(models)
}

// Generate emits the contract package in the component's namespace, which the query fills import, and the handler
// package serve under it. It refuses, with no file, a declaration that the query runtime cannot serve, naming the
// query and the setting; among them a query whose fill does not implement the query slot (ST-SUP-RC33-OC02).
func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	m, err := plan(ctx)
	if err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, &gen.Wiring{}, nil
	}
	files, err := render(m)
	if err != nil {
		return nil, nil, err
	}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{}, nil
}

// kind is the kind of a query: a get decides its permission on one resource before the fill, a list pages the
// entity it lists with the sealed cursor, and a stream sends loaded values one by one.
type kind uint8

const (
	getKind kind = iota + 1
	listKind
	streamKind
)

// entity is one entity that a query reads, with its read-only value and its authorization.
type entity struct {
	value          capability.Value
	variable       string // the unexported variable of its query.Entity
	resourceType   string
	readPermission string
	listPermission string
	// search is true for an entity of small expected cardinality: its list reads the rows of the provider's
	// resource search (ST-SUP-RP11-LC20).
	search bool
	// local is true for the declaration that decides a get query's permission on its requested row in place of the
	// entity's read permission (ST-SUP-RC33-OC05); it shares the value type of the entity it copies.
	local bool
}

// refusal is one declared refusal of a query.
type refusal struct {
	name, goName, code string
	status             int
}

// queryModel is one checked query.
type queryModel struct {
	decl       types.Query
	goName     string
	kind       kind
	method     string // the pattern method, GET
	path       string // the path under the base path
	member     string // the request member of a get query
	memberGo   string
	inPath     bool
	resource   *entity // the entity whose resource type the query acts on; nil when no read is protected
	decide     *entity // the declaration that decides a get query's permission on its row, when it is not the read permission
	reads      []*entity
	refusals   []refusal
	fillImport string
	fillAlias  string
	// rpc is true for a get or list query that a unary gRPC method also serves (ST-SUP-RC07-OC19); a stream query is
	// served over gRPC alone (ST-SUP-RC33-OC08).
	rpc bool
}

// model is what the generator renders.
type model struct {
	namespace      string
	pkg            string
	contractImport string
	probeImport    string
	entities       []*entity
	queries        []*queryModel
	decided        bool // a get query decides a permission that is not the read permission of its resource
	basePath       string
}

var (
	queryName     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	memberName    = regexp.MustCompile(`^[a-z][A-Za-z0-9]{0,62}$`)
	refusalName   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	refusalCode   = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,62}$`)
	permissionRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	pathParameter = regexp.MustCompile(`\{([^}]*)\}`)
)

// refusalStatus are the statuses that a declared refusal may have (ST-DECL-INV56).
var refusalStatus = map[int]bool{400: true, 403: true, 404: true, 409: true, 412: true, 422: true}

// plan checks the context and builds the model. It gives no model for a service that declares no query: the
// component is then in the service only because its archetype holds it, and it emits nothing (ST-SUP-RC20-OC39).
func plan(ctx gen.Context) (*model, error) {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return nil, err
	}
	if len(ctx.ComponentConfig) != 0 {
		return nil, fmt.Errorf("query: the query component has no component settings")
	}
	if len(ctx.Queries) == 0 {
		return nil, nil
	}
	if ctx.Tenancy == nil || ctx.Tenancy.Key != tenantColumn {
		return nil, fmt.Errorf("query: the query runtime reads tenant-owned tables with the tenant column %s; declare pooled tenancy with that key", tenantColumn)
	}
	probes := ctx.PeerNamespaces[probeComponent]
	if probes == "" {
		return nil, fmt.Errorf("query: a query handle gives its fill the query's typed probe, which the %s component emits; add it to the service", probeComponent)
	}
	m := &model{namespace: strings.TrimSuffix(ctx.OutputNamespace, "/"), pkg: path.Base(ctx.OutputNamespace),
		basePath: strings.TrimSuffix(ctx.BasePath, "/")}
	m.contractImport = generatedImport(ctx, m.namespace)
	m.probeImport = generatedImport(ctx, probes)
	declared := map[string]types.Entity{}
	for _, e := range ctx.Entities {
		declared[e.Name] = e
	}
	entities := map[string]*entity{}
	goNames := map[string]string{}
	routes := map[string]string{}
	for _, decl := range ctx.Queries {
		q, err := checkQuery(ctx, decl, declared, entities, m)
		if err != nil {
			return nil, err
		}
		if other, used := goNames[q.goName]; used {
			return nil, fmt.Errorf("query: the queries %s and %s have the same Go name %s", other, decl.Name, q.goName)
		}
		goNames[q.goName] = decl.Name
		route := q.method + " " + q.path
		if other, used := routes[route]; used && q.kind != streamKind {
			return nil, fmt.Errorf("query: the queries %s and %s have the same route %s", other, decl.Name, decl.Route)
		}
		routes[route] = decl.Name
		m.queries = append(m.queries, q)
	}
	if err := checkRoutes(m.queries); err != nil {
		return nil, err
	}
	return m, nil
}

// checkRoutes mounts the pattern of each get and list query on a net/http ServeMux, as the handlers do, so a pattern
// that the mux refuses or two that conflict are refused at generation, naming the query, and never panic at start.
func checkRoutes(queries []*queryModel) (err error) {
	mux := http.NewServeMux()
	var current *queryModel
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("query: the query %s has the route %q, which net/http refuses: %v", current.decl.Name, current.decl.Route, r)
		}
	}()
	for _, q := range queries {
		if q.kind == streamKind {
			continue
		}
		current = q
		mux.Handle(q.method+" "+q.path, http.NotFoundHandler())
	}
	return nil
}

// generatedImport is the import path of a generated package in the namespace ns.
func generatedImport(ctx gen.Context, ns string) string {
	parts := []string{strings.TrimSuffix(ctx.ModuleName, "/")}
	if ctx.OutDirName != "" {
		parts = append(parts, ctx.OutDirName)
	}
	return strings.Join(append(parts, strings.TrimSuffix(ns, "/")), "/")
}

// checkQuery checks one declared query and binds it to its entities and its fill.
func checkQuery(ctx gen.Context, decl types.Query, declared map[string]types.Entity, entities map[string]*entity, m *model) (*queryModel, error) {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("query: the query %s %s", decl.Name, fmt.Sprintf(format, args...))
	}
	if !queryName.MatchString(decl.Name) {
		return nil, fmt.Errorf("query: the query name %q is not in lower kebab case", decl.Name)
	}
	q := &queryModel{decl: decl, goName: capability.MixedCase(decl.Name)}
	method, route, ok := strings.Cut(strings.TrimSpace(decl.Route), " ")
	route = strings.TrimSpace(route)
	if !ok || method != "GET" || !strings.HasPrefix(route, "/") || strings.ContainsAny(route, " \t?#") {
		return nil, refuse("has the route %q; a query route is GET and a path", decl.Route)
	}
	q.method, q.path = method, m.basePath+route
	if len(decl.Reads) == 0 {
		return nil, refuse("reads no entity")
	}
	for _, name := range decl.Reads {
		e, err := entityOf(name, declared, entities, m)
		if err != nil {
			return nil, refuse("reads %s: %v", name, err)
		}
		if slices.Contains(q.reads, e) {
			return nil, refuse("reads %s twice", name)
		}
		q.reads = append(q.reads, e)
	}
	if ctx.Authorization != nil {
		for _, e := range q.reads {
			if e.resourceType == "" {
				return nil, refuse("reads %s, which declares no resource type while the declaration binds authorization (ST-STD-TXN-R18)", e.value.Entity.Name)
			}
		}
	}
	protected := slices.ContainsFunc(q.reads, func(e *entity) bool { return e.resourceType != "" })
	if protected || decl.ResourceType != "" || decl.Permission != "" {
		if decl.ResourceType == "" || !permissionRe.MatchString(decl.Permission) {
			return nil, refuse("reads a protected entity and needs a resource type and a permission (ST-DECL-INV46)")
		}
		for _, e := range q.reads {
			if e.resourceType == decl.ResourceType {
				q.resource = e
				break
			}
		}
		if q.resource == nil {
			return nil, refuse("acts on the resource type %s, which no entity it reads has", decl.ResourceType)
		}
	}
	switch {
	case ctx.QueryStreams[decl.Name]:
		q.kind = streamKind
		if q.resource == nil && len(q.reads) != 1 {
			return nil, refuse("is a stream of more than one entity; give it the resource type of the entity it sends")
		}
		if e := q.sent(); decl.Permission != e.readPermission {
			return nil, refuse("is a stream with the permission %s, and Send decides the read permission %s of %s; a stream query declares the read permission of the entity it sends", decl.Permission, e.readPermission, e.value.Entity.Name)
		}
	case decl.ResourceMember != "":
		q.kind = getKind
	default:
		q.kind = listKind
	}
	q.rpc = decl.Method != "" && q.kind != streamKind
	if decl.ResourceMember != "" {
		if !memberName.MatchString(decl.ResourceMember) {
			return nil, refuse("names the resource member %q, which is not a member name in lower camel case", decl.ResourceMember)
		}
		if q.resource == nil && len(q.reads) != 1 {
			return nil, refuse("names the resource member %s and reads more than one entity with no resource type to tell which it names", decl.ResourceMember)
		}
		if q.resource == nil {
			// An entity with no resource type is not protected: the get reads its row before the fill, with no decision.
			q.resource = q.reads[0]
		}
		q.member, q.memberGo = decl.ResourceMember, memberGoName(decl.ResourceMember)
	}
	for _, p := range pathParameter.FindAllStringSubmatch(route, -1) {
		if p[1] != q.member || q.member == "" {
			return nil, refuse("has the path parameter %s, which is not its resource member", p[0])
		}
		q.inPath = true
	}
	names := map[string]bool{}
	for _, r := range decl.Refusals {
		switch {
		case !refusalName.MatchString(r.Name) || names[r.Name]:
			return nil, refuse("has the refusal %q, whose name is not lower snake case or is used twice", r.Name)
		case !refusalCode.MatchString(r.Code) || r.Code == "internal" || r.Code == "conflict":
			return nil, refuse("has the refusal %s with the code %q, which is not a code or is reserved", r.Name, r.Code)
		case !refusalStatus[r.Status]:
			return nil, refuse("has the refusal %s with the status %d, which a refusal may not have", r.Name, r.Status)
		}
		names[r.Name] = true
		q.refusals = append(q.refusals, refusal{name: r.Name, goName: q.goName + capability.MixedCase(r.Name), code: r.Code, status: r.Status})
	}
	if q.kind == getKind && q.resource.resourceType != "" && decl.Permission != q.resource.readPermission {
		// The get decides the query's permission on the requested row only, not the entity's read permission
		// (ST-SUP-RC33-OC05): the handler reads that row through a declaration of the entity of its own, in the
		// package internal under the contract, which no fill can import. The repositories that the fill gets keep the
		// entity's read permission, so the fill reads no other row on the query's permission.
		local := *q.resource
		local.variable = capability.LowerFirst(q.goName) + "Resource"
		local.readPermission, local.local = decl.Permission, true
		q.decide = &local
		m.decided = true
	}
	if err := bindFill(ctx, q); err != nil {
		return nil, err
	}
	return q, nil
}

// bindFill binds the query to its one fill, which must implement the query slot (ST-SUP-RC33-OC02).
func bindFill(ctx gen.Context, q *queryModel) error {
	if len(q.decl.Fill) != 1 {
		return fmt.Errorf("query: the query %s names %d fills; it names exactly one fill of the slot %s", q.decl.Name, len(q.decl.Fill), Slot)
	}
	fill := q.decl.Fill[0]
	implements, held := ctx.FillSlots[fill]
	switch {
	case !held:
		return fmt.Errorf("query: the query %s names the fill %s, which the project does not hold at fills/%s/fill.yaml", q.decl.Name, fill, fill)
	case implements != Slot:
		return fmt.Errorf("query: the query %s names the fill %s, which implements %s, not the query slot %s", q.decl.Name, fill, implements, Slot)
	}
	imp, alias, err := capability.FillImport(ctx.ModuleName, fill)
	if err != nil {
		return fmt.Errorf("query: the query %s: %w", q.decl.Name, err)
	}
	q.fillImport, q.fillAlias = imp, alias
	return nil
}

// entityOf gives the checked entity of the declaration that a query reads, once for every query.
func entityOf(name string, declared map[string]types.Entity, entities map[string]*entity, m *model) (*entity, error) {
	if e := entities[name]; e != nil {
		return e, nil
	}
	decl, ok := declared[name]
	if !ok {
		return nil, fmt.Errorf("the declaration does not declare it")
	}
	if decl.Tenant != "" && decl.Tenant != "owned" {
		return nil, fmt.Errorf("its tenant is %s; a query reads only tenant-owned entities, with the tenant predicate", decl.Tenant)
	}
	value, err := capability.ValueOf(decl)
	if err != nil {
		return nil, err
	}
	e := &entity{value: value, variable: capability.LowerFirst(value.GoType) + "Entity", resourceType: decl.ResourceType}
	switch decl.ExpectedCardinality {
	case "", types.CardinalityLarge:
	case types.CardinalitySmall:
		if e.resourceType == "" {
			return nil, fmt.Errorf("its expected cardinality is small but it has no resource type; only a protected entity lists through the resource search")
		}
		e.search = true
	default:
		return nil, fmt.Errorf("its expected cardinality is %q; it is small or large", decl.ExpectedCardinality)
	}
	if e.resourceType != "" {
		e.readPermission = decl.Permissions[types.OpRead]
		e.listPermission = decl.Permissions[types.OpList]
		for op, p := range map[string]string{"read": e.readPermission, "list": e.listPermission} {
			if !permissionRe.MatchString(p) || p == types.PermissionPublic {
				return nil, fmt.Errorf("its resource type is %s and its %s permission is %q; a query reads a protected entity on a declared permission", e.resourceType, op, p)
			}
		}
	}
	entities[name] = e
	m.entities = append(m.entities, e)
	return e, nil
}

// memberGoName is the Go field of a request member in lower camel case: orderId is OrderID.
func memberGoName(member string) string {
	name := strings.ToUpper(member[:1]) + member[1:]
	if strings.HasSuffix(name, "Id") {
		name = strings.TrimSuffix(name, "Id") + "ID"
	}
	return name
}
