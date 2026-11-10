package idempotency

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

const todoBase = "https://errors.example.com/todo/"

// todoContext is the context of the test declaration: the idempotency component with Required on, the retention
// and the problem base given, Fingerprint body, and the owned entity Todo in the collection todos with create, read,
// list, patch, update and delete. Todo has the fields title, done and note, which a create gives, and the
// server-managed created_at; title and done are patchable.
func todoContext(requiredOn []types.Operation, retention, base string) gen.Context {
	return gen.Context{
		OutputNamespace: "internal/idempotency",
		ErrorTypeBase:   base,
		Idempotency:     &types.Idempotency{RequiredOn: requiredOn, Retention: retention, Fingerprint: "body"},
		Entities: []types.Entity{{Name: "Todo", Fields: []types.Field{
			{Name: "title", Type: types.FieldTypeString},
			{Name: "done", Type: types.FieldTypeBool, Optional: true},
			{Name: "note", Type: types.FieldTypeString, Optional: true},
			{Name: "created_at", Type: types.FieldTypeTimestamp},
		}}},
		Collections: []types.Collection{{Name: "todos", Entity: "Todo", Patchable: []string{"title", "done"},
			Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpPatch, types.OpUpdate, types.OpDelete}}},
	}
}

// generated runs the generator and returns the emitted files by path.
func generated(t *testing.T, ctx gen.Context) map[string][]byte {
	t.Helper()
	files, wiring, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if wiring == nil {
		t.Fatal("generate gave no wiring")
	}
	out := make(map[string][]byte, len(files))
	for _, file := range files {
		out[file.Path] = file.Bytes()
	}
	return out
}

// emitted is what a test reads of the emitted settings file: its string and duration constants by name, and its
// Operations table.
type emitted struct {
	constants  map[string]string
	retention  time.Duration
	operations []operation
}

// readSettings parses the emitted settings file.
func readSettings(t *testing.T, source []byte) emitted {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "settings.go", source, 0)
	if err != nil {
		t.Fatalf("parse the emitted settings: %v\n%s", err, source)
	}
	e := emitted{constants: map[string]string{}}
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok || len(spec.Values) != 1 {
			return true
		}
		name := spec.Names[0].Name
		switch value := spec.Values[0].(type) {
		case *ast.BasicLit:
			text, err := strconv.Unquote(value.Value)
			if err != nil {
				t.Fatalf("constant %s: %v", name, err)
			}
			e.constants[name] = text
		case *ast.CallExpr:
			n, err := strconv.ParseInt(value.Args[0].(*ast.BasicLit).Value, 10, 64)
			if err != nil {
				t.Fatalf("constant %s: %v", name, err)
			}
			e.retention = time.Duration(n)
		case *ast.CompositeLit:
			for _, element := range value.Elts {
				var op operation
				for _, field := range element.(*ast.CompositeLit).Elts {
					kv := field.(*ast.KeyValueExpr)
					key := kv.Key.(*ast.Ident).Name
					if key == "Required" {
						op.Required = kv.Value.(*ast.Ident).Name == "true"
						continue
					}
					text, _ := strconv.Unquote(kv.Value.(*ast.BasicLit).Value)
					switch key {
					case "Collection":
						op.Collection = text
					case "Operation":
						op.Operation = text
					case "Method":
						op.Method = text
					}
				}
				e.operations = append(e.operations, op)
			}
		}
		return true
	})
	return e
}

func TestSettings(t *testing.T) {
	t.Run("ST-SUP-RC30-OC01-T05 The emitted settings list only the create and patch operations, required as Required on says", func(t *testing.T) {
		both := readSettings(t, generated(t, todoContext([]types.Operation{types.OpCreate, types.OpPatch}, "", todoBase))["internal/idempotency/settings.go"])
		want := []operation{
			{Collection: "todos", Operation: "create", Method: "POST", Required: true},
			{Collection: "todos", Operation: "patch", Method: "PATCH", Required: true},
		}
		if !reflect.DeepEqual(both.operations, want) {
			t.Fatalf("Required on create and patch: operations %+v, want %+v", both.operations, want)
		}
		createOnly := readSettings(t, generated(t, todoContext([]types.Operation{types.OpCreate}, "", todoBase))["internal/idempotency/settings.go"])
		want[1].Required = false
		if !reflect.DeepEqual(createOnly.operations, want) {
			t.Fatalf("Required on create only: operations %+v, want %+v", createOnly.operations, want)
		}
	})

	t.Run("ST-SUP-RC30-OC02-T01 The three problem types sit under the declared problem base", func(t *testing.T) {
		for _, base := range []string{todoBase, "https://problems.example.org/other/"} {
			e := readSettings(t, generated(t, todoContext([]types.Operation{types.OpCreate}, "", base))["internal/idempotency/settings.go"])
			want := map[string]string{
				"ProblemMissing":    base + "idempotency-key-missing",
				"ProblemInProgress": base + "idempotency-key-in-progress",
				"ProblemReused":     base + "idempotency-key-reused",
			}
			for name, uri := range want {
				if e.constants[name] != uri {
					t.Errorf("base %s: %s is %q, want %q", base, name, e.constants[name], uri)
				}
			}
		}
		for i, name := range []string{"ProblemMissing", "ProblemInProgress", "ProblemReused"} {
			if want := runtimeConstant(t, "wrap.go", name); ProblemSlugs[i] != want {
				t.Errorf("problem slug %q is not the runtime's %s %q", ProblemSlugs[i], name, want)
			}
		}
		if _, _, err := new(Generator).Generate(todoContext([]types.Operation{types.OpCreate}, "", "")); err == nil ||
			!strings.Contains(err.Error(), "error_type_base") {
			t.Fatalf("a declaration with no problem base: error %v, want one that names error_type_base", err)
		}
	})

	t.Run("ST-SUP-RC30-OC04-T05 The emitted retention is the declared one, or 24h when the declaration gives none", func(t *testing.T) {
		for _, c := range []struct {
			declared string
			want     time.Duration
		}{{"48h", 48 * time.Hour}, {"90m", 90 * time.Minute}, {"", 24 * time.Hour}} {
			e := readSettings(t, generated(t, todoContext([]types.Operation{types.OpCreate}, c.declared, todoBase))["internal/idempotency/settings.go"])
			if e.retention != c.want {
				t.Errorf("retention %q: emitted %s, want %s", c.declared, e.retention, c.want)
			}
			if e.constants["Fingerprint"] != "body" {
				t.Errorf("retention %q: Fingerprint is %q, want body", c.declared, e.constants["Fingerprint"])
			}
		}
	})
}

// TestGolden compares each emitted file of the test declaration with its copy in testdata/golden. Set
// STEGO_UPDATE_GOLDEN=1 to write the copies again.
func TestGolden(t *testing.T) {
	t.Run("ST-SUP-RC30-OC01-T05 golden output", func(t *testing.T) {
		files := generated(t, todoContext([]types.Operation{types.OpCreate, types.OpPatch}, "48h", todoBase))
		if len(files) != 4 {
			t.Fatalf("emitted %d files, want schema.go, settings.go, fingerprint.go and purge.go", len(files))
		}
		for name, got := range files {
			golden := filepath.Join("testdata", "golden", filepath.Base(name)+".golden")
			if os.Getenv("STEGO_UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from %s:\n%s", name, golden, got)
			}
		}
	})
}

// runtimeConstant reads the text of a string constant of the runtime package github.com/jsell-rh/stego/registry/runtime/idempotency from
// its source in this repository's registry/runtime folder.
func runtimeConstant(t *testing.T, file, name string) string {
	t.Helper()
	source := filepath.Join("..", "..", "..", "registry", "runtime", "idempotency", file)
	tree, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", source, err)
	}
	text, found := "", false
	ast.Inspect(tree, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, ident := range spec.Names {
			if ident.Name == name && i < len(spec.Values) {
				if literal, ok := spec.Values[i].(*ast.BasicLit); ok {
					if value, err := strconv.Unquote(literal.Value); err == nil {
						text, found = value, true
					}
				}
			}
		}
		return true
	})
	if !found {
		t.Fatalf("%s holds no string constant %s", source, name)
	}
	return text
}
