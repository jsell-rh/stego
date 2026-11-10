package auditlog

import (
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// The tests of the generated module. They are copied into the generated package and run there with the pinned Go
// SDK against the runtime module of this repository.
var (
	//go:embed testdata/fixture_test.go
	fixtureTests string
	//go:embed testdata/migration_test.go
	migrationTests string
	//go:embed testdata/redaction_test.go
	redactionTests string
	//go:embed testdata/sink_test.go
	sinkTests string
)

const componentFile = "../../../registry/components/audit-log/component.yaml"

// component reads the audit-log component of the registry.
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
func componentConfig(c *registry.Component) map[string]any {
	config := map[string]any{}
	for name, field := range c.Config {
		if field.Default != nil {
			config[name] = field.Default
		}
	}
	return config
}

// customerContext is the declaration of the RC28 test records: a rest-crud service with the audit-log component,
// audit level change, the audit-sink port bound, and the owned entity Customer with the fields name (pii),
// api_token (secret) and plan (no sensitivity). The entity Vendor has a second pii field, contact, so that a
// redaction of the first pii field only is caught, and Plan has no sensitive field.
func customerContext(t *testing.T, audit *types.Audit) gen.Context {
	t.Helper()
	return gen.Context{
		ModuleName:      "example.com/customers",
		OutputNamespace: "internal/auditlog",
		ServiceName:     "customers",
		ComponentConfig: componentConfig(component(t)),
		Audit:           audit,
		PortBindings:    map[string]string{"audit-sink": "platform-audit-sink"},
		Entities: []types.Entity{
			{Name: "Customer", Fields: []types.Field{
				{Name: "name", Type: types.FieldTypeString, Sensitivity: "pii"},
				{Name: "api_token", Type: types.FieldTypeString, Sensitivity: "secret"},
				{Name: "plan", Type: types.FieldTypeString}}},
			{Name: "Vendor", Fields: []types.Field{
				{Name: "label", Type: types.FieldTypeString, Sensitivity: "public"},
				{Name: "contact", Type: types.FieldTypeString, Sensitivity: "pii"}}},
			{Name: "Plan", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}}},
		},
	}
}

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

// emitted returns the source of the emitted audit wiring.
func emitted(t *testing.T, files []gen.File) string {
	t.Helper()
	for _, f := range files {
		if f.Path == "internal/auditlog/audit.go" {
			return string(f.Bytes())
		}
	}
	t.Fatal("the generator emitted no internal/auditlog/audit.go")
	return ""
}

func TestComponent(t *testing.T) {
	t.Run("ST-SUP-RC28-OC07-T02 The component requires the audit-sink port and provides no port", func(t *testing.T) {
		c := component(t)
		var requires, provides []string
		for _, p := range c.Requires {
			requires = append(requires, p.Name)
		}
		for _, p := range c.Provides {
			provides = append(provides, p.Name)
		}
		if !slices.Contains(requires, "audit-sink") {
			t.Fatalf("requires = %v, want audit-sink in it", requires)
		}
		if len(provides) != 0 {
			t.Fatalf("provides = %v, want none", provides)
		}
		ctx := customerContext(t, nil)
		ctx.PortBindings = map[string]string{"auth-provider": "jwt-auth"}
		if _, _, err := new(Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "audit-sink") {
			t.Fatalf("Generate without the audit-sink port: %v, want a refusal that names audit-sink", err)
		}
		if err := new(Generator).ValidateContext(ctx); err == nil || !strings.Contains(err.Error(), "audit-sink") {
			t.Fatalf("ValidateContext without the audit-sink port: %v, want a refusal that names audit-sink", err)
		}
		// The declaration's audit.sink names the port binding in its place.
		ctx.Audit = &types.Audit{Sink: "auth-provider"}
		if _, _, err := new(Generator).Generate(ctx); err != nil {
			t.Fatalf("Generate with audit.sink bound: %v", err)
		}
	})
}

// anchorRegistration reads the emitted job registration: the value of AnchorPeriod in seconds, and whether Jobs
// gives AnchorPeriod as the period of the anchor job.
func anchorRegistration(t *testing.T, source string) (int64, bool) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "audit.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	seconds := int64(-1)
	registered := false
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			if len(n.Names) == 1 && n.Names[0].Name == "AnchorPeriod" && len(n.Values) == 1 {
				// AnchorPeriod = <n> * time.Second
				if b, ok := n.Values[0].(*ast.BinaryExpr); ok && b.Op == token.MUL {
					if lit, ok := b.X.(*ast.BasicLit); ok {
						seconds, _ = strconv.ParseInt(lit.Value, 10, 64)
					}
				}
			}
		case *ast.FuncDecl:
			if n.Name.Name != "Jobs" {
				return true
			}
			ast.Inspect(n.Body, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "NewAnchorer" {
					return true
				}
				for _, arg := range call.Args {
					lit, ok := arg.(*ast.CompositeLit)
					if !ok {
						continue
					}
					for _, elt := range lit.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, _ := kv.Key.(*ast.Ident)
						value, _ := kv.Value.(*ast.Ident)
						if key != nil && value != nil && key.Name == "Period" && value.Name == "AnchorPeriod" {
							registered = true
						}
					}
				}
				return true
			})
		}
		return true
	})
	return seconds, registered
}

func TestJobs(t *testing.T) {
	t.Run("ST-SUP-RC28-OC08-T01 The emitted job schedule registers the anchor job at the anchor period of the audit-log component", func(t *testing.T) {
		c := component(t)
		seconds, registered := anchorRegistration(t, emitted(t, generate(t, customerContext(t, nil))))
		if !registered {
			t.Fatal("Jobs does not register the anchor job at AnchorPeriod")
		}
		if seconds != 3600 {
			t.Fatalf("AnchorPeriod = %d s, want the 1h that the component declaration sets", seconds)
		}
		// A copy of the registry whose component sets the anchor period 15m.
		dir := t.TempDir()
		text, err := os.ReadFile(componentFile)
		if err != nil {
			t.Fatal(err)
		}
		changed := strings.Replace(string(text), "default: 1h", "default: 15m", 1)
		if changed == string(text) {
			t.Fatal("the component declaration has no anchor_period default of 1h")
		}
		copied := filepath.Join(dir, "component.yaml")
		if err := os.WriteFile(copied, []byte(changed), 0o644); err != nil {
			t.Fatal(err)
		}
		c, err = registry.ParseComponent(copied)
		if err != nil {
			t.Fatal(err)
		}
		ctx := customerContext(t, nil)
		ctx.ComponentConfig = componentConfig(c)
		seconds, registered = anchorRegistration(t, emitted(t, generate(t, ctx)))
		if !registered || seconds != 900 {
			t.Fatalf("after the change: AnchorPeriod = %d s, registered %t; want one anchor job at 15m", seconds, registered)
		}
	})
}

func TestGrants(t *testing.T) {
	t.Run("ST-SUP-RC28-OC06-T02 The grants name audit_event with SELECT and INSERT only", func(t *testing.T) {
		_, wiring, err := new(Generator).Generate(customerContext(t, nil))
		if err != nil {
			t.Fatal(err)
		}
		var events []string
		for _, o := range wiring.DatabaseAccess {
			if slices.ContainsFunc(o.Privileges, func(p string) bool { return p == "DELETE" || p == "TRUNCATE" }) {
				t.Errorf("the grants give %v on %s", o.Privileges, o.Name)
			}
			if o.Name == "audit_event" {
				events = o.Privileges
			}
		}
		if !slices.Equal(events, []string{"INSERT", "SELECT"}) {
			t.Fatalf("the grants on audit_event are %v, want INSERT and SELECT only", events)
		}
	})
}

// service is a generated service module in a temporary folder, which builds against the runtime module of this
// repository.
type service struct {
	t   *testing.T
	dir string
}

// generateService generates ctx into a new module, with the tests of the generated module in its package.
func generateService(t *testing.T, ctx gen.Context) *service {
	t.Helper()
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	s := &service{t: t, dir: t.TempDir()}
	s.write("go.mod", "module "+ctx.ModuleName+"\n\ngo 1.26.10\n\nrequire stego.dev/runtime v0.0.0\n\nreplace stego.dev/runtime => "+runtimeDir+"\n")
	sum, err := os.ReadFile(filepath.Join(runtimeDir, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	s.write("go.sum", string(sum))
	for _, f := range generate(t, ctx) {
		s.write(f.Path, string(f.Bytes()))
	}
	for name, text := range map[string]string{"fixture_test.go": fixtureTests, "migration_test.go": migrationTests,
		"redaction_test.go": redactionTests, "sink_test.go": sinkTests} {
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
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	return cmd.CombinedOutput()
}

// run runs the test of the generated module whose subtest name starts with id, and fails when it fails, or when
// that subtest did not pass: a missing result is not a pass. A skip passes only when STEGO_REQUIRE_POSTGRES is
// not 1.
func (s *service) run(id string) {
	s.t.Helper()
	pattern := "^Test[A-Za-z]+$/^" + id + "(_|$)"
	out, err := s.goTool("test", "-count=1", "-timeout=120s", "-v", "-run", pattern, "./internal/auditlog")
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

// TestGeneratedAuditWiring builds the generated audit wiring against the runtime module and runs the tests of the
// generated module. The PostgreSQL tests use the server that STEGO_TEST_POSTGRES_DSN names, a superuser URL.
func TestGeneratedAuditWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a generated module")
	}
	s := generateService(t, customerContext(t, nil))
	if out, err := s.goTool("vet", "./..."); err != nil {
		t.Fatalf("go vet of the generated module: %v\n%s", err, out)
	}
	for _, id := range []string{"ST-SUP-RC28-OC06-T01", "ST-SUP-RC28-OC06-T02", "ST-SUP-RC28-OC06-T03",
		"ST-SUP-RC28-OC06-T04", "ST-SUP-RC28-OC01-T01", "ST-SUP-RC28-OC08-T02"} {
		t.Run(id, func(t *testing.T) {
			s.t = t
			s.run(id)
		})
	}
}
