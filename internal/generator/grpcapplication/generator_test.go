package grpcapplication_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

const schema = `syntax="proto3";package sample.v1;
message Request{string text=1;optional string label=2;}
message Response{string text=1;}
service Records{rpc Echo(Request)returns(Response);rpc Watch(Request)returns(stream Response);}`

func TestGRPCRejectsInvalidContracts(t *testing.T) {
	for _, source := range []string{`syntax="proto3";message A{string x=1;int32 y=1;}`, `syntax="proto3";import "missing.proto";message A{}`, `syntax="proto2";message A{}`, `syntax="proto3";message A{reserved 1;string x=1;}`} {
		ctx := standalone(t, gen.Context{ModuleName: "example.com/test", OutDirName: "out", OutputNamespace: "grpcapi", AuthPackage: "example.com/test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, Inputs: map[string][]byte{"api.proto": []byte(source)}, ComponentConfig: map[string]any{"proto_files": []any{map[string]any{"path": "api.proto", "import_path": "api.proto"}}}})
		if _, _, err := new(grpcapplication.ProcessGenerator).Generate(ctx); err == nil {
			t.Fatalf("invalid protobuf accepted: %s", source)
		}
	}
}

func TestProtobufPackageDirectoriesCanBeImported(t *testing.T) {
	for _, importPath := range []string{"api.proto", "sample-tools/api.proto", "type/api.proto"} {
		t.Run(importPath, func(t *testing.T) {
			ctx := standalone(t, gen.Context{ModuleName: "example.com/protonames", OutDirName: "out", OutputNamespace: "grpcapi", AuthPackage: "example.com/protonames/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, Inputs: map[string][]byte{"api.proto": []byte(`syntax="proto3";package sample;message Record{}`)}, ComponentConfig: map[string]any{"proto_files": []any{map[string]any{"path": "api.proto", "import_path": importPath}}}})
			generator := new(grpcapplication.ProcessGenerator)
			if err := generator.ValidateContext(ctx); err != nil {
				t.Fatal(err)
			}
			files, _, err := generator.Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			project := t.TempDir()
			for _, file := range files {
				if !strings.HasPrefix(file.Path, "grpcapi/pb/") {
					continue
				}
				name := filepath.Join(project, "out", file.Path)
				if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, file.Content, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module example.com/protonames\ngo 1.26.11\nrequire google.golang.org/protobuf v1.36.11\n"), 0644); err != nil {
				t.Fatal(err)
			}
			name := "example.com/protonames/out/grpcapi/pb"
			if dir := filepath.ToSlash(filepath.Dir(importPath)); dir != "." {
				name += "/" + dir
			}
			source := "package main\nimport pb " + fmt.Sprintf("%q", name) + "\nfunc main(){_=pb.Record{}}\n"
			if err := os.WriteFile(filepath.Join(project, "main.go"), []byte(source), 0644); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("go", "build", "-mod=mod", "./...")
			command.Dir = project
			command.Env = append(os.Environ(), "GOWORK=off")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("protobuf import failed: %v\n%s", err, output)
			}
		})
	}
}

func TestGRPCApplicationNeedsDataAccess(t *testing.T) {
	t.Run("ST-SUP-RC07-OC19-T02 A service with no data access or without the command and query components is refused", func(t *testing.T) {
		base := func() gen.Context {
			return gen.Context{ModuleName: "example.com/records", OutDirName: "out", OutputNamespace: "grpcapi",
				PeerNamespaces:  map[string]string{"command": "commands", "query": "queries"},
				Entities:        []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}}}},
				Inputs:          map[string][]byte{"api.proto": []byte(schema)},
				ComponentConfig: map[string]any{"proto_files": []any{map[string]any{"path": "api.proto", "import_path": "sample/v1/records.proto"}}}}
		}
		for name, c := range map[string]struct {
			change func(*gen.Context)
			want   []string
		}{
			"no data access":       {func(c *gen.Context) { c.Entities = nil }, []string{"no entity, no command and no query", "grpc-processes", "ST-SUP-RC20-OC39"}},
			"no command component": {func(c *gen.Context) { delete(c.PeerNamespaces, "command") }, []string{"no command component", "ST-SUP-RC20-OC39"}},
			"no query component":   {func(c *gen.Context) { delete(c.PeerNamespaces, "query") }, []string{"no query component", "ST-SUP-RC20-OC39"}},
			"neither component":    {func(c *gen.Context) { c.PeerNamespaces = map[string]string{} }, []string{"no command and no query component", "ST-SUP-RC20-OC39"}},
		} {
			t.Run(name, func(t *testing.T) {
				ctx := base()
				c.change(&ctx)
				generator := new(grpcapplication.Generator)
				validation := generator.ValidateContext(ctx)
				files, wiring, generation := generator.Generate(ctx)
				for _, err := range []error{validation, generation} {
					if err == nil {
						t.Fatalf("the service with %s was accepted", name)
					}
					for _, want := range c.want {
						if !strings.Contains(err.Error(), want) {
							t.Fatalf("the refusal %q does not name %q", err, want)
						}
					}
				}
				if len(files) != 0 || wiring != nil {
					t.Fatal("a refused service got output")
				}
			})
		}
	})
}

func TestGRPCApplicationHasNoFactory(t *testing.T) {
	t.Run("ST-SUP-RC07-OC18-T02 The gRPC component's configuration has no factory setting", func(t *testing.T) {
		component, err := os.ReadFile(filepath.Join("..", "..", "..", "registry", "components", "grpc-application", "component.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var declaration struct {
			Config map[string]any `yaml:"config"`
		}
		if err := yaml.Unmarshal(component, &declaration); err != nil {
			t.Fatal(err)
		}
		if _, present := declaration.Config["factory_package"]; present || len(declaration.Config) == 0 {
			t.Fatalf("the configuration schema of grpc-application is %v, want proto_files and no factory_package", declaration.Config)
		}
		ctx := gen.Context{ModuleName: "example.com/orders", OutDirName: "out", OutputNamespace: "grpcapi",
			PeerNamespaces: map[string]string{"command": "commands", "query": "queries"},
			Entities:       []types.Entity{{Name: "Order", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}}}},
			ComponentConfig: map[string]any{"factory_package": "internal/rpc",
				"proto_files": []any{map[string]any{"path": "api.proto", "import_path": "sample/v1/records.proto"}}},
			Inputs: map[string][]byte{"api.proto": []byte(schema)}}
		files, wiring, err := new(grpcapplication.Generator).Generate(ctx)
		if err == nil || !strings.Contains(err.Error(), "factory_package") || !strings.Contains(err.Error(), "ST-DECL-INV72") {
			t.Fatalf("grpc-application with factory_package gave %v, want a fault that names factory_package and ST-DECL-INV72", err)
		}
		if len(files) != 0 || wiring != nil {
			t.Fatal("grpc-application with factory_package wrote output")
		}
	})
}
