package restapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
)

// panicCalls counts the calls of panic in the Go files of files whose path holds part.
func panicCalls(t *testing.T, files map[string][]byte, part string) int {
	t.Helper()
	calls := 0
	for name, data := range files {
		if !strings.HasSuffix(name, ".go") || !strings.Contains(name, part) {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, data, 0)
		if err != nil {
			t.Fatalf("%s does not parse: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "panic" {
					calls++
				}
			}
			return true
		})
	}
	return calls
}

func TestGenerationDefaultLiteral(t *testing.T) {
	t.Run("ST-SUP-RC20-OC17-T04 A generation default is written as a Go literal of its type, and any other default is refused", func(t *testing.T) {
		hostile := `""; panic("declaration code"); _ = ""`
		before := panicCalls(t, crudFiles(t, crudContext()), "")
		ctx := crudContext()
		ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, types.Field{Name: "generation", Type: types.FieldTypeString, Default: hostile})
		files := crudFiles(t, ctx)
		if got := panicCalls(t, files, ""); got != before {
			t.Errorf("the default added %d calls of panic to the generated code", got-before)
		}
		var found bool
		for _, data := range files {
			found = found || strings.Contains(string(data), "= "+strconv.Quote(hostile))
		}
		if !found {
			t.Error("no generated file sets the generation to the quoted default")
		}
		for _, field := range []types.Field{
			{Name: "generation", Type: types.FieldTypeTimestamp, Default: hostile},
			{Name: "generation", Type: types.FieldTypeInt64, Default: "1; panic(1)"},
			{Name: "generation", Type: types.FieldTypeJsonb, Default: map[string]any{"a": 1}},
		} {
			ctx := crudContext()
			ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, field)
			if files, _, err := new(Generator).Generate(ctx); err == nil || files != nil {
				t.Errorf("the default %#v of a %s field was accepted", field.Default, field.Type)
			}
		}
	})
}
