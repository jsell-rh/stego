package restapi

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/cliapplication"
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
			{Name: "meta", Type: types.FieldTypeJsonb, Optional: true}, {Name: "due_at", Type: types.FieldTypeTimestamp, Optional: true}, {Name: "seen_at", Type: types.FieldTypeTimestamp},
			{Name: "note", Type: types.FieldTypeString, Optional: true}},
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
		// Meeting gives timestamps that the client sets (ST-DECL-Q16): starts_at is required, ends_at optional, and
		// noted_at stays server-managed (ST-SUP-RC20-OC17). Its state, seats, rate and remote have defaults, which a
		// body that leaves them out stores (ST-DECL-EN03, ST-SUP-RC20-OC37-T14).
		types.Entity{Name: "Meeting", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString},
			{Name: "starts_at", Type: types.FieldTypeTimestamp, ClientSet: true}, {Name: "ends_at", Type: types.FieldTypeTimestamp, Optional: true, ClientSet: true},
			{Name: "noted_at", Type: types.FieldTypeTimestamp},
			{Name: "state", Type: types.FieldTypeEnum, Values: []string{"planned", "held"}, Default: "planned"},
			{Name: "seats", Type: types.FieldTypeInt32, Default: 8}, {Name: "rate", Type: types.FieldTypeDouble, Default: 1.5},
			{Name: "remote", Type: types.FieldTypeBool, Default: true},
			// code is an optional unique key: a second row with the same code is a conflict that names it
			// (ST-SUP-RC20-OC50).
			{Name: "code", Type: types.FieldTypeString, Optional: true, Unique: true}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "meeting", Permissions: permissions(types.OpUpdate, types.OpPatch), PublicReasons: reasons}},
		// Label gives an optional implicit field with a read: the view holds it as a pointer, and a row whose
		// implicit column is NULL is not found in the collection (ST-SUP-RC20-OC23, ST-SUP-Q128 (B)).
		types.Entity{Name: "Label", Fields: []types.Field{{Name: "source", Type: types.FieldTypeString, Optional: true}, {Name: "name", Type: types.FieldTypeString}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "label", Permissions: permissions(), PublicReasons: reasons}},
		// Ticket gives the fields that a collection write cannot change (ST-DECL-Q45 (a)): sku, meta and due_at with
		// writable create, state with writable never and a default, rank with writable never and no default, pin
		// a secret with writable create, and recovery a compare-only secret with writable never (ST-SUP-RC20-OC44
		// to ST-SUP-RC20-OC46, ST-SUP-RC20-OC48).
		types.Entity{Name: "Ticket", Fields: []types.Field{{Name: "folder_id", Type: types.FieldTypeRef, To: "Folder"}, {Name: "code", Type: types.FieldTypeString},
			{Name: "title", Type: types.FieldTypeString}, {Name: "sku", Type: types.FieldTypeString, Writable: types.WritableCreate},
			{Name: "state", Type: types.FieldTypeEnum, Values: []string{"open", "closed"}, Default: "open", Writable: types.WritableNever},
			{Name: "meta", Type: types.FieldTypeJsonb, Optional: true, Writable: types.WritableCreate},
			{Name: "due_at", Type: types.FieldTypeTimestamp, Optional: true, ClientSet: true, Writable: types.WritableCreate},
			{Name: "rank", Type: types.FieldTypeInt32, Optional: true, Writable: types.WritableNever},
			{Name: "pin", Type: types.FieldTypeString, Optional: true, Sensitivity: "secret", Writable: types.WritableCreate},
			{Name: "recovery", Type: types.FieldTypeString, Optional: true, Sensitivity: "secret", CompareOnly: true, Writable: types.WritableNever}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "ticket", Permissions: permissions(types.OpUpdate, types.OpPatch, types.OpUpsert), PublicReasons: reasons,
				References: map[string]types.Reference{"folder_id": {Relation: "folder", Permission: "edit"}}}},
		// Rack, Card and Mark give a parent chain of three levels: a mark is of a card, which is of a rack, which is
		// of a folder (ST-SUP-RC20-OC24).
		types.Entity{Name: "Rack", Fields: []types.Field{{Name: "folder_id", Type: types.FieldTypeRef, To: "Folder"}, {Name: "name", Type: types.FieldTypeString}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "rack", Permissions: permissions(), PublicReasons: reasons,
				References: map[string]types.Reference{"folder_id": {Relation: "parent", Permission: "edit"}}}},
		types.Entity{Name: "Card", Fields: []types.Field{{Name: "rack_id", Type: types.FieldTypeRef, To: "Rack"}, {Name: "title", Type: types.FieldTypeString}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "card", Permissions: permissions(), PublicReasons: reasons,
				References: map[string]types.Reference{"rack_id": {Relation: "parent", Permission: "edit"}}}},
		types.Entity{Name: "Mark", Fields: []types.Field{{Name: "card_id", Type: types.FieldTypeRef, To: "Card"}, {Name: "label", Type: types.FieldTypeString}, {Name: "note", Type: types.FieldTypeString}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "mark", Permissions: permissions(types.OpUpdate, types.OpPatch, types.OpDelete, types.OpUpsert), PublicReasons: reasons,
				References: map[string]types.Reference{"card_id": {Relation: "parent", Permission: "edit"}}}},
	)
	return withSchema(gen.Context{
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
			{Name: "todos", Entity: "Todo", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpPatch, types.OpUpdate, types.OpDelete}, Patchable: []string{"title", "done", "meta", "note"}},
			{Name: "folders", Entity: "Folder", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpDelete}},
			{Name: "folder-books", Entity: "Book", Scope: map[string]string{"folder_id": "Folder"}, Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpUpdate, types.OpPatch, types.OpDelete}, Patchable: []string{"title"}},
			{Name: "folder-settings", Entity: "Setting", Scope: map[string]string{"folder_id": "Folder"}, Implicit: map[string]string{"source": "setting"}, Operations: []types.Operation{types.OpList, types.OpUpsert}, UpsertKey: []string{"name"}},
			{Name: "red-events", Entity: "Event", PathPrefix: "/red-events", Implicit: map[string]string{"source": "red"}, Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpUpdate, types.OpPatch, types.OpDelete}, Patchable: []string{"note"}},
			{Name: "blue-events", Entity: "Event", PathPrefix: "/blue-events", Implicit: map[string]string{"source": "blue"}, Operations: []types.Operation{types.OpCreate, types.OpList}},
			{Name: "books", Entity: "Book", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate}},
			{Name: "folder-flags", Entity: "Setting", Scope: map[string]string{"folder_id": "Folder"}, Implicit: map[string]string{"source": "flag"}, PathPrefix: "/folders/{folder_id}/flags", Operations: []types.Operation{types.OpUpsert}, UpsertKey: []string{"name"}},
			{Name: "folder-quotas", Entity: "Quota", Scope: map[string]string{"folder_id": "Folder"}, Operations: []types.Operation{types.OpList, types.OpUpsert}, UpsertKey: []string{"name"}, Concurrency: types.ConcurrencyOptimistic},
			{Name: "meetings", Entity: "Meeting", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpPatch}, Patchable: []string{"starts_at", "ends_at"}},
			{Name: "green-labels", Entity: "Label", PathPrefix: "/green-labels", Implicit: map[string]string{"source": "green"}, Operations: []types.Operation{types.OpCreate, types.OpRead}},
			{Name: "tickets", Entity: "Ticket", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpPatch, types.OpUpsert}, Patchable: []string{"title"}, UpsertKey: []string{"code"}},
			// /folders/{folder_id}/racks, /folders/{folder_id}/racks/{rack_id}/cards and
			// /folders/{folder_id}/racks/{rack_id}/cards/{card_id}/marks.
			{Name: "folder-racks", Entity: "Rack", Scope: map[string]string{"folder_id": "Folder"}, Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList}},
			{Name: "rack-cards", Entity: "Card", Scope: map[string]string{"rack_id": "Rack"}, Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList}},
			{Name: "card-marks", Entity: "Mark", Scope: map[string]string{"card_id": "Card"}, Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpUpdate, types.OpPatch, types.OpDelete, types.OpUpsert}, Patchable: []string{"note"}, UpsertKey: []string{"label"}},
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
	})
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
	p.write("go.mod", []byte("module "+ctx.ModuleName+"\n\ngo 1.26.11\n\nrequire github.com/jsell-rh/stego/registry/runtime v0.180.0\n\nreplace github.com/jsell-rh/stego/registry/runtime => "+runtimeDir+"\n"))
	p.mustGo("mod", "tidy")
	return p
}

// crudRun is the one run of the checks of the generated service: one go test of the check package with every
// check, so the service is built and its test binary run once.
type crudRun struct {
	// cli is the path of the generated CLI program, which the checks get as STEGO_TEST_CLI.
	cli    string
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
		run.output, run.err = p.goCommand([]string{"STEGO_TEST_SPICEDB=" + endpoint, "STEGO_TEST_SPICEDB_KEY=" + key, "STEGO_TEST_CLI=" + run.cli},
			"test", "-count=1", "-timeout=300s", "-v", "-run", "^TestCrud$", "./check")
	})
	if !ownPass(run.output, "TestCrud", id) {
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
	cli := p.addCLI()
	p.mustGo("vet", "./...")
	endpoint, key := startCrudSpiceDB(t)
	run := &crudRun{cli: cli}
	for _, id := range crudCheckIDs {
		t.Run(id, func(t *testing.T) { run.passed(t, p, endpoint, key, strings.SplitN(id, " ", 2)[0]) })
	}
}

// crudCLIFactory is the factory of the generated CLI of the test service: one immutable apply resource, Folder, whose
// identity is its name, on the folders collection. It reads from the OpenAPI document of the service that the service
// keeps the Idempotency-Key of a create, and refuses to start when the document does not state it.
const crudCLIFactory = `package cliapp

import (
	_ "embed"

	command "example.com/crud/out/cli/command"
)

//go:embed openapi.json
var document []byte

// Commands gives the CLI of the test service, which keeps the Idempotency-Key of a write.
func Commands() command.Application {
	app, err := command.IdempotentWritesFrom(command.Application{ConfigEnv: "CRUD_CLI_CONFIG", ConfigName: "crud", Resources: []command.ApplyResource{{
		Kind: "Folder", APIVersion: "crud/v1", Path: "/api/v1/folders",
		CreateFields:      []command.Field{{Flag: "name", Key: "name", Type: "string", Required: true}},
		ImmutableIdentity: []string{"name"},
	}}}, document)
	if err != nil || !app.Resources[0].IdempotentWrites {
		panic("the OpenAPI document of the service does not state the Idempotency-Key of a create of a folder")
	}
	return app
}
`

// addCLI writes the CLI that the cli-application component emits, with the factory crudCLIFactory, into the project
// and builds it. It gives the path of the CLI program, which a check runs against the test service.
func (p *crudProject) addCLI() string {
	p.t.Helper()
	files, _, err := new(cliapplication.Generator).Generate(gen.Context{ModuleName: crudModule, OutDirName: "out", OutputNamespace: "cli",
		ComponentConfig: map[string]any{"factory_package": "cliapp"}})
	if err != nil {
		p.t.Fatal(err)
	}
	for _, f := range files {
		p.write("out/"+f.Path, f.Bytes())
	}
	document, err := os.ReadFile(filepath.Join(p.dir, "out", "api", "openapi.json"))
	if err != nil {
		p.t.Fatal(err)
	}
	p.write("cliapp/openapi.json", document)
	p.write("cliapp/app.go", []byte(crudCLIFactory))
	p.mustGo("mod", "tidy")
	program := filepath.Join(p.dir, "bin", "crud-cli")
	p.mustGo("build", "-o", program, "./out/cli/cmd")
	return program
}

func TestTopLevelCreate(t *testing.T) {
	t.Run("ST-SUP-RC29-OC01-T04 The create command of an entity with no parent decides its permission on the service-level resource", func(t *testing.T) {
		decided := func() gen.Context {
			c := crudContext()
			c.Tenancy = &gen.Tenancy{Key: "tenant_id", Provisioning: &types.TenantProvisioning{ResourceType: "service", Provision: "provision", Retire: "retire"}}
			c.Entities[3].Permissions[types.OpCreate] = "create"
			return c
		}
		files := crudFiles(t, decided())
		folders := string(files["api/folders_handler.go"])
		want := regexp.MustCompile(`\tPermission: +"create",\n\tResource: +&command\.Resource\{Type: "service", Service: "crud"\},\n\}\)\)$`)
		start := strings.Index(folders, "var foldersCreateCommand = ")
		if start < 0 {
			t.Fatal("the handler of folders declares no create command")
		}
		end := strings.Index(folders[start:], "}))")
		if spec := folders[start : start+end+3]; !want.MatchString(spec) {
			t.Errorf("the create command of folders is\n%s\nwant it to decide create on service:crud", spec)
		}
		// A create under a parent stays decided on the parent through its reference, and a public create decides
		// nothing of its own.
		books := string(files["api/folder_books_handler.go"])
		if strings.Contains(books, "Service:") {
			t.Error("the create of folder-books, a collection under a parent, is decided on the service-level resource")
		}
		if orders := string(files["api/orders_handler.go"]); strings.Contains(orders, "Service:") {
			t.Error("the public create of orders decides a permission")
		}
		for name, change := range map[string]func(*gen.Context){
			"a create under a parent that is not public": func(c *gen.Context) { c.Entities[4].Permissions[types.OpCreate] = "edit" },
			"no tenant provisioning":                     func(c *gen.Context) { c.Tenancy.Provisioning = nil },
		} {
			c := decided()
			change(&c)
			if files, _, err := new(Generator).Generate(c); err == nil || files != nil {
				t.Errorf("%s: generation gave %d files and %v, want a refusal", name, len(files), err)
			}
		}
	})
}

func TestNestedScope(t *testing.T) {
	t.Run("ST-SUP-RC20-OC24-T11 A collection under a scoped collection is generated with the route and the OpenAPI parameters of each ancestor, and a chain that names a parameter twice or out of order is refused", func(t *testing.T) {
		marks := "/api/v1/folders/{folder_id}/racks/{rack_id}/cards/{card_id}/marks"
		files, wiring, err := new(Generator).Generate(crudContext())
		if err != nil {
			t.Fatalf("a collection three levels under its top ancestor is refused: %v", err)
		}
		routes := strings.Join(wiring.Routes, "\n")
		for _, route := range []string{"POST " + marks, "GET " + marks, "PUT " + marks, "GET " + marks + "/{id}", "PUT " + marks + "/{id}",
			"PATCH " + marks + "/{id}", "DELETE " + marks + "/{id}", "POST /api/v1/folders/{folder_id}/racks/{rack_id}/cards"} {
			if !strings.Contains(routes, fmt.Sprintf("%q", route)) {
				t.Errorf("the wiring has no route %s:\n%s", route, routes)
			}
		}
		var doc struct {
			Paths map[string]map[string]struct {
				Parameters []struct{ Name, In string } `json:"parameters"`
			} `json:"paths"`
		}
		for _, f := range files {
			if strings.HasSuffix(f.Path, "openapi.json") {
				if err := json.Unmarshal(f.Content, &doc); err != nil {
					t.Fatal(err)
				}
			}
		}
		for path, want := range map[string][]string{marks: {"folder_id", "rack_id", "card_id"}, marks + "/{id}": {"folder_id", "rack_id", "card_id", "id"}} {
			operations, ok := doc.Paths[path]
			if !ok {
				t.Fatalf("the OpenAPI document has no path %s", path)
			}
			for method, op := range operations {
				var got []string
				for _, p := range op.Parameters {
					if p.In == "path" {
						got = append(got, p.Name)
					}
				}
				if !slices.Equal(got, want) {
					t.Errorf("%s %s has the path parameters %v, want %v", method, path, got, want)
				}
			}
		}
		// A leaf path prefix keeps the chain of its ancestors.
		c := crudContext()
		c.Collections[len(c.Collections)-1].PathPrefix = "/notes"
		_, wiring, err = new(Generator).Generate(c)
		if err != nil || !strings.Contains(strings.Join(wiring.Routes, "\n"), "\"GET /api/v1/folders/{folder_id}/racks/{rack_id}/cards/{card_id}/notes/{id}\"") {
			t.Errorf("a leaf path prefix under the chain gave %v, want the route under each ancestor", err)
		}
		for name, change := range map[string]func(*gen.Context){
			"a scope field of the same name at two levels": func(c *gen.Context) {
				mark := &c.Entities[len(c.Entities)-1]
				mark.Fields[0].Name = "rack_id"
				mark.References = map[string]types.Reference{"rack_id": {Relation: "parent", Permission: "edit"}}
				c.Collections[len(c.Collections)-1].Scope = map[string]string{"rack_id": "Card"}
			},
			"a full path that names a parameter of no ancestor": func(c *gen.Context) {
				c.Collections[len(c.Collections)-1].PathPrefix = "/folders/{folder_id}/boxes/{box_id}/cards/{card_id}/marks"
			},
			"a full path that names the ancestors out of order": func(c *gen.Context) {
				c.Collections[len(c.Collections)-1].PathPrefix = "/racks/{rack_id}/folders/{folder_id}/cards/{card_id}/marks"
			},
			// The link of the entity CardRack and the link of Card by its field rack have one Go name, CardRackLink
			// (security review of PR #409, L2).
			"two links of one Go name": func(c *gen.Context) {
				c.Entities = append(c.Entities, types.Entity{Name: "CardRack", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}},
					EntityAuthorization: types.EntityAuthorization{ResourceType: "cardrack", Permissions: map[types.Operation]string{types.OpCreate: "public", types.OpRead: "view", types.OpList: "view"},
						PublicReasons: map[types.Operation]string{types.OpCreate: "a new row has no resource to decide on before it exists"}}})
				for i := range c.Entities {
					if c.Entities[i].Name == "Card" {
						c.Entities[i].Fields = append(c.Entities[i].Fields, types.Field{Name: "rack", Type: types.FieldTypeRef, To: "CardRack"})
						c.Entities[i].Permissions[types.OpUpdate] = "edit"
					}
				}
				c.Collections = append(c.Collections,
					types.Collection{Name: "card-racks", Entity: "CardRack", Operations: []types.Operation{types.OpCreate, types.OpRead}},
					types.Collection{Name: "cardrack-cards", Entity: "Card", Scope: map[string]string{"rack": "CardRack"}, Operations: []types.Operation{types.OpRead, types.OpUpdate}})
			},
		} {
			c := crudContext()
			change(&c)
			if files, wiring, err := new(Generator).Generate(c); err == nil || files != nil || wiring != nil {
				t.Errorf("%s: generation gave %d files and %v, want a refusal", name, len(files), err)
			} else if name == "two links of one Go name" && !strings.Contains(err.Error(), "CardRackLink") {
				t.Errorf("%s: generation refused with %v, want the refusal of the link name CardRackLink", name, err)
			}
		}
	})
}

func TestFieldRelationship(t *testing.T) {
	t.Run("ST-SUP-RC29-OC05-T06 A public create of an entity with a field-sourced relationship is refused, and a refused field value is 400 naming the field", func(t *testing.T) {
		c := crudContext()
		c.Entities[0].Fields = append(c.Entities[0].Fields, types.Field{Name: "owner_id", Type: types.FieldTypeString})
		c.Entities[0].Relationships = []types.Relationship{{Relation: "owner", SubjectSource: types.SubjectSourceField, SubjectField: "owner_id", SubjectType: "user"}}
		files, wiring, err := new(Generator).Generate(c)
		if err == nil || files != nil || wiring != nil || !strings.Contains(err.Error(), "public create of Order") || !strings.Contains(err.Error(), "owner_id") {
			t.Fatalf("a public create with a field relationship gave %d files and %v, want a refusal naming the entity and the field", len(files), err)
		}
		// A caller-sourced relationship is the caller's own: the public create is not refused for it.
		c = crudContext()
		c.Entities[0].Relationships = []types.Relationship{{Relation: "creator", SubjectSource: types.SubjectSourceCaller, SubjectType: "user"}}
		if _, _, err := new(Generator).Generate(c); err != nil {
			t.Fatalf("a public create with a caller relationship is refused: %v", err)
		}
		// The runtime is the guard of a field subject on every interface; the handlers answer its refusal with 400 and
		// one validation entry that names the field.
		answer := extractMethodBody(string(crudFiles(t, c)["api/runtime.go"]), "func (rt *Runtime) answerWrite(")
		if !strings.Contains(answer, "case http.StatusBadRequest:") || !strings.Contains(answer, "Validation([]ValidationError{{Field: o.Field, Message: o.Reason}})") {
			t.Fatalf("answerWrite does not answer a refused field value with 400 naming the field:\n%s", answer)
		}
	})
}

// crudCheckIDs are the checks of the generated service that this test runs, each with the ID of the test record it
// proves.
var crudCheckIDs = []string{
	"ST-SUP-RC20-OC44-T01 An update refuses a changed create or never field, names each, changes nothing, and keeps a field that the body leaves out or repeats",
	"ST-SUP-RC20-OC45-T01 A create and an upsert that inserts store the default of a never field and refuse another value, and an upsert of a stored row keeps each kept field",
	"ST-SUP-RC20-OC46-T01 A patch refuses a create or never field that the body names with another value, and changes nothing",
	"ST-SUP-RC20-OC15-T03 A patch refuses a member that is not kind, a patchable field or a kept field, also in another letter case, and changes nothing",
	"ST-SUP-RC20-OC44-T02 A secret kept field is refused whenever the body gives it, so the answer tells nothing of its value",
	"ST-SUP-RC20-OC48-T03 A compare-only secret with writable never is refused whenever a body gives it, and no write stores it",
	"ST-SUP-RC20-OC48-T04 No answer holds a member of a secret field, also not of a required one",
	"ST-SUP-RC20-OC48-T05 An update that leaves out a required secret, or gives it empty, answers 200 and keeps the stored value",
	"ST-SUP-RC20-OC51-T01 A list refuses a query parameter that it does not read, names it, and quotes no value",
	"ST-SUP-RC04-OC23-T06 The emitted apply command finds a resource through the list of a generated service",
	"ST-SUP-RC04-OC31-T02 The emitted CLI writes to a generated service that requires a key, and the second request of a write gets the stored answer",
	"ST-SUP-RC30-OC08-T02 Each answer of the generated service to a create or a patch with a key carries Idempotency-Kept: 1, a problem and a 503 too, and no other answer carries it",
	"ST-SUP-RC04-OC34-T02 The emitted CLI sends a write again after a 5xx of the service with Idempotency-Kept, not after a 5xx with none, and warns when a 2xx has none",
	"ST-SUP-RC20-OC45-T03 An upsert by a caller who may not write under the parent gets the same 404 whether a row of the key exists or not",
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
	"ST-SUP-RC20-OC55-T01 A create, an update and an upsert refuse a member that they do not take, also in another letter case, store nothing, and take a read row sent back",
	"ST-SUP-RC20-OC55-T07 A write refuses a member given twice, cuts a long member name, and ignores a server-set member of another type",
	"ST-SUP-RC30-OC04-T01 The key row commits with the write",
	"ST-SUP-RC30-OC04-T03 The same key from another tenant or principal is a new key",
	"ST-SUP-RC28-OC02-T02 A committed mutation leaves exactly one audit row",
	"ST-SUP-RC28-OC03-T01 A denied mutation writes one audit row with outcome denied after the rollback",
	"ST-SUP-RC28-OC04-T01 A single read of an entity with a pii field writes one read audit row",
	"ST-SUP-RC28-OC04-T03 A list that returns a pii field writes one read audit row per returned resource, at most one page",
	"ST-SUP-RC28-OC01-T03 A secret field reaches neither before, after nor the stored row",
	"ST-SUP-RC20-OC38-T01 A list on the runtime data path pages by cursor in the requested order, with no total",
	"ST-SUP-RC20-OC37-T08 A jsonb field is written, read and patched as JSON on the runtime data path",
	"ST-SUP-RC20-OC37-T11 A denied create answers 404 with no empty ID, and a bad Idempotency-Key names its fault",
	"ST-SUP-RC20-OC37-T13 A list, a read and a write that the provider cannot decide now answer 503 with Retry-After, a refused provider 500, and the log names the cause",
	"ST-SUP-RC20-OC37-T10 A NULL timestamp is absent from the answer, and each timestamp is answered in UTC",
	"ST-SUP-RC20-OC37-T14 A create or an update that leaves out a field with a default stores the default",
	"ST-SUP-RC20-OC50-T01 A create or an update that breaks a unique key answers 409 that names the key and no value",
	"ST-SUP-RC20-OC37-T12 An optional field that is NULL is absent from the answer, and an empty or false value is answered",
	"ST-SUP-RC28-OC01-T06 A secret jsonb field reaches neither the audit rows, the answer nor the stored response",
	"ST-SUP-RC28-OC01-T07 A pii jsonb field is the redaction marker in the before, the after and the patch",
	"ST-SUP-RC20-OC24-T05 A scoped collection on the runtime data path checks each row against the parent of the path",
	"ST-SUP-RC20-OC23-T04 Implicit fields are forced and filtered on the runtime data path, under their path prefixes",
	"ST-SUP-RC20-OC37-T09 A list search on the runtime data path filters on declared fields with bound values",
	"ST-SUP-RC20-OC13-T03 An upsert on the runtime data path creates the row of its key with 201 and updates it with 200",
	"ST-SUP-RC20-OC14-T01 An upsert with optimistic concurrency updates only under the If-Match of the stored generation",
	"ST-SUP-RC20-OC13-T04 An upsert finds its row only under its own parent and implicit value",
	"ST-SUP-RC20-OC13-T05 An upsert calls its validate gate before both branches and on_entity_changed after both",
	"ST-SUP-RC20-OC23-T05 A write of a row of another implicit value is not found and changes nothing, and a write that gives the implicit field another value is refused",
	"ST-SUP-RC20-OC23-T07 A read of a row whose optional implicit field is NULL is not found",
	"ST-SUP-RC20-OC24-T06 A list cursor of one parent is refused for another",
	"ST-SUP-RC20-OC24-T07 An upper-case UUID in a path or a ref names the same row as its lower-case form",
	"ST-SUP-RC20-OC24-T09 A collection three levels under its top ancestor checks every ancestor of the path, and a wrong, foreign or smuggled ancestor is 404",
	"ST-SUP-RC20-OC24-T10 A grant on a resource of the path reaches the rows below it through the scoped routes, and a caller with no grant gets 404",
	"ST-SUP-RC20-OC24-T12 A missing, wrong or denied path gives the same 404 with the same calls to the authorization port, and changes nothing",
	"ST-SUP-RC20-OC24-T13 The parent chain check reads the ancestors only after the decision on the deepest resource, and decides nothing on them",
	"ST-SUP-RP11-LC20-T04 A search of the generated service uses the index of its field, and a sensitive field neither searches nor orders",
	"ST-SUP-RC20-OC17-T05 A client-set timestamp keeps the client's time in UTC and refuses a value that is not an RFC 3339 time",
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
	// A create of todos requires the key, which the check reads before the body (ST-SUP-RC30-OC02).
	large.Header.Set("Idempotency-Key", "\"k-1\"")
	answer := httptest.NewRecorder()
	handler.ServeHTTP(answer, large)
	if answer.Code != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("a body over the limit got %d and called the handler %v: %s", answer.Code, called, answer.Body)
	}
	small := httptest.NewRequest(http.MethodPost, "/api/v1/todos", strings.NewReader("{\"title\":7}"))
	small.Header.Set("Content-Type", "application/json")
	small.Header.Set("Idempotency-Key", "\"k-2\"")
	answer = httptest.NewRecorder()
	handler.ServeHTTP(answer, small)
	if answer.Code != http.StatusBadRequest || called {
		t.Fatalf("an invalid body below the limit got %d and called the handler %v: %s", answer.Code, called, answer.Body)
	}
}
`

// everyFieldCheck is a test of the generated validation.go: a body with two bad members gets one entry for each.
const everyFieldCheck = `package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestEveryField(t *testing.T) {
	handler := NewValidationMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("an invalid body reached the handler")
	}))
	r := httptest.NewRequest(http.MethodPost, "/api/v1/todos", strings.NewReader("{\"title\":7,\"done\":\"no\"}"))
	r.Header.Set("Content-Type", "application/json")
	// A create of todos requires the key, which the check reads before the body (ST-SUP-RC30-OC02).
	r.Header.Set("Idempotency-Key", "\"k-1\"")
	answer := httptest.NewRecorder()
	handler.ServeHTTP(answer, r)
	var problem struct {
		Errors []struct {
			Field string \x60json:"field"\x60
		} \x60json:"validation_errors"\x60
	}
	if err := json.Unmarshal(answer.Body.Bytes(), &problem); answer.Code != http.StatusBadRequest || err != nil {
		t.Fatalf("got %d (%v): %s", answer.Code, err, answer.Body)
	}
	var fields []string
	for _, e := range problem.Errors {
		fields = append(fields, e.Field)
	}
	slices.Sort(fields)
	if !slices.Equal(fields, []string{"done", "title"}) {
		t.Fatalf("the problem lists %v, want one entry for done and one for title: %s", fields, answer.Body)
	}
}
`

// requiredMemberCheck is a test of the generated validation.go: a create or a PUT whose body leaves out the required
// member title gets a 400 validation problem that names title, before any decode of the handler; a body that holds
// title reaches the handler.
const requiredMemberCheck = `package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequiredMember(t *testing.T) {
	reached := false
	handler := NewValidationMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	send := func(method, path, body string) *httptest.ResponseRecorder {
		reached = false
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		// A create of todos requires the key, which the check reads before the body (ST-SUP-RC30-OC02).
		r.Header.Set("Idempotency-Key", "\"k-1\"")
		answer := httptest.NewRecorder()
		handler.ServeHTTP(answer, r)
		return answer
	}
	const path = "/api/v1/todos/0b9c6a5e-6f7a-4c1e-9a52-2f1d3c4b5a69"
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/todos", "{\"done\":true}"},
		{http.MethodPost, "/api/v1/todos", "{}"},
		{http.MethodPut, path, "{\"done\":true}"},
		{http.MethodPut, path, "{}"},
	} {
		answer := send(c.method, c.path, c.body)
		var problem struct {
			Errors []struct {
				Field   string \x60json:"field"\x60
				Message string \x60json:"message"\x60
			} \x60json:"validation_errors"\x60
		}
		if err := json.Unmarshal(answer.Body.Bytes(), &problem); answer.Code != http.StatusBadRequest || err != nil || reached {
			t.Fatalf("%s %s %s got %d (%v), handler reached %v: %s", c.method, c.path, c.body, answer.Code, err, reached, answer.Body)
		}
		if len(problem.Errors) != 1 || problem.Errors[0].Field != "title" || !strings.Contains(problem.Errors[0].Message, "title") {
			t.Fatalf("%s %s %s: the problem lists %+v, want one entry that names title", c.method, c.path, c.body, problem.Errors)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		target := "/api/v1/todos"
		if method == http.MethodPut {
			target = path
		}
		if answer := send(method, target, "{\"title\":\"t\"}"); answer.Code != http.StatusNoContent || !reached {
			t.Fatalf("%s %s with title got %d, handler reached %v: %s", method, target, answer.Code, reached, answer.Body)
		}
	}
}
`

// unknownMemberCheck is a test of the generated validation.go: a create, an update, an upsert and a patch whose body
// holds a member that the closed schema does not take get a 400 validation problem whose entry names the member in its
// field, before any decode of the handler, and the problem holds no value of the body.
const unknownMemberCheck = `package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnknownMember(t *testing.T) {
	handler := NewValidationMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("a body with an unknown member reached the handler")
	}))
	const todo = "/api/v1/todos/0b9c6a5e-6f7a-4c1e-9a52-2f1d3c4b5a69"
	const ticket = "{\"folder_id\":\"0b9c6a5e-6f7a-4c1e-9a52-2f1d3c4b5a69\",\"code\":\"c1\",\"title\":\"t\",\"sku\":\"s1\","
	for _, c := range []struct{ method, path, body, member string }{
		{http.MethodPost, "/api/v1/todos", "{\"title\":\"t\",\"Title\":\"value-x\"}", "Title"},
		{http.MethodPut, todo, "{\"title\":\"t\",\"titel\":\"value-x\"}", "titel"},
		{http.MethodPatch, todo, "{\"titel\":\"value-x\"}", "titel"},
		{http.MethodPut, "/api/v1/tickets", ticket + "\"tenant_id\":\"value-x\"}", "tenant_id"},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		r.Header.Set("Content-Type", "application/json")
		// A create of todos requires the key, which the check reads before the body (ST-SUP-RC30-OC02).
		r.Header.Set("Idempotency-Key", "\"k-1\"")
		answer := httptest.NewRecorder()
		handler.ServeHTTP(answer, r)
		var problem struct {
			Errors []struct {
				Field string \x60json:"field"\x60
			} \x60json:"validation_errors"\x60
		}
		if err := json.Unmarshal(answer.Body.Bytes(), &problem); answer.Code != http.StatusBadRequest || err != nil || len(problem.Errors) != 1 ||
			problem.Errors[0].Field != c.member || strings.Contains(answer.Body.String(), "value-x") {
			t.Fatalf("%s %s %s got %d (%v): %s, want one entry whose field is %s and no value", c.method, c.path, c.body, answer.Code, err, answer.Body, c.member)
		}
	}
}
`

// keyFirstCheck is a test of the generated validation.go: the Idempotency-Key header of a create and a patch is read
// before the body, as a command reads it, so a malformed key gets idempotency-key-invalid and an absent required key
// idempotency-key-missing also when the body is not valid, and a valid key with a body that is not valid gets the
// validation problem with Idempotency-Kept: 1. KEY_BASE is the problem base of the key types, and KEY_REQUIRED tells
// whether the create requires the key.
const keyFirstCheck = `package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeyFirst(t *testing.T) {
	handler := NewValidationMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("a request with a body that is not valid reached the handler")
	}))
	send := func(method, path, key string, set bool) (*httptest.ResponseRecorder, string) {
		r := httptest.NewRequest(method, path, strings.NewReader("{\"title\":7}"))
		r.Header.Set("Content-Type", "application/json")
		if set {
			r.Header.Set("Idempotency-Key", key)
		}
		answer := httptest.NewRecorder()
		handler.ServeHTTP(answer, r)
		var problem struct {
			Type string \x60json:"type"\x60
		}
		if err := json.Unmarshal(answer.Body.Bytes(), &problem); err != nil || answer.Code != http.StatusBadRequest {
			t.Fatalf("%s %s with the key %q got %d (%v): %s", method, path, key, answer.Code, err, answer.Body)
		}
		return answer, problem.Type
	}
	const todo = "/api/v1/todos/0b9c6a5e-6f7a-4c1e-9a52-2f1d3c4b5a69"
	for _, c := range []struct{ method, path string }{{http.MethodPost, "/api/v1/todos"}, {http.MethodPatch, todo}} {
		for _, key := range []string{"k-unquoted", "\"\""} {
			if answer, got := send(c.method, c.path, key, true); got != "KEY_BASE"+"idempotency-key-invalid" || answer.Header().Get("Idempotency-Kept") != "" {
				t.Errorf("%s %s with the malformed key %q and a body that is not valid got the type %q, want %sidempotency-key-invalid: %s", c.method, c.path, key, got, "KEY_BASE", answer.Body)
			}
		}
		answer, got := send(c.method, c.path, "\"k-1\"", true)
		if !strings.HasSuffix(got, "/validation-error") || answer.Header().Get("Idempotency-Kept") != "1" {
			t.Errorf("%s %s with a valid key and a body that is not valid got the type %q and Idempotency-Kept %q, want validation-error and 1", c.method, c.path, got, answer.Header().Get("Idempotency-Kept"))
		}
	}
	answer, got := send(http.MethodPost, "/api/v1/todos", "", false)
	want := "validation-error"
	if KEY_REQUIRED {
		want = "KEY_BASE" + "idempotency-key-missing"
	}
	if !strings.HasSuffix(got, want) {
		t.Errorf("a create with no key and a body that is not valid got the type %q, want %s: %s", got, want, answer.Body)
	}
}
`

// TestValidationKeyFirst runs keyFirstCheck on the generated service of crudContext, with the idempotency component and
// Required on create and patch, and with no idempotency component, whose key types sit under the problem base too.
func TestValidationKeyFirst(t *testing.T) {
	t.Run("ST-SUP-RC30-OC02-T04 A create or a patch with a malformed Idempotency-Key gets idempotency-key-invalid under the problem base before the check of its body, with or without the idempotency component", func(t *testing.T) {
		for _, component := range []bool{true, false} {
			ctx := crudContext()
			ctx.Conventions.RequestValidation = "openapi-schema"
			if !component {
				ctx.Idempotency = nil
				delete(ctx.PeerNamespaces, "idempotency")
			}
			p := newCrudProject(t, ctx, crudFiles(t, ctx))
			check := strings.ReplaceAll(strings.ReplaceAll(keyFirstCheck, "KEY_BASE", ctx.ErrorTypeBase), "KEY_REQUIRED", strconv.FormatBool(component))
			p.write("out/api/key_first_test.go", []byte(strings.ReplaceAll(check, "\\x60", "`")))
			if output, err := p.goCommand(nil, "test", "-count=1", "-run", "^TestKeyFirst$", "./out/api"); err != nil {
				t.Fatalf("with the idempotency component %v, the check of the key before the body failed: %v\n%s", component, err, output)
			}
		}
	})
}

func TestValidationEveryField(t *testing.T) {
	ctx := crudContext()
	ctx.Conventions.RequestValidation = "openapi-schema"
	p := newCrudProject(t, ctx, crudFiles(t, ctx))
	p.write("out/api/every_field_test.go", []byte(strings.ReplaceAll(everyFieldCheck, "\\x60", "`")))
	p.write("out/api/required_member_test.go", []byte(strings.ReplaceAll(requiredMemberCheck, "\\x60", "`")))
	p.write("out/api/unknown_member_test.go", []byte(strings.ReplaceAll(unknownMemberCheck, "\\x60", "`")))
	t.Run("ST-SUP-RC20-OC05-T04 A body with two bad members gets a validation problem with one entry for each", func(t *testing.T) {
		if output, err := p.goCommand(nil, "test", "-count=1", "-run", "^TestEveryField$", "./out/api"); err != nil {
			t.Fatalf("the check of every field failed: %v\n%s", err, output)
		}
	})
	t.Run("ST-SUP-RC20-OC05-T07 A create or a PUT that leaves out a required member gets a validation problem that names it", func(t *testing.T) {
		if output, err := p.goCommand(nil, "test", "-count=1", "-run", "^TestRequiredMember$", "./out/api"); err != nil {
			t.Fatalf("the check of a missing required member failed: %v\n%s", err, output)
		}
	})
	t.Run("ST-SUP-RC20-OC55-T06 The request check names an unknown member of a create, an update, an upsert and a patch in the field of its entry", func(t *testing.T) {
		if output, err := p.goCommand(nil, "test", "-count=1", "-run", "^TestUnknownMember$", "./out/api"); err != nil {
			t.Fatalf("the check of an unknown member failed: %v\n%s", err, output)
		}
	})
}

// validationCapCheck is a test of the generated validation.go: a failure of many members lists at most
// MaxValidationErrors of them, and then one entry that counts the rest.
const validationCapCheck = `package api

import (
	"fmt"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestValidationCap(t *testing.T) {
	for _, failures := range []int{MaxValidationErrors, MaxValidationErrors + 1, 120} {
		var multi openapi3.MultiError
		for i := 0; i < failures; i++ {
			multi = append(multi, &openapi3.SchemaError{Reason: "value must be a string"})
		}
		// A nested MultiError counts against the same cap.
		got := parseValidationErrors(openapi3.MultiError{multi[:failures/2], multi[failures/2:]})
		want := failures
		if failures > MaxValidationErrors {
			want = MaxValidationErrors + 1
		}
		if len(got) != want {
			t.Fatalf("%d failures give %d entries, want %d", failures, len(got), want)
		}
		last := got[len(got)-1].Message
		if failures > MaxValidationErrors && last != fmt.Sprintf("%d more failures not listed", failures-MaxValidationErrors) {
			t.Fatalf("%d failures end with %q, want the count of the failures not listed", failures, last)
		}
		if failures <= MaxValidationErrors && last != "value must be a string" {
			t.Fatalf("%d failures end with %q, want no count entry", failures, last)
		}
	}
	if MaxValidationErrors != 50 {
		t.Fatalf("MaxValidationErrors is %d, want 50", MaxValidationErrors)
	}
}
`

// markedValueCheck is a test of the generated validation.go: no validation entry quotes a value of the request.
const markedValueCheck = `package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
)

func TestNoMarkedValue(t *testing.T) {
	handler := NewValidationMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("an invalid body reached the handler")
	}))
	for _, body := range []string{
		"{\"title\":7,\"done\":\"MARKED-VALUE-1\"}",
		"{\"title\":{\"k\":\"MARKED-VALUE-2\"},\"done\":true}",
		"{\"title\":[\"MARKED-VALUE-3\"]}",
		"{\"title\":\"MARKED-VALUE-4",
		"{\"title\": MARKED-VALUE-5}",
		"MARKED-VALUE-6",
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/todos", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		answer := httptest.NewRecorder()
		handler.ServeHTTP(answer, r)
		if answer.Code != http.StatusBadRequest {
			t.Fatalf("the body %s got %d: %s", body, answer.Code, answer.Body)
		}
		if strings.Contains(answer.Body.String(), "MARKED-VALUE") {
			t.Fatalf("the problem for the body %s quotes the value: %s", body, answer.Body)
		}
	}
	// The error forms that quote a value in their own text: a schema error with no reason and a parse error.
	for _, err := range []error{
		&openapi3.SchemaError{Value: "MARKED-VALUE-7"},
		&openapi3filter.ParseError{Value: "MARKED-VALUE-8", Reason: "cannot decode the body"},
		&openapi3filter.ParseError{Value: "MARKED-VALUE-9"},
		&openapi3filter.RequestError{Err: &openapi3filter.ParseError{Value: "MARKED-VALUE-10"}},
	} {
		for _, entry := range parseValidationErrors(err) {
			if strings.Contains(entry.Field+entry.Message, "MARKED-VALUE") {
				t.Fatalf("the entry %+v quotes the value of %T", entry, err)
			}
		}
	}
}
`

func TestValidationBounds(t *testing.T) {
	ctx := crudContext()
	ctx.Conventions.RequestValidation = "openapi-schema"
	p := newCrudProject(t, ctx, crudFiles(t, ctx))
	p.write("out/api/cap_test.go", []byte(validationCapCheck))
	p.write("out/api/marked_test.go", []byte(markedValueCheck))
	t.Run("ST-SUP-RC20-OC05-T05 A validation problem lists at most 50 failed members and one entry that counts the rest", func(t *testing.T) {
		if output, err := p.goCommand(nil, "test", "-count=1", "-run", "^TestValidationCap$", "./out/api"); err != nil {
			t.Fatalf("the check of the cap failed: %v\n%s", err, output)
		}
	})
	t.Run("ST-SUP-RC20-OC05-T06 No validation entry quotes a value of the request", func(t *testing.T) {
		if output, err := p.goCommand(nil, "test", "-count=1", "-run", "^TestNoMarkedValue$", "./out/api"); err != nil {
			t.Fatalf("the check of the marked values failed: %v\n%s", err, output)
		}
	})
}

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

func TestValidationTokenLimit(t *testing.T) {
	t.Run("ST-SUP-RC20-OC21-T05 A body with more JSON values than MaxBodyTokens is refused before the schema check builds its failures", func(t *testing.T) {
		ctx := crudContext()
		ctx.Conventions.RequestValidation = "openapi-schema"
		files := crudFiles(t, ctx)
		p := newCrudProject(t, ctx, files)
		p.write("out/api/token_limit_test.go", []byte(tokenLimitCheck))
		if output, err := p.goCommand(nil, "test", "-count=1", "-run", "^TestTokenLimit$", "./out/api"); err != nil {
			t.Fatalf("the token limit check failed: %v\n%s", err, output)
		}
	})
}

// tokenLimitCheck is a test of the generated validation.go: a body of more than MaxBodyTokens values gets one
// validation entry and no schema failures, and the schema check is not run for it.
const tokenLimitCheck = `package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func post(t *testing.T, body string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	called := false
	handler := NewValidationMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/todos", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	answer := httptest.NewRecorder()
	handler.ServeHTTP(answer, request)
	return answer, called
}

func entries(t *testing.T, answer *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var problem struct {
		Errors []map[string]any ` + "`json:\"validation_errors\"`" + `
	}
	if err := json.Unmarshal(answer.Body.Bytes(), &problem); err != nil {
		t.Fatalf("the answer is not a problem: %v: %s", err, answer.Body)
	}
	return problem.Errors
}

func TestTokenLimit(t *testing.T) {
	// A request schema with an array of integers whose items each fail, so the schema check would make one failure
	// for each item of a body that the limit lets through.
	openAPISpec = []byte(` + "`" + `{"openapi":"3.0.3","info":{"title":"t","version":"1"},"paths":{"/api/v1/todos":{"post":{"requestBody":{"required":true,"content":{"application/json":{"schema":{"type":"object","properties":{"tags":{"type":"array","items":{"type":"integer","maximum":0}},"name":{"type":"string","maxLength":5}}}}}},"responses":{"200":{"description":"ok"}}}}}}` + "`" + `)
	items := func(n int) string { return strings.Repeat("1,", n-1) + "1" }
	// One MiB of items: about 524000 values.
	many := (MaxRequestBody - 64) / 2
	for name, body := range map[string]string{
		"a one MiB array":                 "{\"tags\":[" + items(many) + "]}",
		"1e400 first":                     "{\"tags\":[1e400," + items(many-1) + "]}",
		"-1e400 first":                    "{\"tags\":[-1e400," + items(many-1) + "]}",
		"1e400 last":                      "{\"tags\":[" + items(many-1) + ",1e400]}",
		"1e400 in a member before":        "{\"name\":1e400,\"tags\":[" + items(many-1) + "]}",
		"one token more than the limit":   "{\"tags\":[" + items(MaxBodyTokens-2) + "]}",
	} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		answer, called := post(t, body)
		runtime.ReadMemStats(&after)
		if answer.Code != http.StatusBadRequest || called {
			t.Fatalf("%s got %d and called the handler %v", name, answer.Code, called)
		}
		list := entries(t, answer)
		if len(list) != 1 || !strings.Contains(list[0]["message"].(string), "more than 10000 JSON values") {
			t.Fatalf("%s got %d entries %.200v, want one entry that names the limit", name, len(list), list)
		}
		if grown := after.TotalAlloc - before.TotalAlloc; grown > 32<<20 {
			t.Fatalf("%s allocated %d bytes", name, grown)
		}
	}
	// At the limit, the schema check runs and lists its failures, at most 50 and one entry that counts the rest.
	answer, called := post(t, "{\"tags\":["+items(MaxBodyTokens-3)+"]}")
	if answer.Code != http.StatusBadRequest || called {
		t.Fatalf("a bad body at the token limit got %d and called the handler %v", answer.Code, called)
	}
	if list := entries(t, answer); len(list) != 51 {
		t.Fatalf("a body at the token limit got %d entries, want 50 failures and one count", len(list))
	}
	// A body that is not JSON is refused, also when it is cut after a value.
	for _, body := range []string{"{", "{\"tags\":[1,", "{\"tags\" 1}", "1e400 x"} {
		// The limit refuses it itself: an error of the count never lets a body pass to the schema check.
		if answer, called := post(t, body); answer.Code != http.StatusBadRequest || called || !strings.Contains(answer.Body.String(), "not valid JSON") {
			t.Fatalf("the body %q got %d and called the handler %v: %s", body, answer.Code, called, answer.Body)
		}
	}
	if answer, called := post(t, "{\"tags\":[0],\"name\":\"ok\"}"); answer.Code != http.StatusNoContent || !called {
		t.Fatalf("a valid body got %d and called the handler %v", answer.Code, called)
	}
}
`

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
	t.Run("ST-SUP-RC20-OC40-T01 With kafka-producer each write gives the handle its change events for the Kafka destination", func(t *testing.T) {
		ctx := crudContext()
		ctx.PeerNamespaces["kafka-producer"] = "internal/events"
		ctx.PeerNamespaces["outbox"] = "internal/outbox"
		files := crudFiles(t, ctx)
		order := `ChangeEvents: []command.ChangeEvents{{Entity: "Order", Created: "crud.order.created", Updated: "crud.order.updated", Deleted: "crud.order.deleted"}}`
		for name, want := range map[string]map[string]int{
			"api/orders_handler.go": {
				`Destinations: []command.Destination{{Name: AfterSlotsDestination, Idempotent: true, MustLand: true, Effects: []string{"orders.after_create"}}},`: 1,
				// The create, the update and the delete of orders each give the handle the events of Order.
				order: 3,
			},
			"api/folder_settings_handler.go": {
				// The upsert writes the setting and reads its folder through the handle: it gives the events of both.
				`ChangeEvents: []command.ChangeEvents{{Entity: "Setting", Created: "crud.setting.created", Updated: "crud.setting.updated", Deleted: "crud.setting.deleted"}, {Entity: "Folder", Created: "crud.folder.created", Updated: "crud.folder.updated", Deleted: "crud.folder.deleted"}}`: 1,
			},
		} {
			source := string(files[name])
			for w, n := range want {
				if strings.Count(source, w) != n {
					t.Errorf("%s holds %d times %s, want %d", name, strings.Count(source, w), w, n)
				}
			}
		}
		// The handle stages each event after the write of its row, so the fill stages none, has no payload helper, and
		// the command declares no Kafka destination whose names a fill could stage (ST-SUP-RP10-LC36).
		for name, source := range files {
			if bytes.Contains(source, []byte("kafkaoutbox")) || bytes.Contains(source, []byte("changeEvent(")) {
				t.Errorf("%s names the Kafka destination or stages a change event in the fill", name)
			}
		}
		// Without kafka-producer no command names the Kafka destination or change events.
		for name, source := range crudFiles(t, crudContext()) {
			if bytes.Contains(source, []byte("kafkaoutbox")) || bytes.Contains(source, []byte("ChangeEvents")) {
				t.Errorf("%s names the Kafka destination or change events without kafka-producer", name)
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

// ownPass tells whether output, the text of go test -v, holds the pass line of the subtest of id itself, directly
// under a test whose name matches parent, a regular expression. A subtest one level down has the same name prefix,
// but go test indents its line four more spaces, so its pass never counts as the pass of its parent
// (ST-STD-CODE-R07).
func ownPass(output, parent, id string) bool {
	return regexp.MustCompile(`(?m)^    --- PASS: ` + parent + `/` + regexp.QuoteMeta(id) + `_`).MatchString(output)
}

func TestOwnPass(t *testing.T) {
	t.Run("ST-STD-CODE-R07-T02 In the restapi wrapper, a pass of a subtest under the subtest of a record is not its pass", func(t *testing.T) {
		output := "=== RUN   TestCrud/ST-X-T01_a\n        --- PASS: TestCrud/ST-X-T01_a/child (0.00s)\n    --- FAIL: TestCrud/ST-X-T01_a (0.00s)\n" +
			"    --- PASS: TestCrud/ST-X-T02_b (0.00s)\n        --- FAIL: TestCrud/ST-X-T02_b/child (0.00s)\n    --- PASS: TestCrud/ST-X-T03_c/d (0.00s)\n"
		if ownPass(output, "TestCrud", "ST-X-T01") {
			t.Fatal("a failed subtest counts as passed because a subtest under it passed")
		}
		if !ownPass(output, "TestCrud", "ST-X-T02") || !ownPass(output, "TestCrud", "ST-X-T03") {
			t.Fatal("a passed subtest, also one whose title holds a slash, does not count as passed")
		}
	})
}

// waitFailureCheck is a test of the generated api package: a wait for a revocation whose database cannot serve now
// answers 503 with Retry-After, and another failure 500 (security review of PR #440, I1).
const waitFailureCheck = `package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
)

func TestWaitFailure(t *testing.T) {
	answer := httptest.NewRecorder()
	answerWaitFailure(answer, httptest.NewRequest(http.MethodPost, "/api/v1/todos", nil), syscall.ECONNREFUSED)
	if answer.Code != http.StatusServiceUnavailable || answer.Header().Get("Retry-After") == "" {
		t.Fatalf("a database that cannot serve the wait got %d with Retry-After %q, want 503 with Retry-After", answer.Code, answer.Header().Get("Retry-After"))
	}
	answer = httptest.NewRecorder()
	answerWaitFailure(answer, httptest.NewRequest(http.MethodPost, "/api/v1/todos", nil), errors.New("another failure"))
	if answer.Code != http.StatusInternalServerError {
		t.Fatalf("another failure of the wait got %d, want 500", answer.Code)
	}
}
`

func TestWaitFailureAnswer(t *testing.T) {
	t.Run("ST-SUP-RC20-OC49-T03 A wait whose database cannot serve now answers 503 with Retry-After, and the reads and writes use that answer", func(t *testing.T) {
		ctx := crudContext()
		files := crudFiles(t, ctx)
		checked := 0
		for name, source := range files {
			if !strings.HasSuffix(name, "_handler.go") {
				continue
			}
			checked++
			text := string(source)
			if waits := strings.Count(text, ".AwaitRevocation(") + strings.Count(text, ".AwaitWrite("); waits == 0 || strings.Count(text, "answerWaitFailure(w, r, err)") != waits {
				t.Fatalf("%s has %d waits and %d calls of answerWaitFailure, want one call for each wait", name, waits, strings.Count(text, "answerWaitFailure(w, r, err)"))
			}
		}
		if checked == 0 {
			t.Fatal("the rest-api component emits no handler file")
		}
		p := newCrudProject(t, ctx, files)
		p.write("out/api/wait_failure_test.go", []byte(waitFailureCheck))
		if output, err := p.goCommand(nil, "test", "-count=1", "-run", "^TestWaitFailure$", "./out/api"); err != nil {
			t.Fatalf("the generated answer of a failed wait: %v\n%s", err, output)
		}
	})
}
