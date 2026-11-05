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
)

const schema = `syntax="proto3";package sample.v1;
message Request{string text=1;optional string label=2;}
message Response{string text=1;}
service Records{rpc Echo(Request)returns(Response);rpc Watch(Request)returns(stream Response);}`

func TestGRPCRejectsInvalidContracts(t *testing.T) {
	for _, source := range []string{`syntax="proto3";message A{string x=1;int32 y=1;}`, `syntax="proto3";import "missing.proto";message A{}`, `syntax="proto2";message A{}`, `syntax="proto3";message A{reserved 1;string x=1;}`} {
		ctx := gen.Context{ModuleName: "example.com/test", OutDirName: "out", OutputNamespace: "grpcapi", StorageContract: "example.com/test/out/contracts/storage", AuthPackage: "example.com/test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, Inputs: map[string][]byte{"api.proto": []byte(source)}, ComponentConfig: map[string]any{"factory_package": "domain", "proto_files": []any{map[string]any{"path": "api.proto", "import_path": "api.proto"}}}}
		if _, _, err := new(grpcapplication.Generator).Generate(ctx); err == nil {
			t.Fatalf("invalid protobuf accepted: %s", source)
		}
	}
}

func TestProtobufPackageDirectoriesCanBeImported(t *testing.T) {
	for _, importPath := range []string{"api.proto", "sample-tools/api.proto", "type/api.proto"} {
		t.Run(importPath, func(t *testing.T) {
			ctx := gen.Context{ModuleName: "example.com/protonames", OutDirName: "out", OutputNamespace: "grpcapi", StorageContract: "example.com/protonames/out/contracts/storage", AuthPackage: "example.com/protonames/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, Inputs: map[string][]byte{"api.proto": []byte(`syntax="proto3";package sample;message Record{}`)}, ComponentConfig: map[string]any{"factory_package": "domain", "proto_files": []any{map[string]any{"path": "api.proto", "import_path": importPath}}}}
			generator := new(grpcapplication.Generator)
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
			if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module example.com/protonames\ngo 1.26.10\nrequire google.golang.org/protobuf v1.36.11\n"), 0644); err != nil {
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
