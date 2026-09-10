package compiler

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/cliapplication"
	"github.com/jsell-rh/stego/internal/generator/controller"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
	"github.com/jsell-rh/stego/internal/generator/kafkaproducer"
	"github.com/jsell-rh/stego/internal/generator/kubernetesclient"
	"github.com/jsell-rh/stego/internal/generator/outbox"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/postgresclient"
	"github.com/jsell-rh/stego/internal/generator/restapi"
	"github.com/jsell-rh/stego/internal/generator/rhssoauth"
	"github.com/jsell-rh/stego/internal/generator/tslsearch"
	"github.com/jsell-rh/stego/internal/types"
)

func TestLibraryGeneratorsCheckGoPackageNames(t *testing.T) {
	for name, generator := range map[string]gen.Generator{
		"controller": new(controller.Generator), "grpc": new(grpcapplication.Generator), "http": new(httpapplication.Generator), "jwt": new(jwtauth.Generator), "kafka": new(kafkaproducer.Generator), "kubernetes": new(kubernetesclient.Generator), "outbox": new(outbox.Generator), "storage": new(postgresadapter.Generator), "postgres client": new(postgresclient.Generator), "rest": new(restapi.Generator), "sso": new(rhssoauth.Generator), "search": new(tslsearch.Generator),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{ModuleName: "example.com/names", OutDirName: "out", OutputNamespace: "go-services/worker", StorageContract: "example.com/names/out/contracts/storage", EventsContract: "example.com/names/out/contracts/events", AuthPackage: "example.com/names/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "http-application": "application", "outbox": "queue"}, Entities: []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}}}}, Collections: []types.Collection{{Name: "records", Entity: "Record", Operations: []types.Operation{types.OpRead}}}, ComponentConfig: map[string]any{}}
			if name == "http" || name == "grpc" {
				ctx.ComponentConfig["factory_package"] = "domain"
			}
			if name == "grpc" {
				ctx.ComponentConfig["proto_files"] = []any{map[string]any{"path": "api.proto", "import_path": "api.proto"}}
				ctx.Inputs = map[string][]byte{"api.proto": []byte(`syntax="proto3";package sample;message Record{}`)}
			}
			validator, ok := generator.(gen.ContextValidator)
			if !ok {
				t.Fatal("library has no preflight")
			}
			for _, namespace := range []string{"bad-name", "nested/type", "nested/_", "nested/main", "nested/9worker", "nested/café"} {
				ctx.OutputNamespace = namespace
				checked := validator.ValidateContext(ctx)
				if checked == nil {
					t.Fatal("invalid library passed preflight", namespace)
				}
				files, wiring, err := generator.Generate(ctx)
				if err == nil || err.Error() != checked.Error() || files != nil || wiring != nil {
					t.Fatal("direct generation bypassed package check", namespace, err)
				}
			}
			ctx.OutputNamespace = "go-services/worker"
			if err := validator.ValidateContext(ctx); err != nil {
				t.Fatal("nested library rejected", err)
			}
			files, _, err := generator.Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if strings.HasSuffix(file.Path, ".go") {
					if _, err := parser.ParseFile(token.NewFileSet(), file.Path, file.Content, parser.AllErrors); err != nil {
						t.Fatal("invalid generated source", file.Path, err)
					}
				}
			}
		})
	}
}

func TestCLIContainerAllowsHyphens(t *testing.T) {
	ctx := gen.Context{ModuleName: "example.com/names", OutDirName: "generated-code", OutputNamespace: "cli-tools", ComponentConfig: map[string]any{"factory_package": "domain"}}
	generator := new(cliapplication.Generator)
	if err := generator.ValidateContext(ctx); err != nil {
		t.Fatal(err)
	}
	files, _, err := generator.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if !strings.HasPrefix(file.Path, "cli-tools/") {
			t.Fatal("wrong CLI container", file.Path)
		}
		if _, err := parser.ParseFile(token.NewFileSet(), file.Path, file.Content, parser.AllErrors); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOutputDirectoryMustBeAGoImportPath(t *testing.T) {
	input := snapshotTestInput(t)
	input.OutDir = filepath.Join(input.ProjectDir, "bad directory")
	input.Generators["stub-api"] = rejectedInputGenerator{t}
	result, err := Validate(input)
	if err != nil || !result.HasErrors() || !strings.Contains(FormatValidation(result), "Go import namespace") {
		t.Fatal("invalid output directory passed", result, err)
	}
	if plan, err := Reconcile(input); plan != nil || err == nil {
		t.Fatal("invalid output directory reached rendering", plan, err)
	}
}

func TestApplicationFactoriesRequireGoImportPaths(t *testing.T) {
	for name, generator := range map[string]gen.Generator{"cli": new(cliapplication.Generator), "http": new(httpapplication.Generator), "grpc": new(grpcapplication.Generator)} {
		t.Run(name, func(t *testing.T) {
			for _, factory := range []string{"bad factory", "café/factory"} {
				input := applicationPreflightInput(t, generator, factory)
				result, err := Validate(input)
				if err != nil || !result.HasErrors() {
					t.Fatal("invalid factory import passed", factory, result, err)
				}
				if plan, err := Reconcile(input); plan != nil || err == nil {
					t.Fatal("invalid factory import rendered", factory, plan, err)
				}
			}
			input := applicationPreflightInput(t, generator, "domain-code/factory")
			if result, err := Validate(input); err != nil || result.HasErrors() {
				t.Fatal("valid factory import rejected", result, err)
			}
		})
	}
}

func TestProtobufGeneratedPackagesMustBeImportable(t *testing.T) {
	generator := new(grpcapplication.Generator)
	ctx := gen.Context{ModuleName: "example.com/names", OutDirName: "out", OutputNamespace: "grpcapi", StorageContract: "example.com/names/out/contracts/storage", AuthPackage: "example.com/names/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, Inputs: map[string][]byte{"api.proto": []byte(`syntax="proto3";package sample;message Record{}`)}}
	for _, name := range []string{"main/api.proto", "bad space/api.proto", "café/api.proto"} {
		t.Run(name, func(t *testing.T) {
			ctx.ComponentConfig = map[string]any{"factory_package": "domain", "proto_files": []any{map[string]any{"path": "api.proto", "import_path": name}}}
			if err := generator.ValidateContext(ctx); err == nil {
				t.Fatal("unimportable generated protobuf passed preflight", name)
			}
			if files, _, err := generator.Generate(ctx); err == nil || files != nil {
				t.Fatal("unimportable protobuf was generated", name, err)
			}
		})
	}
}
