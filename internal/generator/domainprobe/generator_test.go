package domainprobe

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	gotypes "go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// reservationContext is the declaration of the RC34 test records, as the compiler gives it to the generator: the
// entity Reservation, the command reserve-and-charge, the query open-reservations, the slot binding
// reservations.before_create and three observations.
func reservationContext() gen.Context {
	return gen.Context{
		ModuleName:      "example.com/reservations",
		OutputNamespace: "probes",
		Entities: []types.Entity{{Name: "Reservation", Fields: []types.Field{
			{Name: "sku", Type: types.FieldTypeString}, {Name: "qty", Type: types.FieldTypeInt64},
			{Name: "holder_email", Type: types.FieldTypeString}}}},
		GoModelSources: map[string]gen.GoModelSource{"postgres-adapter": {ImportPath: "example.com/reservations/store",
			Models: []gen.GoModel{{Name: "Reservation", GoType: "Reservation", Fields: []gen.GoModelField{
				{Name: "id", Selection: "ID", Type: types.FieldTypeString},
				{Name: "sku", Selection: "Sku", Type: types.FieldTypeString},
				{Name: "qty", Selection: "Qty", Type: types.FieldTypeInt64},
				{Name: "holder_email", Selection: "HolderEmail", Type: types.FieldTypeString}}}}}},
		Emitters: []gen.Emitter{{Name: "reserve-and-charge", Kind: gen.EmitterCommand},
			{Name: "open-reservations", Kind: gen.EmitterQuery},
			{Name: "reservations.before_create", Kind: gen.EmitterSlotBinding}},
		Observations: []types.Observation{
			{Name: "reservation_refused", Kind: "event", Severity: "warn", EmittedBy: []string{"reserve-and-charge"},
				Fields: []types.ObservationField{{Name: "sku", Type: "Reservation.sku"}, {Name: "reason", Type: "enum(short | expired)"}}},
			{Name: "reservations_refused", Kind: "counter", Severity: "warn", EmittedBy: []string{"reserve-and-charge"},
				Fields: []types.ObservationField{{Name: "reason", Type: "enum(short | expired)"}}},
			{Name: "reservation_wait", Kind: "histogram", Severity: "info", Value: "wait: ms", EmittedBy: []string{"reservations.before_create"}},
		},
	}
}

// storeSource is the entity model package that the Go model provider of the context names.
const storeSource = "package store\n\n// Reservation is the entity model.\ntype Reservation struct {\n\tID, Sku, HolderEmail string\n\tQty int64\n}\n"

// service is a generated service module in a temporary folder, which builds against the runtime module of this
// repository.
type service struct {
	t   *testing.T
	dir string
}

// generateService generates ctx into a new module with the entity model package.
func generateService(t *testing.T, ctx gen.Context) *service {
	t.Helper()
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	s.write("store/store.go", storeSource)
	for _, f := range files {
		s.write(f.Path, string(f.Bytes()))
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

// goOutput runs the Go SDK in the module and gives its standard output and its standard error apart: with an
// empty module cache, the go command writes the modules it downloads to standard error.
func (s *service) goOutput(args ...string) ([]byte, []byte, error) {
	name := os.Getenv("STEGO_TEST_PINNED_GO")
	if name == "" {
		name = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// probePackage type-checks the generated probe package from its source, with the export data of its imports.
func (s *service) probePackage() (*gotypes.Package, []*ast.File, *gotypes.Info, *token.FileSet) {
	s.t.Helper()
	out, stderr, err := s.goOutput("list", "-export", "-deps", "-json=ImportPath,Export", "./probes")
	if err != nil {
		s.t.Fatalf("go list: %v\n%s", err, stderr)
	}
	exports := map[string]string{}
	for decoder := json.NewDecoder(bytes.NewReader(out)); decoder.More(); {
		var p struct{ ImportPath, Export string }
		if err := decoder.Decode(&p); err != nil {
			s.t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
	}
	fset := token.NewFileSet()
	names, _ := filepath.Glob(filepath.Join(s.dir, "probes", "*.go"))
	var files []*ast.File
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			s.t.Fatal(err)
		}
		files = append(files, f)
	}
	conf := gotypes.Config{Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		return os.Open(exports[path])
	})}
	info := &gotypes.Info{Uses: map[*ast.Ident]gotypes.Object{}, Selections: map[*ast.SelectorExpr]*gotypes.Selection{}}
	pkg, err := conf.Check("example.com/reservations/probes", fset, files, info)
	if err != nil {
		s.t.Fatal(err)
	}
	return pkg, files, info, fset
}

// methods lists the method names of the named type.
func methods(t *testing.T, pkg *gotypes.Package, name string) []string {
	t.Helper()
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		t.Fatalf("the probe package declares no %s", name)
	}
	set := gotypes.NewMethodSet(obj.Type())
	var out []string
	for i := 0; i < set.Len(); i++ {
		out = append(out, set.At(i).Obj().Name())
	}
	sort.Strings(out)
	return out
}

func TestEmitters(t *testing.T) {
	t.Run("ST-SUP-RC34-OC03-T01 Each emitter's probe holds its own observations only", func(t *testing.T) {
		pkg, _, _, _ := generateService(t, reservationContext()).probePackage()
		for name, want := range map[string]string{
			"ReserveAndChargeProbe":         "ReservationRefused ReservationsRefused",
			"ReservationsBeforeCreateProbe": "ReservationWait",
			"OpenReservationsProbe":         "",
		} {
			if got := strings.Join(methods(t, pkg, name), " "); got != want {
				t.Errorf("%s has the methods %q, want %q", name, got, want)
			}
		}
	})
}

func TestProbeCallsOnlyTheRuntime(t *testing.T) {
	t.Run("ST-SUP-RC34-OC05-T01 A generated probe method calls only the probe runtime", func(t *testing.T) {
		_, files, info, _ := generateService(t, reservationContext()).probePackage()
		refused := map[string]bool{"io": true, "log": true, "log/slog": true, "fmt": true}
		for _, f := range files {
			for _, spec := range f.Imports {
				path := strings.Trim(spec.Path.Value, `"`)
				if refused[path] || strings.HasPrefix(path, "go.opentelemetry.io") {
					t.Errorf("the probe package imports %s", path)
				}
			}
		}
		count := 0
		for _, f := range files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || !strings.HasSuffix(receiverType(fn), "Probe") {
					continue
				}
				count++
				emits := 0
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if s := info.Selections[sel]; s != nil {
						m := s.Obj()
						if m.Pkg() != nil && m.Pkg().Path() == "github.com/jsell-rh/stego/registry/runtime/probe" && m.Name() == "Emit" {
							emits++
						} else if m.Pkg() == nil || m.Pkg().Path() != "github.com/jsell-rh/stego/registry/runtime/command" {
							t.Errorf("%s calls %s", fn.Name.Name, m.Name())
						}
					}
					return true
				})
				if emits != 1 {
					t.Errorf("%s calls the emit function of the probe runtime %d times, want once", fn.Name.Name, emits)
				}
			}
		}
		if count != 3 {
			t.Fatalf("the probe package has %d probe methods, want 3", count)
		}
	})
}

func receiverType(fn *ast.FuncDecl) string {
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// enumFill is a fill package that passes an untyped string constant and a converted value that the declaration
// does not declare, and a test of it that records what the probe sends and drops.
const enumFill = `package enum

import "example.com/reservations/probes"

// Untyped passes the untyped string constant "short".
func Untyped(p probes.ReserveAndChargeProbe, r probes.LoadedReservation) { p.ReservationRefused(r, "short") }

// Converted passes a value that the declaration does not declare.
func Converted(p probes.ReserveAndChargeProbe, r probes.LoadedReservation) {
	p.ReservationRefused(r, probes.ReservationRefusedReason("lost"))
}

// Count emits reservations_refused with the reason given.
func Count(p probes.ReserveAndChargeProbe, reason probes.ReservationsRefusedReason) { p.ReservationsRefused(reason) }
`

const enumFillTest = `package enum

import (
	"testing"

	"example.com/reservations/probes"
	"github.com/jsell-rh/stego/registry/runtime/probe"
)

func TestEnum(t *testing.T) {
	t.Run("ST-SUP-RC34-OC02-T01 the declared value is sent and a value outside the set is dropped and counted", func(t *testing.T) {
		recorder := probe.NewRecorder(nil)
		p, call := probes.RecordReserveAndCharge(recorder)
		Count(p, "short")
		Count(p, probes.ReservationsRefusedReason("lost"))
		call.Commit()
		recorder.ExpectEmitted(t, "reservations_refused", probe.FieldValue{Name: "reason", Value: "short"})
		if got := len(recorder.Emissions("reservations_refused")); got != 1 {
			t.Errorf("%d emissions of reservations_refused, want 1", got)
		}
		if recorder.Dropped() != 1 {
			t.Errorf("%d emissions dropped, want the one of lost", recorder.Dropped())
		}
	})
}
`

func TestEnumField(t *testing.T) {
	t.Run("ST-SUP-RC34-OC02-T01 An enum field is a named type with one constant per value", func(t *testing.T) {
		s := generateService(t, reservationContext())
		pkg, _, _, _ := s.probePackage()
		obj := pkg.Scope().Lookup("ReservationRefusedReason")
		if obj == nil {
			t.Fatal("the probe package declares no ReservationRefusedReason")
		}
		named, ok := obj.Type().(*gotypes.Named)
		if basic, isBasic := obj.Type().Underlying().(*gotypes.Basic); !ok || !isBasic || basic.Kind() != gotypes.String {
			t.Fatalf("ReservationRefusedReason is %s, want a named string type", obj.Type())
		}
		var constants []string
		for _, name := range pkg.Scope().Names() {
			if c, isConst := pkg.Scope().Lookup(name).(*gotypes.Const); isConst && gotypes.Identical(c.Type(), named) {
				constants = append(constants, name)
			}
		}
		if strings.Join(constants, " ") != "ReservationRefusedReasonExpired ReservationRefusedReasonShort" {
			t.Errorf("the constants of ReservationRefusedReason are %v", constants)
		}
		s.write("fills/enum/enum.go", enumFill)
		s.write("fills/enum/enum_test.go", enumFillTest)
		if out, err := s.goTool("test", "-count=1", "./fills/enum"); err != nil {
			t.Fatalf("the fills that pass \"short\" and ReservationRefusedReason(\"lost\") do not both compile and pass: %v\n%s", err, out)
		}
	})
}
