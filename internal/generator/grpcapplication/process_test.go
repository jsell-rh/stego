package grpcapplication_test

import (
	"fmt"
	"os"

	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
)

func processContext(t *testing.T) gen.Context {
	t.Helper()
	source, err := os.ReadFile("testdata/process_factory.go")
	if err != nil {
		t.Fatal(err)
	}
	return gen.Context{ServiceName: "records", ModuleName: "example.com/grpc-test", OutDirName: "out", OutputNamespace: "grpcapi", StorageContract: "example.com/grpc-test/out/contracts/storage", AuthPackage: "example.com/grpc-test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "otel-tracing": "telemetry"}, Inputs: map[string][]byte{"api.proto": []byte(schema), "factory/rpc.go": source}, ComponentConfig: map[string]any{"factory_package": "sample", "proto_files": []any{map[string]any{"path": "api.proto", "import_path": "sample/v1/records.proto"}}, "processes": []any{map[string]any{"name": "records", "factory_package": "factory"}}}}
}
func TestGRPCProcessValidation(t *testing.T) {
	for _, name := range []string{"valid", "missing-source", "missing-open", "wrong-context", "wrong-result", "build-tag", "main-package", "generated-factory", "duplicate", "missing-telemetry", "unknown-field", "invalid-name", "empty-list", "wrong-list"} {
		t.Run(name, func(t *testing.T) {
			ctx := processContext(t)
			entry := ctx.ComponentConfig["processes"].([]any)[0].(map[string]any)
			switch name {
			case "missing-source":
				delete(ctx.Inputs, "factory/rpc.go")
			case "missing-open":
				ctx.Inputs["factory/rpc.go"] = []byte("package factory")
			case "wrong-context":
				ctx.Inputs["factory/rpc.go"] = []byte(strings.Replace(string(ctx.Inputs["factory/rpc.go"]), "ctx context.Context", "ctx string", 1))
			case "wrong-result":
				ctx.Inputs["factory/rpc.go"] = []byte(strings.Replace(string(ctx.Inputs["factory/rpc.go"]), "process.Application", "any", 1))
			case "build-tag":
				ctx.Inputs["factory/rpc.go"] = append([]byte("//go:build linux\n\n"), ctx.Inputs["factory/rpc.go"]...)
			case "main-package":
				ctx.Inputs["factory/rpc.go"] = []byte("package main\nfunc Open(){}")
			case "generated-factory":
				entry["factory_package"] = "out/factory"
			case "duplicate":
				ctx.ComponentConfig["processes"] = []any{entry, entry}
			case "missing-telemetry":
				delete(ctx.PeerNamespaces, "otel-tracing")
			case "unknown-field":
				entry["ignored"] = true
			case "invalid-name":
				entry["name"] = "../escape"
			case "empty-list":
				ctx.ComponentConfig["processes"] = []any{}
			case "wrong-list":
				ctx.ComponentConfig["processes"] = "records"
			}
			generator := new(grpcapplication.Generator)
			validation := generator.ValidateContext(ctx)
			_, _, generation := generator.Generate(ctx)
			if name == "valid" {
				if validation != nil || generation != nil {
					t.Fatal(validation, generation)
				}
			} else if validation == nil || generation == nil {
				t.Fatal("invalid process accepted")
			}
		})
	}
	ctx := processContext(t)
	generator := new(grpcapplication.Generator)
	inputs, err := generator.InputFiles(ctx.ComponentConfig)
	if err != nil || !reflect.DeepEqual(inputs, gen.SourceInputs([]string{"api.proto", "factory/rpc.go"})) {
		t.Fatal("factory source is not a compiler input", inputs, err)
	}
	first, _, err := generator.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := generator.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Fatal("process generation is not stable")
	}
}
