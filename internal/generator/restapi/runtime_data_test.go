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
	// Folder and Book give a scoped collection: each book is of one folder, and its create is decided on edit of the
	// folder through the reference (ST-SUP-RC20-OC24). Event gives two collections under path prefixes, each with
	// one value of the implicit field source (ST-SUP-RC20-OC23).
	entities = append(entities,
		types.Entity{Name: "Folder", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "folder", Permissions: permissions(types.OpDelete), PublicReasons: reasons}},
		types.Entity{Name: "Book", Fields: []types.Field{{Name: "folder_id", Type: types.FieldTypeRef, To: "Folder"}, {Name: "title", Type: types.FieldTypeString}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "book", Permissions: permissions(types.OpUpdate, types.OpPatch, types.OpDelete), PublicReasons: reasons,
				References: map[string]types.Reference{"folder_id": {Relation: "folder", Permission: "edit"}}}},
		types.Entity{Name: "Setting", Fields: []types.Field{{Name: "folder_id", Type: types.FieldTypeRef, To: "Folder"}, {Name: "name", Type: types.FieldTypeString}, {Name: "value", Type: types.FieldTypeString}, {Name: "source", Type: types.FieldTypeString}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "setting", Permissions: map[types.Operation]string{types.OpRead: "view", types.OpList: "view", types.OpUpsert: "edit"},
				References: map[string]types.Reference{"folder_id": {Relation: "folder", Permission: "edit"}}}},
		// Event is of small expected cardinality: its list reads the rows of the resource search (ST-SUP-RC33-OC09).
		types.Entity{Name: "Event", Fields: []types.Field{{Name: "source", Type: types.FieldTypeString}, {Name: "note", Type: types.FieldTypeString}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "event", Permissions: permissions(types.OpUpdate, types.OpPatch, types.OpDelete), PublicReasons: reasons, ExpectedCardinality: types.CardinalitySmall}},
		// Quota gives an upsert with optimistic concurrency: the If-Match precondition names the generation
		// (ST-SUP-RC20-OC14).
		types.Entity{Name: "Quota", Fields: []types.Field{{Name: "folder_id", Type: types.FieldTypeRef, To: "Folder"}, {Name: "name", Type: types.FieldTypeString}, {Name: "amount", Type: types.FieldTypeInt64},
			{Name: "generation", Type: types.FieldTypeInt64, Default: 1}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "quota", Permissions: map[types.Operation]string{types.OpRead: "view", types.OpList: "view", types.OpUpsert: "edit"},
				References: map[string]types.Reference{"folder_id": {Relation: "folder", Permission: "edit"}}}},
	)
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
			{Name: "folders", Entity: "Folder", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpDelete}},
			{Name: "folder-books", Entity: "Book", Scope: map[string]string{"folder_id": "Folder"}, Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpUpdate, types.OpPatch, types.OpDelete}, Patchable: []string{"title"}},
			{Name: "folder-settings", Entity: "Setting", Scope: map[string]string{"folder_id": "Folder"}, Implicit: map[string]string{"source": "setting"}, Operations: []types.Operation{types.OpList, types.OpUpsert}, UpsertKey: []string{"name"}},
			{Name: "red-events", Entity: "Event", PathPrefix: "/red-events", Implicit: map[string]string{"source": "red"}, Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpUpdate, types.OpPatch, types.OpDelete}, Patchable: []string{"note"}},
			{Name: "blue-events", Entity: "Event", PathPrefix: "/blue-events", Implicit: map[string]string{"source": "blue"}, Operations: []types.Operation{types.OpCreate, types.OpList}},
			{Name: "books", Entity: "Book", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate}},
			{Name: "folder-flags", Entity: "Setting", Scope: map[string]string{"folder_id": "Folder"}, Implicit: map[string]string{"source": "flag"}, PathPrefix: "/folders/{folder_id}/flags", Operations: []types.Operation{types.OpUpsert}, UpsertKey: []string{"name"}},
			{Name: "folder-quotas", Entity: "Quota", Scope: map[string]string{"folder_id": "Folder"}, Operations: []types.Operation{types.OpList, types.OpUpsert}, UpsertKey: []string{"name"}, Concurrency: types.ConcurrencyOptimistic},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Collection: "orders", Chain: []string{"order-gate"}, ShortCircuit: true},
			{Slot: "after_create", Collection: "orders", Gate: []string{"order-after"}},
			{Slot: "validate", Collection: "customers", Gate: []string{"customer-gate"}},
			{Slot: "validate", Collection: "folder-settings", Gate: []string{"setting-gate"}},
			{Slot: "on_entity_changed", Collection: "folder-settings", Gate: []string{"setting-changed"}},
		},
		SlotsPackage:   "slots",
		Conventions:    types.Convention{ResponseFormat: "envelope"},
		Authorization:  &types.Authorization{Provider: "spicedb", SchemaPath: "authz/schema.zed", SubjectFromIdentity: "user"},
		Tenancy:        &gen.Tenancy{Key: "tenant_id"},
		Idempotency:    &types.Idempotency{RequiredOn: []types.Operation{types.OpCreate, types.OpPatch}, Retention: "24h", Fingerprint: "body"},
		PeerNamespaces: map[string]string{"idempotency": "idempotency", "postgres-adapter": "storage", gen.TenancyComponent: "tenancy", "command": "commands", "query": "queries"},
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
		// A slot is of the rest-api component, or of the event-publisher mixin, as on_entity_changed.
		data, err := os.ReadFile(filepath.Join(root, "components", "rest-api", "slots", name+".proto"))
		if os.IsNotExist(err) {
			data, err = os.ReadFile(filepath.Join(root, "mixins", "event-publisher", "slots", name+".proto"))
		}
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
	adapter := gen.Context{OutputNamespace: "storage", ModuleName: ctx.ModuleName, ServiceName: ctx.ServiceName,
		Tenancy: ctx.Tenancy, Entities: ctx.Entities, Collections: ctx.Collections, PeerNamespaces: map[string]string{"postgres-adapter": "storage", gen.TenancyComponent: "tenancy", "command": "commands", "query": "queries"},
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

// withNotes adds the entity Note, with one reference to Folder, and the collection notes with upsert and no
// create: optional tells whether the reference is optional.
func withNotes(optional bool) func(*gen.Context) {
	return func(c *gen.Context) {
		c.Entities = append(c.Entities, types.Entity{Name: "Note", Fields: []types.Field{{Name: "folder_id", Type: types.FieldTypeRef, To: "Folder", Optional: optional}, {Name: "name", Type: types.FieldTypeString}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "note", Permissions: map[types.Operation]string{types.OpRead: "view", types.OpList: "view", types.OpUpsert: "edit"},
				References: map[string]types.Reference{"folder_id": {Relation: "folder", Permission: "edit"}}}})
		c.Collections = append(c.Collections, types.Collection{Name: "notes", Entity: "Note", Operations: []types.Operation{types.OpUpsert}, UpsertKey: []string{"name"}})
	}
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
	p.write("go.mod", []byte("module "+ctx.ModuleName+"\n\ngo 1.26.11\n\nrequire github.com/jsell-rh/stego/registry/runtime v0.57.0\n\nreplace github.com/jsell-rh/stego/registry/runtime => "+runtimeDir+"\n"))
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
			"Upsert": "command.Run", "Read": "query.Run", "List": "query.Run"}
		methods := map[types.Operation]string{types.OpCreate: "Create", types.OpUpdate: "Update", types.OpPatch: "Patch",
			types.OpDelete: "Delete", types.OpUpsert: "Upsert", types.OpRead: "Read", types.OpList: "List"}
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
			for _, refused := range []string{"database/sql", "gorm", "Storage", "h.store", "sql.Open", "pgx"} {
				if bytes.Contains(source, []byte(refused)) {
					t.Errorf("%s holds %q: a handler holds no store and opens no connection", name, refused)
				}
			}
		}
	})
	t.Run("ST-SUP-RC20-OC37-T04 Generation refuses what the runtime data path does not serve", func(t *testing.T) {
		cases := map[string]func(*gen.Context){
			"a create that is not public":           func(c *gen.Context) { c.Entities[0].Permissions[types.OpCreate] = "edit" },
			"a public patch":                        func(c *gen.Context) { c.Entities[0].Permissions[types.OpPatch] = "public" },
			"a delete with no permission":           func(c *gen.Context) { delete(c.Entities[0].Permissions, types.OpDelete) },
			"an upsert with optimistic concurrency": func(c *gen.Context) { c.Collections[5].Concurrency = types.ConcurrencyOptimistic },
			"an upsert with no key":                 func(c *gen.Context) { c.Collections[5].UpsertKey = nil },
			"an optimistic upsert with no decided insert": func(c *gen.Context) {
				c.Entities[7].References = nil
				c.Entities[7].Permissions[types.OpCreate] = "public"
			},
			"an upsert of a row with no permission":        func(c *gen.Context) { delete(c.Entities[5].Permissions, types.OpUpsert) },
			"an implicit field of a number type":           func(c *gen.Context) { c.Collections[0].Implicit = map[string]string{"qty": "1"} },
			"a path parameter that is not the scope field": func(c *gen.Context) { c.Collections[0].PathPrefix = "/stock/{sku}" },
			"an implicit field that a patch may change": func(c *gen.Context) {
				c.Collections[6].Patchable = append(c.Collections[6].Patchable, "source")
			},
			"a scope field that a patch may change": func(c *gen.Context) {
				c.Collections[4].Patchable = append(c.Collections[4].Patchable, "folder_id")
			},
			"a scope field that is not a ref": func(c *gen.Context) {
				c.Collections[3].Scope = map[string]string{"name": "Order"}
			},
			"a bytes field": func(c *gen.Context) {
				c.Entities[0].Fields = append(c.Entities[0].Fields, types.Field{Name: "doc", Type: types.FieldTypeBytes})
			},
			"no resource type": func(c *gen.Context) { c.Entities[0].ResourceType = "" },
			"no tenancy":       func(c *gen.Context) { c.Tenancy = nil },
			"no authorization": func(c *gen.Context) { c.Authorization = nil },
			"an upsert whose only reference is optional": withNotes(true),
			"a slot it never calls": func(c *gen.Context) {
				c.SlotBindings = append(c.SlotBindings, types.SlotDeclaration{Slot: "before_upsert", Collection: "orders", Gate: []string{"g"}})
			},
			// The runtime data path is the one data path, so a service with collections needs the command and the
			// query components (ST-SUP-RC20-OC39).
			"no command component": func(c *gen.Context) { delete(c.PeerNamespaces, "command") },
			"no query component":   func(c *gen.Context) { delete(c.PeerNamespaces, "query") },
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
		// An upsert whose reference is required decides its insert on the parent, so it is not refused.
		c := crudContext()
		withNotes(false)(&c)
		if _, _, err := new(Generator).Generate(c); err != nil {
			t.Errorf("an upsert with a required reference is refused: %v", err)
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
	"ST-SUP-RC20-OC24-T05 A scoped collection on the runtime data path checks each row against the parent of the path",
	"ST-SUP-RC20-OC23-T04 Implicit fields are forced and filtered on the runtime data path, under their path prefixes",
	"ST-SUP-RC20-OC37-T09 A list search on the runtime data path filters on declared fields with bound values",
	"ST-SUP-RC20-OC13-T03 An upsert on the runtime data path creates the row of its key with 201 and updates it with 200",
	"ST-SUP-RC20-OC14-T01 An upsert with optimistic concurrency updates only under the If-Match of the stored generation",
	"ST-SUP-RC20-OC13-T04 An upsert finds its row only under its own parent and implicit value",
	"ST-SUP-RC20-OC13-T05 An upsert calls its validate gate before both branches and on_entity_changed after both",
	"ST-SUP-RC20-OC23-T05 A write of a row of another implicit value is not found and changes nothing, and a patch keeps the implicit value",
	"ST-SUP-RC20-OC24-T06 A list cursor of one parent is refused for another",
	"ST-SUP-RC20-OC24-T07 An upper-case UUID in a path or a ref names the same row as its lower-case form",
	"ST-SUP-RP11-LC20-T04 A search of the generated service uses the index of its field, and a sensitive field neither searches nor orders",
	"ST-SUP-RC33-OC09-T02 A search of an entity of small expected cardinality reads only the rows that the caller may see, so hidden rows that match do not fill its pages",
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
		files, _, err := new(Generator).Generate(onRuntime(ctx))
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

func TestUpsertGateSeesNoServerField(t *testing.T) {
	t.Run("ST-SUP-RC20-OC13-T06 The validate gate of an upsert sees no server-managed field, and the insert sets them after it", func(t *testing.T) {
		ctx := crudContext()
		// Setting gets the server-managed fields created_by and stamped_at.
		ctx.Entities[5].Fields = append(ctx.Entities[5].Fields, types.Field{Name: "created_by", Type: types.FieldTypeString}, types.Field{Name: "stamped_at", Type: types.FieldTypeTimestamp})
		source := string(crudFiles(t, ctx)["api/folder_settings_handler.go"])
		start := strings.Index(source, "func (h *FolderSettingsHandler) Upsert(")
		if start < 0 {
			t.Fatal("no Upsert handler of folder-settings")
		}
		upsert := source[start:]
		gate := strings.Index(upsert, "h.validateGate.Evaluate(")
		blank := strings.Index(upsert, "setting.CreatedBy = serverBlank.CreatedBy")
		insert := strings.Index(upsert, "if len(found) == 0 {")
		caller := strings.Index(upsert, "setting.CreatedBy = callerName(r.Context())")
		if gate < 0 || blank < 0 || insert < 0 || caller < 0 {
			t.Fatalf("the Upsert handler lacks the gate, the cleared field, the insert branch or the caller value:\n%s", upsert)
		}
		if !(blank < gate && gate < insert && insert < caller) {
			t.Errorf("the gate does not see the cleared field before the insert sets it from the caller: clear %d, gate %d, insert %d, caller %d", blank, gate, insert, caller)
		}
		if strings.Count(upsert[:gate], "callerName(") != 0 || strings.Count(upsert[:gate], "time.Now()") != 0 {
			t.Error("a server value is set before the gate of the upsert")
		}
	})
}

func TestSlotsSeeCanonicalRefs(t *testing.T) {
	t.Run("ST-SUP-RC20-OC24-T08 The decode of a body gives each ref in lower case, before the before and validate slots see it", func(t *testing.T) {
		ctx := crudContext()
		// books takes folder_id from the body; it gets a validate gate, a patch of folder_id and a before_patch gate.
		for i := range ctx.Collections {
			if ctx.Collections[i].Name == "books" {
				ctx.Collections[i].Operations = append(ctx.Collections[i].Operations, types.OpPatch)
				ctx.Collections[i].Patchable = []string{"folder_id", "title"}
			}
		}
		ctx.SlotBindings = append(ctx.SlotBindings, types.SlotDeclaration{Slot: "validate", Collection: "books", Gate: []string{"book-gate"}},
			types.SlotDeclaration{Slot: "before_patch", Collection: "books", Gate: []string{"book-patch-gate"}})
		source := string(crudFiles(t, ctx)["api/books_handler.go"])
		section := func(start, end string) string {
			i := strings.Index(source, start)
			if i < 0 {
				t.Fatalf("the books handler has no %q:\n%s", start, source)
			}
			j := strings.Index(source[i:], end)
			if j < 0 {
				t.Fatalf("the books handler has no %q after %q", end, start)
			}
			return source[i : i+j]
		}
		decode := section("func decodeBooks(", "\n}\n")
		if !strings.Contains(decode, "book.FolderID = canonicalID(book.FolderID)") {
			t.Errorf("the decode of a books body does not give folder_id in lower case:\n%s", decode)
		}
		patch := section("func decodeBooksPatch(", "\n}\n")
		if !strings.Contains(patch, "*patch.FolderID = canonicalID(*patch.FolderID)") {
			t.Errorf("the decode of a books patch does not give folder_id in lower case:\n%s", patch)
		}
		for _, handler := range []string{"Create", "Update", "Patch"} {
			body := section("func (h *BooksHandler) "+handler+"(", "\n}\n")
			run := strings.Index(body, "command.Run(")
			gate := strings.Index(body, ".Evaluate(ctx, ")
			if gate < 0 || run < 0 || !strings.Contains(body[run:gate], "decodeBooks") {
				t.Errorf("the %s handler of books does not give the gate the value of the decode:\n%s", handler, body)
			}
		}
	})
}

func TestChangeEvents(t *testing.T) {
	t.Run("ST-SUP-RC20-OC40-T01 With kafka-producer each write stages its change event for the Kafka destination after its write", func(t *testing.T) {
		ctx := crudContext()
		ctx.PeerNamespaces["kafka-producer"] = "internal/events"
		ctx.PeerNamespaces["outbox"] = "internal/outbox"
		files := crudFiles(t, ctx)
		for name, want := range map[string][]string{
			"api/orders_handler.go": {
				`Destinations: []command.Destination{{Name: AfterSlotsDestination, Idempotent: true, MustLand: true, Effects: []string{"orders.after_create"}}, {Name: kafkaoutbox.DestinationName, Idempotent: true, Effects: []string{"crud.order.created"}}}`,
				`Destinations: []command.Destination{{Name: kafkaoutbox.DestinationName, Idempotent: true, Effects: []string{"crud.order.updated"}}}`,
				`Destinations: []command.Destination{{Name: kafkaoutbox.DestinationName, Idempotent: true, Effects: []string{"crud.order.deleted"}}}`,
				`ch.Effects().Stage(ctx, kafkaoutbox.DestinationName, "crud.order.created", created, changeEvent("Order", "created", persisted.ID))`,
				`ch.Effects().Stage(ctx, kafkaoutbox.DestinationName, "crud.order.updated", updated, changeEvent("Order", "updated", persisted.ID))`,
				`ch.Effects().Stage(ctx, kafkaoutbox.DestinationName, "crud.order.deleted", from, changeEvent("Order", "deleted", id))`,
			},
			"api/folder_settings_handler.go": {
				`{Name: kafkaoutbox.DestinationName, Idempotent: true, Effects: []string{"crud.setting.created", "crud.setting.updated"}}`,
				`ch.Effects().Stage(ctx, kafkaoutbox.DestinationName, "crud.setting.created", created, changeEvent("Setting", "created", persisted.ID))`,
				`ch.Effects().Stage(ctx, kafkaoutbox.DestinationName, "crud.setting.updated", updated, changeEvent("Setting", "updated", persisted.ID))`,
			},
			"api/runtime.go": {"func changeEvent(entity, action, id string) json.RawMessage {"},
		} {
			source := string(files[name])
			for _, w := range want {
				if strings.Count(source, w) != 1 {
					t.Errorf("%s holds %d times %s, want once", name, strings.Count(source, w), w)
				}
			}
		}
		// Each event is staged after the write of its command, in its fill, so a write that fails or a before slot
		// that halts stages none.
		orders := string(files["api/orders_handler.go"])
		for write, stage := range map[string]string{"repo.Create(ctx,": `"crud.order.created", created`, "repo.Delete(ctx, from)": `"crud.order.deleted", from`} {
			if i, j := strings.Index(orders, write), strings.Index(orders, stage); i < 0 || j < i {
				t.Errorf("the event %s is not staged after %s", stage, write)
			}
		}
		// A read stages nothing: the read handlers do not name the destination.
		if strings.Contains(string(files["api/blue_events_handler.go"]), "crud.event.updated") {
			t.Error("the collection blue-events, with create and list, stages an update event")
		}
		for name, source := range crudFiles(t, crudContext()) {
			if bytes.Contains(source, []byte("kafkaoutbox")) || bytes.Contains(source, []byte("changeEvent")) {
				t.Errorf("%s names the Kafka destination without kafka-producer", name)
			}
		}
		for name, change := range map[string]func(*gen.Context){
			"no outbox component":                       func(c *gen.Context) { delete(c.PeerNamespaces, "outbox") },
			"an event type that Kafka keeps for itself": func(c *gen.Context) { c.ServiceName = "__crud" },
			"an event type with a space":                func(c *gen.Context) { c.ServiceName = "my crud" },
			"an event type longer than 128 characters":  func(c *gen.Context) { c.ServiceName = strings.Repeat("c", 120) },
		} {
			c := crudContext()
			c.PeerNamespaces["kafka-producer"] = "internal/events"
			c.PeerNamespaces["outbox"] = "internal/outbox"
			change(&c)
			if files, _, err := new(Generator).Generate(c); err == nil || files != nil || !strings.Contains(err.Error(), "ST-SUP-RC20-OC40") {
				t.Errorf("%s: generation gave %d files and %v, want a refusal that names ST-SUP-RC20-OC40", name, len(files), err)
			}
		}
	})
}
