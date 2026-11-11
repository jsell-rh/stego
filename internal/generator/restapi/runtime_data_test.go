package restapi

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/slot"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/crud_test.go
var crudChecks []byte

// crudModule is the module of the generated test service.
const crudModule = "example.com/crud"

// crudContext is the context that the compiler gives the rest-api generator on the runtime data path for the test
// service of ST-SUP-RC20-OC37: pooled tenancy, spicedb-authz with the subject type user, idempotency required on
// create and patch with the problem base https://errors.example.com/crud/, and three collections: orders over the
// owned entity Order (sku, qty) with create, read, list, patch and delete, a short-circuit chain on before_create and a gate on after_create; customers
// over Customer (name pii, api_token secret, plan, vault a secret jsonb, contact a pii jsonb) with create, read, list, update and delete and a gate on
// validate (ST-SUP-RC28); and todos over Todo (title, done, and meta of type jsonb) with create, read, list, patch,
// update and delete (ST-SUP-RC30).
func crudContext() gen.Context {
	public := map[types.Operation]string{types.OpCreate: "public"}
	reasons := map[types.Operation]string{types.OpCreate: "a new row has no resource to decide on before it exists"}
	permissions := func(ops ...types.Operation) map[types.Operation]string {
		p := map[types.Operation]string{types.OpCreate: public[types.OpCreate], types.OpRead: "view", types.OpList: "view"}
		for _, op := range ops {
			p[op] = "edit"
		}
		return p
	}
	entities := []types.Entity{
		{Name: "Order", Fields: []types.Field{{Name: "sku", Type: types.FieldTypeString}, {Name: "qty", Type: types.FieldTypeInt32}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "order", Permissions: permissions(types.OpPatch, types.OpDelete), PublicReasons: reasons}},
		{Name: "Customer", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString, Sensitivity: "pii"},
			{Name: "api_token", Type: types.FieldTypeString, Sensitivity: "secret"}, {Name: "plan", Type: types.FieldTypeString},
			{Name: "vault", Type: types.FieldTypeJsonb, Sensitivity: "secret", Optional: true},
			{Name: "contact", Type: types.FieldTypeJsonb, Sensitivity: "pii", Optional: true}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "customer", Permissions: permissions(types.OpUpdate, types.OpDelete), PublicReasons: reasons}},
		{Name: "Todo", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}, {Name: "done", Type: types.FieldTypeBool, Optional: true},
			{Name: "meta", Type: types.FieldTypeJsonb, Optional: true}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "todo", Permissions: permissions(types.OpPatch, types.OpUpdate, types.OpDelete), PublicReasons: reasons}},
	}
	return gen.Context{
		ModuleName:      crudModule,
		OutDirName:      "out",
		OutputNamespace: "api",
		ServiceName:     "crud",
		BasePath:        "/api/v1",
		ErrorTypeBase:   "https://errors.example.com/crud/",
		Entities:        entities,
		Collections: []types.Collection{
			{Name: "orders", Entity: "Order", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpPatch, types.OpDelete}, Patchable: []string{"qty"}},
			{Name: "customers", Entity: "Customer", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpUpdate, types.OpDelete}},
			{Name: "todos", Entity: "Todo", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpPatch, types.OpUpdate, types.OpDelete}, Patchable: []string{"title", "done", "meta"}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Collection: "orders", Chain: []string{"order-gate"}, ShortCircuit: true},
			{Slot: "after_create", Collection: "orders", Gate: []string{"order-after"}},
			{Slot: "validate", Collection: "customers", Gate: []string{"customer-gate"}},
		},
		SlotsPackage:   "slots",
		Conventions:    types.Convention{ResponseFormat: "envelope"},
		Authorization:  &types.Authorization{Provider: "spicedb", SchemaPath: "authz/schema.zed", SubjectFromIdentity: "user"},
		Tenancy:        &gen.Tenancy{Key: "tenant_id"},
		Idempotency:    &types.Idempotency{RequiredOn: []types.Operation{types.OpCreate, types.OpPatch}, Retention: "24h", Fingerprint: "body"},
		PeerNamespaces: map[string]string{"idempotency": "idempotency", "postgres-adapter": "storage", gen.TenancyComponent: "tenancy"},
		RuntimeData:    true,
	}
}

// crudFiles generates the rest-api files of ctx, by path.
func crudFiles(t testing.TB, ctx gen.Context) map[string][]byte {
	t.Helper()
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		out[f.Path] = f.Bytes()
	}
	return out
}

// slotFiles generates the slot contract package of the bound slots from the rest-api slot protos of the registry,
// as the compiler does.
func slotFiles(t testing.TB, ctx gen.Context) map[string][]byte {
	t.Helper()
	root := filepath.Join("..", "..", "..", "registry")
	common, err := os.ReadFile(filepath.Join(root, "common", "types.proto"))
	if err != nil {
		t.Fatal(err)
	}
	imported, err := slot.ParseProto(bytes.NewReader(common))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, b := range ctx.SlotBindings {
		names[b.Slot] = true
	}
	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	out := map[string][]byte{}
	emitted := map[string]bool{}
	for _, name := range sorted {
		data, err := os.ReadFile(filepath.Join(root, "components", "rest-api", "slots", name+".proto"))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := slot.ParseProto(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		iface, err := slot.GenerateInterfaceExcluding("slots/"+name+".go", "slots", parsed, []*slot.ProtoFile{imported}, emitted)
		if err != nil {
			t.Fatal(err)
		}
		out[iface.Path] = iface.Bytes()
		all := map[string]slot.Message{}
		for _, m := range imported.Messages {
			all[imported.Package+"."+m.Name], all[m.Name] = m, m
		}
		for _, m := range parsed.Messages {
			all[parsed.Package+"."+m.Name], all[m.Name] = m, m
		}
		for name := range slot.CollectAllReferencedTypes(parsed, all) {
			emitted[name] = true
		}
		ops, err := slot.GenerateOperators("slots/"+name+"_operators.go", "slots", parsed)
		if err != nil {
			t.Fatal(err)
		}
		out[ops.Path] = ops.Bytes()
	}
	return out
}

// crudTables is the migration 001_initial that the PostgreSQL adapter creates for the declaration on the runtime
// data path (ST-SUP-RC01-OC41).
func crudTables(t testing.TB, ctx gen.Context) []byte {
	t.Helper()
	adapter := gen.Context{OutputNamespace: "storage", ModuleName: ctx.ModuleName, ServiceName: ctx.ServiceName, RuntimeData: true,
		Tenancy: ctx.Tenancy, Entities: ctx.Entities, PeerNamespaces: map[string]string{"postgres-adapter": "storage", gen.TenancyComponent: "tenancy"},
		ComponentConfig: map[string]any{"schema_generation": "tables-v1", "writer_lease": "off"}}
	files, _, err := new(postgresadapter.Generator).Generate(adapter)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == "storage/migrations/001_initial.sql" {
			return f.Content
		}
	}
	t.Fatal("the PostgreSQL adapter gave no migration 001_initial")
	return nil
}

// TestDumpRuntimeData writes the generated files of the test service to STEGO_CRUD_DUMP, for a reader.
func TestDumpRuntimeData(t *testing.T) {
	dir := os.Getenv("STEGO_CRUD_DUMP")
	if dir == "" {
		t.Skip("STEGO_CRUD_DUMP is not set: ST-SUP-RC20-OC37 is proved by TestCrudService")
	}
	ctx := crudContext()
	files := crudFiles(t, ctx)
	for name, data := range slotFiles(t, ctx) {
		files[name] = data
	}
	for name, data := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// crudPinnedGo is the Go command that builds the generated service: STEGO_TEST_PINNED_GO, else bin/go of the SDK that
// runs the test.
func crudPinnedGo() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// crudProject is the generated service module in a temporary folder.
type crudProject struct {
	t   *testing.T
	dir string
}

func (p *crudProject) write(name string, data []byte) {
	p.t.Helper()
	full := filepath.Join(p.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// goCommand runs the pinned go command in the project, with no workspace, and gives its output.
func (p *crudProject) goCommand(env []string, args ...string) (string, error) {
	p.t.Helper()
	cmd := exec.Command(crudPinnedGo(), args...)
	cmd.Dir = p.dir
	cmd.Env = append(append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local"), env...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (p *crudProject) mustGo(args ...string) string {
	p.t.Helper()
	output, err := p.goCommand(nil, args...)
	if err != nil {
		p.t.Fatalf("go %v: %v\n%s", args, err, output)
	}
	return output
}

// newCrudProject writes the generated service of ctx under out/, its slot contracts, its tables and its checks into a
// module that requires the runtime library from this repository's registry/runtime folder.
func newCrudProject(t *testing.T, ctx gen.Context, files map[string][]byte) *crudProject {
	t.Helper()
	p := &crudProject{t: t, dir: t.TempDir()}
	for name, data := range files {
		p.write("out/"+name, data)
	}
	for name, data := range slotFiles(t, ctx) {
		p.write("out/"+name, data)
	}
	p.write("check/crud_test.go", crudChecks)
	p.write("check/tables.sql", crudTables(t, ctx))
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(runtimeDir, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("go.sum", sums)
	p.write("go.mod", []byte("module "+ctx.ModuleName+"\n\ngo 1.26.11\n\nrequire github.com/jsell-rh/stego/registry/runtime v0.26.0\n\nreplace github.com/jsell-rh/stego/registry/runtime => "+runtimeDir+"\n"))
	p.mustGo("mod", "tidy")
	return p
}

// crudRun is the one run of the checks of the generated service: one go test of the check package with every
// check, so the service is built and its test binary run once.
type crudRun struct {
	once   sync.Once
	output string
	err    error
}

// passed tells whether the check id of the generated service ran and passed. The first call runs every check
// against the local PostgreSQL server of STEGO_TEST_POSTGRES_DSN and a local SpiceDB container. A missing server
// fails the test: a missing result is not a pass.
func (run *crudRun) passed(t *testing.T, p *crudProject, endpoint, key, id string) {
	t.Helper()
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	run.once.Do(func() {
		run.output, run.err = p.goCommand([]string{"STEGO_TEST_SPICEDB=" + endpoint, "STEGO_TEST_SPICEDB_KEY=" + key},
			"test", "-count=1", "-timeout=300s", "-v", "-run", "^TestCrud$", "./check")
	})
	if !strings.Contains(run.output, "--- PASS: TestCrud/"+id+"_") {
		t.Fatalf("the check %s of the generated service did not run and pass (go test: %v):\n%s", id, run.err, run.output)
	}
}

// crudSpiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04), as the runtime's tests pin
// it.
const crudSpiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// startCrudSpiceDB starts a local SpiceDB container without privilege, with an in-memory datastore and limits, and
// gives its endpoint and preshared key. It removes the container when the test ends.
func startCrudSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	if endpoint := os.Getenv("STEGO_TEST_SPICEDB"); endpoint != "" {
		return endpoint, os.Getenv("STEGO_TEST_SPICEDB_KEY")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is not on the PATH: this test needs a local SpiceDB container")
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name, key := "stego-crud-spicedb-"+hex.EncodeToString(suffix), "crud-"+hex.EncodeToString(suffix)
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--memory", "256m", "--cpus", "1",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--stop-timeout", "1",
		"-p", "127.0.0.1::50051", crudSpiceDBImage, "serve", "--grpc-preshared-key", key, "--datastore-engine", "memory")
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run SpiceDB: %v\n%s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	var port string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		out, err := exec.Command("docker", "port", name, "50051/tcp").Output()
		if err == nil && len(out) > 0 {
			port = strings.TrimSpace(strings.Split(string(out), "\n")[0])
			break
		}
	}
	if port == "" {
		t.Fatal("the SpiceDB container publishes no port")
	}
	return port, key
}

// handlerCalls gives, for each handler method of the file, the selectors that it calls, such as command.Run.
func handlerCalls(t *testing.T, source []byte) map[string][]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "handler.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string][]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok {
						calls[fn.Name.Name] = append(calls[fn.Name.Name], x.Name+"."+sel.Sel.Name)
					}
				}
			}
			return true
		})
	}
	return calls
}

// TestCrudService generates the test service on the runtime data path, builds it with the pinned Go and runs its
// handlers over HTTP against PostgreSQL and a local SpiceDB container.
func TestCrudService(t *testing.T) {
	ctx := crudContext()
	files := crudFiles(t, ctx)
	t.Run("ST-SUP-RC20-OC37-T01 The handlers call Run and hold no store", func(t *testing.T) {
		want := map[string]string{"Create": "command.Run", "Update": "command.Run", "Patch": "command.Run", "Delete": "command.Run",
			"Read": "query.Run", "List": "query.Run"}
		methods := map[types.Operation]string{types.OpCreate: "Create", types.OpUpdate: "Update", types.OpPatch: "Patch",
			types.OpDelete: "Delete", types.OpRead: "Read", types.OpList: "List"}
		for _, c := range ctx.Collections {
			name := "api/" + collectionToSnakeCase(c.Name) + "_handler.go"
			source, ok := files[name]
			if !ok {
				t.Fatalf("no handler file %s for the collection %s", name, c.Name)
			}
			calls := handlerCalls(t, source)
			for _, op := range c.Operations {
				method := methods[op]
				found := false
				for _, call := range calls[method] {
					found = found || call == want[method]
				}
				if !found {
					t.Errorf("%s: the handler %s calls %q, not %s", name, method, calls[method], want[method])
				}
			}
		}
		for name, source := range files {
			if !strings.HasPrefix(name, "api/") || strings.HasPrefix(name, "api/data/") {
				continue
			}
			for _, refused := range []string{"database/sql", "gorm.io", "Storage", "h.store", "sql.Open", "pgx"} {
				if bytes.Contains(source, []byte(refused)) {
					t.Errorf("%s holds %q: a handler holds no store and opens no connection", name, refused)
				}
			}
		}
	})
	t.Run("ST-SUP-RC20-OC37-T04 Generation refuses what the runtime data path does not serve", func(t *testing.T) {
		cases := map[string]func(*gen.Context){
			"a create that is not public": func(c *gen.Context) { c.Entities[0].Permissions[types.OpCreate] = "edit" },
			"a public patch":              func(c *gen.Context) { c.Entities[0].Permissions[types.OpPatch] = "public" },
			"a delete with no permission": func(c *gen.Context) { delete(c.Entities[0].Permissions, types.OpDelete) },
			"an upsert": func(c *gen.Context) {
				c.Collections[0].Operations = append(c.Collections[0].Operations, types.OpUpsert)
				c.Collections[0].UpsertKey = []string{"sku"}
			},
			"implicit fields": func(c *gen.Context) { c.Collections[0].Implicit = map[string]string{"sku": "fixed"} },
			"a path prefix":   func(c *gen.Context) { c.Collections[0].PathPrefix = "/stock" },
			"a bytes field": func(c *gen.Context) {
				c.Entities[0].Fields = append(c.Entities[0].Fields, types.Field{Name: "doc", Type: types.FieldTypeBytes})
			},
			"no resource type": func(c *gen.Context) { c.Entities[0].ResourceType = "" },
			"no tenancy":       func(c *gen.Context) { c.Tenancy = nil },
			"no authorization": func(c *gen.Context) { c.Authorization = nil },
			"a slot it never calls": func(c *gen.Context) {
				c.SlotBindings = append(c.SlotBindings, types.SlotDeclaration{Slot: "before_upsert", Collection: "orders", Gate: []string{"g"}})
			},
		}
		for name, change := range cases {
			c := crudContext()
			change(&c)
			if files, wiring, err := new(Generator).Generate(c); err == nil || files != nil || wiring != nil {
				t.Errorf("%s: generation gave %d files and %v, want a refusal", name, len(files), err)
			}
		}
		if _, _, err := new(Generator).Generate(crudContext()); err != nil {
			t.Errorf("the test declaration is refused: %v", err)
		}
	})
	p := newCrudProject(t, ctx, files)
	p.mustGo("vet", "./...")
	endpoint, key := startCrudSpiceDB(t)
	run := &crudRun{}
	for _, id := range crudCheckIDs {
		t.Run(id, func(t *testing.T) { run.passed(t, p, endpoint, key, strings.SplitN(id, " ", 2)[0]) })
	}
}

// crudCheckIDs are the checks of the generated service that this test runs, each with the ID of the test record it
// proves.
var crudCheckIDs = []string{
	"ST-SUP-RC20-OC37-T01 A collection create runs as a command",
	"ST-SUP-RC20-OC37-T02 A refusing before slot stops the command before any write",
	"ST-SUP-RC20-OC37-T03 A write that the caller may not make, or of a missing row, is 404 and changes nothing",
	"ST-SUP-RC20-OC37-T05 A before slot that halts writes nothing and fires no after slot",
	"ST-SUP-RC20-OC37-T06 A failed after slot answers the committed response with no error text",
	"ST-SUP-RC20-OC34-T03 A failed after slot is retried through the outbox, never reaches the answer, and each failure and retry is visible",
	"ST-SUP-RC20-OC35-T01 Every route takes the tenant from the verified identity",
	"ST-SUP-RC20-OC35-T02 A tenant in a header is not accepted",
	"ST-SUP-RC20-OC35-T03 A tenant in a query parameter is not accepted",
	"ST-SUP-RC20-OC35-T04 A tenant in a body field is not accepted",
	"ST-SUP-RC30-OC04-T01 The key row commits with the write",
	"ST-SUP-RC30-OC04-T03 The same key from another tenant or principal is a new key",
	"ST-SUP-RC28-OC02-T02 A committed mutation leaves exactly one audit row",
	"ST-SUP-RC28-OC03-T01 A denied mutation writes one audit row with outcome denied after the rollback",
	"ST-SUP-RC28-OC04-T01 A single read of an entity with a pii field writes one read audit row",
	"ST-SUP-RC28-OC04-T03 A list that returns a pii field writes one read audit row per returned resource, at most one page",
	"ST-SUP-RC28-OC01-T03 A secret field reaches neither before, after nor the stored row",
	"ST-SUP-RC20-OC38-T01 A list on the runtime data path pages by cursor in the requested order, with no total",
	"ST-SUP-RC20-OC37-T08 A jsonb field is written, read and patched as JSON on the runtime data path",
	"ST-SUP-RC28-OC01-T06 A secret jsonb field reaches neither the audit rows, the answer nor the stored response",
	"ST-SUP-RC28-OC01-T07 A pii jsonb field is the redaction marker in the before, the after and the patch",
}

// bodyLimitCheck is a test of the generated api package: the validation middleware answers a body larger than
// MaxRequestBody with 413 and does not call the handler, and still checks a body below the limit.
const bodyLimitCheck = `package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBodyLimit(t *testing.T) {
	called := false
	handler := NewValidationMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	large := httptest.NewRequest(http.MethodPost, "/api/v1/todos", strings.NewReader("{\"title\":\""+strings.Repeat("a", MaxRequestBody)+"\"}"))
	large.Header.Set("Content-Type", "application/json")
	answer := httptest.NewRecorder()
	handler.ServeHTTP(answer, large)
	if answer.Code != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("a body over the limit got %d and called the handler %v: %s", answer.Code, called, answer.Body)
	}
	small := httptest.NewRequest(http.MethodPost, "/api/v1/todos", strings.NewReader("{\"title\":7}"))
	small.Header.Set("Content-Type", "application/json")
	answer = httptest.NewRecorder()
	handler.ServeHTTP(answer, small)
	if answer.Code != http.StatusBadRequest || called {
		t.Fatalf("an invalid body below the limit got %d and called the handler %v: %s", answer.Code, called, answer.Body)
	}
}
`

func TestValidationBodyLimit(t *testing.T) {
	t.Run("ST-SUP-RC20-OC21-T04 The emitted middleware reads at most MaxRequestBody bytes and answers a larger body with 413", func(t *testing.T) {
		ctx := crudContext()
		ctx.Conventions.RequestValidation = "openapi-schema"
		files := crudFiles(t, ctx)
		if !strings.Contains(string(files["api/validation.go"]), "http.MaxBytesReader(w, r.Body, MaxRequestBody)") {
			t.Fatalf("the middleware does not bound the body:\n%s", files["api/validation.go"])
		}
		p := newCrudProject(t, ctx, files)
		p.write("out/api/limit_test.go", []byte(bodyLimitCheck))
		if output, err := p.goCommand(nil, "test", "-count=1", "-run", "^TestBodyLimit$", "./out/api"); err != nil {
			t.Fatalf("the body limit check failed: %v\n%s", err, output)
		}
	})
}

// corsCheck is a test of the generated cors.go alone, in a module of its own.
const corsCheck = `package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(t *testing.T, method, origin string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	called := false
	handler := NewCORSMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(method, "/api/v1/todos", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w, called
}

func TestCORSUnset(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		w, called := serve(t, method, "https://evil.example")
		for name := range w.Header() {
			if len(name) > 13 && name[:13] == "Access-Contro" || name == "Vary" {
				t.Errorf("%s with no CORS_ALLOWED_ORIGINS set the header %s", method, name)
			}
		}
		if !called || w.Code != http.StatusOK {
			t.Errorf("%s with no CORS_ALLOWED_ORIGINS did not reach the handler: %d", method, w.Code)
		}
	}
}

func TestCORSSet(t *testing.T) {
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://app.example, https://admin.example")
	w, called := serve(t, http.MethodGet, "https://admin.example")
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example" || !called {
		t.Errorf("a listed origin got %q, handler called %v", got, called)
	}
	if w.Header().Get("Vary") != "Origin" || w.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Errorf("a listed origin got no Vary: Origin or no allowed methods: %v", w.Header())
	}
	w, called = serve(t, http.MethodGet, "https://evil.example")
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" || !called || w.Header().Get("Vary") != "Origin" {
		t.Errorf("an origin not listed got %q, Vary %q, handler called %v", got, w.Header().Get("Vary"), called)
	}
	w, called = serve(t, http.MethodGet, "")
	if w.Header().Get("Access-Control-Allow-Origin") != "" || !called {
		t.Errorf("a request with no Origin got an allowed origin")
	}
	w, called = serve(t, http.MethodOptions, "https://app.example")
	if w.Code != http.StatusNoContent || called || w.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Errorf("a preflight of a listed origin got %d, handler called %v, origin %q", w.Code, called, w.Header().Get("Access-Control-Allow-Origin"))
	}
}
`

func TestCORSOrigins(t *testing.T) {
	t.Run("ST-SUP-RC20-OC27-T02 The emitted CORS middleware sends no header by default and echoes only a listed origin", func(t *testing.T) {
		ctx := basicContext()
		ctx.Conventions.CORS = "enabled"
		files, _, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p := &crudProject{t: t, dir: t.TempDir()}
		for _, f := range files {
			if f.Path == "internal/api/cors.go" {
				p.write("api/cors.go", f.Bytes())
			}
		}
		p.write("api/cors_test.go", []byte(corsCheck))
		p.write("go.mod", []byte("module example.com/cors\n\ngo 1.26.11\n"))
		if output, err := p.goCommand(nil, "test", "-count=1", "./api"); err != nil {
			t.Fatalf("the CORS check failed: %v\n%s", err, output)
		}
	})
}

// storeBodyCheck is a test of the generated api package of the GORM store path: a body over MaxRequestBody that the
// validation middleware passes on, because a %2F in a path parameter matches no route of the document, still gets 413
// from the body reader that the handlers use.
const storeBodyCheck = `package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStoreBodyLimit(t *testing.T) {
	reached := false
	handler := NewValidationMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if _, err := readBody(w, r); err != nil {
			handleError(w, r, bodyError(err))
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	large := httptest.NewRequest(http.MethodPut, "/users/a%2Fb", strings.NewReader(strings.Repeat("a", 3<<20)))
	large.Header.Set("Content-Type", "application/json")
	answer := httptest.NewRecorder()
	handler.ServeHTTP(answer, large)
	if answer.Code != http.StatusRequestEntityTooLarge || !reached {
		t.Fatalf("a 3 MiB body with an encoded slash got %d, handler reached %v", answer.Code, reached)
	}
}
`

func TestStoreBodyLimit(t *testing.T) {
	t.Run("ST-SUP-RC20-OC21-T05 On the GORM store path a body over MaxRequestBody gets 413 also when no route of the document matches", func(t *testing.T) {
		ctx := basicContext()
		ctx.Conventions.RequestValidation = "openapi-schema"
		files := crudFiles(t, ctx)
		for name, data := range files {
			if strings.Contains(name, "handler_") && strings.Contains(string(data), "io.ReadAll(") {
				t.Errorf("%s reads a body with no limit", name)
			}
		}
		p := &crudProject{t: t, dir: t.TempDir()}
		for name, data := range files {
			p.write("api/"+filepath.Base(name), data)
		}
		p.write("api/body_test.go", []byte(storeBodyCheck))
		p.write("go.mod", []byte("module example.com/store\n\ngo 1.26.11\n\nrequire github.com/getkin/kin-openapi v0.149.0\n"))
		p.mustGo("mod", "tidy")
		if output, err := p.goCommand(nil, "test", "-count=1", "-run", "^TestStoreBodyLimit$", "./api"); err != nil {
			t.Fatalf("the body limit check failed: %v\n%s", err, output)
		}
	})
}
