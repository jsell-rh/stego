package postgresadapter

import (
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/database_test.go
var databaseTests []byte

//go:embed testdata/database_pool_test.go
var databasePoolTests []byte

func TestGeneratedDatabaseDriver(t *testing.T)               { testDatabaseModule(t, true) }
func TestGeneratedDatabasePoolWithoutTelemetry(t *testing.T) { testDatabaseModule(t, false) }

func testDatabaseModule(t *testing.T, traced bool) {
	t.Helper()
	ctx := basicContext()
	ctx.ModuleName, ctx.OutputNamespace = "example.com/dbprobe", "storage"
	if traced {
		ctx.PeerNamespaces = map[string]string{"otel-tracing": "tracing"}
	}
	file, err := generateDatabaseOpener(ctx)
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	files := []gen.File{file, {Path: "storage/database_pool_test.go", Content: databasePoolTests}, {Path: "tracing/tracing.go", Content: []byte(`package tracing
import("context";"sync")
type Key struct{}
type Record struct { Call, Outcome string; Value any }
var Events=make(chan Record,128)
func TraceDatabase(ctx context.Context,call string)(context.Context,func(string)){
 if ctx.Value(Key{})==nil{return ctx,func(string){}}
 var once sync.Once
 return ctx,func(outcome string){once.Do(func(){Events<-Record{call,outcome,ctx.Value(Key{})}})}
}
`)}, {Path: "go.mod", Content: []byte("module example.com/dbprobe\ngo 1.25.0\nrequire github.com/jackc/pgx/v5 v5.11.0\n")}}
	if traced {
		files = append(files, gen.File{Path: "storage/database_test.go", Content: databaseTests})
	}
	for _, f := range files {
		if !traced && f.Path == "tracing/tracing.go" {
			continue
		}
		name := filepath.Join(project, f.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, f.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-count=1", "-timeout=60s", "./..."}} {
		command := exec.Command("go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("database driver: %v %s", err, output)
		}
	}
}

func TestDatabasePeerValidation(t *testing.T) {
	ctx := basicContext()
	ctx.PeerNamespaces = map[string]string{"otel-tracing": "../invalid"}
	if err := new(Generator).ValidateContext(ctx); err == nil {
		t.Fatal("invalid telemetry namespace accepted")
	}
}

// Check the emitted source so new helper names also require validation.
func TestDatabaseReservedNames(t *testing.T) {
	ctx := basicContext()
	ctx.ModuleName = "example.com/dbprobe"
	ctx.PeerNamespaces = map[string]string{"otel-tracing": "tracing"}
	generated, err := generateDatabaseOpener(ctx)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), generated.Path, generated.Bytes(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			if declaration.Recv == nil {
				names = append(names, declaration.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				switch spec := spec.(type) {
				case *ast.ImportSpec:
					name := ""
					if spec.Name != nil {
						name = spec.Name.Name
					} else {
						imported, err := strconv.Unquote(spec.Path.Value)
						if err != nil {
							t.Fatal(err)
						}
						name = path.Base(imported)
					}
					if name != "_" && name != "." {
						names = append(names, name)
					}
				case *ast.TypeSpec:
					names = append(names, spec.Name.Name)
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						names = append(names, name.Name)
					}
				}
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no database names found")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			candidate := ctx
			candidate.Entities = []types.Entity{{Name: name, Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}}}}
			generator := new(Generator)
			if err := generator.ValidateContext(candidate); err == nil || !strings.Contains(err.Error(), "collides") {
				t.Fatalf("name %q must fail preflight: %v", name, err)
			}
			files, _, err := generator.Generate(candidate)
			if err == nil || !strings.Contains(err.Error(), "collides") || len(files) != 0 {
				t.Fatalf("name %q must fail before output: files=%d err=%v", name, len(files), err)
			}
		})
	}
}

func TestDatabasePoolWiring(t *testing.T) {
	for _, traced := range []bool{false, true} {
		ctx := basicContext()
		ctx.ModuleName = "example.com/pool"
		if traced {
			ctx.PeerNamespaces = map[string]string{"otel-tracing": "tracing"}
		}
		files, wiring, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if wiring.DatabaseOpener == nil || wiring.DatabaseOpener.Function != "OpenDatabase" || wiring.DatabaseOpener.Namespace != ctx.OutputNamespace {
			t.Fatal("pool factory missing from wiring")
		}
		source := findFile(t, files, ctx.OutputNamespace+"/database.go")
		if strings.Contains(string(source.Bytes()), "example.com/pool/tracing") != traced {
			t.Fatal("optional telemetry import differs")
		}
	}
}
