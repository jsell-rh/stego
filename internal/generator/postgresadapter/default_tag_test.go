package postgresadapter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
)

// modelCounts parses models.go and counts its calls and its init functions.
func modelCounts(t *testing.T, models []byte) (calls, inits int) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "models.go", models, 0)
	if err != nil {
		t.Fatalf("models.go does not parse: %v", err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			calls++
		case *ast.FuncDecl:
			if n.Name.Name == "init" {
				inits++
			}
		}
		return true
	})
	return calls, inits
}

func TestDefaultLeftOutOfTheTag(t *testing.T) {
	t.Run("ST-SUP-RC01-OC06-T03 A string default with a backtick or a semicolon does not reach the GORM tag", func(t *testing.T) {
		plain, _, err := new(Generator).Generate(basicContext())
		if err != nil {
			t.Fatal(err)
		}
		baseCalls, baseInits := modelCounts(t, findFile(t, plain, "internal/storage/models.go").Bytes())
		for _, value := range []string{"x` }; func init() { panic(4242) }; type zz struct { a string `", "x;type:text); DROP TABLE users; --"} {
			ctx := basicContext()
			ctx.Entities[1].Fields = append(ctx.Entities[1].Fields, types.Field{Name: "motto", Type: types.FieldTypeString, Default: value})
			files, _, err := new(Generator).Generate(ctx)
			if err != nil {
				t.Fatalf("generation refused the default %q: %v", value, err)
			}
			models := findFile(t, files, "internal/storage/models.go").Bytes()
			if calls, inits := modelCounts(t, models); calls != baseCalls || inits != baseInits {
				t.Errorf("the default %q added %d init functions and %d calls to models.go", value, inits-baseInits, calls-baseCalls)
			}
			if strings.Contains(string(models), "DROP TABLE") || strings.Contains(string(models), "default:x") {
				t.Errorf("the default %q reached a GORM tag:\n%s", value, models)
			}
		}
	})
}
