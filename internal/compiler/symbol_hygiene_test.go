package compiler

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	schema "github.com/jsell-rh/stego/internal/types"
)

// Import paths can end in a predeclared Go name. Such an import must not
// change the meaning of error, nil, make, or other names in generated startup.
func TestGeneratedImportsPreserveLanguageAndStartupNames(t *testing.T) {
	project := t.TempDir()
	const module = "example.com/symbolcheck"
	input := AssemblerInput{ModuleName: module, ServiceName: "symbols", GoVersion: "1.22", OutDirName: "out"}
	names := append(types.Universe.Names(), "init", "main", "run", "error2", "main2", "run2", "nil2", "handler0", "handler02")
	// Derive template declarations independently so new startup helpers must
	// also be protected by the allocator.
	file, err := parser.ParseFile(token.NewFileSet(), "startup.go", "package main\n"+httpLifecycleSource+httpTLSSource+taskLifecycleSource+databaseConfigSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			if declaration.Recv == nil {
				if !reservedImportNames()[declaration.Name.Name] {
					t.Fatal("startup function name is not reserved", declaration.Name.Name)
				}
				names = append(names, declaration.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, spec.Name.Name)
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						names = append(names, name.Name)
					}
				}
			}
		}
	}
	sources := map[string]string{}
	var imports, checks strings.Builder
	for i, name := range names {
		componentPath := "internal/" + name
		input.Wirings = append(input.Wirings, ComponentWiring{Name: fmt.Sprintf("symbol-%d", i), Wiring: &gen.Wiring{
			Imports:      []string{componentPath},
			Constructors: []string{fmt.Sprintf("%s.NewHandler%d()", name, i)},
			Routes:       []string{fmt.Sprintf(`mux.HandleFunc("GET /%d", handler%d.ServeHTTP)`, i, i)},
		}})
		sources["out/"+componentPath+"/fixture.go"] = fmt.Sprintf(`package fixture
import ("net/http"; "context")
var Called bool
type Handler struct{}
func NewHandler%d() *Handler { Called = true; return &Handler{} }
func (*Handler) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (*Handler) Run(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
`, i)
		fmt.Fprintf(&imports, "fixture%d %q\n", i, module+"/out/"+componentPath)
		fmt.Fprintf(&checks, "if !fixture%d.Called { t.Fatal(%q) }\n", i, "constructor was not called: "+name)
	}
	input.Wirings = append(input.Wirings, ComponentWiring{Name: "tls-transport-name", Wiring: &gen.Wiring{
		Imports: []string{"internal/tlsfixture"}, Constructors: []string{"tlsfixture.NewStegoHTTPTransport()"},
		Routes: []string{`mux.HandleFunc("GET /tls-name", stegoHTTPTransport.ServeHTTP)`},
	}})
	sources["out/internal/tlsfixture/fixture.go"] = `package tlsfixture
import "net/http"
type Handler struct{}
func NewStegoHTTPTransport() *Handler { return &Handler{} }
func (*Handler) ServeHTTP(http.ResponseWriter, *http.Request) {}
`
	input.Wirings[0].Wiring.BackgroundTasks = []int{0}
	input.SlotsPackage = "internal/slots"
	fillNames := []string{"error", "error2", "nil", "main", "run"}
	input.SlotBindings = []schema.SlotDeclaration{{Slot: "before_create", Gate: fillNames}}
	sources["out/internal/slots/slots.go"] = "package slots\ntype Gate struct{}\nfunc NewBeforeCreateGate(...any) *Gate { return &Gate{} }\n"
	for i, name := range fillNames {
		sources["fills/"+name+"/fill.go"] = "package fill\nvar Called bool\nfunc New() any { Called = true; return struct{}{} }\n"
		fmt.Fprintf(&imports, "fill%d %q\n", i, module+"/fills/"+name)
		fmt.Fprintf(&checks, "if !fill%d.Called { t.Fatal(%q) }\n", i, "fill constructor was not called: "+name)
	}
	files, err := Assemble(input)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Assemble(input)
	if err != nil || !reflect.DeepEqual(files, again) {
		t.Fatal("symbol allocation was not deterministic", err)
	}
	for _, file := range files {
		name := "out/" + file.Path
		if file.Path == "go.mod" {
			name = file.Path
		}
		sources[name] = string(file.Bytes())
	}
	sources["out/main_test.go"] = "package main\nimport (\"testing\"\n" + imports.String() + ")\nfunc TestStartup(t *testing.T) {\nt.Setenv(\"PORT\", \"invalid-port\")\nif err := run(); err == nil { t.Fatal(\"invalid port accepted\") }\n" + checks.String() + "}\n"
	for name, source := range sources {
		name = filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("go", "test", "-race", "-mod=readonly", "./...")
	command.Dir = project
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated symbol bindings: %v\n%s", err, output)
	}
}

func TestConstructorCannotShadowPredeclaredNames(t *testing.T) {
	for _, name := range types.Universe.Names() {
		t.Run(name, func(t *testing.T) {
			function := "New" + strings.ToUpper(name[:1]) + name[1:]
			files, err := Assemble(AssemblerInput{ModuleName: "example.com/symbolcheck", ServiceName: "symbols", GoVersion: "1.22", OutDirName: "out", Wirings: []ComponentWiring{{Name: "reserved", Wiring: &gen.Wiring{
				Imports: []string{"internal/fixture"}, Constructors: []string{"fixture." + function + "()"},
				Routes: []string{fmt.Sprintf(`mux.HandleFunc("GET /", %s.ServeHTTP)`, name)},
			}}}})
			if err == nil || len(files) != 0 {
				t.Fatal("constructor with an ambiguous predeclared name was accepted", name)
			}
		})
	}
}

func TestAliasAllocationDoesNotReuseAPreviouslyAssignedSuffix(t *testing.T) {
	counts, used := map[string]int{}, map[string]bool{}
	assigned := map[string]bool{}
	for _, base := range []string{"api", "api", "api2", "api3", "api2", "api22", "api3"} {
		alias := disambiguateAlias(base, counts, used)
		if assigned[alias] {
			t.Fatal("alias was assigned twice", base, alias)
		}
		assigned[alias] = true
	}
}

func BenchmarkAssembleWithImportsAndFills(b *testing.B) {
	input := AssemblerInput{ModuleName: "example.com/assemblybench", ServiceName: "bench", GoVersion: "1.22", OutDirName: "out", SlotsPackage: "internal/slots"}
	input.SlotBindings = []schema.SlotDeclaration{{Slot: "before_create", Gate: []string{"policy", "audit", "error", "main", "error2"}}}
	for i := range 12 {
		name := fmt.Sprintf("component%d", i)
		input.Wirings = append(input.Wirings, ComponentWiring{Name: name, Wiring: &gen.Wiring{
			Imports: []string{"internal/" + name}, Constructors: []string{fmt.Sprintf("%s.NewHandler%d()", name, i)},
			Routes: []string{fmt.Sprintf(`mux.HandleFunc("GET /%d", handler%d.ServeHTTP)`, i, i)},
		}})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		files, err := Assemble(input)
		if err != nil || len(files) != 2 {
			b.Fatal("assembly failed", err)
		}
	}
}
