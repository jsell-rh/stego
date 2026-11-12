package httpapplication_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
)

func TestApplicationRejectsInvalidFactory(t *testing.T) {
	for _, factory := range []any{nil, 42, "../escape", "out/domain", "/tmp/domain", ""} {
		_, _, err := new(httpapplication.Generator).Generate(gen.Context{OutputNamespace: "application", OutDirName: "out", ModuleName: "example.com/service", AuthPackage: "example.com/service/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, ComponentConfig: map[string]any{"factory_package": factory}})
		if err == nil {
			t.Fatalf("invalid factory was accepted: %v", factory)
		}
	}
}

func TestApplicationFactoryGetsNoStorage(t *testing.T) {
	t.Run("ST-SUP-RC10-OC31-T01 The emitted bridge passes no storage value to the factory", func(t *testing.T) {
		ctx := gen.Context{OutputNamespace: "application", OutDirName: "out", ModuleName: "example.com/service",
			AuthPackage: "example.com/service/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "postgres-adapter": "store"},
			ComponentConfig: map[string]any{"factory_package": "domain"}}
		files, wiring, err := new(httpapplication.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var bridge []byte
		for _, file := range files {
			if file.Path == "application/bridge.go" {
				bridge = file.Bytes()
			}
		}
		tree, err := parser.ParseFile(token.NewFileSet(), "bridge.go", bridge, 0)
		if err != nil {
			t.Fatal(err)
		}
		// No import of a database package or of a generated package other than the verifier's.
		aliases := map[string]string{}
		for _, spec := range tree.Imports {
			name, _ := strconv.Unquote(spec.Path.Value)
			if strings.HasPrefix(name, "database/sql") || strings.HasPrefix(name, "example.com/service/out/") && name != ctx.AuthPackage {
				t.Fatalf("the bridge imports %s", name)
			}
			alias := name[strings.LastIndex(name, "/")+1:]
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			aliases[alias] = name
		}
		calls := 0
		ast.Inspect(tree, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncDecl:
				if n.Name.Name != "NewHandler" {
					return true
				}
				params := n.Type.Params.List
				if len(params) != 1 || len(params[0].Names) != 1 {
					t.Fatalf("NewHandler takes %d parameters, want the verifier alone", n.Type.Params.NumFields())
				}
				star, ok := params[0].Type.(*ast.StarExpr)
				selector, isSelector := star.X.(*ast.SelectorExpr)
				if !ok || !isSelector || selector.Sel.Name != "Verifier" || aliases[selector.X.(*ast.Ident).Name] != ctx.AuthPackage {
					t.Fatal("the parameter of NewHandler is not the verifier of the auth package")
				}
			case *ast.CallExpr:
				selector, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "New" {
					return true
				}
				if ident, ok := selector.X.(*ast.Ident); !ok || ident.Name != "application" {
					return true
				}
				calls++
				if len(n.Args) != 1 {
					t.Fatalf("the bridge calls application.New with %d arguments, want the verifier alone", len(n.Args))
				}
				if ident, ok := n.Args[0].(*ast.Ident); !ok || ident.Name != "verifier" {
					t.Fatal("the bridge passes application.New another value than the verifier")
				}
			}
			return true
		})
		if calls != 1 {
			t.Fatalf("the bridge calls application.New %d times, want once", calls)
		}
		if len(wiring.ConstructorResources) != 0 || strings.Join(wiring.ConstructorDeps[0], ",") != "verifierFromEnvironment" {
			t.Fatalf("the wiring gives the bridge %v and %v, want the verifier alone", wiring.ConstructorResources, wiring.ConstructorDeps)
		}
	})
}
