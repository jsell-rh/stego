package outbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestSchemaSQLMatchesMigration(t *testing.T) {
	files, _, err := new(Generator).Generate(gen.Context{OutputNamespace: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Path != "queue/schema.go" {
			continue
		}
		tree, err := parser.ParseFile(token.NewFileSet(), file.Path, file.Bytes(), 0)
		if err != nil {
			t.Fatal(err)
		}
		declaration := tree.Decls[0].(*ast.GenDecl).Specs[0].(*ast.ValueSpec)
		value, err := strconv.Unquote(declaration.Values[0].(*ast.BasicLit).Value)
		if err != nil || value != string(migration) {
			t.Fatal("bootstrap SQL differs from its migration")
		}
		return
	}
	t.Fatal("bootstrap SQL is missing")
}
