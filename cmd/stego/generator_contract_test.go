package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
)

// generatorUse is one use of a package, or of a name in it, in generator code.
type generatorUse struct {
	file, use string
}

// generatorUses returns every import and every package-qualified name in the
// generator source, without tests and test data.
func generatorUses(t *testing.T) []generatorUse {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(source), "../../internal/generator")
	var uses []generatorUse
	files := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "testdata" {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(files, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(root, path)
		local := map[string]string{}
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			uses = append(uses, generatorUse{name, imported})
			alias := imported[strings.LastIndex(imported, "/")+1:]
			if strings.HasPrefix(alias, "v") && strings.Contains(imported, "/") {
				if _, err := strconv.Atoi(alias[1:]); err == nil {
					trimmed := strings.TrimSuffix(imported, "/"+alias)
					alias = trimmed[strings.LastIndex(trimmed, "/")+1:]
				}
			}
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			local[alias] = imported
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok {
				if ident, ok := selector.X.(*ast.Ident); ok && local[ident.Name] != "" {
					uses = append(uses, generatorUse{name, local[ident.Name] + "." + selector.Sel.Name})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) == 0 {
		t.Fatal("no generator source was read")
	}
	return uses
}

func forbiddenGeneratorUses(t *testing.T, forbidden []string, allowed map[generatorUse]string) {
	t.Helper()
	for _, use := range generatorUses(t) {
		if !slices.Contains(forbidden, use.use) {
			continue
		}
		if allowed[use] == "" {
			t.Errorf("%s uses %s", use.file, use.use)
		}
		delete(allowed, use)
	}
	for use := range allowed {
		t.Errorf("the allowed use of %s in %s is gone; remove it from the list", use.use, use.file)
	}
}

func TestGeneratorsDoNoIO(t *testing.T) {
	t.Run("ST-STD-GEN-R01-T01 No generator source imports a package for files, processes or the network, or calls an HTTP client or server", func(t *testing.T) {
		forbiddenGeneratorUses(t, []string{
			"os", "os/exec", "os/signal", "io/ioutil", "syscall", "net", "plugin",
			"net/http.Get", "net/http.Head", "net/http.Post", "net/http.PostForm",
			"net/http.Client", "net/http.DefaultClient", "net/http.Transport", "net/http.DefaultTransport",
			"net/http.ListenAndServe", "net/http.ListenAndServeTLS", "net/http.Serve", "net/http.ServeTLS",
			"net/http.Server", "net/http.FileServer", "net/http.Dir",
		}, map[generatorUse]string{})
	})
}

func TestGeneratorsReadNoClockOrRandomness(t *testing.T) {
	t.Run("ST-STD-GEN-R02-T01 No generator source reads the clock, a timer, the environment or a source of randomness", func(t *testing.T) {
		forbiddenGeneratorUses(t, []string{
			"math/rand", "math/rand/v2", "crypto/rand",
			"os.Getenv", "os.LookupEnv", "os.Environ", "os.Hostname",
			"time.Now", "time.Since", "time.Until", "time.Sleep", "time.After", "time.AfterFunc",
			"time.Tick", "time.NewTimer", "time.NewTicker",
			"context.WithTimeout", "context.WithDeadline", "context.WithTimeoutCause", "context.WithDeadlineCause",
		}, map[generatorUse]string{})
	})
}

// TestGenerationIgnoresMapOrder plans each built-in archetype ten times in
// one process. Go starts each map iteration at a random place, so a generator
// or the assembler that depends on map order gives different files.
func TestGenerationIgnoresMapOrder(t *testing.T) {
	projects := map[string]func(t *testing.T){
		"rest-crud": func(t *testing.T) { generatingProject(t, generatingDeclaration) },
		"rpc-service": func(t *testing.T) {
			generatingProject(t, rpcDeclaration)
			t.Setenv("STEGO_MODULE", "example.com/grpc-test")
			writeProjectFile(t, "factory/rpc.go", string(rpcFactory(t)))
			writeProjectFile(t, "api.proto", rpcProto)
		},
		"browser-service": func(t *testing.T) {
			generatingProject(t, browserDeclaration)
			writeProjectFile(t, "ui/index.html", "<html><head></head><body>Private application</body></html>")
		},
	}
	plans := map[string][][]gen.File{}
	covered := map[string]bool{}
	for _, archetype := range slices.Sorted(maps.Keys(projects)) {
		t.Run(archetype, func(t *testing.T) {
			projects[archetype](t)
			for range 10 {
				input, err := buildReconcilerInput()
				if err != nil {
					t.Fatal(err)
				}
				plan, err := compiler.Reconcile(input)
				if err != nil {
					t.Fatal(err)
				}
				plans[archetype] = append(plans[archetype], plan.GeneratedFiles)
				for component := range plan.NewState.LastApplied.Components {
					covered[component] = true
				}
			}
		})
	}
	if len(plans) != len(projects) {
		t.Fatal("an archetype could not be planned")
	}
	t.Logf("components covered: %v", slices.Sorted(maps.Keys(covered)))
	assembled := func(file gen.File) bool { return file.Path == "main.go" || file.Path == "go.mod" }
	same := func(t *testing.T, keep func(gen.File) bool) {
		t.Helper()
		for archetype, runs := range plans {
			first := slices.DeleteFunc(slices.Clone(runs[0]), func(file gen.File) bool { return !keep(file) })
			if len(first) == 0 {
				t.Fatalf("%s: no file to compare", archetype)
			}
			for _, run := range runs[1:] {
				files := slices.DeleteFunc(slices.Clone(run), func(file gen.File) bool { return !keep(file) })
				if len(files) != len(first) {
					t.Fatalf("%s: one plan has %d files and another %d", archetype, len(first), len(files))
				}
				for i := range files {
					if files[i].Path != first[i].Path || !bytes.Equal(files[i].Content, first[i].Content) {
						t.Fatalf("%s: file %d differs between two plans: %s and %s", archetype, i, first[i].Path, files[i].Path)
					}
				}
			}
		}
	}
	t.Run("ST-GEN-DS02-T01 Each component generator returns the same files for the same inputs", func(t *testing.T) {
		same(t, func(file gen.File) bool { return !assembled(file) })
	})
	t.Run("ST-GEN-DS03-T01 The assembler writes the same main.go and go.mod for the same wirings", func(t *testing.T) {
		same(t, assembled)
	})
	t.Run("ST-STD-GEN-R03-T01 Planning each built-in archetype ten times gives the same files in the same order", func(t *testing.T) {
		same(t, func(gen.File) bool { return true })
	})
}
