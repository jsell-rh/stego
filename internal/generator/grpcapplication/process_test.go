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
	if err != nil || strings.Join(inputs, ",") != "api.proto,factory/rpc.go" {
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
func TestGeneratedGRPCProcesses(t *testing.T) {
	ctx := processContext(t)
	ctx.StorageContract = ""
	delete(ctx.ComponentConfig, "factory_package")
	project := t.TempDir()
	var wirings []compiler.ComponentWiring
	write := func(name string, data []byte) {
		t.Helper()
		target := filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		namespace string
		generator gen.Generator
		config    map[string]any
	}{
		{"auth", new(jwtauth.Generator), map[string]any{"mode": "verifier"}},
		{"telemetry", new(oteltracing.Generator), nil},
		{"grpcapi", new(grpcapplication.ProcessGenerator), ctx.ComponentConfig},
	} {
		current := ctx
		current.OutputNamespace = item.namespace
		current.ComponentConfig = item.config
		files, wiring, err := item.generator.Generate(current)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			write("out/"+file.Path, file.Bytes())
		}
		name := map[string]string{"auth": "jwt-auth", "telemetry": "otel-tracing", "grpcapi": "grpc-processes"}[item.namespace]
		wirings = append(wirings, compiler.ComponentWiring{Name: name, Wiring: wiring})
	}
	shared, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, OutDirName: "out", GoVersion: "1.26.0", Wirings: wirings})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range shared {
		name := "out/" + file.Path
		if file.Path == "go.mod" {
			name = file.Path
		}
		write(name, file.Bytes())
	}
	for source, target := range map[string]string{"process_factory.go": "factory/rpc.go", "process_runtime_test.go": "out/grpcapi/process/runtime_test.go", "process_health_test.go": "out/grpcapi/process/health_test.go", "process_integration_test.go": "integration/process_test.go", "process_telemetry_test.go": "integration/telemetry_test.go"} {
		data, err := os.ReadFile(filepath.Join("testdata", source))
		if err != nil {
			t.Fatal(err)
		}
		write(target, data)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"vet", "-mod=readonly", "./..."}, {"test", "-v", "-race", "-mod=readonly", "-count=1", "-timeout=3m", "./..."}} {
		command := exec.Command("go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("generated RPC process: %v\n%s", err, output)
		}
		if args[0] == "test" {
			t.Log(string(output))
		}
	}
}
