package query

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	parse "github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// shopModule is the module of the test service of testdata/service.yaml.
const shopModule = "example.com/shop"

// shopDeclaration reads testdata/service.yaml: the pooled service shop with the protected entities Order (status,
// total; read view, list browse) and Charge (order_id, card_last4 as pii, amount), the get query get-order with the
// refusal order_unknown, the list queries list-orders and list-charges and the query watch-orders.
func shopDeclaration(t *testing.T) *types.ServiceDeclaration {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var decl types.ServiceDeclaration
	if err := parse.DecodeStrict(data, "service.yaml", &decl); err != nil {
		t.Fatal(err)
	}
	return &decl
}

// shopContext is the context of the query component for the test service, as the compiler gives it: the fills of
// testdata/fills each implement the query slot, and watch-orders is a server stream.
func shopContext(t *testing.T) gen.Context {
	t.Helper()
	decl := shopDeclaration(t)
	fills := map[string]string{}
	var emitters []gen.Emitter
	for _, q := range decl.Queries {
		fills[q.Fill[0]] = Slot
		emitters = append(emitters, gen.Emitter{Name: q.Name, Kind: gen.EmitterQuery})
	}
	return gen.Context{
		OutputNamespace: "queries", ModuleName: shopModule, OutDirName: "out", ServiceName: decl.Name,
		BasePath: decl.BasePath, Entities: decl.Entities, Queries: decl.Queries, Emitters: emitters,
		Tenancy:        &gen.Tenancy{Key: "tenant_id"},
		PeerNamespaces: map[string]string{"query": "queries", probeComponent: "probes"},
		FillSlots:      fills, QueryStreams: map[string]bool{"watch-orders": true},
		Authorization: decl.Authorization,
	}
}

// generated runs the generator on ctx and gives its files by path.
func generated(t *testing.T, ctx gen.Context) map[string][]byte {
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

func TestGenerator(t *testing.T) {
	t.Run("ST-SUP-RC33-OC02-T01 One handler is emitted per query, bound to its fill", func(t *testing.T) {
		files := generated(t, shopContext(t))
		source := files["queries/serve/serve.go"]
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "serve.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		// The import alias of each fill package, by the fill's folder.
		aliases := map[string]string{}
		for _, spec := range file.Imports {
			p, _ := strconv.Unquote(spec.Path.Value)
			if rest, ok := strings.CutPrefix(p, shopModule+"/fills/"); ok && spec.Name != nil {
				aliases[rest] = spec.Name.Name
			}
		}
		if len(aliases) != 5 {
			t.Fatalf("serve.go imports the fills %v, want the five fills of the five queries", aliases)
		}
		// Each fill alias is used in the function bodies by exactly the code of its own query.
		uses := map[string]map[string]bool{}
		routes := map[string]int{}
		ast.Inspect(file, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				ast.Inspect(fn, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && sel.Sel.Name == "Fill" {
							if uses[id.Name] == nil {
								uses[id.Name] = map[string]bool{}
							}
							uses[id.Name][fn.Name.Name] = true
						}
					}
					if call, ok := n.(*ast.CallExpr); ok && fn.Name.Name == "Routes" {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Handle" {
							route, _ := strconv.Unquote(call.Args[0].(*ast.BasicLit).Value)
							routes[route]++
						}
					}
					return true
				})
			}
			return true
		})
		want := map[string]string{"get-order": "decidedGetOrder", "get-order-total": "decidedGetOrderTotal", "list-orders": "New",
			"list-charges": "New", "watch-orders": "StreamWatchOrders"}
		for fill, function := range want {
			used := uses[aliases[fill]]
			if len(used) != 1 || !used[function] {
				t.Errorf("the fill %s is called from %v, want from %s only", fill, used, function)
			}
		}
		if strings.Count(string(source), "query.Serve(") != 4 {
			t.Errorf("serve.go has %d query.Serve calls, want one for each of the four get and list queries", strings.Count(string(source), "query.Serve("))
		}
		for _, route := range []string{"GET /api/shop/v1/orders/{orderId}", "GET /api/shop/v1/orders/{orderId}/total", "GET /api/shop/v1/orders", "GET /api/shop/v1/charges"} {
			if routes[route] != 1 {
				t.Errorf("Routes mounts %s %d times, want once", route, routes[route])
			}
		}
		if len(routes) != 4 {
			t.Errorf("Routes mounts %v, want the four routes of the get and list queries", routes)
		}
	})

	t.Run("ST-SUP-RC33-OC02-T02 A query bound to a fill of another slot is refused", func(t *testing.T) {
		ctx := shopContext(t)
		ctx.FillSlots["get-order"] = "command.command"
		files, wiring, err := new(Generator).Generate(ctx)
		if err == nil || !strings.Contains(err.Error(), "get-order") || !strings.Contains(err.Error(), "fill get-order") ||
			!strings.Contains(err.Error(), "command.command") {
			t.Fatalf("Generate with the fill of another slot: %v, want a fault that names the query get-order and its fill", err)
		}
		if len(files) != 0 || wiring != nil {
			t.Fatalf("Generate gave %d files and a wiring with its fault, want none", len(files))
		}
		if err := new(Generator).ValidateContext(ctx); err == nil {
			t.Fatal("ValidateContext accepts the fill of another slot")
		}
		delete(ctx.FillSlots, "get-order")
		if _, _, err := new(Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "fills/get-order/fill.yaml") {
			t.Fatalf("Generate with a fill the project does not hold: %v, want a fault that names its fill.yaml", err)
		}
	})

	t.Run("ST-SUP-RC33-OC02-T05 A service that declares no query gets no query output", func(t *testing.T) {
		// The archetypes hold the query component (ST-SUP-RC20-OC39), so a service with collections only has it.
		ctx := shopContext(t)
		ctx.Queries, ctx.Emitters, ctx.FillSlots, ctx.QueryStreams = nil, nil, nil, nil
		ctx.Tenancy, ctx.Authorization = nil, nil
		delete(ctx.PeerNamespaces, probeComponent)
		if err := new(Generator).ValidateContext(ctx); err != nil {
			t.Fatalf("ValidateContext refuses a service with no query: %v", err)
		}
		files, wiring, err := new(Generator).Generate(ctx)
		if err != nil || len(files) != 0 || wiring == nil {
			t.Fatalf("Generate with no query gave %d files, wiring %v and %v; want no file, an empty wiring and no fault", len(files), wiring, err)
		}
		models, err := new(Generator).GoModels(ctx)
		if err != nil || len(models) != 0 {
			t.Fatalf("GoModels with no query gave %d models and %v; want none", len(models), err)
		}
	})

	t.Run("ST-SUP-RC33-OC02-T03 A query that the query runtime cannot serve is refused at generation", func(t *testing.T) {
		index := func(ctx gen.Context, name string) int {
			for i, q := range ctx.Queries {
				if q.Name == name {
					return i
				}
			}
			t.Fatalf("no query %s", name)
			return -1
		}
		cases := map[string]struct {
			change func(*gen.Context)
			want   []string
		}{
			"a route that net/http refuses": {func(c *gen.Context) { c.Queries[index(*c, "get-order")].Route = "GET /orders/{" },
				[]string{"get-order", "net/http"}},
			"two routes that conflict": {func(c *gen.Context) {
				c.Queries[index(*c, "get-order-total")].Route = "GET /orders/{id}"
				c.Queries[index(*c, "get-order-total")].ResourceMember = "id"
			}, []string{"get-order", "net/http"}},
			"a stream that does not declare the read permission": {func(c *gen.Context) { c.Queries[index(*c, "watch-orders")].Permission = "browse" },
				[]string{"watch-orders", "browse", "view"}},
			"an entity with no resource type while authorization is bound": {func(c *gen.Context) {
				c.Entities[1].ResourceType = ""
			}, []string{"list-charges", "Charge", "ST-STD-TXN-R18"}},
		}
		for name, c := range cases {
			ctx := shopContext(t)
			c.change(&ctx)
			files, wiring, err := new(Generator).Generate(ctx)
			if err == nil || len(files) != 0 || wiring != nil {
				t.Errorf("%s: Generate gave %d files and the error %v, want a refusal and no file", name, len(files), err)
				continue
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: the fault %q does not name %s", name, err, want)
				}
			}
		}
	})
}
