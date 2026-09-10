package grpcapplication_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"github.com/jsell-rh/stego/internal/generator/outbox"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/types"
)

const schema = `syntax="proto3";package sample.v1;
message Request{string text=1;optional string label=2;}
message Response{string text=1;}
service Records{rpc Echo(Request)returns(Response);rpc Watch(Request)returns(stream Response);}`

func TestGeneratedGRPCApplication(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprint(watch), func(t *testing.T) { testGeneratedGRPCApplication(t, watch) })
	}
}
func testGeneratedGRPCApplication(t *testing.T, watch bool) {
	ctx := gen.Context{ServiceName: "records", ModuleName: "example.com/grpc-test", OutDirName: "out", EventsContract: "example.com/grpc-test/out/contracts/events", StorageContract: "example.com/grpc-test/out/contracts/storage", AuthPackage: "example.com/grpc-test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "postgres-adapter": "store", "grpc-application": "grpcapi", "outbox": "queue"}, Entities: []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}}}}, Inputs: map[string][]byte{"api/records.proto": []byte(schema)}}
	if watch {
		ctx.PeerNamespaces["otel-tracing"] = "telemetry"
	}
	project := t.TempDir()
	var files []gen.File
	var wirings []compiler.ComponentWiring
	for _, item := range []struct {
		name      string
		generator gen.Generator
		config    map[string]any
	}{
		{"otel-tracing", new(oteltracing.Generator), nil},
		{"postgres-adapter", new(postgresadapter.Generator), map[string]any{"migrations": "external"}},
		{"outbox", new(outbox.Generator), nil},
		{"jwt-auth", new(jwtauth.Generator), map[string]any{"mode": "verifier"}},
		{"grpc-application", new(grpcapplication.Generator), map[string]any{"watch_events": watch, "factory_package": "sample", "proto_files": []any{map[string]any{"path": "api/records.proto", "import_path": "sample/v1/records.proto"}}}},
	} {
		if item.name == "otel-tracing" && !watch {
			continue
		}
		ctx.OutputNamespace = ctx.PeerNamespaces[item.name]
		ctx.ComponentConfig = item.config
		generated, wiring, err := item.generator.Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		wirings = append(wirings, compiler.ComponentWiring{Name: item.name, Wiring: wiring})
	}
	shared, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, OutDirName: "out", GoVersion: new(grpcapplication.Generator).MinimumGoVersion(), Wirings: wirings})
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
	tests := []string{"sample.go", "runtime_test.go", "stream_headers_test.go"}
	if watch {
		tests = append(tests, "client_telemetry_test.go")
	}
	for _, name := range tests {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(project, "sample"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "sample", name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy", "-go=" + new(grpcapplication.Generator).MinimumGoVersion()}, {"vet", "-mod=readonly", "./..."}, {"test", "-race", "-mod=readonly", "-timeout=30s", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated gRPC application: %v\n%s", err, output)
		}
	}
}

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
			if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module example.com/protonames\ngo 1.26.8\nrequire google.golang.org/protobuf v1.36.11\n"), 0644); err != nil {
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
