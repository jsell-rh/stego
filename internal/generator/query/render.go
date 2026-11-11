package query

import (
	"bytes"
	"fmt"
	"go/format"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/capability"
	"github.com/jsell-rh/stego/internal/types"
)

// The import paths of the runtime library that the generated code uses.
const (
	queryImport = "github.com/jsell-rh/stego/registry/runtime/query"
	auditImport = "github.com/jsell-rh/stego/registry/runtime/audit"
	authnImport = "github.com/jsell-rh/stego/registry/runtime/authn"
	protoImport = "google.golang.org/protobuf/proto"
)

// render gives the contract package and the handler package of the model.
func render(m *model) ([]gen.File, error) {
	var files []gen.File
	var decide []byte
	if m.decided {
		decide = renderDecide(m)
	}
	for _, f := range []struct {
		name   string
		source []byte
	}{
		{path.Join(m.namespace, m.pkg+".go"), renderContract(m)},
		{path.Join(m.namespace, "serve", "serve.go"), renderServe(m)},
		{path.Join(m.namespace, "internal", "decide", "decide.go"), decide},
	} {
		if f.source == nil {
			continue
		}
		formatted, err := format.Source(f.source)
		if err != nil {
			return nil, fmt.Errorf("query: formatting %s: %w\n%s", f.name, err, f.source)
		}
		files = append(files, gen.File{Path: f.name, Content: formatted})
	}
	return files, nil
}

// writer is the text of one emitted file.
type writer struct{ bytes.Buffer }

// raw writes text as it is, and a new line.
func (w *writer) raw(text string) {
	w.WriteString(text)
	w.WriteByte('\n')
}

func (w *writer) p(format string, args ...any) {
	fmt.Fprintf(&w.Buffer, format, args...)
	w.WriteByte('\n')
}

// reads gives the read kinds that the entities' load functions use.
func (m *model) reads() map[string]bool {
	used := map[string]bool{}
	for _, e := range m.entities {
		for _, f := range e.value.Fields {
			used[f.Read] = true
		}
	}
	return used
}

// sensitive tells if an entity has a pii or a secret field.
func (m *model) sensitive() bool {
	for _, e := range m.entities {
		for _, f := range e.value.Fields {
			if f.Sensitivity == "pii" || f.Sensitivity == "secret" {
				return true
			}
		}
	}
	return false
}

// renderContract is the query slot contract: the read-only value and the query.Entity of each entity a query
// reads, and for each query its request, its response, its handle and fill types, its query.Query, the repositories
// of the entities it reads and its refusal values (ST-SUP-RC33-OC01, ST-SUP-RC33-OC07). A fill imports this package
// only: it reaches the runtime library through the types and the functions that the package gives.
func renderContract(m *model) []byte {
	var w writer
	used := m.reads()
	w.p("// Package %s is the query slot contract of the service (ST-SUP-RC33): the read-only value of each entity that", m.pkg)
	w.p("// a query reads, and for each query its request, response, handle and fill types, its repositories and its")
	w.p("// refusals. The fill of a query is the function Fill of its package under fills, of the query's fill type: it gets")
	w.p("// the decoded request and one read-only query handle, and nothing else that reaches storage, authorization,")
	w.p("// the outbox or the network.")
	w.p("package %s\n\nimport (", m.pkg)
	if used[capability.ReadJSON] {
		w.p("\"encoding/json\"")
	}
	if used[capability.ReadTime] {
		w.p("\"time\"")
	}
	w.p("\nprobes %q", m.probeImport)
	if m.sensitive() {
		w.p("%q", auditImport)
	}
	w.p("%q\n)\n", queryImport)
	w.p("// Cursor is the position of a list: the zero Cursor is its start. The handler reads it from the sealed text form")
	w.p("// of the request's cursor parameter, and writes the next one in the same form (ST-SUP-RP11-LC15).")
	w.p("type Cursor = query.Cursor\n")
	w.p("// ListRequest is one page of a list through a repository; ListResult is its answer.")
	w.p("type ListRequest = query.ListRequest\n")
	w.p("// ListResult is one page of a list: the allowed values, and the cursor of the next page when More is true.")
	w.p("type ListResult[T any] = query.ListResult[T]\n")
	w.p("// Repository is the tenant-scoped read of one entity through one query handle.")
	w.p("type Repository[T any] = query.Repository[T]\n")
	w.p("// ErrNotFound is the error of a read of a row that is not there or that the caller may not read.")
	w.p("var ErrNotFound = query.ErrNotFound\n")
	for _, e := range m.entities {
		renderEntity(&w, e)
	}
	for _, q := range m.queries {
		renderQueryContract(&w, q)
	}
	renderRefusalType(&w, m)
	return w.Bytes()
}

// renderEntity writes the read-only value, the loaded type, the load function and the query.Entity of one entity.
func renderEntity(w *writer, e *entity) {
	v := e.value
	w.p("// %s is the read-only value of one stored %s.", v.GoType, v.Entity.Name)
	w.p("type %s struct {", v.GoType)
	for _, f := range v.Fields {
		w.p("%s %s `json:%s`", f.GoName, f.GoType, strconv.Quote(f.Column))
	}
	w.p("}\n")
	w.p("// Loaded%s is a %s that a read through a query handle loaded. Only a handle makes one (ST-SUP-RP11-LC12).", v.GoType, v.Entity.Name)
	w.p("type Loaded%s = query.Loaded[%s]\n", v.GoType, v.GoType)
	w.p("// load%s builds a %s from a stored row. It is a function of the row alone.", v.GoType, v.GoType)
	w.p("func load%s(r query.Row) (%s, error) {\nvar v %s\nvar err error", v.GoType, v.GoType, v.GoType)
	for _, f := range v.Fields {
		w.p("if v.%s, err = %s; err != nil {\nreturn %s{}, err\n}", f.GoName, readExpr(f), v.GoType)
	}
	w.p("return v, nil\n}\n")
	renderSpec(w, e, "load"+v.GoType)
	w.p("// Load%s builds a %s from a stored row, for the declarations of the handler package.", v.GoType, v.GoType)
	w.p("func Load%s(r query.Row) (%s, error) { return load%s(r) }\n", v.GoType, v.GoType, v.GoType)
	w.p("// Entity%s is the declaration of %s that the fills' repositories read through, with its read permission.", v.GoType, v.Entity.Name)
	w.p("func Entity%s() *query.Entity[%s] { return %s }\n", v.GoType, v.GoType, e.variable)
}

// renderSpec writes the query.Entity of one entity: its table, its columns, its authorization and the sensitivity of
// its fields.
func renderSpec(w *writer, e *entity, load string) {
	v := e.value
	columns := make([]string, 0, len(v.Fields))
	var sensitive []string
	for _, f := range v.Fields {
		columns = append(columns, strconv.Quote(f.Column))
		switch f.Sensitivity {
		case "pii":
			sensitive = append(sensitive, strconv.Quote(f.Column)+": audit.PII")
		case "secret":
			sensitive = append(sensitive, strconv.Quote(f.Column)+": audit.Secret")
		}
	}
	sort.Strings(sensitive)
	name := v.Entity.Name
	if e.local {
		// A query reads each entity name once: the declaration that decides the requested row has a name of its own.
		name += "/" + strings.TrimSuffix(e.variable, "Resource")
		w.p("// %s is the declaration of %s that decides the permission %s on the requested row only.", e.variable, v.Entity.Name, e.readPermission)
	} else {
		w.p("// %s is the declaration of %s for the query runtime: its table, its columns, its authorization and the", e.variable, v.Entity.Name)
		w.p("// sensitivity of its fields.")
	}
	w.p("var %s = must(query.NewEntity(query.Spec{Name: %q, Schema: \"public\", Table: %q, Key: \"id\",", e.variable, name, v.Table)
	w.p("Columns: []string{%s},", strings.Join(columns, ", "))
	if e.resourceType != "" {
		w.p("ResourceType: %q, ReadPermission: %q, ListPermission: %q,", e.resourceType, e.readPermission, e.listPermission)
	}
	if len(sensitive) > 0 {
		w.p("Fields: audit.Fields{%s},", strings.Join(sensitive, ", "))
	}
	w.p("}, %s))\n", load)
}

// readExpr is the expression that reads the field's column from the row r.
func readExpr(f capability.Field) string {
	c := strconv.Quote(f.Column)
	switch f.Read {
	case capability.ReadText:
		return "r.Text(" + c + ")"
	case capability.ReadInt:
		return "r.Int(" + c + ")"
	case capability.ReadInt32:
		return "r.Int32(" + c + ")"
	case capability.ReadBool:
		return "r.Bool(" + c + ")"
	case capability.ReadTime:
		return "r.Time(" + c + ")"
	case capability.ReadFloat:
		return "r.Float(" + c + ")"
	case capability.ReadBytes:
		return "r.Bytes(" + c + ")"
	}
	return "r.JSON(" + c + ")"
}

// requestType is the Go type of a query's request.
func (q *queryModel) requestType() string { return q.goName + "Request" }

// responseType is the Go type of a query's response.
func (q *queryModel) responseType() string { return q.goName + "Response" }

// probeType is the typed probe of the query, from the domain-probe generator (ST-SUP-RC34-OC03).
func (q *queryModel) probeType() string { return "probes." + q.goName + "Probe" }

// queryVar is the unexported variable of the query's query.Query.
func (q *queryModel) queryVar() string { return capability.LowerFirst(q.goName) + "Query" }

// sent is the entity that a stream query sends: the entity of its resource type, or its one read.
func (q *queryModel) sent() *entity {
	if q.resource != nil {
		return q.resource
	}
	return q.reads[0]
}

// renderQueryContract writes the slot contract of one query.
func renderQueryContract(w *writer, q *queryModel) {
	name := q.decl.Name
	w.p("// %s is the decoded request of the query %s.", q.requestType(), name)
	w.p("type %s struct {", q.requestType())
	if q.member != "" {
		w.p("// %s is the ID of the %s that the query is decided on.", q.memberGo, q.resource.value.Entity.Name)
		w.p("%s string `json:%s`", q.memberGo, strconv.Quote(q.member))
	}
	if q.kind == listKind {
		w.p("// After is the position after which the page starts; the zero Cursor is the start of the list.")
		w.p("After Cursor `json:\"-\"`")
		w.p("// Limit is the most values that the page holds, from 1 to 1000, or 0 for the default of 100.")
		w.p("Limit int `json:\"limit,omitempty\"`")
	}
	w.p("}\n")
	switch q.kind {
	case getKind:
		w.p("// %s is the response of %s, which the handler writes as JSON.", q.responseType(), name)
		w.p("type %s = any\n", q.responseType())
	case listKind:
		w.p("// %s is one page of %s: the items, which the handler writes as JSON, and the cursor of the next page,", q.responseType(), name)
		w.p("// the zero Cursor at the end of the list.")
		w.p("type %s struct {\nItems any\nNext Cursor\n}\n", q.responseType())
	}
	w.p("// %sHandle is the read-only query handle of %s: bound to one READ ONLY transaction, the caller's tenant and", q.goName, name)
	w.p("// principal, and the query's typed probe.")
	w.p("type %sHandle = query.Handle[%s]\n", q.goName, q.probeType())
	if q.kind == streamKind {
		e := q.sent()
		w.p("// %sStream is the stream handle of %s: Send sends one loaded %s, which it decides when it sends it.", q.goName, name, e.value.Entity.Name)
		w.p("type %sStream = query.Stream[%s, %s]\n", q.goName, e.value.GoType, q.probeType())
		w.p("// %sFill is the query slot of %s, a server stream: the fill gets the decoded request and the stream handle.", q.goName, name)
		w.p("type %sFill = query.StreamFill[%s, %s, %s]\n", q.goName, q.requestType(), e.value.GoType, q.probeType())
	} else {
		w.p("// %sFill is the query slot of %s: the fill gets the decoded request and the query handle, and returns", q.goName, name)
		w.p("// the response.")
		w.p("type %sFill = query.Fill[%s, %s, %s]\n", q.goName, q.requestType(), q.responseType(), q.probeType())
	}
	var reads []string
	for _, e := range q.reads {
		if q.kind != listKind || e != q.resource {
			reads = append(reads, e.variable)
		}
	}
	// A query that reads the declaration that decides its row is declared in the package internal/decide only.
	if q.decide == nil {
		w.p("// %s is the declaration of the query %s for the query runtime.", q.queryVar(), name)
		if q.kind == listKind && q.resource != nil {
			w.p("var %s = must(query.NewListQuery(%q, %s, %q%s))\n", q.queryVar(), name, q.resource.variable, q.decl.Permission, prefixed(reads))
		} else {
			w.p("var %s = must(query.NewQuery(%q%s))\n", q.queryVar(), name, prefixed(reads))
		}
		w.p("// %s is the declaration of the query %s, which the handler runs.", q.goName, name)
		w.p("func %s() *query.Query { return %s }\n", q.goName, q.queryVar())
	}
	for _, e := range q.reads {
		w.p("// %s%s is the repository of %s through a handle of %s.", q.goName, e.value.GoType+"s", e.value.Entity.Name, name)
		w.p("func %s%ss(h *%sHandle) (*Repository[%s], error) { return query.Repo(h, %s) }\n", q.goName, e.value.GoType, q.goName, e.value.GoType, e.variable)
	}
	if q.kind == streamKind {
		e := q.sent()
		w.p("// %sEntity is the entity that the stream %s sends.", q.goName, name)
		w.p("func %sEntity() *query.Entity[%s] { return %s }\n", q.goName, e.value.GoType, e.variable)
	}
	for _, r := range q.refusals {
		w.p("// %s is the refusal %s of %s, with the status %d and the code %s. Its text is the code alone.", r.goName, r.name, name, r.status, r.code)
		w.p("func %s() error { return %s }\n", r.goName, capability.LowerFirst(r.goName))
		w.p("var %s = query.NewRefusal(%q, %q, %d, %q)\n", capability.LowerFirst(r.goName), name, r.name, r.status, r.code)
	}
}

// prefixed joins names, each after a comma.
func prefixed(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return ", " + strings.Join(names, ", ")
}

// renderRefusalType writes the refusal type of the runtime, the lookup Declared of the declared refusals of each
// query, which the handler gives query.HTTP as its refusal lookup (ST-SUP-RC33-OC07, ST-SUP-RP11-LC17), and the
// constructor helper of the contract package.
func renderRefusalType(w *writer, m *model) {
	w.raw(`// Refusal is one refusal that a query declares (ST-SUP-RC33-OC07): the runtime's type. A fill returns only the
// refusal values of this package, which Declared knows by identity, and its text is its code alone.
type Refusal = query.Refusal

`)
	w.p("// declared are the refusal lookups of the queries that declare refusals, by query name.\nvar declared = map[string]func(error) (int, string, bool){")
	for _, q := range m.queries {
		if len(q.refusals) == 0 {
			continue
		}
		var values []string
		for _, r := range q.refusals {
			values = append(values, capability.LowerFirst(r.goName))
		}
		w.p("%q: query.Lookup(%s),", q.decl.Name, strings.Join(values, ", "))
	}
	w.p("}\n")
	w.raw(`// Declared gives the status and the code of a refusal that the query named queryName declares, and false for every
// other error, such as a refusal of another query or one that a fill made.
func Declared(queryName string, err error) (int, string, bool) {
	lookup, ok := declared[queryName]
	if !ok {
		return 0, "", false
	}
	return lookup(err)
}
`)
	w.raw(`
// must gives v, and panics on a declaration that the runtime refuses, which the generator's checks rule out.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
`)
}

// renderServe is the handler package: one handler per get and list query, served through query.Serve and bound to
// the query's one fill, and the run of each stream query (ST-SUP-RC33-OC02, ST-SUP-RP11-LC17). The bind, the limit,
// the page body and the refusal lookup are the runtime's (ST-STD-RUNTIME-R05).
func renderServe(m *model) []byte {
	var w writer
	hasHTTP, hasStream, hasRPC := false, false, false
	for _, q := range m.queries {
		hasHTTP = hasHTTP || q.kind != streamKind
		hasStream = hasStream || q.kind == streamKind
		hasRPC = hasRPC || q.rpc || q.kind == streamKind
	}
	w.p("// Package serve serves the declared queries of the service (ST-SUP-RC33): one handler for each get and list")
	w.p("// query through query.Serve, and the run of each stream query through query.RunStream, each bound to the query's")
	w.p("// one fill.")
	w.p("package serve\n\nimport (\n\"context\"\n\"errors\"\n\"net/http\"")
	w.p("\n%s %q\nprobes %q", m.pkg, m.contractImport, m.probeImport)
	if m.decided {
		w.p("decide %q", m.contractImport+"/internal/decide")
	}
	for _, q := range m.queries {
		w.p("%s %q", q.fillAlias, q.fillImport)
	}
	if hasRPC {
		w.p("\n%q", protoImport)
	}
	w.p("\n%q\n%q\n)\n", authnImport, queryImport)
	w.raw(`// Config is what the query handlers need.
type Config struct {
	// Runtime runs each query fill in a READ ONLY transaction of the caller's tenant. A list query needs its cursor
	// key.
	Runtime *query.Runtime
	// Verifier verifies the bearer token of each request before its handler binds the call (ST-SUP-RC33-OC04).
	Verifier *authn.Verifier
	// Probes binds the probe call of each query call.
	Probes query.Probes
	// SubjectType is the authorization subject type that a verified identity maps to, such as user.
	SubjectType string
}

// Handlers holds the handler of each get and list query of the service.
type Handlers struct {
	cfg Config`)
	for _, q := range m.queries {
		if q.kind != streamKind {
			w.p("%s http.Handler", capability.LowerFirst(q.goName))
		}
	}
	w.p("}\n")
	w.p("// The fills, each of its query's fill type: a fill that does not implement its query's slot does not compile.\nvar (")
	for _, q := range m.queries {
		w.p("_ %s.%sFill = %s.Fill", m.pkg, q.goName, q.fillAlias)
	}
	w.p(")\n")
	w.p("// New makes the handler of each get and list query of cfg, bound to the query's one fill.\nfunc New(cfg Config) (*Handlers, error) {")
	w.p("if cfg.Runtime == nil || cfg.Verifier == nil || cfg.Probes == nil || cfg.SubjectType == \"\" {\nreturn nil, errors.New(\"serve: the query handlers need the query runtime, the verifier, the probes and the subject type\")\n}")
	w.p("h := &Handlers{cfg: cfg}")
	if hasHTTP {
		w.p("var err error")
	}
	for _, q := range m.queries {
		if q.kind == streamKind {
			continue
		}
		renderServeCall(&w, m, q)
	}
	w.p("return h, nil\n}\n")
	w.p("// Routes mounts the handler of each get and list query at its route, behind the verifier: a request whose token")
	w.p("// the verifier refuses is answered 401 and reaches no transaction and no fill.")
	w.p("func (h *Handlers) Routes(mux *http.ServeMux) {")
	for _, q := range m.queries {
		if q.kind != streamKind {
			w.p("mux.Handle(%q, h.cfg.Verifier.Middleware(h.%s))", q.method+" "+q.path, capability.LowerFirst(q.goName))
		}
	}
	w.p("}\n")
	for _, q := range m.queries {
		switch q.kind {
		case getKind:
			renderGet(&w, m, q)
		case listKind:
			renderList(&w, m, q)
		case streamKind:
			renderStream(&w, m, q)
		}
		renderRPC(&w, m, q)
	}
	if !hasStream {
		w.raw(`// noStream keeps the import of context for a service with no stream query.
var _ context.Context
`)
	}
	return w.Bytes()
}

// renderServeCall writes the query.Serve call of one get or list query in New.
func renderServeCall(w *writer, m *model, q *queryModel) {
	field := capability.LowerFirst(q.goName)
	w.p("if h.%s, err = query.Serve(cfg.Runtime, query.HTTP[%s.%s, %s.%s, %s]{", field, m.pkg, q.requestType(), m.pkg, q.responseType(), q.probeType())
	w.p("Query: %s.%s(),", q.queryPkg(m), q.goName)
	w.p("Bind: query.BindHTTP(cfg.Probes, cfg.SubjectType, %s.%s(), probes.New%sProbe),", q.queryPkg(m), q.goName, q.goName)
	w.p("Decode: decode%s,", q.goName)
	if q.kind == getKind {
		w.p("Fill: decided%s,\nEncode: query.EncodeValue[%s.%s],", q.goName, m.pkg, q.responseType())
	} else {
		w.p("Fill: %s.Fill,", q.fillAlias)
		w.p("Next: func(res %s.%s) query.Cursor { return res.Next },", m.pkg, q.responseType())
		w.p("Encode: func(res %s.%s, next string) ([]byte, error) { return query.EncodePage(res.Items, next) },", m.pkg, q.responseType())
	}
	w.p("Refusal: func(err error) (int, string, bool) { return %s.Declared(%q, err) },", m.pkg, q.decl.Name)
	w.p("}); err != nil {\nreturn nil, err\n}")
}

// memberRead is the expression that reads a get query's resource member from the request r.
func (q *queryModel) memberRead() string {
	if q.inPath {
		return "r.PathValue(" + strconv.Quote(q.member) + ")"
	}
	return "r.URL.Query().Get(" + strconv.Quote(q.member) + ")"
}

// renderGet writes the decode function and the decided fill of a get query: the fill runs only after the query's
// permission on its resource is allowed, and a deny is not found, as a missing row is (ST-SUP-RC33-OC05,
// ST-SUP-RP11-LC06).
func renderGet(w *writer, m *model, q *queryModel) {
	e := q.resource
	w.p("// decode%s reads the request of %s: the ID of its %s.", q.goName, q.decl.Name, e.value.Entity.Name)
	w.p("func decode%s(r *http.Request, _ query.Cursor) (%s.%s, error) {", q.goName, m.pkg, q.requestType())
	w.p("id := %s\nif id == \"\" {\nreturn %s.%s{}, query.ErrBadRequest\n}", q.memberRead(), m.pkg, q.requestType())
	w.p("return %s.%s{%s: id}, nil\n}\n", m.pkg, q.requestType(), q.memberGo)
	w.p("// decided%s decides the permission %s on the %s of the request at the floor before it calls the fill %s,", q.goName, q.decl.Permission, e.value.Entity.Name, q.decl.Fill[0])
	w.p("// once. A row that is not there and a deny are both not found, and neither reaches the fill.")
	w.p("func decided%s(ctx context.Context, req %s.%s, h *%s.%sHandle) (%s.%s, error) {", q.goName, m.pkg, q.requestType(), m.pkg, q.goName, m.pkg, q.responseType())
	if q.decide != nil {
		w.p("if err := decide.Decide%s(ctx, h, req.%s); err != nil {\nreturn nil, err\n}", q.goName, q.memberGo)
	} else {
		w.p("repo, err := %s.%s%ss(h)\nif err != nil {\nreturn nil, err\n}", m.pkg, q.goName, e.value.GoType)
		w.p("if _, err := repo.Get(ctx, req.%s); err != nil {\nreturn nil, err\n}", q.memberGo)
	}
	w.p("return %s.Fill(ctx, req, h)\n}\n", q.fillAlias)
}

// renderList writes the decode function of a list query: the cursor that Serve read and the limit parameter.
func renderList(w *writer, m *model, q *queryModel) {
	w.p("// decode%s reads the request of %s: the cursor after which the page starts and the page's limit.", q.goName, q.decl.Name)
	w.p("func decode%s(r *http.Request, after query.Cursor) (%s.%s, error) {", q.goName, m.pkg, q.requestType())
	w.p("limit, err := query.Limit(r)\nif err != nil {\nreturn %s.%s{}, err\n}", m.pkg, q.requestType())
	w.p("return %s.%s{After: after, Limit: limit}, nil\n}\n", m.pkg, q.requestType())
}

// renderStream writes the bind and the run of a stream query: the transport of the stream binds the verified caller
// and gives each message to send; RunStream decides each value when the fill sends it (ST-SUP-RP11-LC13).
func renderStream(w *writer, m *model, q *queryModel) {
	e := q.sent()
	w.p("// Bind%s gives the bound call of the stream %s for the verified identity of ctx, with a request ID that the", q.goName, q.decl.Name)
	w.p("// server makes (ST-SUP-RP11-LC19).")
	w.p("func (h *Handlers) Bind%s(ctx context.Context) (query.Bound[%s], error) {", q.goName, q.probeType())
	w.p("return query.BindContext(ctx, h.cfg.Probes, h.cfg.SubjectType, %s.%s(), probes.New%sProbe, query.NewRequestID())\n}\n", m.pkg, q.goName, q.goName)
	w.p("// Stream%s runs the stream %s with its fill %s in one READ ONLY transaction: send writes each allowed %s to", q.goName, q.decl.Name, q.decl.Fill[0], e.value.Entity.Name)
	w.p("// the client. The count of dropped values that it returns is for the service's own telemetry, never the client.")
	w.p("func (h *Handlers) Stream%s(ctx context.Context, b query.Bound[%s], req %s.%s, send func(%s.%s) error) (uint64, error) {",
		q.goName, q.probeType(), m.pkg, q.requestType(), m.pkg, e.value.GoType)
	w.p("return query.RunStream(ctx, h.cfg.Runtime, %s.%s(), b, req, %s.%sEntity(), send, %s.Fill)\n}\n", m.pkg, q.goName, m.pkg, q.goName, q.fillAlias)
}

// renderRPC writes the gRPC handler of a query that a gRPC method serves (ST-SUP-RC07-OC19): a get or list query
// through query.ServeRPC, with the fill, the decision, Next, Encode and the refusal lookup of its HTTP handler, and a
// stream query through query.ServeStream on its Stream function (ST-SUP-RC33-OC08). The handler is generic in the
// method's messages, which the gRPC application gives with the functions that read the request members of the
// message: the resource member of a get or stream query, and the cursor and the limit of a list query.
func renderRPC(w *writer, m *model, q *queryModel) {
	if !q.rpc && q.kind != streamKind {
		return
	}
	req := m.pkg + "." + q.requestType()
	bind := fmt.Sprintf("query.BindRPC(h.cfg.Probes, h.cfg.SubjectType, %s.%s(), probes.New%sProbe)", q.queryPkg(m), q.goName, q.goName)
	refusal := fmt.Sprintf("func(err error) (int, string, bool) { return %s.Declared(%q, err) }", m.pkg, q.decl.Name)
	switch q.kind {
	case getKind:
		w.p("// Serve%sRPC gives the unary gRPC handler of %s for the method's messages In and Out: the HTTP handler's", q.goName, q.decl.Name)
		w.p("// decision and fill on the %s that member reads from the request message. An empty member is not valid.", q.resource.value.Entity.Name)
		w.p("func Serve%sRPC[In any, Out proto.Message](h *Handlers, member func(In) string, newOut func() Out) (func(context.Context, In) (Out, error), error) {", q.goName)
		w.p("if h == nil || member == nil || newOut == nil {\nreturn nil, errors.New(\"serve: the gRPC handler of %s needs the handlers, the member and the response\")\n}", q.decl.Name)
		w.p("return query.ServeRPC(h.cfg.Runtime, query.RPC[In, Out, %s, %s.%s, %s]{", req, m.pkg, q.responseType(), q.probeType())
		w.p("Query: %s.%s(),\nBind: %s,", q.queryPkg(m), q.goName, bind)
		w.p("Decode: func(in In, _ query.Cursor) (%s, error) {\nid := member(in)\nif id == \"\" {\nreturn %s{}, query.ErrBadRequest\n}\nreturn %s{%s: id}, nil\n},", req, req, req, q.memberGo)
		w.p("Fill: decided%s,\nEncode: query.EncodeValue[%s.%s],\nNewResponse: newOut,\nRefusal: %s,\n})\n}\n", q.goName, m.pkg, q.responseType(), refusal)
	case listKind:
		w.p("// Serve%sRPC gives the unary gRPC handler of %s for the method's messages In and Out: the HTTP handler's fill", q.goName, q.decl.Name)
		w.p("// on the page after the cursor that cursor reads from the request message, of at most the values that limit")
		w.p("// reads, 0 for the default; the response message takes the items and the cursor of the next page as next.")
		w.p("func Serve%sRPC[In any, Out proto.Message](h *Handlers, cursor func(In) string, limit func(In) int64, newOut func() Out) (func(context.Context, In) (Out, error), error) {", q.goName)
		w.p("if h == nil || cursor == nil || newOut == nil {\nreturn nil, errors.New(\"serve: the gRPC handler of %s needs the handlers, the cursor and the response\")\n}", q.decl.Name)
		w.p("return query.ServeRPC(h.cfg.Runtime, query.RPC[In, Out, %s, %s.%s, %s]{", req, m.pkg, q.responseType(), q.probeType())
		w.p("Query: %s.%s(),\nBind: %s,\nCursor: cursor,", q.queryPkg(m), q.goName, bind)
		w.p("Decode: func(in In, after query.Cursor) (%s, error) {\nvar n int64\nif limit != nil {\nn = limit(in)\n}\nif n < 0 || n > query.MaxLimit {\nreturn %s{}, query.ErrBadRequest\n}\nreturn %s{After: after, Limit: int(n)}, nil\n},", req, req, req)
		w.p("Fill: %s.Fill,", q.fillAlias)
		w.p("Next: func(res %s.%s) query.Cursor { return res.Next },", m.pkg, q.responseType())
		w.p("Encode: func(res %s.%s, next string) ([]byte, error) { return query.EncodePage(res.Items, next) },", m.pkg, q.responseType())
		w.p("NewResponse: newOut,\nRefusal: %s,\n})\n}\n", refusal)
	case streamKind:
		e := q.sent()
		w.p("// Serve%sStream gives the server stream handler of %s for the method's messages In and Out, the only", q.goName, q.decl.Name)
		w.p("// handler of the query (ST-SUP-RC33-OC08): it runs Stream%s once, and sends each allowed %s as one message.", q.goName, e.value.Entity.Name)
		params, decode := "", fmt.Sprintf("func(In) (%s, error) { return %s{}, nil }", req, req)
		if q.member != "" {
			params = "member func(In) string, "
			decode = fmt.Sprintf("func(in In) (%s, error) {\nif member == nil {\nreturn %s{}, query.ErrBadRequest\n}\nid := member(in)\nif id == \"\" {\nreturn %s{}, query.ErrBadRequest\n}\nreturn %s{%s: id}, nil\n}", req, req, req, req, q.memberGo)
		}
		w.p("func Serve%sStream[In any, Out proto.Message](h *Handlers, %snewOut func() Out) (func(context.Context, In, func(Out) error) error, error) {", q.goName, params)
		w.p("if h == nil || newOut == nil {\nreturn nil, errors.New(\"serve: the stream handler of %s needs the handlers and the message\")\n}", q.decl.Name)
		w.p("return query.ServeStream(query.StreamRPC[In, Out, %s, %s.%s, %s]{", req, m.pkg, e.value.GoType, q.probeType())
		w.p("Bind: %s,\nDecode: %s,\nStream: h.Stream%s,\nNewMessage: newOut,\nRefusal: %s,\n})\n}\n", bind, decode, q.goName, refusal)
	}
}

// queryPkg is the package that declares the query: internal/decide for a get query that decides a permission of its
// own, else the contract.
func (q *queryModel) queryPkg(m *model) string {
	if q.decide != nil {
		return "decide"
	}
	return m.pkg
}

// renderDecide is the package internal/decide under the contract: for each get query whose permission is not the
// read permission of its resource, the declaration that decides that permission on the requested row, the query that
// reads it, and the decision the handler makes before the fill (ST-SUP-RC33-OC05). A fill cannot import a package
// under internal of the contract, so its repositories keep the entity's read permission.
func renderDecide(m *model) []byte {
	var w writer
	sensitive := false
	for _, q := range m.queries {
		if q.decide == nil {
			continue
		}
		for _, f := range q.decide.value.Fields {
			sensitive = sensitive || f.Sensitivity == "pii" || f.Sensitivity == "secret"
		}
	}
	w.p("// Package decide holds the declarations that decide the permission of a get query on its requested row, when it")
	w.p("// is not the read permission of the entity (ST-SUP-RC33-OC05). Only the handler package imports it.")
	w.p("package decide\n\nimport (\n\"context\"\n\n%s %q", m.pkg, m.contractImport)
	if sensitive {
		w.p("%q", auditImport)
	}
	w.p("%q\n)\n", queryImport)
	for _, q := range m.queries {
		e := q.decide
		if e == nil {
			continue
		}
		renderSpec(&w, e, m.pkg+".Load"+e.value.GoType)
		var reads []string
		for _, r := range q.reads {
			reads = append(reads, m.pkg+".Entity"+r.value.GoType+"()")
		}
		w.p("// %s is the declaration of the query %s: it reads the row it decides and the entities it declares.", q.queryVar(), q.decl.Name)
		w.p("var %s = must(query.NewQuery(%q, %s%s))\n", q.queryVar(), q.decl.Name, e.variable, prefixed(reads))
		w.p("// %s is the declaration of the query %s, which the handler runs.", q.goName, q.decl.Name)
		w.p("func %s() *query.Query { return %s }\n", q.goName, q.queryVar())
		w.p("// Decide%s decides the permission %s on the %s id at the floor, before the fill of %s; a row that is not", q.goName, q.decl.Permission, e.value.Entity.Name, q.decl.Name)
		w.p("// there and a deny are both not found.")
		w.p("func Decide%s(ctx context.Context, h *%s.%sHandle, id string) error {", q.goName, m.pkg, q.goName)
		w.p("repo, err := query.Repo(h, %s)\nif err != nil {\nreturn err\n}\n_, err = repo.Get(ctx, id)\nreturn err\n}\n", e.variable)
	}
	w.raw(`// must gives v, and panics on a declaration that the runtime refuses, which the generator's checks rule out.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}`)
	return w.Bytes()
}

// ViewFile gives a Go file of the package pkg that holds, for each named entity, its read-only value type <Name>View
// with no secret field, its loaded type, its load function and its query.Entity with the read and list permissions
// that the entity declares, as the query contract holds them (ST-SUP-RC33-OC01); the row reads are the runtime's.
// It is for a generator that declares queries of its own, such as the rest-api generator for generated CRUD
// (ST-SUP-RC20-OC37), so the read-only value has one source (ST-SUP-RC33-G02). It refuses an entity that a query
// cannot read.
func ViewFile(pkg string, names []string, entities []types.Entity) ([]byte, error) {
	declared := map[string]types.Entity{}
	for _, e := range entities {
		declared[e.Name] = e
	}
	m := &model{}
	byName := map[string]*entity{}
	for _, name := range names {
		e, err := entityOf(name, declared, byName, m)
		if err != nil {
			return nil, fmt.Errorf("query: the entity %s: %w", name, err)
		}
		e.value.GoType = e.value.Entity.Name + "View"
		e.variable = capability.LowerFirst(e.value.GoType) + "Entity"
	}
	used := m.reads()
	var w writer
	w.p("package %s\n\nimport (", pkg)
	if used[capability.ReadJSON] {
		w.p("\"encoding/json\"")
	}
	if used[capability.ReadTime] {
		w.p("\"time\"")
	}
	w.p("")
	if m.sensitive() {
		w.p("%q", auditImport)
	}
	w.p("%q\n)\n", queryImport)
	for _, e := range m.entities {
		renderEntity(&w, e)
	}
	w.raw(`// must gives v, and panics on a declaration that the runtime refuses, which the generator's checks rule out.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
`)
	source, err := format.Source(w.Bytes())
	if err != nil {
		return nil, fmt.Errorf("query: formatting the read-only values: %w\n%s", err, w.Bytes())
	}
	return source, nil
}
