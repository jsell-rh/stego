package command

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	gotypes "go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/domainprobe"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// testModule is the module of the generated test service.
const testModule = "example.com/orders"

// ordersProto is the proto file of the command messages of the test service.
const ordersProto = `syntax = "proto3";

package orders.v1;

// ReserveAndChargeRequest is the request of reserve-and-charge.
message ReserveAndChargeRequest {
  string order_id = 1;
  string sku = 2;
  int32 qty = 3;
  int64 amount_cents = 4;
  string card_last4 = 5;
}

// ReserveAndChargeResponse is the response of reserve-and-charge.
message ReserveAndChargeResponse {
  string reservation_id = 1;
  string charge_id = 2;
}
`

// serviceYAML is the service declaration of the RC32 test records: the owned entities Order (resource type order),
// Reservation and Charge (amount_cents, card_last4 with sensitivity pii), the outbox destination billing, and the
// command reserve-and-charge that writes Reservation and Charge, needs reserve on order, may write order#holder and
// may stage effects for billing, bound to the fill reserve-and-charge.
const serviceYAML = `name: orders
archetype: rest-crud
language: go
base_path: /api/v1
tenancy:
  kind: pooled
authorization:
  provider: spicedb
  schema_path: authz/schema.zed
  subject_from_identity: user
entities:
  - name: Order
    resource_type: order
    permissions:
      read: view
      list: view
    fields:
      - name: sku
        type: string
  - name: Hold
    resource_type: hold
    permissions:
      read: view
    fields:
      - name: order_id
        type: ref
        to: Order
  - name: Reservation
    fields:
      - name: order_id
        type: string
      - name: sku
        type: string
      - name: qty
        type: int32
  - name: Charge
    fields:
      - name: order_id
        type: string
      - name: amount_cents
        type: int64
      - name: card_last4
        type: string
        sensitivity: pii
commands:
  - name: reserve-and-charge
    writes: [Reservation, Charge, Order, Hold]
    resource_type: order
    permission: reserve
    resource_member: orderId
    route: POST /orders/{orderId}/reservations
    fill: reserve-and-charge
    relationships_written: [order#holder]
    effects:
      - destination: billing
        name: charge-requested
    refusals:
      - name: short_stock
        status: 409
        code: reservation.short_stock
    retry_on_conflict: "1"
observations:
  - name: reservations_made
    kind: counter
    severity: info
    emitted_by: [reserve-and-charge]
    fields:
      - name: size
        type: enum(small | large)
`

// declaration reads serviceYAML.
func declaration(t testing.TB) *types.ServiceDeclaration {
	t.Helper()
	var svc types.ServiceDeclaration
	if err := yaml.Unmarshal([]byte(serviceYAML), &svc); err != nil {
		t.Fatal(err)
	}
	return &svc
}

// commandContext is the context that the compiler gives the command generator for the declaration, with the fill
// reserve-and-charge implementing slot.
func commandContext(t testing.TB, svc *types.ServiceDeclaration, slot string) gen.Context {
	t.Helper()
	var commands []gen.Command
	for _, c := range svc.Commands {
		commands = append(commands, gen.Command{Command: c, FillSlot: slot})
	}
	return gen.Context{
		ModuleName:      testModule,
		OutDirName:      "out",
		OutputNamespace: "commands",
		ServiceName:     svc.Name,
		BasePath:        svc.BasePath,
		Entities:        svc.Entities,
		Commands:        commands,
		Authorization:   svc.Authorization,
		Tenancy:         &gen.Tenancy{Key: "tenant_id"},
		Observations:    svc.Observations,
		PeerNamespaces:  map[string]string{"domain-probe": "probes", "command": "commands"},
		ComponentConfig: map[string]any{"proto_files": []any{map[string]any{"path": "proto/orders.proto", "import_path": "orders/v1/orders.proto"}}},
		Inputs:          map[string][]byte{"proto/orders.proto": []byte(ordersProto)},
	}
}

// generated generates ctx and gives the files by path.
func generated(t testing.TB, ctx gen.Context) map[string][]byte {
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

// TestDump writes the generated files of the test declaration to STEGO_COMMAND_DUMP, for a reader.
func TestDump(t *testing.T) {
	dir := os.Getenv("STEGO_COMMAND_DUMP")
	if dir == "" {
		t.Skip("STEGO_COMMAND_DUMP is not set")
	}
	for name, data := range generated(t, commandContext(t, declaration(t), commandSlot)) {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// exportImporter is a go/types importer that reads the export data that go list -export gives for each package of
// the generated module.
func exportImporter(t *testing.T, p *project, fset *token.FileSet, pattern string) gotypes.Importer {
	t.Helper()
	out := p.mustGo("list", "-export", "-deps", "-f", "{{.ImportPath}}={{.Export}}", pattern)
	files := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if path, file, ok := strings.Cut(line, "="); ok && file != "" {
			files[path] = file
		}
	}
	return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
}

func TestContract(t *testing.T) {
	t.Run("ST-SUP-RC32-OC01-T01 The fill gets the request and one command handle", func(t *testing.T) {
		p := newProject(t, commandContext(t, declaration(t), commandSlot), map[string][]byte{"fills/reserve-and-charge/fill.go": serviceFill})
		fset := token.NewFileSet()
		imp := exportImporter(t, p, fset, "./out/commands")
		pkg, err := imp.Import(testModule + "/out/commands")
		if err != nil {
			t.Fatal(err)
		}
		obj := pkg.Scope().Lookup("ReserveAndChargeFill")
		if obj == nil {
			t.Fatal("the contract has no slot type ReserveAndChargeFill")
		}
		sig, ok := obj.Type().Underlying().(*gotypes.Signature)
		if !ok {
			t.Fatalf("ReserveAndChargeFill is %s, not a function type", obj.Type())
		}
		params := sig.Params()
		if params.Len() != 3 {
			t.Fatalf("the slot takes %d parameters, want the context, the request and the handle", params.Len())
		}
		if got := params.At(0).Type().String(); got != "context.Context" {
			t.Errorf("the first parameter is %s, want context.Context", got)
		}
		if got := params.At(1).Type().String(); got != "*"+testModule+"/out/commands.ReserveAndChargeRequest" {
			t.Errorf("the second parameter is %s, want the decoded request message", got)
		}
		handle, ok := params.At(2).Type().(*gotypes.Pointer)
		named, _ := gotypes.Unalias(handle.Elem()).(*gotypes.Named)
		if !ok || named == nil || named.Obj().Pkg().Path() != "github.com/jsell-rh/stego/registry/runtime/command" || named.Obj().Name() != "Handle" {
			t.Errorf("the third parameter is %s, want *command.Handle", params.At(2).Type())
		}
		for i := 0; i < params.Len(); i++ {
			for _, refused := range []string{"database/sql", "net", "net/http", "github.com/jsell-rh/stego/registry/runtime/authz.Runtime", "github.com/authzed", "github.com/jackc"} {
				if strings.Contains(params.At(i).Type().String(), refused) {
					t.Errorf("the parameter %s has a type of %s", params.At(i).Type(), refused)
				}
			}
		}
	})

	t.Run("ST-SUP-RC32-OC02-T01 One handler is emitted per command, bound to its fill", func(t *testing.T) {
		files := generated(t, commandContext(t, declaration(t), commandSlot))
		fset := token.NewFileSet()
		bound, err := parser.ParseFile(fset, "fills.go", files["commands/handlers/fills.go"], 0)
		if err != nil {
			t.Fatal(err)
		}
		var fills []string
		for _, spec := range bound.Imports {
			if strings.Contains(spec.Path.Value, "/fills/") {
				fills = append(fills, strings.Trim(spec.Path.Value, `"`))
			}
		}
		if len(fills) != 1 || fills[0] != testModule+"/fills/reserve-and-charge" {
			t.Errorf("the handlers bind the fills %q, want only %s/fills/reserve-and-charge", fills, testModule)
		}
		source, err := parser.ParseFile(fset, "handlers.go", files["commands/handlers/handlers.go"], 0)
		if err != nil {
			t.Fatal(err)
		}
		var handlers, registered []string
		ast.Inspect(source, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Recv == nil && ast.IsExported(n.Name.Name) && n.Type.Results != nil && len(n.Type.Results.List) == 2 {
					if sel, ok := n.Type.Results.List[0].Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Handler" {
						handlers = append(handlers, n.Name.Name)
					}
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Handle" && len(n.Args) == 2 {
					if lit, ok := n.Args[0].(*ast.BasicLit); ok {
						registered = append(registered, lit.Value)
					}
				}
			}
			return true
		})
		if len(handlers) != 1 || handlers[0] != "ReserveAndCharge" {
			t.Errorf("the HTTP handlers are %q, want one for reserve-and-charge", handlers)
		}
		if len(registered) != 1 || registered[0] != `"POST /api/v1/orders/{orderId}/reservations"` {
			t.Errorf("the registered routes are %q, want the one route of reserve-and-charge", registered)
		}
		if !strings.Contains(string(files["commands/handlers/fills.go"]), "ReserveAndCharge: fillReserveAndCharge.New()") {
			t.Error("the handler of reserve-and-charge is not bound to the fill reserve-and-charge")
		}
	})

	t.Run("ST-SUP-RC32-OC06-T02 The parent of a row is the one entity with a resource type that it refers to", func(t *testing.T) {
		files := generated(t, commandContext(t, declaration(t), commandSlot))
		if !regexp.MustCompile(`Parent: +\[\]command\.Reference\[Hold\]\{\{Entity: "Order", ResourceType: "order", ID: func\(v Hold\) string \{ return v.OrderID \}\}\},`).Match(files["commands/commands.go"]) {
			t.Error("the entity Hold, which has a resource type of its own and refers to Order, has no Parent")
		}
		for name, change := range map[string]func(*types.Entity){
			"two parents": func(e *types.Entity) {
				e.Fields = append(e.Fields, types.Field{Name: "other_id", Type: types.FieldTypeRef, To: "Order"})
			},
			"an optional parent": func(e *types.Entity) { e.Fields[0].Optional = true },
		} {
			svc := declaration(t)
			for i := range svc.Entities {
				if svc.Entities[i].Name == "Hold" {
					change(&svc.Entities[i])
				}
			}
			if err := new(Generator).ValidateContext(commandContext(t, svc, commandSlot)); err == nil || !strings.Contains(err.Error(), "Hold") {
				t.Errorf("%s: generation gave %v, want a fault that names Hold", name, err)
			}
		}
	})

	t.Run("ST-SUP-RC32-OC02-T02 A command bound to a fill of another slot is refused", func(t *testing.T) {
		files, wiring, err := new(Generator).Generate(commandContext(t, declaration(t), "rest-api.before_create"))
		if err == nil || !strings.Contains(err.Error(), "reserve-and-charge") || !strings.Contains(err.Error(), "the fill reserve-and-charge") {
			t.Fatalf("generation gave %v, want a fault that names the command and the fill", err)
		}
		if files != nil || wiring != nil {
			t.Errorf("a refused generation gave %d files", len(files))
		}
		if err := new(Generator).ValidateContext(commandContext(t, declaration(t), "rest-api.before_create")); err == nil {
			t.Error("the preflight check accepts a fill of another slot")
		}
	})
}

// pinnedGo is the Go command that builds the generated service: STEGO_TEST_PINNED_GO, else bin/go of the SDK that
// runs the test.
func pinnedGo() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// project is a generated service module in a temporary folder.
type project struct {
	t   *testing.T
	dir string
}

func (p *project) write(name string, data []byte) {
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
func (p *project) goCommand(args ...string) (string, error) {
	p.t.Helper()
	cmd := exec.Command(pinnedGo(), args...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (p *project) mustGo(args ...string) string {
	p.t.Helper()
	output, err := p.goCommand(args...)
	if err != nil {
		p.t.Fatalf("go %v: %v\n%s", args, err, output)
	}
	return output
}

// newProject generates the service of ctx with the command and the domain-probe generators under out/, and
// extra files beside them, into a module that requires the runtime library from this repository's
// registry/runtime folder.
func newProject(t *testing.T, ctx gen.Context, extra map[string][]byte) *project {
	t.Helper()
	p := &project{t: t, dir: t.TempDir()}
	for name, data := range generated(t, ctx) {
		p.write("out/"+name, data)
	}
	probes := ctx
	probes.OutputNamespace = "probes"
	probes.ComponentConfig = map[string]any{}
	probes.Emitters = nil
	for _, c := range ctx.Commands {
		probes.Emitters = append(probes.Emitters, gen.Emitter{Name: c.Name, Kind: gen.EmitterCommand})
	}
	files, _, err := new(domainprobe.Generator).Generate(probes)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		p.write("out/"+f.Path, f.Bytes())
	}
	for name, data := range extra {
		p.write(name, data)
	}
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(runtimeDir, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("go.sum", sums)
	p.write("go.mod", []byte("module "+ctx.ModuleName+"\n\ngo "+new(Generator).MinimumGoVersion()+
		"\n\nrequire github.com/jsell-rh/stego/registry/runtime v0.2.0\n\nreplace github.com/jsell-rh/stego/registry/runtime => "+runtimeDir+"\n"))
	p.mustGo("mod", "tidy")
	return p
}
