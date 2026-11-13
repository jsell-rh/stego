package spicedbauthz

import (
	_ "embed"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

var update = flag.Bool("update", false, "write the golden files of the emitted wiring")

const componentFile = "../../../registry/components/spicedb-authz/component.yaml"

// documentSchema is the schema authz/schema.zed of the RC29 test records: the resource type document with the
// relations owner and viewer and the permissions view = owner + viewer and edit = owner. The resource type project
// is the second protected resource type, so that a wiring that holds only the first entity is caught.
const documentSchema = `definition user {}

definition service_account {}

definition document {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}

definition project {
	relation owner: user
	permission view = owner
	permission edit = owner
}
`

// component reads the spicedb-authz component of the registry.
func component(t *testing.T) *registry.Component {
	t.Helper()
	c, err := registry.ParseComponent(componentFile)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// componentConfig gives the setting defaults of the component, as the compiler resolves them when the service
// declaration overrides none.
func componentConfig(t *testing.T) map[string]any {
	t.Helper()
	config := map[string]any{}
	for name, field := range component(t).Config {
		if field.Default != nil {
			config[name] = field.Default
		}
	}
	return config
}

var crud = []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList}

func permissions(read, list, create, update, delete string) map[types.Operation]string {
	return map[types.Operation]string{types.OpRead: read, types.OpList: list, types.OpCreate: create, types.OpUpdate: update, types.OpDelete: delete}
}

// documentContext is the declaration of the RC29 test records: a rest-crud service with pooled tenancy and the
// spicedb-authz component, the schema authz/schema.zed, and the owned entity Document with the resource type
// document, the permissions read: view, list: view, create: edit, update: edit and delete: edit, and the
// relationship owner from the field owner_id with the subject type user. Project is a second protected entity with
// a resource type of its own, and Note is an entity that no collection reads, so it is not protected.
func documentContext(t *testing.T) gen.Context {
	t.Helper()
	return gen.Context{
		ModuleName:      "example.com/documents",
		OutputNamespace: "internal/authz",
		ServiceName:     "documents",
		ComponentConfig: componentConfig(t),
		Tenancy:         &gen.Tenancy{Key: "tenant_id"},
		Authorization:   &types.Authorization{Provider: "spicedb", SchemaPath: "authz/schema.zed", SubjectFromIdentity: "user"},
		Inputs:          map[string][]byte{"authz/schema.zed": []byte(documentSchema)},
		Entities: []types.Entity{
			{Name: "Document", Fields: []types.Field{
				{Name: "title", Type: types.FieldTypeString},
				{Name: "owner_id", Type: types.FieldTypeString}},
				EntityAuthorization: types.EntityAuthorization{ResourceType: "document",
					Permissions: permissions("view", "view", "edit", "edit", "edit"),
					Relationships: []types.Relationship{{Relation: "owner", SubjectSource: types.SubjectSourceField,
						SubjectField: "owner_id", SubjectType: "user"}}}},
			{Name: "Project", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}},
				EntityAuthorization: types.EntityAuthorization{ResourceType: "project",
					Permissions: permissions("view", "view", "edit", "edit", "edit")}},
			{Name: "Note", Fields: []types.Field{{Name: "text", Type: types.FieldTypeString}}},
		},
		Collections: []types.Collection{
			{Name: "documents", Entity: "Document", Operations: crud},
			{Name: "projects", Entity: "Project", Operations: crud},
		},
	}
}

// generate runs the generator and fails on a refusal.
func generate(t *testing.T, ctx gen.Context) []gen.File {
	t.Helper()
	files, wiring, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wiring == nil {
		t.Fatal("the generator gave no wiring")
	}
	return files
}

// emitted returns the emitted file of the name under internal/authz.
func emitted(t *testing.T, files []gen.File, name string) string {
	t.Helper()
	for _, f := range files {
		if f.Path == "internal/authz/"+name {
			return string(f.Bytes())
		}
	}
	t.Fatalf("the generator emitted no internal/authz/%s", name)
	return ""
}

// refused runs the generator, expects a refusal that names each of names, and no file.
func refused(t *testing.T, ctx gen.Context, names ...string) {
	t.Helper()
	files, _, err := new(Generator).Generate(ctx)
	if err == nil {
		t.Fatal("the generation was not refused")
	}
	if len(files) != 0 {
		t.Errorf("the refused generation gave %d files", len(files))
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %q: %v", name, err)
		}
	}
	if verr := new(Generator).ValidateContext(ctx); verr == nil || verr.Error() != err.Error() {
		t.Errorf("ValidateContext gives %v, want the refusal of Generate", verr)
	}
}

// operationEntries reads the composite literals of the emitted Operations table.
func operationEntries(t *testing.T, source string) []map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "authz.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "Operations" || len(spec.Values) != 1 {
			return true
		}
		for _, elt := range spec.Values[0].(*ast.CompositeLit).Elts {
			entry := map[string]string{}
			for _, field := range elt.(*ast.CompositeLit).Elts {
				kv := field.(*ast.KeyValueExpr)
				switch v := kv.Value.(type) {
				case *ast.BasicLit:
					entry[kv.Key.(*ast.Ident).Name], _ = strconv.Unquote(v.Value)
				case *ast.Ident:
					entry[kv.Key.(*ast.Ident).Name] = v.Name
				}
			}
			entries = append(entries, entry)
		}
		return false
	})
	return entries
}

func TestOperations(t *testing.T) {
	t.Run("ST-SUP-RC29-OC01-T01 Each declared operation maps to one permission of the resource type", func(t *testing.T) {
		entries := operationEntries(t, emitted(t, generate(t, documentContext(t)), "authz.go"))
		want := map[string]string{"read": "view", "list": "view", "create": "edit", "update": "edit", "delete": "edit"}
		got := map[string]string{}
		projects := 0
		for _, e := range entries {
			switch e["Collection"] {
			case "documents":
				if e["ResourceType"] != "document" {
					t.Errorf("the entry %v is not on the resource type document", e)
				}
				if _, twice := got[e["Operation"]]; twice {
					t.Errorf("the operation %s has two entries", e["Operation"])
				}
				got[e["Operation"]] = e["Permission"]
			case "projects":
				projects++
				if e["ResourceType"] != "project" {
					t.Errorf("the entry %v is not on the resource type project", e)
				}
			}
		}
		if len(got) != 5 {
			t.Errorf("Document has %d entries, want 5: %v", len(got), got)
		}
		for op, permission := range want {
			if got[op] != permission {
				t.Errorf("%s maps to %q, want %q", op, got[op], permission)
			}
		}
		if projects != 5 {
			t.Errorf("Project has %d entries, want 5: a table that holds only the first entity", projects)
		}
	})

	t.Run("ST-SUP-RC29-OC01-T02 An operation whose permission the schema lacks is refused", func(t *testing.T) {
		ctx := documentContext(t)
		ctx.Entities[0].Permissions[types.OpUpdate] = "rename"
		refused(t, ctx, "Document", "update", "rename")
	})
}

func TestChecking(t *testing.T) {
	t.Run("ST-SUP-RC29-OC13-T01 An undeclared resource type, permission, relation or caveat parameter is refused at checking", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			change func(*gen.Context)
			names  []string
		}{
			{"resource type folder", func(ctx *gen.Context) { ctx.Entities[0].ResourceType = "folder" }, []string{"folder", "Document"}},
			{"permission share", func(ctx *gen.Context) { ctx.Entities[0].Permissions[types.OpRead] = "share" }, []string{"share", "Document"}},
			{"relationship editor", func(ctx *gen.Context) { ctx.Entities[0].Relationships[0].Relation = "editor" }, []string{"editor", "Document"}},
			{"context field request.ip", func(ctx *gen.Context) { ctx.Entities[0].ContextFields = []string{"request.ip"} }, []string{"ip", "Document"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				ctx := documentContext(t)
				c.change(&ctx)
				refused(t, ctx, c.names...)
			})
		}
	})

	t.Run("ST-SUP-RC29-OC11-T02 A declared relation the schema lacks is refused at generation", func(t *testing.T) {
		ctx := documentContext(t)
		ctx.SlotBindings = []types.SlotDeclaration{{Collection: "documents", Slot: "after_create", Chain: []string{"relate"},
			SlotAuthorization: types.SlotAuthorization{RelationshipsWritten: []string{"document#editor"}}}}
		refused(t, ctx, "slots[0]", "document#editor")
	})
}

// userManagementContext is the declaration of examples/user-management with authorization bound: pooled tenancy,
// a schema with the resource types organization and member, and each collection's entity protected. Its
// collections and their operations are those of the example.
func userManagementContext(t *testing.T) gen.Context {
	t.Helper()
	schema := `definition user {}

definition organization {
	relation admin: user
	relation member: user
	permission view = admin + member
	permission manage = admin
}

definition member {
	relation self: user
	relation org: organization
	relation org_admin: user
	permission view = self + org_admin
	permission manage = org_admin
}

definition setting {
	relation org_admin: user
	permission view = org_admin
	permission manage = org_admin
}

definition audit_event {
	relation reader: user
	permission view = reader
	permission write = reader
}
`
	all := map[types.Operation]string{}
	for _, op := range []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList, types.OpUpsert, types.OpPatch} {
		all[op] = "manage"
	}
	all[types.OpRead], all[types.OpList] = "view", "view"
	audit := map[types.Operation]string{types.OpCreate: "write", types.OpList: "view"}
	return gen.Context{
		ModuleName:      "example.com/user-management",
		OutputNamespace: "internal/authz",
		ServiceName:     "user-management",
		ComponentConfig: componentConfig(t),
		Tenancy:         &gen.Tenancy{Key: "tenant_id"},
		Authorization:   &types.Authorization{Provider: "spicedb", SchemaPath: "authz/schema.zed", SubjectFromIdentity: "user"},
		Inputs:          map[string][]byte{"authz/schema.zed": []byte(schema)},
		Entities: []types.Entity{
			{Name: "Organization", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}},
				EntityAuthorization: types.EntityAuthorization{ResourceType: "organization", Permissions: all,
					Relationships: []types.Relationship{{Relation: "admin", SubjectSource: types.SubjectSourceCaller, SubjectType: "user"}}}},
			{Name: "User", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString}, {Name: "org_id", Type: types.FieldTypeRef, To: "Organization"}},
				EntityAuthorization: types.EntityAuthorization{ResourceType: "member", Permissions: all,
					Relationships: []types.Relationship{{Relation: "self", SubjectSource: types.SubjectSourceCaller, SubjectType: "user"}},
					// The reference to the organization is a relation that the runtime writes (ST-DECL-INV85).
					References: map[string]types.Reference{"org_id": {Relation: "org", Permission: "manage"}}}},
			{Name: "OrgSetting", Fields: []types.Field{{Name: "key", Type: types.FieldTypeString}},
				EntityAuthorization: types.EntityAuthorization{ResourceType: "setting", Permissions: all}},
			{Name: "AuditEvent", Fields: []types.Field{{Name: "action", Type: types.FieldTypeString}},
				EntityAuthorization: types.EntityAuthorization{ResourceType: "audit_event", Permissions: audit}},
		},
		Collections: []types.Collection{
			{Name: "organizations", Entity: "Organization", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpDelete, types.OpList}},
			{Name: "org-users", Entity: "User", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList, types.OpPatch}},
			{Name: "all-users", Entity: "User", Operations: []types.Operation{types.OpList}},
			{Name: "org-settings", Entity: "OrgSetting", Operations: []types.Operation{types.OpList, types.OpUpsert}},
			{Name: "user-audit-events", Entity: "AuditEvent", Operations: []types.Operation{types.OpCreate, types.OpList}},
			{Name: "org-audit-events", Entity: "AuditEvent", Operations: []types.Operation{types.OpCreate, types.OpList}},
		},
	}
}

// resourceIDScan reads the emitted Go for the resources it builds: the calls of the runtime's NewResource and
// ResourceID, and each place that formats a resource ID itself, which is a composite literal of
// stegoauthz.Resource, a call of fmt or strings that builds text, or a string concatenation.
func resourceIDScan(t *testing.T, source string) (built int, formatted []string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "authz.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	selector := func(e ast.Expr) (string, string) {
		if sel, ok := e.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok {
				return x.Name, sel.Sel.Name
			}
		}
		return "", ""
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name == "Migration" || fn.Name.Name == "WriteSchema" {
			// Migration builds SQL text and WriteSchema an error text, not resource IDs.
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				pkg, name := selector(n.Fun)
				switch {
				case pkg == "stegoauthz" && (name == "NewResource" || name == "ResourceID"):
					built++
				case pkg == "fmt" || pkg == "strings" || pkg == "strconv" || pkg == "base64":
					formatted = append(formatted, fset.Position(n.Pos()).String()+" "+pkg+"."+name)
				}
			case *ast.CompositeLit:
				if pkg, name := selector(n.Type); pkg == "stegoauthz" && name == "Resource" {
					formatted = append(formatted, fset.Position(n.Pos()).String()+" stegoauthz.Resource literal")
				}
			case *ast.BinaryExpr:
				if n.Op == token.ADD {
					formatted = append(formatted, fset.Position(n.Pos()).String()+" a string concatenation")
				}
			}
			return true
		})
	}
	return built, formatted
}

func TestResourceIDs(t *testing.T) {
	t.Run("ST-SUP-RC29-OC22-T01 Generated code takes resource IDs from the runtime", func(t *testing.T) {
		ctx := userManagementContext(t)
		source := emitted(t, generate(t, ctx), "authz.go")
		built, formatted := resourceIDScan(t, source)
		if len(formatted) > 0 {
			t.Errorf("the emitted code formats resource IDs itself: %v", formatted)
		}
		if built != len(ctx.Entities) {
			t.Errorf("the emitted code builds %d resources through the runtime, want one for each of the %d protected entities", built, len(ctx.Entities))
		}
		// The scan can fail: a resource formatted in place is found.
		planted := strings.Replace(source, "return stegoauthz.NewResource(t, \"organization\", key)",
			"return stegoauthz.Resource{Type: \"organization\", ID: t.ID() + \"/\" + key}, nil", 1)
		if planted == source {
			t.Fatal("the emitted code has no resource function for organization")
		}
		if _, formatted := resourceIDScan(t, planted); len(formatted) == 0 {
			t.Error("the scan does not find a resource ID formatted in place")
		}
	})
}

func TestMigration(t *testing.T) {
	t.Run("ST-SUP-RC29-OC06-T04 The emitted wiring is the golden wiring, and its migration grants the worker role only authz_token", func(t *testing.T) {
		files := generate(t, documentContext(t))
		for _, name := range []string{"authz.go", "schema.zed"} {
			got := emitted(t, files, name)
			golden := filepath.Join("testdata", "golden", name+".golden")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("internal/authz/%s differs from %s; run go test -update after a deliberate change", name, golden)
			}
		}
		source := emitted(t, files, "authz.go")
		for _, table := range []string{"documents", "projects"} {
			grant := `GRANT SELECT (tenant_id, id), UPDATE (authz_token) ON public.\"` + table + `\" TO`
			if strings.Count(source, grant) != 1 {
				t.Errorf("the migration does not grant the worker role UPDATE on authz_token of %s once", table)
			}
		}
		if strings.Contains(source, "notes") {
			t.Error("the migration names the unprotected table notes")
		}
		schema := emitted(t, files, "schema.zed")
		if strings.Count(schema, "definition stego_floor {") != 1 || !strings.HasPrefix(schema, documentSchema) {
			t.Errorf("schema.zed is not the declared schema with one floor marker definition:\n%s", schema)
		}
	})
}

func TestTenancy(t *testing.T) {
	t.Run("ST-SUP-RC29-OC06-T05 A service without pooled tenancy on tenant_id is refused", func(t *testing.T) {
		ctx := documentContext(t)
		ctx.Tenancy = nil
		refused(t, ctx, "pooled", "tenant-qualified")
		ctx.Tenancy = &gen.Tenancy{Key: "org_id"}
		refused(t, ctx, "org_id", "tenant_id")
	})
}

// spiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04): version 1.57.0 at its digest,
// the image of the runtime's own SpiceDB tests.
const spiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// startSpiceDB gives the gRPC endpoint of a local SpiceDB serve-testing container without privilege, which keeps
// one in-memory datastore for each preshared key, and removes it when the test ends. STEGO_TEST_SPICEDB_ENDPOINT
// names a running server in its place. It fails the test when docker is not on the PATH, as pgtest does: a missing
// result is not a pass; only STEGO_TEST_NO_CONTAINERS=1 makes it skip.
func startSpiceDB(t *testing.T) string {
	t.Helper()
	if endpoint := os.Getenv("STEGO_TEST_SPICEDB_ENDPOINT"); endpoint != "" {
		return endpoint
	}
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("STEGO_TEST_NO_CONTAINERS") == "1" {
			t.Skip("docker is not on the PATH, and STEGO_TEST_NO_CONTAINERS=1: these tests need a local SpiceDB container")
		}
		t.Fatal("docker is not on the PATH: these tests need a local SpiceDB container")
	}
	name := "stego-spicedbauthz-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--memory", "256m", "--cpus", "1",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "-p", "127.0.0.1::50051", spiceDBImage, "serve-testing").CombinedOutput()
	if err != nil {
		t.Fatalf("start the SpiceDB container: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	port, err := exec.Command("docker", "port", name, "50051/tcp").Output()
	if err != nil {
		t.Fatalf("read the port of the SpiceDB container: %v", err)
	}
	return strings.TrimSpace(strings.Split(string(port), "\n")[0])
}

// The tests of the generated module. They are copied into the generated package and run there with the pinned Go
// SDK against the runtime module of this repository.
var (
	//go:embed testdata/relay_test.go
	relayTests string
)

// service is a generated service module in a temporary folder, which builds against the runtime module of this
// repository.
type service struct {
	t   *testing.T
	dir string
	env []string
}

// generateService generates ctx into a new module, with the tests of the generated module in its package.
func generateService(t *testing.T, ctx gen.Context) *service {
	t.Helper()
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	s := &service{t: t, dir: t.TempDir()}
	s.write("go.mod", "module "+ctx.ModuleName+"\n\ngo 1.26.11\n\nrequire github.com/jsell-rh/stego/registry/runtime v0.0.0\n\nreplace github.com/jsell-rh/stego/registry/runtime => "+runtimeDir+"\n")
	sum, err := os.ReadFile(filepath.Join(runtimeDir, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	s.write("go.sum", string(sum))
	for _, f := range generate(t, ctx) {
		s.write(f.Path, string(f.Bytes()))
	}
	for name, text := range map[string]string{"relay_test.go": relayTests} {
		s.write(filepath.Join(ctx.OutputNamespace, name), text)
	}
	return s
}

func (s *service) write(name, content string) {
	s.t.Helper()
	full := filepath.Join(s.dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// goTool runs the pinned Go SDK, STEGO_TEST_PINNED_GO, else the SDK of the test, in the module.
func (s *service) goTool(args ...string) ([]byte, error) {
	name := os.Getenv("STEGO_TEST_PINNED_GO")
	if name == "" {
		name = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = s.dir
	cmd.Env = append(append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local"), s.env...)
	return cmd.CombinedOutput()
}

// run runs the test of the generated module whose subtest name starts with id, and fails when it fails, or when
// that subtest did not pass: a missing result is not a pass. A skip passes only when STEGO_REQUIRE_POSTGRES is
// not 1.
func (s *service) run(id string) {
	s.t.Helper()
	pattern := "^Test[A-Za-z]+$/^" + id + "(_|$)"
	out, err := s.goTool("test", "-count=1", "-timeout=180s", "-v", "-run", pattern, "./internal/authz")
	if err != nil {
		s.t.Fatalf("go test -run %s: %v\n%s", pattern, err, out)
	}
	text := string(out)
	passed := regexp.MustCompile(`--- PASS: Test[A-Za-z]+/` + id + `_`).MatchString(text)
	skipped := regexp.MustCompile(`--- SKIP: Test[A-Za-z]+/` + id + `_`).MatchString(text)
	switch {
	case passed:
	case skipped && os.Getenv("STEGO_REQUIRE_POSTGRES") != "1":
		s.t.Skipf("the generated test %s skipped: no PostgreSQL server", id)
	default:
		s.t.Fatalf("the generated test %s did not pass\n%s", id, out)
	}
	s.t.Log(text)
}

// TestGeneratedAuthzWiring builds the generated authorization wiring against the runtime module and runs the
// tests of the generated module, against the PostgreSQL server that STEGO_TEST_POSTGRES_DSN names, a superuser
// URL, and a local SpiceDB container. The module is generated with the alert bound 1s and the watch interval
// 100ms, so the watch test does not wait for the 5s default.
func TestGeneratedAuthzWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a generated module")
	}
	ctx := documentContext(t)
	ctx.ComponentConfig["alert_bound"] = "1s"
	ctx.ComponentConfig["watch_interval"] = "100ms"
	s := generateService(t, ctx)
	if out, err := s.goTool("vet", "./..."); err != nil {
		t.Fatalf("go vet of the generated module: %v\n%s", err, out)
	}
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") != "" || os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
		s.env = append(s.env, "STEGO_TEST_SPICEDB_ENDPOINT="+startSpiceDB(t))
	}
	for _, id := range []string{
		"ST-SUP-RC29-OC05-T04",
		"ST-SUP-RC29-OC06-T01", "ST-SUP-RC29-OC06-T02", "ST-SUP-RC29-OC06-T03",
		"ST-SUP-RC29-OC07-T01", "ST-SUP-RC29-OC07-T02", "ST-SUP-RC29-OC07-T03", "ST-SUP-RC29-OC07-T04",
		"ST-SUP-RC29-OC07-T05", "ST-SUP-RC29-OC07-T06", "ST-SUP-RC29-OC23-T01",
	} {
		t.Run(id, func(t *testing.T) {
			s.t = t
			s.run(id)
		})
	}
}
