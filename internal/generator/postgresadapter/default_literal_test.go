package postgresadapter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
)

// sourceCounts parses a generated Go file and counts its calls and its init functions.
func sourceCounts(t *testing.T, source []byte) (calls, inits int) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "tables.go", source, 0)
	if err != nil {
		t.Fatalf("tables.go does not parse: %v", err)
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

func TestDefaultIsALiteral(t *testing.T) {
	t.Run("ST-SUP-RC01-OC06-T03 A string default with a quote, a backtick or a semicolon reaches 001_initial only as a string literal", func(t *testing.T) {
		plain, _, err := new(Generator).Generate(basicContext())
		if err != nil {
			t.Fatal(err)
		}
		baseCalls, baseInits := sourceCounts(t, findFile(t, plain, "internal/storage/tables.go").Bytes())
		baseStatements := strings.Count(findFileContent(t, plain, "internal/storage/migrations/001_initial.sql"), ";\n")
		for _, value := range []string{"x` }; func init() { panic(4242) }; const zz = `", "x'); DROP TABLE users; --"} {
			ctx := basicContext()
			ctx.Entities[1].Fields = append(ctx.Entities[1].Fields, types.Field{Name: "motto", Type: types.FieldTypeString, Default: value})
			files, _, err := new(Generator).Generate(ctx)
			if err != nil {
				t.Fatalf("generation refused the default %q: %v", value, err)
			}
			source := findFile(t, files, "internal/storage/tables.go").Bytes()
			if calls, inits := sourceCounts(t, source); calls != baseCalls || inits != baseInits {
				t.Errorf("the default %q added %d init functions and %d calls to tables.go", value, inits-baseInits, calls-baseCalls)
			}
			body := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")
			literal := `"motto" text NOT NULL DEFAULT '` + strings.ReplaceAll(value, "'", "''") + `'`
			if !strings.Contains(body, literal) {
				t.Errorf("the default %q is not the string literal %s:\n%s", value, literal, body)
			}
			if got := strings.Count(strings.Replace(body, literal, "", 1), ";\n"); got != baseStatements {
				t.Errorf("the default %q changed the statements of 001_initial from %d to %d", value, baseStatements, got)
			}
		}
	})
}
