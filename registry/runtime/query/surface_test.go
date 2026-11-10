package query

import (
	"go/types"
	"reflect"
	"strings"
	"testing"
)

// surface walks, by reflection, the method sets of the query handle, of the stream query handle and of the result of
// Repo, and of every type of this package that their methods return, to any depth. It calls visit for each method
// and each struct field of those types; a type of another package, such as the fill's own entity type, is not
// walked.
func surface(visit func(owner string, method *reflect.Method, field *reflect.StructField)) int {
	roots := []reflect.Type{
		reflect.TypeOf(&Handle[noProbe]{}),
		reflect.TypeOf(&Stream[Order, noProbe]{}),
		reflect.TypeOf(Repo[Order, noProbe]),
	}
	pkg := reflect.TypeOf(Handle[noProbe]{}).PkgPath()
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if seen[typ] {
			return
		}
		seen[typ] = true
		if typ.Kind() == reflect.Func {
			for i := 0; i < typ.NumOut(); i++ {
				walk(typ.Out(i))
			}
			return
		}
		if typ.PkgPath() != pkg {
			return
		}
		for _, t := range []reflect.Type{typ, reflect.PointerTo(typ)} {
			for i := 0; i < t.NumMethod(); i++ {
				m := t.Method(i)
				visit(typ.Name(), &m, nil)
				for j := 0; j < m.Type.NumOut(); j++ {
					walk(m.Type.Out(j))
				}
			}
		}
		if typ.Kind() == reflect.Struct {
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				visit(typ.Name(), nil, &f)
				if f.IsExported() {
					walk(f.Type)
				}
			}
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return len(seen)
}

// params lists the parameter types of a method value's type, without its receiver.
func params(m *reflect.Method) []reflect.Type {
	var out []reflect.Type
	for i := 1; i < m.Type.NumIn(); i++ {
		out = append(out, m.Type.In(i))
	}
	return out
}

func TestSurface(t *testing.T) {
	t.Run("ST-SUP-RP11-LC01-T02 The query handle offers no way to change its tenant, principal or transaction", func(t *testing.T) {
		methods := 0
		surface(func(owner string, m *reflect.Method, f *reflect.StructField) {
			if f != nil {
				// The handle types hold no exported field. A result type such as ListResult holds data only, and the
				// walk goes on into its fields.
				base, _, _ := strings.Cut(owner, "[")
				if f.IsExported() && (base == "Handle" || base == "Authz" || base == "Repository" || base == "Stream" || base == "Loaded") {
					t.Errorf("%s has the exported field %s", owner, f.Name)
				}
				return
			}
			methods++
			if strings.HasPrefix(m.Name, "Set") || strings.HasPrefix(m.Name, "With") || strings.HasPrefix(m.Name, "Bind") {
				t.Errorf("%s has the setter %s", owner, m.Name)
			}
			for _, p := range params(m) {
				for p.Kind() == reflect.Pointer {
					p = p.Elem()
				}
				switch p.PkgPath() + "." + p.Name() {
				case "github.com/jsell-rh/stego/registry/runtime/tenant.Tenant", "github.com/jsell-rh/stego/registry/runtime/authz.Principal", "database/sql.Tx", "github.com/jsell-rh/stego/registry/runtime/authz.Subject":
					t.Errorf("%s.%s takes a %s", owner, m.Name, p)
				}
			}
		})
		if methods < 10 {
			t.Fatalf("the walk saw %d methods; it reads too little", methods)
		}
	})

	t.Run("ST-SUP-RP11-LC02-T01 No method of the query handle returns or takes a database value", func(t *testing.T) {
		types := surface(func(owner string, m *reflect.Method, f *reflect.StructField) {
			var all []reflect.Type
			if f != nil {
				if !f.IsExported() {
					return
				}
				all = []reflect.Type{f.Type}
			} else {
				all = params(m)
				for i := 0; i < m.Type.NumOut(); i++ {
					all = append(all, m.Type.Out(i))
				}
			}
			for _, typ := range all {
				for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
					typ = typ.Elem()
				}
				if p := typ.PkgPath(); p == "database/sql" || p == "database/sql/driver" || typ.Kind() == reflect.UnsafePointer {
					t.Errorf("%s gives or takes %s", owner, typ)
				}
			}
			if m == nil {
				return
			}
			name := strings.ToLower(m.Name)
			for _, word := range []string{"exec", "query", "prepare", "sql", "raw", "statement", "where"} {
				if strings.Contains(name, word) {
					for _, p := range params(m) {
						if p.Kind() == reflect.String {
							t.Errorf("%s.%s takes text that can be SQL", owner, m.Name)
						}
					}
				}
			}
		})
		if types < 5 {
			t.Fatalf("the walk saw %d types; it reads too little", types)
		}
	})

	t.Run("ST-SUP-RP11-LC04-T01 The query handle has no write method", func(t *testing.T) {
		refused := map[string]bool{"Create": true, "Update": true, "Upsert": true, "Patch": true, "Delete": true,
			"Relate": true, "Unrelate": true, "Stage": true, "Commit": true, "Rollback": true, "Savepoint": true,
			"Begin": true, "BeginTx": true}
		methods := 0
		surface(func(owner string, m *reflect.Method, _ *reflect.StructField) {
			if m == nil {
				return
			}
			methods++
			if refused[m.Name] {
				t.Errorf("%s has the method %s", owner, m.Name)
			}
		})
		if methods < 10 {
			t.Fatalf("the walk saw %d methods; it reads too little", methods)
		}
	})

	t.Run("ST-SUP-RP11-LC11-T02 The query handle offers no writer, logger or other probe", func(t *testing.T) {
		pkg, _, _ := typeCheck(t)
		handle := pkg.Scope().Lookup("Handle").(*types.TypeName).Type().(*types.Named)
		probeParam := handle.TypeParams().At(0)
		writer := types.NewInterfaceType([]*types.Func{types.NewFunc(0, nil, "Write", types.NewSignatureType(nil, nil, nil,
			types.NewTuple(types.NewVar(0, nil, "p", types.NewSlice(types.Typ[types.Byte]))),
			types.NewTuple(types.NewVar(0, nil, "n", types.Typ[types.Int]), types.NewVar(0, nil, "err", types.Universe.Lookup("error").Type())), false))}, nil).Complete()
		probes := 0
		seen := map[*types.Named]bool{}
		var walk func(*types.Named)
		walk = func(named *types.Named) {
			if seen[named] || named.Obj().Pkg() != pkg {
				return
			}
			seen[named] = true
			set := types.NewMethodSet(types.NewPointer(named))
			for i := 0; i < set.Len(); i++ {
				m := set.At(i).Obj().(*types.Func)
				if !m.Exported() {
					continue
				}
				results := m.Type().(*types.Signature).Results()
				if m.Name() == "Probe" && named.Obj().Name() == "Handle" {
					probes++
					// A method has its own receiver type parameters: the first is the handle's P.
					recv := m.Type().(*types.Signature).RecvTypeParams()
					if results.Len() != 1 || recv.Len() != 1 || results.At(0).Type() != recv.At(0) {
						t.Errorf("Handle.Probe returns %s, want the query's own probe type %s", results, probeParam)
					}
					continue
				}
				for j := 0; j < results.Len(); j++ {
					typ := results.At(j).Type()
					text := types.TypeString(typ, nil)
					if types.Implements(typ, writer) || types.Implements(types.NewPointer(typ), writer) ||
						strings.Contains(text, "log.Logger") || strings.Contains(text, "slog.Logger") ||
						strings.Contains(text, "github.com/jsell-rh/stego/registry/runtime/probe.") {
						t.Errorf("%s.%s returns %s", named.Obj().Name(), m.Name(), text)
					}
					for {
						if ptr, ok := typ.(*types.Pointer); ok {
							typ = ptr.Elem()
							continue
						}
						if slice, ok := typ.(*types.Slice); ok {
							typ = slice.Elem()
							continue
						}
						break
					}
					if next, ok := typ.(*types.Named); ok {
						walk(next.Origin())
					}
				}
			}
		}
		walk(handle)
		if probes != 1 {
			t.Errorf("Handle has %d methods Probe, want 1", probes)
		}
		if len(seen) < 2 {
			t.Errorf("the walk saw %d types; it reads too little", len(seen))
		}
	})
}
