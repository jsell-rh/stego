package command

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
	"reflect"
	"strings"
	"testing"
)

const modulePath = "stego.dev/runtime"

// surface walks the method sets of the handle's types to any depth by reflection: the handle, the repository and the
// loaded value that Repo and its methods give, and every type that their exported methods take or return. It walks
// the method sets of the types of the runtime module only; a type of another package is reported but not walked,
// because its methods are not the handle's, such as the driver.Valuer method of a uuid.UUID that a revocation gives.
type surface struct {
	seen    map[reflect.Type]bool
	types   []reflect.Type
	methods []surfaceMethod
}

type surfaceMethod struct {
	owner  reflect.Type
	method reflect.Method
}

func walkSurface(roots ...reflect.Type) *surface {
	s := &surface{seen: map[reflect.Type]bool{}}
	for _, root := range roots {
		s.visit(root)
	}
	return s
}

func (s *surface) visit(t reflect.Type) {
	if t == nil || s.seen[t] {
		return
	}
	s.seen[t] = true
	s.types = append(s.types, t)
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		s.visit(t.Elem())
	case reflect.Map:
		s.visit(t.Key())
		s.visit(t.Elem())
	case reflect.Func:
		for i := range t.NumIn() {
			s.visit(t.In(i))
		}
		for i := range t.NumOut() {
			s.visit(t.Out(i))
		}
	}
	if !inModule(t) {
		return
	}
	for i := range t.NumMethod() {
		m := t.Method(i)
		s.methods = append(s.methods, surfaceMethod{owner: t, method: m})
		s.visit(m.Type)
	}
}

// inModule reports whether the named type t, or the type that a pointer t points to, belongs to the runtime module.
func inModule(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	path := t.PkgPath()
	return path == modulePath || strings.HasPrefix(path, modulePath+"/")
}

func handleRoots() []reflect.Type {
	return []reflect.Type{
		reflect.TypeFor[*Handle[testProbe]](),
		reflect.TypeFor[*Repository[user]](),
		reflect.TypeFor[Loaded[user]](),
	}
}

// exportedAPI type-checks the package's files that are not tests with go/types.
func exportedAPI(t *testing.T) *types.Package {
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
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, os.ErrNotExist
		}
		return os.Open(file)
	})
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
	pkg, err := (&types.Config{Importer: imp}).Check(modulePath+"/command", fset, files, nil)
	if err != nil {
		t.Fatalf("type-check the package: %v", err)
	}
	return pkg
}

// methodsOf lists the exported methods of the named type name of pkg, on its pointer.
func methodsOf(pkg *types.Package, name string) []*types.Func {
	obj := pkg.Scope().Lookup(name)
	set := types.NewMethodSet(types.NewPointer(obj.Type()))
	var out []*types.Func
	for i := range set.Len() {
		if fn, ok := set.At(i).Obj().(*types.Func); ok && fn.Exported() {
			out = append(out, fn)
		}
	}
	return out
}

func TestHandleSurface(t *testing.T) {
	t.Run("ST-SUP-RP10-LC01-T02 The handle offers no way to change its tenant, principal or transaction", func(t *testing.T) {
		handle := reflect.TypeFor[*Handle[testProbe]]()
		returned := []reflect.Type{handle.Elem()}
		for i := range handle.NumMethod() {
			m := handle.Method(i)
			if strings.HasPrefix(m.Name, "Set") || strings.HasPrefix(m.Name, "With") || strings.HasPrefix(m.Name, "Bind") {
				t.Errorf("Handle has the setter %s", m.Name)
			}
			for j := 1; j < m.Type.NumIn(); j++ {
				in := m.Type.In(j).String()
				if strings.Contains(in, "tenant.Tenant") || strings.Contains(in, "authz.Principal") || strings.Contains(in, "sql.Tx") {
					t.Errorf("Handle.%s takes a %s", m.Name, in)
				}
			}
			for j := range m.Type.NumOut() {
				returned = append(returned, m.Type.Out(j))
			}
		}
		for _, r := range returned {
			if r.Kind() == reflect.Pointer {
				r = r.Elem()
			}
			if r.Kind() != reflect.Struct || r == reflect.TypeFor[testProbe]() {
				continue
			}
			for i := range r.NumField() {
				if r.Field(i).IsExported() {
					t.Errorf("%s, a value of the handle, has the exported field %s", r, r.Field(i).Name)
				}
			}
		}
		if handle.NumMethod() == 0 {
			t.Fatal("the walk found no method")
		}
	})

	t.Run("ST-SUP-RP10-LC02-T01 No method of the handle returns or takes a database value", func(t *testing.T) {
		s := walkSurface(handleRoots()...)
		for _, typ := range s.types {
			base := typ
			for base.Kind() == reflect.Pointer || base.Kind() == reflect.Slice {
				base = base.Elem()
			}
			if path := base.PkgPath(); path == "database/sql" || path == "database/sql/driver" {
				t.Errorf("the handle reaches the type %s", typ)
			}
			if base.Kind() == reflect.UnsafePointer {
				t.Errorf("the handle reaches an unsafe.Pointer")
			}
		}
		// No method takes SQL text: no parameter is named for a statement, and no method runs one.
		pkg := exportedAPI(t)
		for _, name := range []string{"Handle", "Repository", "Authz", "Effects"} {
			for _, fn := range methodsOf(pkg, name) {
				switch fn.Name() {
				case "Exec", "ExecContext", "Query", "QueryContext", "QueryRow", "QueryRowContext", "Prepare", "PrepareContext":
					t.Errorf("%s.%s runs a statement", name, fn.Name())
				}
				params := fn.Type().(*types.Signature).Params()
				for i := range params.Len() {
					p := params.At(i)
					lower := strings.ToLower(p.Name())
					if p.Type().String() == "string" && (strings.Contains(lower, "sql") || strings.Contains(lower, "query") ||
						strings.Contains(lower, "statement") || strings.Contains(lower, "stmt")) {
						t.Errorf("%s.%s takes SQL text as %s", name, fn.Name(), p.Name())
					}
				}
			}
		}
		if len(s.methods) < 10 {
			t.Fatalf("the walk found only %d methods", len(s.methods))
		}
	})

	t.Run("ST-SUP-RP10-LC09-T01 The handle has no transaction control", func(t *testing.T) {
		for _, m := range walkSurface(handleRoots()...).methods {
			switch m.method.Name {
			case "Commit", "Rollback", "Savepoint", "Begin", "BeginTx":
				t.Errorf("%s has the method %s", m.owner, m.method.Name)
			}
			for i := range m.method.Type.NumOut() {
				if out := m.method.Type.Out(i).String(); strings.Contains(out, "sql.Tx") || strings.Contains(out, "sql.Conn") {
					t.Errorf("%s.%s returns a %s", m.owner, m.method.Name, out)
				}
			}
		}
	})

	t.Run("ST-SUP-RP10-LC07-T03 The effect stager exposes no client", func(t *testing.T) {
		s := walkSurface(reflect.TypeFor[*Effects]())
		for _, typ := range s.types {
			base := typ
			for base.Kind() == reflect.Pointer || base.Kind() == reflect.Slice {
				base = base.Elem()
			}
			path := base.PkgPath()
			if path == "net" || path == "net/http" || strings.Contains(path, "kafka") || strings.Contains(path, "nats") ||
				strings.Contains(path, "amqp") || strings.Contains(path, "grpc") {
				t.Errorf("the effect stager reaches the type %s", typ)
			}
			if base.PkgPath() == modulePath+"/authz" && base.Name() == "Relationships" {
				t.Errorf("the effect stager reaches the relationships port")
			}
		}
		if len(s.methods) == 0 {
			t.Fatal("the walk found no method of the effect stager")
		}
	})

	t.Run("ST-SUP-RP10-LC14-T02 The command handle offers no writer, logger or other probe", func(t *testing.T) {
		pkg := exportedAPI(t)
		probes := 0
		for _, fn := range methodsOf(pkg, "Handle") {
			results := fn.Type().(*types.Signature).Results()
			for i := range results.Len() {
				out := results.At(i).Type()
				text := out.String()
				if fn.Name() == "Probe" {
					probes++
					if _, ok := out.(*types.TypeParam); !ok {
						t.Errorf("Probe returns %s, not the command's own probe type P", text)
					}
					continue
				}
				for _, banned := range []string{"io.Writer", "log.Logger", "slog.Logger", "stego.dev/runtime/probe."} {
					if strings.Contains(text, banned) {
						t.Errorf("Handle.%s returns %s", fn.Name(), text)
					}
				}
				// The values it returns offer none either.
				if named, ok := typesNamed(out); ok && named.Obj().Pkg() == pkg {
					for _, inner := range methodsOf(pkg, named.Obj().Name()) {
						r := inner.Type().(*types.Signature).Results()
						for j := range r.Len() {
							for _, banned := range []string{"io.Writer", "log.Logger", "slog.Logger", "stego.dev/runtime/probe."} {
								if strings.Contains(r.At(j).Type().String(), banned) {
									t.Errorf("%s.%s returns %s", named.Obj().Name(), inner.Name(), r.At(j).Type())
								}
							}
						}
					}
				}
			}
		}
		if probes != 1 {
			t.Fatalf("Handle has %d Probe methods, want one", probes)
		}
	})
}

// typesNamed gives the named type of t or of the type that the pointer t points to.
func typesNamed(t types.Type) (*types.Named, bool) {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	return named, ok
}
