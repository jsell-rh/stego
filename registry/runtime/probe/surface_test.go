package probe

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// exportImporter imports packages from the export data that go list builds, and the probe package from the source
// that the test checked.
type exportImporter struct {
	gc    types.Importer
	probe *types.Package
}

func (i *exportImporter) Import(path string) (*types.Package, error) {
	if path == "github.com/jsell-rh/stego/registry/runtime/probe" && i.probe != nil {
		return i.probe, nil
	}
	return i.gc.Import(path)
}

// checkPackage type-checks the package source files that are not tests with go/types, and returns the package and
// the importer that also resolves it.
func checkPackage(t *testing.T) (*types.Package, *exportImporter, *token.FileSet) {
	t.Helper()
	out, err := exec.Command("go", "list", "-export", "-deps", "-f", "{{.ImportPath}}={{.Export}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	exports := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, file, _ := strings.Cut(line, "=")
		exports[path] = file
	}
	fset := token.NewFileSet()
	imp := &exportImporter{gc: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, os.ErrNotExist
		}
		return os.Open(file)
	})}
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	config := types.Config{Importer: imp}
	pkg, err := config.Check("github.com/jsell-rh/stego/registry/runtime/probe", fset, files, nil)
	if err != nil {
		t.Fatalf("type-check the package: %v", err)
	}
	imp.probe = pkg
	return pkg, imp, fset
}

// exported lists the exported functions, the exported methods of exported types and the exported struct fields of
// a package.
func exported(pkg *types.Package) (funcs map[string]*types.Signature, fields map[string]types.Type) {
	funcs, fields = map[string]*types.Signature{}, map[string]types.Type{}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		switch obj := obj.(type) {
		case *types.Func:
			funcs[name] = obj.Type().(*types.Signature)
		case *types.TypeName:
			named, ok := obj.Type().(*types.Named)
			if !ok {
				continue
			}
			for _, typ := range []types.Type{named, types.NewPointer(named)} {
				set := types.NewMethodSet(typ)
				for i := 0; i < set.Len(); i++ {
					m := set.At(i).Obj()
					if m.Exported() {
						funcs[name+"."+m.Name()] = m.Type().(*types.Signature)
					}
				}
			}
			if st, ok := named.Underlying().(*types.Struct); ok {
				for i := 0; i < st.NumFields(); i++ {
					if f := st.Field(i); f.Exported() {
						fields[name+"."+f.Name()] = f.Type()
					}
				}
			}
			if iface, ok := named.Underlying().(*types.Interface); ok {
				for i := 0; i < iface.NumMethods(); i++ {
					funcs[name+"."+iface.Method(i).Name()] = iface.Method(i).Type().(*types.Signature)
				}
			}
		}
	}
	return funcs, fields
}

// chooses tells whether a type lets its holder choose a sink, a writer or a format: an io.Writer or a value that
// writes, or the package's own sink and format types.
func chooses(typ types.Type, writer *types.Interface) bool {
	if slice, ok := typ.(*types.Slice); ok {
		typ = slice.Elem()
	}
	if types.Implements(typ, writer) || types.Implements(types.NewPointer(typ), writer) {
		return true
	}
	if named, ok := types.Unalias(typ).(*types.Named); ok {
		switch named.Obj().Name() {
		case "sink", "format", "consoleSink", "otlpSink", "queue", "Writer", "File", "Logger":
			return true
		}
	}
	return false
}

// generatedProbe is the shape of a generated probe package of ST-SUP-RC34-OC01 to ST-SUP-RC34-OC06, for the
// command reserve-and-charge and its observation reservation_refused.
const generatedProbe = `package probes

import (
	"context"

	"github.com/jsell-rh/stego/registry/runtime/probe"
)

type ReservationRefusedReason string

const (
	ReservationRefusedReasonShort   ReservationRefusedReason = "short"
	ReservationRefusedReasonExpired ReservationRefusedReason = "expired"
)

type LoadedReservation struct{ sku string }

func (r LoadedReservation) SKU() string { return r.sku }

type Observations struct{ reservationRefused *probe.Observation }

func NewObservations() (*Observations, error) {
	refused, err := probe.Declare(probe.Spec{Name: "reservation_refused", Kind: probe.Event, Severity: probe.Warn, EmittedBy: []string{"reserve-and-charge"}, Fields: []probe.Field{{Name: "sku", Type: probe.FieldText}, {Name: "reason", Type: probe.FieldEnum, Values: []string{"short", "expired"}}}})
	if err != nil {
		return nil, err
	}
	return &Observations{reservationRefused: refused}, nil
}

type ReserveAndChargeProbe struct {
	o *Observations
	p *probe.Probe
}

func BindReserveAndCharge(o *Observations, call *probe.Call) ReserveAndChargeProbe {
	return ReserveAndChargeProbe{o: o, p: call.Probe()}
}

func (p ReserveAndChargeProbe) ReservationRefused(reservation LoadedReservation, reason ReservationRefusedReason) {
	p.p.Emit(p.o.reservationRefused, probe.Text(reservation.sku), probe.Enum(string(reason)))
}

func NewReserveAndChargeTestProbe(o *Observations, r *probe.Recorder) (ReserveAndChargeProbe, *probe.Call) {
	call := r.Bind(context.Background(), "tenant-test", "principal-test", "reserve-and-charge")
	return BindReserveAndCharge(o, call), call
}
`

func TestProbeSurface(t *testing.T) {
	t.Run("ST-SUP-RP12-LC01-T01 The emit function has no message or free attribute parameter", func(t *testing.T) {
		pkg, _, _ := checkPackage(t)
		funcs, fields := exported(pkg)
		emit, ok := funcs["Probe.Emit"]
		if !ok {
			t.Fatal("no method Probe.Emit")
		}
		params := emit.Params()
		if params.Len() != 2 || !emit.Variadic() || emit.Results().Len() != 0 {
			t.Fatalf("Probe.Emit is %v, want a descriptor and typed values, with no result", emit)
		}
		if p, ok := params.At(0).Type().(*types.Pointer); !ok || p.Elem().(*types.Named).Obj().Name() != "Observation" {
			t.Fatalf("the first parameter of Emit is %v, want *Observation", params.At(0).Type())
		}
		if s, ok := params.At(1).Type().(*types.Slice); !ok || s.Elem().(*types.Named).Obj().Name() != "Value" {
			t.Fatalf("the values of Emit are %v, want ...Value", params.At(1).Type())
		}
		// Neither the descriptor nor a value can carry free text, an error, a map or an attribute: they have no
		// exported field, and a value is made only by a constructor of one typed kind.
		for name := range fields {
			if strings.HasPrefix(name, "Observation.") || strings.HasPrefix(name, "Value.") {
				t.Fatalf("%s is an exported field", name)
			}
		}
		for _, name := range []string{"Text", "Integer", "Bool", "Enum", "Measure", "Number"} {
			sig := funcs[name]
			if sig == nil || sig.Params().Len() != 1 || sig.Variadic() {
				t.Fatalf("the constructor %s is %v, want one typed parameter", name, sig)
			}
			switch types.Unalias(sig.Params().At(0).Type()).Underlying().(type) {
			case *types.Basic:
			default:
				t.Fatalf("the constructor %s takes %v", name, sig.Params().At(0).Type())
			}
		}
		for name, sig := range funcs {
			if !strings.HasPrefix(name, "Probe.") {
				continue
			}
			for i := 0; i < sig.Params().Len(); i++ {
				typ := sig.Params().At(i).Type()
				switch u := typ.Underlying().(type) {
				case *types.Map:
					t.Fatalf("%s takes a map", name)
				case *types.Interface:
					t.Fatalf("%s takes %v", name, u)
				case *types.Basic:
					if u.Kind() == types.String {
						t.Fatalf("%s takes a string", name)
					}
				}
			}
		}
	})

	t.Run("ST-SUP-RP12-LC17-T01 No probe, fill or declaration input chooses a sink", func(t *testing.T) {
		pkg, imp, fset := checkPackage(t)
		ioPkg, err := imp.Import("io")
		if err != nil {
			t.Fatal(err)
		}
		writer := ioPkg.Scope().Lookup("Writer").Type().Underlying().(*types.Interface)
		check := func(owner string, funcs map[string]*types.Signature, fields map[string]types.Type, allowed func(string) bool) {
			for name, sig := range funcs {
				if allowed(name) {
					continue
				}
				for i := 0; i < sig.Params().Len(); i++ {
					if chooses(sig.Params().At(i).Type(), writer) {
						t.Fatalf("%s %s takes %v, which chooses a sink, a writer or a format", owner, name, sig.Params().At(i).Type())
					}
				}
			}
			for name, typ := range fields {
				if chooses(typ, writer) {
					t.Fatalf("%s field %s is %v, which chooses a sink, a writer or a format", owner, name, typ)
				}
			}
		}
		funcs, fields := exported(pkg)
		// Start and Recorder are the stated exceptions; Start takes no sink, writer or format either: only the
		// process environment chooses them.
		check("the package", funcs, fields, func(name string) bool {
			return strings.HasPrefix(name, "Recorder.") || name == "NewRecorder" || strings.HasPrefix(name, "TB.")
		})
		if start := funcs["Start"]; start == nil || start.Params().Len() != 2 {
			t.Fatalf("Start is %v", start)
		}
		file, err := parser.ParseFile(fset, "probes.go", generatedProbe, 0)
		if err != nil {
			t.Fatal(err)
		}
		generated, err := (&types.Config{Importer: imp}).Check("example.com/orders/probes", fset, []*ast.File{file}, nil)
		if err != nil {
			t.Fatalf("the generated probe does not compile against the runtime: %v", err)
		}
		gfuncs, gfields := exported(generated)
		check("the generated probe", gfuncs, gfields, func(name string) bool { return name == "NewReserveAndChargeTestProbe" })
		method := gfuncs["ReserveAndChargeProbe.ReservationRefused"]
		if method == nil || method.Results().Len() != 0 || method.Variadic() {
			t.Fatalf("the generated probe method is %v", method)
		}
	})
}
