package httpapplication_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/types"
)

func TestGeneratedApplicationEndpoint(t *testing.T) {
	ctx := gen.Context{ModuleName: "example.com/http-test", OutDirName: "out", StorageContract: "example.com/http-test/out/contracts/storage", AuthPackage: "example.com/http-test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "postgres-adapter": "store", "http-application": "application"}, Entities: []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}}}}}
	project := t.TempDir()
	var files []gen.File
	var wirings []compiler.ComponentWiring
	for _, item := range []struct {
		name      string
		generator gen.Generator
		config    map[string]any
	}{
		{"postgres-adapter", new(postgresadapter.Generator), map[string]any{"migrations": "external"}},
		{"jwt-auth", new(jwtauth.Generator), map[string]any{"mode": "verifier"}},
		{"http-application", new(httpapplication.Generator), map[string]any{"factory_package": "sample"}},
	} {
		ctx.OutputNamespace = ctx.PeerNamespaces[item.name]
		ctx.ComponentConfig = item.config
		generated, wiring, err := item.generator.Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		wirings = append(wirings, compiler.ComponentWiring{Name: item.name, Wiring: wiring})
	}
	shared, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, OutDirName: "out", GoVersion: "1.26.8", Wirings: wirings})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, shared...)
	for _, file := range files {
		name := filepath.Join(project, "out", file.Path)
		if file.Path == "go.mod" {
			name = filepath.Join(project, file.Path)
		}
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(project, "sample"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sample.go", "endpoint_test.go", "lifecycle_test.go", "client_test.go", "fields_test.go"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "sample", name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	completionTest, err := os.ReadFile(filepath.Join("testdata", "client_completion_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "out", "application", "client", "completion_test.go"), completionTest, 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-mod=readonly", "-timeout=30s", "./..."}} {
		command := exec.Command("go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("generated HTTP application: %v\n%s", err, output)
		}
	}
	if os.Getenv("STEGO_BENCH_PROJECTION") == "1" {
		command := exec.Command("go", "test", "-run=^$", "-bench=^BenchmarkProjectionPage$", "-benchtime=200ms", "-count=3", "./sample")
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("projection benchmark: %v\n%s", err, output)
		}
		t.Logf("projection benchmark:\n%s", output)
	}
}
func TestApplicationRejectsInvalidFactory(t *testing.T) {
	for _, factory := range []any{nil, 42, "../escape", "out/domain", "/tmp/domain", ""} {
		_, _, err := new(httpapplication.Generator).Generate(gen.Context{OutputNamespace: "application", OutDirName: "out", ModuleName: "example.com/service", AuthPackage: "example.com/service/out/auth", StorageContract: "example.com/service/out/contracts/storage", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, ComponentConfig: map[string]any{"factory_package": factory}})
		if err == nil {
			t.Fatalf("invalid factory was accepted: %v", factory)
		}
	}
}

func TestApplicationRequiresVerifierProducer(t *testing.T) {
	ctx := gen.Context{ModuleName: "example.com/http-test", OutDirName: "out", OutputNamespace: "application", StorageContract: "example.com/http-test/out/contracts/storage", AuthPackage: "example.com/http-test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, ComponentConfig: map[string]any{"factory_package": "sample"}}
	_, application, err := new(httpapplication.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx.OutputNamespace = "auth"
	ctx.ComponentConfig = nil
	_, authentication, err := new(jwtauth.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, OutDirName: "out", GoVersion: "1.26.8", Wirings: []compiler.ComponentWiring{{Name: "store", Wiring: &gen.Wiring{Constructors: []string{"storage.NewStore(db)"}, Imports: []string{"storage"}, NeedsDB: true}}, {Name: "jwt-auth", Wiring: authentication}, {Name: "http-application", Wiring: application}}})
	if err == nil {
		t.Fatal("application accepted middleware in place of its verifier dependency")
	}
}
