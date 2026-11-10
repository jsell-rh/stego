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
)

// The import paths of the runtime library that the generated code uses.
const (
	queryImport = "stego.dev/runtime/query"
	auditImport = "stego.dev/runtime/audit"
	authnImport = "stego.dev/runtime/authn"
	authzImport = "stego.dev/runtime/authz"
	probeImport = "stego.dev/runtime/probe"
	tenImport   = "stego.dev/runtime/tenant"
)

// render gives the contract package and the handler package of the model.
func render(m *model) ([]gen.File, error) {
	var files []gen.File
	for _, f := range []struct {
		name   string
		source []byte
	}{
		{path.Join(m.namespace, m.pkg+".go"), renderContract(m)},
		{path.Join(m.namespace, "serve", "serve.go"), renderServe(m)},
	} {
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
	w.p("\"errors\"")
	if used[capability.ReadFloat] || used[capability.ReadBytes] || used[capability.ReadJSON] || used[capability.ReadInt32] {
		w.p("\"fmt\"")
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
	renderRefusalType(&w)
	renderReadHelpers(&w, used)
	return w.Bytes()
}

// renderEntity writes the read-only value, the loaded type, the load function and the query.Entity of one entity.
func renderEntity(w *writer, e *entity) {
	v := e.value
	if e.local {
		renderSpec(w, e)
		return
	}
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
	renderSpec(w, e)
}

// renderSpec writes the query.Entity of one entity: its table, its columns, its authorization and the sensitivity of
// its fields.
func renderSpec(w *writer, e *entity) {
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
	if e.local {
		w.p("// %s is the declaration of %s for the get query that decides the permission %s on it.", e.variable, v.Entity.Name, e.readPermission)
	} else {
		w.p("// %s is the declaration of %s for the query runtime: its table, its columns, its authorization and the", e.variable, v.Entity.Name)
		w.p("// sensitivity of its fields.")
	}
	w.p("var %s = must(query.NewEntity(query.Spec{Name: %q, Schema: \"public\", Table: %q, Key: \"id\",", e.variable, v.Entity.Name, v.Table)
	w.p("Columns: []string{%s},", strings.Join(columns, ", "))
	if e.resourceType != "" {
		w.p("ResourceType: %q, ReadPermission: %q, ListPermission: %q,", e.resourceType, e.readPermission, e.listPermission)
	}
	if len(sensitive) > 0 {
		w.p("Fields: audit.Fields{%s},", strings.Join(sensitive, ", "))
	}
	w.p("}, load%s))\n", v.GoType)
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
		return "readInt32(r, " + c + ")"
	case capability.ReadBool:
		return "r.Bool(" + c + ")"
	case capability.ReadTime:
		return "r.Time(" + c + ")"
	case capability.ReadFloat:
		return "readFloat(r, " + c + ")"
	case capability.ReadBytes:
		return "readBytes(r, " + c + ")"
	}
	return "readJSON(r, " + c + ")"
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
	w.p("// %s is the declaration of the query %s for the query runtime.", q.queryVar(), name)
	if q.kind == listKind && q.resource != nil {
		w.p("var %s = must(query.NewListQuery(%q, %s, %q%s))\n", q.queryVar(), name, q.resource.variable, q.decl.Permission, prefixed(reads))
	} else {
		w.p("var %s = must(query.NewQuery(%q%s))\n", q.queryVar(), name, prefixed(reads))
	}
	w.p("// %s is the declaration of the query %s, which the handler runs.", q.goName, name)
	w.p("func %s() *query.Query { return %s }\n", q.goName, q.queryVar())
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
		w.p("func %s() error { return &Refusal{query: %q, name: %q, status: %d, code: %q} }\n", r.goName, name, r.name, r.status, r.code)
	}
}

// prefixed joins names, each after a comma.
func prefixed(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return ", " + strings.Join(names, ", ")
}

// renderRefusalType writes the refusal type and the constructor helper of the contract package.
func renderRefusalType(w *writer) {
	w.raw(`// Refusal is one refusal that a query declares (ST-SUP-RC33-OC07). Its fields are unexported, so a fill returns
// only the refusal values of this package, and its text is its code alone, never a text of the fill.
type Refusal struct {
	query, name, code string
	status            int
}

// Error is the refusal's code.
func (r *Refusal) Error() string {
	if r == nil {
		return "refused"
	}
	return r.code
}

// Declared gives the status and the code of a refusal that the query named queryName declares, and false for every
// other error, such as a refusal of another query or one that new made.
func Declared(queryName string, err error) (int, string, bool) {
	var r *Refusal
	if !errors.As(err, &r) || r == nil || r.query == "" || r.query != queryName {
		return 0, "", false
	}
	return r.status, r.code, true
}

// must gives v, and panics on a declaration that the runtime refuses, which the generator's checks rule out.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
`)
}

// renderReadHelpers writes the read helpers of the column kinds that query.Row does not read itself.
func renderReadHelpers(w *writer, used map[string]bool) {
	if used[capability.ReadInt32] {
		w.raw(`// readInt32 reads an integer column; NULL is 0.
func readInt32(r query.Row, column string) (int32, error) {
	v, err := r.Int(column)
	if err != nil {
		return 0, err
	}
	if v < -1<<31 || v > 1<<31-1 {
		return 0, fmt.Errorf("the column %s holds a value out of the int32 range", column)
	}
	return int32(v), nil
}
`)
	}
	if used[capability.ReadFloat] {
		w.raw(`// readFloat reads a floating point column; NULL is 0.
func readFloat(r query.Row, column string) (float64, error) {
	v, _ := r.Value(column)
	switch x := v.(type) {
	case nil:
		return 0, nil
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	}
	return 0, fmt.Errorf("the column %s is not a number", column)
}
`)
	}
	if used[capability.ReadBytes] {
		w.raw(`// readBytes reads a bytea column; NULL is nil.
func readBytes(r query.Row, column string) ([]byte, error) {
	v, _ := r.Value(column)
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return x, nil
	}
	return nil, fmt.Errorf("the column %s is not bytes", column)
}
`)
	}
	if used[capability.ReadJSON] {
		w.raw(`// readJSON reads a jsonb column; NULL is nil.
func readJSON(r query.Row, column string) (json.RawMessage, error) {
	v, _ := r.Value(column)
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return json.RawMessage(x), nil
	case string:
		return json.RawMessage(x), nil
	}
	return nil, fmt.Errorf("the column %s is not a document", column)
}
`)
	}
}

// renderServe is the handler package: one handler per get and list query, served through query.Serve and bound to
// the query's one fill, and the run function of each stream query (ST-SUP-RC33-OC02, ST-SUP-RP11-LC17).
func renderServe(m *model) []byte {
	var w writer
	hasList, hasGet, hasHTTP := false, false, false
	for _, q := range m.queries {
		hasList = hasList || q.kind == listKind
		hasGet = hasGet || q.kind == getKind
		hasHTTP = hasHTTP || q.kind != streamKind
	}
	w.p("// Package serve serves the declared queries of the service (ST-SUP-RC33): one handler for each get and list")
	w.p("// query through query.Serve, and the run of each stream query through query.RunStream, each bound to the query's")
	w.p("// one fill.")
	w.p("package serve\n\nimport (\n\"context\"")
	if hasHTTP {
		w.p("\"encoding/json\"")
	}
	w.p("\"errors\"\n\"net/http\"")
	if hasList {
		w.p("\"strconv\"")
	}
	w.p("\n%s %q\nprobes %q", m.pkg, m.contractImport, m.probeImport)
	for _, q := range m.queries {
		w.p("%s %q", q.fillAlias, q.fillImport)
	}
	w.p("\n%q\n%q\n%q\n%q\n%q\n)\n", authnImport, authzImport, probeImport, queryImport, tenImport)
	w.raw(`// Probes binds the probe call of one query call to its span context, tenant, principal and query name; the probe
// runtime and its recorder do.
type Probes interface {
	Bind(ctx context.Context, tenant, principal, operation string) *probe.Call
}

// Config is what the query handlers need.
type Config struct {
	// Runtime runs each query fill in a READ ONLY transaction of the caller's tenant. A list query needs its cursor
	// key.
	Runtime *query.Runtime
	// Verifier verifies the bearer token of each request before its handler binds the call (ST-SUP-RC33-OC04).
	Verifier *authn.Verifier
	// Probes binds the probe call of each query call.
	Probes Probes
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
	}
	w.raw(`// bind gives the bound call of a query for the verified identity of ctx: the tenant of its verified claim, the
// principal of its subject and the probe call of the query, bound to both and to the query name. An error is 401.
func bind[P any](ctx context.Context, cfg Config, q *query.Query, newProbe func(*probe.Probe) P, requestID string) (query.Bound[P], error) {
	id, ok := authn.FromContext(ctx)
	if !ok {
		return query.Bound[P]{}, errors.New("serve: the request holds no verified identity")
	}
	t, err := tenant.New(tenant.Claim(id))
	if err != nil {
		return query.Bound[P]{}, err
	}
	p, err := authz.Person(cfg.SubjectType, id)
	if err != nil {
		return query.Bound[P]{}, err
	}
	call := cfg.Probes.Bind(ctx, t.ID(), p.Subject().String(), q.Name())
	return query.Bound[P]{Tenant: t, Principal: p, Call: call, Probe: newProbe, RequestID: requestID}, nil
}
`)
	if hasGet {
		w.raw(`// encodeValue writes the response of a get query as JSON.
func encodeValue(res any, _ string) ([]byte, error) { return json.Marshal(res) }
`)
	}
	if hasList {
		w.raw(`// page is the body of a list response: its items and the sealed text of the next cursor, absent at the end.
type page struct {
	Items any    ` + "`json:\"items\"`" + `
	Next  string ` + "`json:\"next,omitempty\"`" + `
}

// limitOf reads the limit parameter of a list request: absent is 0, the default page.
func limitOf(r *http.Request) (int, error) {
	text := r.URL.Query().Get("limit")
	if text == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(text)
	if err != nil || limit < 1 || limit > query.MaxLimit {
		return 0, query.ErrBadRequest
	}
	return limit, nil
}
`)
	}
	return w.Bytes()
}

// renderServeCall writes the query.Serve call of one get or list query in New.
func renderServeCall(w *writer, m *model, q *queryModel) {
	field := capability.LowerFirst(q.goName)
	w.p("if h.%s, err = query.Serve(cfg.Runtime, query.HTTP[%s.%s, %s.%s, %s]{", field, m.pkg, q.requestType(), m.pkg, q.responseType(), q.probeType())
	w.p("Query: %s.%s(),", m.pkg, q.goName)
	w.p("Bind: func(r *http.Request) (query.Bound[%s], error) {", q.probeType())
	w.p("return bind(r.Context(), cfg, %s.%s(), probes.New%sProbe, r.Header.Get(\"X-Request-Id\"))\n},", m.pkg, q.goName, q.goName)
	w.p("Decode: decode%s,", q.goName)
	if q.kind == getKind {
		w.p("Fill: decided%s,\nEncode: encodeValue,", q.goName)
	} else {
		w.p("Fill: %s.Fill,", q.fillAlias)
		w.p("Next: func(res %s.%s) query.Cursor { return res.Next },", m.pkg, q.responseType())
		w.p("Encode: func(res %s.%s, next string) ([]byte, error) { return json.Marshal(page{Items: res.Items, Next: next}) },", m.pkg, q.responseType())
	}
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
	w.p("repo, err := %s.%s%ss(h)\nif err != nil {\nreturn nil, err\n}", m.pkg, q.goName, e.value.GoType)
	w.p("if _, err := repo.Get(ctx, req.%s); err != nil {\nreturn nil, err\n}", q.memberGo)
	w.p("return %s.Fill(ctx, req, h)\n}\n", q.fillAlias)
}

// renderList writes the decode function of a list query: the cursor that Serve read and the limit parameter.
func renderList(w *writer, m *model, q *queryModel) {
	w.p("// decode%s reads the request of %s: the cursor after which the page starts and the page's limit.", q.goName, q.decl.Name)
	w.p("func decode%s(r *http.Request, after query.Cursor) (%s.%s, error) {", q.goName, m.pkg, q.requestType())
	w.p("limit, err := limitOf(r)\nif err != nil {\nreturn %s.%s{}, err\n}", m.pkg, q.requestType())
	w.p("return %s.%s{After: after, Limit: limit}, nil\n}\n", m.pkg, q.requestType())
}

// renderStream writes the bind and the run of a stream query: the transport of the stream binds the verified caller
// and gives each message to send; RunStream decides each value when the fill sends it (ST-SUP-RP11-LC13).
func renderStream(w *writer, m *model, q *queryModel) {
	e := q.sent()
	w.p("// Bind%s gives the bound call of the stream %s for the verified identity of ctx.", q.goName, q.decl.Name)
	w.p("func (h *Handlers) Bind%s(ctx context.Context, requestID string) (query.Bound[%s], error) {", q.goName, q.probeType())
	w.p("return bind(ctx, h.cfg, %s.%s(), probes.New%sProbe, requestID)\n}\n", m.pkg, q.goName, q.goName)
	w.p("// Stream%s runs the stream %s with its fill %s in one READ ONLY transaction: send writes each allowed %s to", q.goName, q.decl.Name, q.decl.Fill[0], e.value.Entity.Name)
	w.p("// the client. The count of dropped values that it returns is for the service's own telemetry, never the client.")
	w.p("func (h *Handlers) Stream%s(ctx context.Context, b query.Bound[%s], req %s.%s, send func(%s.%s) error) (uint64, error) {",
		q.goName, q.probeType(), m.pkg, q.requestType(), m.pkg, e.value.GoType)
	w.p("return query.RunStream(ctx, h.cfg.Runtime, %s.%s(), b, req, %s.%sEntity(), send, %s.Fill)\n}\n", m.pkg, q.goName, m.pkg, q.goName, q.fillAlias)
}
