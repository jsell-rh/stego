package restapi

import (
	"bytes"
	"fmt"
	"go/format"
	"path"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/command"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/query"
	"github.com/jsell-rh/stego/internal/types"
)

// The runtime data path (gen.Context.RuntimeData) of the rest-api component: each declared collection operation is
// one generated command on stego.dev/runtime/command (create, update, patch, delete) or one query on
// stego.dev/runtime/query (read, list), and each handler calls that command's or query's Run. The handlers open no
// database connection and hold no store; the bound before slots run in the command's fill, before its write, and
// the after slots run after the command committed, so tenancy, idempotency, audit and authorization are applied once,
// by the runtime (ST-SUP-RC20-OC37).

// runtimeIdentity is the auth alias of the slot callers on the runtime data path: the caller is the verified identity
// of the request (ST-SUP-RP13).
const runtimeIdentity = "\x00authn"

// dataFolder is the folder, under the component's namespace, of the entity declarations of the command and the
// query runtime.
const dataFolder = "data"

// runtimeSlots are the slots that the handlers on the runtime data path call.
var runtimeSlots = map[string]bool{
	"before_create": true, "before_patch": true, "before_delete": true, "validate": true,
	"after_create": true, "after_patch": true, "after_delete": true, "on_entity_changed": true,
}

// runtimeFieldTypes are the field types whose Go types are the same in the API type and in the command runtime's
// row type, so a handler converts one into the other.
var runtimeFieldTypes = map[types.FieldType]bool{
	types.FieldTypeString: true, types.FieldTypeEnum: true, types.FieldTypeRef: true, types.FieldTypeInt32: true,
	types.FieldTypeInt64: true, types.FieldTypeFloat: true, types.FieldTypeDouble: true, types.FieldTypeBool: true,
	types.FieldTypeTimestamp: true,
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
}

// permission gives the permission that the operation decides, and empty for a public operation.
func (c *runtimeCollection) permission(op types.Operation) string {
	p := c.entity.Permissions[op]
	if p == types.PermissionPublic {
		return ""
	}
	return p
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
	if !ctx.RuntimeData {
		return nil
	}
	if ctx.Tenancy == nil || ctx.Tenancy.Key != "tenant_id" {
		return fmt.Errorf("rest-api: on the runtime data path each operation runs in the transaction of one tenant, so the service needs pooled tenancy with the tenant column tenant_id")
	}
	if ctx.Authorization == nil || ctx.Authorization.SubjectFromIdentity == "" {
		return fmt.Errorf("rest-api: on the runtime data path each operation decides its permission for the verified principal, so the service needs authorization with subject_from_identity")
	}
	for _, b := range ctx.SlotBindings {
		if !runtimeSlots[b.Slot] {
			return fmt.Errorf("rest-api: collection %s binds the slot %s, which the runtime data path does not call yet", b.Collection, b.Slot)
		}
	}
	for _, c := range ctx.Collections {
		e := entities[c.Entity]
		switch {
		case c.ParentEntity() != "" || len(c.Scope) > 0:
			return fmt.Errorf("rest-api: collection %s is scoped to a parent, which the runtime data path does not serve yet", c.Name)
		case len(c.Implicit) > 0:
			return fmt.Errorf("rest-api: collection %s has implicit fields, which the runtime data path does not serve yet", c.Name)
		case c.PathPrefix != "":
			return fmt.Errorf("rest-api: collection %s has a path prefix, which the runtime data path does not serve yet", c.Name)
		case e.ResourceType == "":
			return fmt.Errorf("rest-api: collection %s reads the entity %s, which has no resource type; on the runtime data path every operation is decided", c.Name, e.Name)
		}
		for _, op := range c.Operations {
			switch op {
			case types.OpUpsert:
				return fmt.Errorf("rest-api: collection %s declares upsert, which the runtime data path does not serve yet", c.Name)
			case types.OpUpdate, types.OpPatch, types.OpDelete:
				// Every write of a row decides a permission on it before the fill; a write with none would run
				// undecided (ST-SUP-RC29-OC01).
				if p := e.Permissions[op]; p == "" || p == types.PermissionPublic {
					return fmt.Errorf("rest-api: collection %s declares %s with the permission %q; on the runtime data path a write of a row decides a permission of %s on it", c.Name, op, p, e.ResourceType)
				}
			case types.OpCreate:
				// A top-level create has no row to decide on before the fill (ST-SUP-RP10-LC31); ST-SUP-Q85 asks
				// which resource it decides on.
				if e.Permissions[op] != types.PermissionPublic {
					return fmt.Errorf("rest-api: collection %s declares create with the permission %q; on the runtime data path a create of a top-level entity has no resource to decide on, so it must be public with a reason (ST-SUP-Q85)", c.Name, e.Permissions[op])
				}
			}
		}
		for _, f := range e.Fields {
			if !runtimeFieldTypes[f.Type] {
				return fmt.Errorf("rest-api: entity %s has the field %s of type %s, which the runtime data path does not serve yet", e.Name, f.Name, f.Type)
			}
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
	entityFile, err := command.EntityFile(dataFolder, names, ctx.Entities, ctx.Tenancy.Key)
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

// renderRuntimeHelpers writes runtime.go: the Runtime that the handlers run on, the command handler steps of the
// command generator (command.RunHelpers), and the answers of the errors of Run.
func renderRuntimeHelpers(ctx gen.Context, names []string, dataImport, slotsImportPath string, hasSlots bool, keys *idempotencyHeader) ([]byte, error) {
	var b bytes.Buffer
	pkg := path.Base(ctx.OutputNamespace)
	fmt.Fprintf(&b, "package %s\n\nimport (\n\t\"context\"\n\t\"errors\"\n\t\"log/slog\"\n\t\"net\"\n\t\"net/http\"\n\t\"sync/atomic\"\n\t\"time\"\n\n", pkg)
	fmt.Fprintf(&b, "\t\"stego.dev/runtime/audit\"\n\t\"stego.dev/runtime/authn\"\n\t\"stego.dev/runtime/authz\"\n\t\"stego.dev/runtime/command\"\n")
	fmt.Fprintf(&b, "\t\"stego.dev/runtime/idempotency\"\n\t\"stego.dev/runtime/probe\"\n\t\"stego.dev/runtime/query\"\n\t\"stego.dev/runtime/tenant\"\n\n")
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
	b.WriteString(command.RunHelpers)
	if hasSlots {
		b.WriteString(slotCallerHelper)
	}
	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("rest-api: formatting runtime.go: %w\n%s", err, b.Bytes())
	}
	return formatted, nil
}

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

// afterSlotFailed records an after slot that failed after its command committed: the write and its key row are
// stored, so the handler still answers the committed response, and the log names the slot and holds no error text
// (ST-SUP-Q89).
func afterSlotFailed(ctx context.Context, slot string) {
	afterSlotFailures.Add(1)
	slog.ErrorContext(ctx, "an after slot failed after its command committed", "slot", slot)
}

// invalidRequest is a request body that does not decode into the collection's type.
type invalidRequest struct{ problem *ServiceError }

func (e *invalidRequest) Error() string { return "the request is not valid" }

// requestKey is the idempotency key of a write that takes no key from the client: a PUT, a DELETE, or a create or
// a patch without the header when the declaration does not require it. Run takes every write inside the
// idempotency wrapper, so the request gets a key of its own that no retry sends again (ST-SUP-Q86).
func requestKey(requestID string) string { return "request:" + requestID }

// commandRequest gives what Run knows of one write.
func (rt *Runtime) commandRequest(r *http.Request, b bound, key, fingerprint, requestID string, body []byte, resource string) command.Request {
	return command.Request{Tenant: b.tenant, Principal: b.principal, Key: key, Fingerprint: fingerprint, Body: body,
		Resource: resource, RequestID: requestID, ActorIP: remoteIP(r.RemoteAddr), UserAgent: r.UserAgent(),
		Retention: rt.Retention}
}

// boundQuery gives the bound call of a read for the verified tenant and principal, with its probe call.
func (rt *Runtime) boundQuery(ctx context.Context, b bound, q *query.Query) query.Bound[struct{}] {
	call := rt.Probes.Bind(ctx, b.tenant.ID(), b.principal.Subject().String(), q.Name())
	return query.Bound[struct{}]{Tenant: b.tenant, Principal: b.principal, Call: call, Probe: noProbe, RequestID: data.NewID()}
}

// answerWrite answers an error of Run and writes the audit row of the denied or failed write in its own short
// transaction (ST-SUP-RC28-OC03): a slot refusal is the slot's status and outcome denied, a deny is 403, a conflict
// 409 and every other error 500. A row that is not there is 403 and outcome denied for an operation that decides a
// permission, as a deny is, so the answer never tells that a row exists or once existed (ST-SUP-Q88), and 404
// otherwise. No error text reaches the answer or the audit row.
func (rt *Runtime) answerWrite(w http.ResponseWriter, r *http.Request, req command.Request, action, resourceType, kind string, decided bool, err error) {
	var refused *slotRefusal
	var invalid *invalidRequest
	switch {
	case errors.As(err, &invalid):
		handleError(w, r, invalid.problem)
		return
	case errors.As(err, &refused):
		status := refused.status
		if status <= 0 {
			status = http.StatusForbidden
		}
		recordOutcome(r.Context(), rt.Commands, req, action, resourceType, outcome{status: status, code: "slot_refused", audit: audit.OutcomeDenied})
		handleError(w, r, errorForStatus(status, refused.detail))
		return
	case errors.Is(err, command.ErrNotFound) && decided:
		recordOutcome(r.Context(), rt.Commands, req, action, resourceType, outcome{status: http.StatusForbidden, code: "denied", audit: audit.OutcomeDenied})
		handleError(w, r, Forbidden("the caller may not "+action+" this "+kind))
		return
	case errors.Is(err, command.ErrNotFound):
		recordOutcome(r.Context(), rt.Commands, req, action, resourceType, outcome{status: http.StatusNotFound, code: "not_found", audit: audit.OutcomeFailed})
		handleError(w, r, NotFound(kind, req.Resource))
		return
	}
	o := failedOutcome(err)
	recordOutcome(r.Context(), rt.Commands, req, action, resourceType, o)
	switch o.status {
	case http.StatusForbidden:
		handleError(w, r, Forbidden("the caller may not "+action+" this "+kind))
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
	fmt.Fprintf(buf, "\t\t\tafterSlotFailed(r.Context(), %q)\n", param.SlotName)
}

// runtimeHandlerWriter writes one handler file.
type runtimeHandlerWriter struct {
	bytes.Buffer
	c          *runtimeCollection
	ctx        gen.Context
	keys       *idempotencyHeader
	envelope   bool
	slotsAlias string
}

func (w *runtimeHandlerWriter) f(format string, args ...any) { fmt.Fprintf(&w.Buffer, format, args...) }

// renderRuntimeHandler writes the handler file of one collection: its command and query declarations, its handler
// type, and one method per declared operation that calls Run.
func renderRuntimeHandler(ctx gen.Context, c *runtimeCollection, dataImport, slotsImportPath string, keys *idempotencyHeader) ([]byte, error) {
	w := &runtimeHandlerWriter{c: c, ctx: ctx, keys: keys, envelope: ctx.Conventions.ResponseFormat == "envelope"}
	if len(c.slots) > 0 {
		w.slotsAlias = "slots"
	}
	e := c.entity
	hasWrite := c.ops[types.OpCreate] || c.ops[types.OpUpdate] || c.ops[types.OpPatch] || c.ops[types.OpDelete]
	hasBody := c.ops[types.OpCreate] || c.ops[types.OpUpdate] || c.ops[types.OpPatch]
	needTime, needFmt := false, false
	for _, f := range e.Fields {
		if f.Type == types.FieldTypeTimestamp && (hasBody || f.Sensitivity == "secret") {
			needTime = true
		}
	}
	if len(c.slots) > 0 && needsFmtForSlotFields(e) {
		needFmt = true
	}
	needUUID := w.envelope && c.ops[types.OpCreate]
	w.f("package %s\n\nimport (\n\t\"context\"\n\t\"encoding/json\"\n", path.Base(ctx.OutputNamespace))
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
	if hasWrite {
		w.f("\t\"stego.dev/runtime/command\"\n\t\"stego.dev/runtime/idempotency\"\n")
	}
	if c.ops[types.OpRead] || c.ops[types.OpList] {
		w.f("\t\"stego.dev/runtime/query\"\n")
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
	w.f(") *%sHandler {\n\treturn &%sHandler{rt: rt", c.pascal, c.pascal)
	for _, sp := range c.slots {
		w.f(", %s: %s", sp.FieldName, sp.FieldName)
	}
	w.f("}\n}\n\n")
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
			w.f("var %s = mustCommand(command.NewCommand(command.CommandSpec[struct{}]{\n\tName: %q,\n\tWrites: []command.Declared{data.%sEntity},\n\tProbe: noProbe,\n", w.commandVar(op), string(op), e.Name)
			if p := c.permission(op); p != "" && op != types.OpCreate {
				token := fmt.Sprintf("SELECT authz_token FROM %s WHERE tenant_id = $1 AND id = $2::uuid FOR SHARE", table)
				w.f("\tPermission: %q,\n\tResource: &command.Resource{Type: %q, Token: %q, ValidID: data.ValidUUID},\n", p, e.ResourceType, token)
			}
			w.f("}))\n\n")
		}
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
		if f.Optional {
			expr = "ptrOf(" + expr + ")"
		}
		w.f(", %s: %s", name, expr)
	}
	w.f("}\n}\n\n")
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
		w.f("\treturn presentEntity(v, %q, v.ID, %s)\n}\n\n", e.Name, resolvedHrefExpr(c.hrefBase, "v.ID"))
	} else {
		w.f("\treturn v\n}\n\n")
	}
	if c.ops[types.OpCreate] || c.ops[types.OpUpdate] {
		w.f("// decode%s gives the %s of a create or an update body. A kind member that is not %s is refused, and the\n// client values of the server-managed fields are cleared (ST-SUP-RC20-OC17, ST-SUP-RC20-OC19).\n", c.pascal, e.Name, e.Name)
		w.f("func decode%s(bodyBytes []byte) (%s, error) {\n", c.pascal, e.Name)
		w.f("\tvar rawMap map[string]json.RawMessage\n\tif err := json.Unmarshal(bodyBytes, &rawMap); err != nil {\n\t\treturn %s{}, &invalidRequest{problem: BadRequest(\"the body is not a JSON object\")}\n\t}\n", e.Name)
		w.f("\tif kindRaw, ok := rawMap[\"kind\"]; ok {\n\t\tvar kind string\n\t\tif err := json.Unmarshal(kindRaw, &kind); err != nil || kind != %q {\n\t\t\treturn %s{}, &invalidRequest{problem: BadRequest(\"kind must be %s\")}\n\t\t}\n\t}\n", e.Name, e.Name, e.Name)
		w.f("\tvar %s %s\n\tif err := json.Unmarshal(bodyBytes, &%s); err != nil {\n\t\treturn %s{}, &invalidRequest{problem: BadRequest(\"the body does not decode into %s\")}\n\t}\n", c.lower, e.Name, c.lower, e.Name, e.Name)
		w.f("\t%s.ID = \"\"\n", c.lower)
		emitClearServerManagedFields(&w.Buffer, c.lower, e)
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
		w.f("\tvar patch %sPatchRequest\n\tif err := json.Unmarshal(bodyBytes, &patch); err != nil {\n\t\treturn %sPatchRequest{}, &invalidRequest{problem: BadRequest(\"the body does not decode into the patch of %s\")}\n\t}\n\treturn patch, nil\n}\n\n", c.pascal, c.pascal, e.Name)
	}
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
			w.f("\t\t%s.%s = %v\n", c.lower, name, f.Default)
		}
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
			emitBeforeDeleteSlot(&w.Buffer, w.slotsAlias, runtimeIdentity, sp, w.c.entity.Name, emitRuntimeSlotCheck)
		case "before_patch":
			emitBeforePatchSlot(&w.Buffer, w.slotsAlias, runtimeIdentity, sp, w.c.lower, w.c.entity, patchFields, emitRuntimeSlotCheck)
		default:
			emitBeforeSlot(&w.Buffer, w.slotsAlias, runtimeIdentity, sp, w.c.lower, w.c.entity, emitRuntimeSlotCheck)
		}
	}
}

// after writes the after slots of an operation, which run after its command committed and wrote, and not after a
// replay or a before slot that halted the command with no write (ST-SUP-RC20-OC34).
func (w *runtimeHandlerWriter) after(op types.Operation, entityVar string, patchFields []types.Field) {
	_, after := slotsForOp(op, w.c.slots)
	if len(after) == 0 {
		return
	}
	w.f("\tif !result.Replayed && result.Response.Status < 300 && !halted(result) {\n")
	for _, sp := range after {
		dispatchAfterSlot(&w.Buffer, w.slotsAlias, runtimeIdentity, sp, entityVar, w.c.entity, op, patchFields, emitRuntimeAfterFailure)
	}
	w.f("\t}\n")
}

// key writes the read of the idempotency key of a create or a patch: required when the declaration requires it,
// and the request's own key when the client sends none (ST-SUP-RC30-OC01).
func (w *runtimeHandlerWriter) key(op types.Operation) {
	required := w.keys != nil && w.keys.required[op]
	w.f("\tkey, err := idempotency.KeyFrom(r.Header, %t)\n\tif err != nil {\n\t\tkeyMissing(w, r)\n\t\treturn\n\t}\n", required)
	w.f("\trequestID := data.NewID()\n\tif key == \"\" {\n\t\tkey = requestKey(requestID)\n\t}\n")
}

// start writes the binding of the verified caller and the read of the body.
func (w *runtimeHandlerWriter) start(body bool) {
	w.f("\tb, err := bind(r.Context())\n\tif err != nil {\n\t\thandleError(w, r, Unauthorized(\"the request is not authenticated\"))\n\t\treturn\n\t}\n")
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
		w.f("\t\t%s.ID = data.NewID()\n", c.lower)
	}
	w.populateCreate()
	w.before(types.OpCreate, nil)
	w.f("\t\trepo, err := data.%sRepository(ch)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", e.Name)
	w.f("\t\tcreated, err := repo.Create(ctx, data.%s(%s), nil)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", e.Name, c.lower)
	w.f("\t\tpersisted = %s(created.Value())\n", e.Name)
	w.respond("http.StatusCreated", "persisted")
	w.f("\t})\n")
	w.finish(types.OpCreate, "")
	w.after(types.OpCreate, "persisted", nil)
	w.f("\twriteResult(w, result)\n}\n\n")
}

func (w *runtimeHandlerWriter) update() {
	c, e := w.c, w.c.entity
	w.f("// Update runs the generated command update of %s through Run, on the row of the path's ID.\n", c.decl.Name)
	w.f("func (h *%sHandler) Update(w http.ResponseWriter, r *http.Request) {\n\tid := r.PathValue(\"id\")\n", c.pascal)
	w.start(true)
	w.f("\tif _, err := decode%s(body); err != nil {\n\t\th.rt.answerWrite(w, r, command.Request{}, \"update\", %q, %q, false, err)\n\t\treturn\n\t}\n", c.pascal, e.ResourceType, e.Name)
	w.f("\trequestID := data.NewID()\n\tfingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, body)\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the body is not a JSON object\"))\n\t\treturn\n\t}\n")
	w.f("\treq := h.rt.commandRequest(r, b, requestKey(requestID), fingerprint, requestID, body, id)\n")
	w.f("\tvar persisted %s\n", e.Name)
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, decode%s, func(ctx context.Context, %s %s, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpUpdate), c.pascal, c.lower, e.Name)
	w.lockRow()
	w.f("\t\t%s.ID = id\n", c.lower)
	w.populateUpdate()
	w.before(types.OpUpdate, nil)
	w.f("\t\tupdated, err := repo.Update(ctx, from, data.%s(%s))\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", e.Name, c.lower)
	w.f("\t\tpersisted = %s(updated.Value())\n", e.Name)
	w.respond("http.StatusOK", "persisted")
	w.f("\t})\n")
	w.finish(types.OpUpdate, "id")
	w.after(types.OpUpdate, "persisted", nil)
	w.f("\twriteResult(w, result)\n}\n\n")
}

// lockRow writes the locked read of the row of the path's ID in a command's fill, as stored.
func (w *runtimeHandlerWriter) lockRow() {
	e := w.c.entity
	w.f("\t\trepo, err := data.%sRepository(ch)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", e.Name)
	w.f("\t\tfrom, err := repo.GetForDelete(ctx, id)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n")
	w.f("\t\tstored := %s(from.Value())\n\t\t_ = stored\n", e.Name)
}

func (w *runtimeHandlerWriter) patch() {
	c, e := w.c, w.c.entity
	fields := c.patchFields()
	w.f("// Patch runs the generated command patch of %s through Run: it changes only the fields that the request\n// gives (ST-SUP-RC20-OC15).\n", c.decl.Name)
	w.f("func (h *%sHandler) Patch(w http.ResponseWriter, r *http.Request) {\n\tid := r.PathValue(\"id\")\n", c.pascal)
	w.start(true)
	w.key(types.OpPatch)
	_, afterPatch := slotsForOp(types.OpPatch, c.slots)
	if len(afterPatch) > 0 {
		w.f("\tpatch, err := decode%sPatch(body)\n\tif err != nil {\n", c.pascal)
	} else {
		w.f("\tif _, err := decode%sPatch(body); err != nil {\n", c.pascal)
	}
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
	for _, f := range e.Fields {
		switch {
		case f.Name == "updated_by":
			w.setField(f, "callerName(r.Context())")
		case f.Name == "updated_time" && f.Type == types.FieldTypeTimestamp:
			w.setField(f, "time.Now().UTC()")
		}
	}
	w.f("\t\tupdated, err := repo.Update(ctx, from, data.%s(%s))\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", e.Name, c.lower)
	w.f("\t\tpersisted = %s(updated.Value())\n", e.Name)
	w.respond("http.StatusOK", "persisted")
	w.f("\t})\n")
	w.finish(types.OpPatch, "id")
	if _, after := slotsForOp(types.OpPatch, c.slots); len(after) > 0 {
		w.f("\t%s := persisted\n", c.lower)
		w.after(types.OpPatch, c.lower, fields)
	}
	w.f("\twriteResult(w, result)\n}\n\n")
}

func (w *runtimeHandlerWriter) remove() {
	c := w.c
	w.f("// Delete runs the generated command delete of %s through Run, and answers 204 with no body (ST-SUP-RC20-OC32).\n", c.decl.Name)
	w.f("func (h *%sHandler) Delete(w http.ResponseWriter, r *http.Request) {\n\tid := r.PathValue(\"id\")\n", c.pascal)
	w.start(false)
	w.f("\trequestID := data.NewID()\n\tfingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, nil)\n\tif err != nil {\n\t\thandleError(w, r, InternalError(\"internal error\"))\n\t\treturn\n\t}\n")
	w.f("\treq := h.rt.commandRequest(r, b, requestKey(requestID), fingerprint, requestID, nil, id)\n")
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, func([]byte) (struct{}, error) { return struct{}{}, nil }, func(ctx context.Context, _ struct{}, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpDelete))
	w.lockRow()
	w.before(types.OpDelete, nil)
	w.f("\t\tif err := repo.Delete(ctx, from); err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n")
	w.f("\t\treturn idempotency.Response{Status: http.StatusNoContent}, nil\n\t})\n")
	w.finish(types.OpDelete, "id")
	w.after(types.OpDelete, "", nil)
	w.f("\twriteResult(w, result)\n}\n\n")
}

func (w *runtimeHandlerWriter) read() {
	c, e := w.c, w.c.entity
	w.f("// Read runs the query read of %s through query.Run: a row that is not there and a deny are both 404\n// (ST-SUP-RP11-LC06), and a read of a pii field has its audit row (ST-SUP-RC28-OC04).\n", c.decl.Name)
	w.f("func (h *%sHandler) Read(w http.ResponseWriter, r *http.Request) {\n\tid := r.PathValue(\"id\")\n", c.pascal)
	w.start(false)
	w.f("\tif !data.ValidUUID(id) {\n\t\thandleError(w, r, NotFound(%q, id))\n\t\treturn\n\t}\n", e.Name)
	q := w.commandVar(types.OpRead)
	w.f("\tvalue, err := query.Run(r.Context(), h.rt.Queries, %s, h.rt.boundQuery(r.Context(), b, %s), id, func(ctx context.Context, id string, qh *query.Handle[struct{}]) (%s, error) {\n", q, q, e.Name)
	w.f("\t\trepo, err := query.Repo(qh, data.Entity%sView())\n\t\tif err != nil {\n\t\t\treturn %s{}, err\n\t\t}\n", e.Name, e.Name)
	w.f("\t\tloaded, err := repo.Get(ctx, id)\n\t\tif err != nil {\n\t\t\treturn %s{}, err\n\t\t}\n\t\treturn %sFromView(loaded.Value()), nil\n\t})\n", e.Name, c.camel)
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
	w.f("\tvar after query.Cursor\n\tif text := r.URL.Query().Get(\"cursor\"); text != \"\" {\n\t\tif after, err = h.rt.Queries.ParseCursor(%s, b.tenant, b.principal, text); err != nil {\n\t\t\thandleError(w, r, BadRequest(\"the cursor is not valid\"))\n\t\t\treturn\n\t\t}\n\t}\n", q)
	w.f("\tpage, err := query.Run(r.Context(), h.rt.Queries, %s, h.rt.boundQuery(r.Context(), b, %s), query.ListRequest{Limit: limit, After: after}, func(ctx context.Context, request query.ListRequest, qh *query.Handle[struct{}]) (%sPage, error) {\n", q, q, c.camel)
	w.f("\t\trepo, err := query.Repo(qh, data.Entity%sView())\n\t\tif err != nil {\n\t\t\treturn %sPage{}, err\n\t\t}\n", e.Name, c.camel)
	w.f("\t\tresult, err := repo.List(ctx, request)\n\t\tif err != nil {\n\t\t\treturn %sPage{}, err\n\t\t}\n", c.camel)
	w.f("\t\tpage := %sPage{next: result.Next, more: result.More}\n\t\tfor _, item := range result.Items {\n\t\t\tpage.items = append(page.items, %sFromView(item.Value()))\n\t\t}\n\t\treturn page, nil\n\t})\n", c.camel, c.camel)
	w.f("\tif err != nil {\n\t\thandleError(w, r, InternalError(\"internal error\"))\n\t\treturn\n\t}\n")
	w.f("\titems := make([]any, 0, len(page.items))\n\tfor _, item := range page.items {\n\t\titems = append(items, %sPresented(item))\n\t}\n", c.camel)
	w.f("\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	if w.envelope {
		w.f("\tresult := map[string]any{\"kind\": %q, \"size\": len(items), \"items\": items}\n", e.Name+"List")
		w.f("\tif page.more {\n\t\tnext, err := h.rt.Queries.CursorText(%s, b.tenant, b.principal, page.next)\n\t\tif err != nil {\n\t\t\thandleError(w, r, InternalError(\"internal error\"))\n\t\t\treturn\n\t\t}\n\t\tresult[\"next\"] = next\n\t}\n", q)
		w.f("\t_ = json.NewEncoder(w).Encode(result)\n}\n\n")
	} else {
		w.f("\t_ = json.NewEncoder(w).Encode(items)\n}\n\n")
	}
}
