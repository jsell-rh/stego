package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/cliapplication"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
)

func TestApplicationFactoryChecksGateValidation(t *testing.T) {
	for name, generator := range map[string]gen.Generator{"cli": new(cliapplication.Generator), "http": new(httpapplication.Generator), "grpc": new(grpcapplication.Generator)} {
		t.Run(name, func(t *testing.T) {
			input := applicationPreflightInput(t, generator, "out/factory")
			project := input.ProjectDir
			result, err := Validate(input)
			if err != nil || !result.HasErrors() || !strings.Contains(FormatValidation(result), "outside generated output") {
				t.Fatalf("invalid factory passed validation: %+v, %v", result, err)
			}
			if plan, err := Reconcile(input); plan != nil || err == nil || !strings.Contains(err.Error(), FormatValidation(result)) {
				t.Fatalf("commands do not share factory validation: %+v, %v", plan, err)
			}
			data, err := os.ReadFile(filepath.Join(project, "out/retained.txt"))
			if err != nil || string(data) != "retained output" {
				t.Fatal("validation changed output", err)
			}
		})
	}
}

func applicationPreflightInput(t *testing.T, generator gen.Generator, factory string) ReconcilerInput {
	t.Helper()
	project, registry, input := setupValidateProject(t)
	input.GoVersion = "1.26.8"
	input.Generators["stub-api"] = generator
	input.Generators["jwt-auth"] = rejectedInputGenerator{t}
	archPath := filepath.Join(registry, "archetypes/test-arch/archetype.yaml")
	arch, err := os.ReadFile(archPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, archPath, strings.Replace(string(arch), "  - stub-store", "  - stub-store\n  - jwt-auth", 1))
	mkdirAll(t, filepath.Join(registry, "components/jwt-auth"))
	writeFile(t, filepath.Join(registry, "components/jwt-auth/component.yaml"), "kind: component\nname: jwt-auth\nversion: 1.0.0\noutput_namespace: auth\nprovides: [auth-provider]\n")
	writeFile(t, filepath.Join(registry, "components/stub-api/component.yaml"), `kind: component
name: stub-api
version: 1.0.0
output_namespace: internal/api
config:
  factory_package: {type: string, default: `+factory+`}
  proto_files:
    type: list
    default: [{path: api.proto, import_path: api.proto}]
    items:
      path: {type: string}
      import_path: {type: string}
requires: [storage-adapter]
provides: [http-server]
`)
	writeFile(t, filepath.Join(project, "api.proto"), "syntax = \"proto3\"; package example; message Record {}")
	mkdirAll(t, filepath.Join(project, "out"))
	writeFile(t, filepath.Join(project, "out/retained.txt"), "retained output")
	return input
}

func TestProtobufChecksGateValidation(t *testing.T) {
	for _, source := range []string{
		`syntax="proto3";message A{string x=1;int32 y=1;}`,
		`syntax="proto3";import "missing.proto";message A{}`,
		`syntax="proto2";message A{}`,
		`syntax="proto3";message A{reserved 1;string x=1;}`,
	} {
		t.Run(source, func(t *testing.T) {
			input := applicationPreflightInput(t, new(grpcapplication.Generator), "domain")
			writeFile(t, filepath.Join(input.ProjectDir, "api.proto"), source)
			result, err := Validate(input)
			if err != nil || !result.HasErrors() {
				t.Fatal("invalid protobuf passed validation", result, err)
			}
			if plan, err := Reconcile(input); plan != nil || err == nil || !strings.Contains(err.Error(), FormatValidation(result)) {
				t.Fatal("protobuf checks differ", plan, err)
			}
		})
	}
	input := applicationPreflightInput(t, new(grpcapplication.Generator), "domain")
	if result, err := Validate(input); err != nil || result.HasErrors() {
		t.Fatal("valid protobuf rejected", result, err)
	}
	if err := os.Remove(filepath.Join(input.ProjectDir, "api.proto")); err != nil {
		t.Fatal(err)
	}
	if result, err := Validate(input); err != nil || !result.HasErrors() || !strings.Contains(FormatValidation(result), "does not exist") {
		t.Fatal("missing source accepted", result, err)
	}
}

type preflightProbe struct {
	inputFiles                  []string
	inputCalls, checks, renders int
	check                       func(gen.Context) error
	render                      func(gen.Context)
}

func (p *preflightProbe) InputFiles(map[string]any) ([]string, error) {
	p.inputCalls++
	return p.inputFiles, nil
}
func (p *preflightProbe) ValidateContext(ctx gen.Context) error {
	p.checks++
	if p.check != nil {
		return p.check(ctx)
	}
	return nil
}
func (p *preflightProbe) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	p.renders++
	if p.render != nil {
		p.render(ctx)
	}
	return nil, nil, nil
}

func TestAllComponentChecksRunBeforeAnyRendering(t *testing.T) {
	input := snapshotTestInput(t)
	first := new(preflightProbe)
	last := &preflightProbe{check: func(gen.Context) error { return fmt.Errorf("rejected component contract") }}
	input.Generators["stub-api"], input.Generators["stub-store"] = first, last
	for _, plan := range []bool{false, true} {
		if plan {
			if value, err := Reconcile(input); value != nil || err == nil {
				t.Fatal("rejected contract produced a plan", value, err)
			}
		} else {
			if result, err := Validate(input); err != nil || !result.HasErrors() {
				t.Fatal("rejected contract passed validation", result, err)
			}
		}
		if first.renders != 0 || last.renders != 0 {
			t.Fatal("rendering started before all checks passed")
		}
	}
	if first.checks != 2 || last.checks != 2 {
		t.Fatal("component checks were skipped")
	}
}

func TestGenerationUsesPreflightInputSnapshot(t *testing.T) {
	input := snapshotTestInput(t)
	source := filepath.Join(input.ProjectDir, "source.txt")
	writeFile(t, source, "original input")
	first := &preflightProbe{render: func(gen.Context) { writeFile(t, source, "later input") }}
	last := &preflightProbe{inputFiles: []string{"source.txt"}}
	var checked gen.Context
	last.check = func(ctx gen.Context) error {
		if string(ctx.Inputs["source.txt"]) != "original input" || ctx.ModuleName != input.ModuleName || ctx.OutDirName != "out" || ctx.PeerNamespaces["stub-api"] != "internal/api" {
			t.Fatal("incomplete preflight context", ctx)
		}
		checked = ctx
		return nil
	}
	last.render = func(ctx gen.Context) {
		if !reflect.DeepEqual(checked, ctx) || string(ctx.Inputs["source.txt"]) != "original input" {
			t.Fatal("generation changed the checked inputs")
		}
	}
	input.Generators["stub-api"], input.Generators["stub-store"] = first, last
	if plan, err := Reconcile(input); plan != nil || err == nil || !strings.Contains(err.Error(), "source.txt changed") {
		t.Fatal("changed source produced a usable plan", plan, err)
	}
	if last.checks != 1 || last.inputCalls != 1 || last.renders != 1 {
		t.Fatal("context was not used once", last)
	}
	for _, name := range []string{"out", "go.mod", ".stego"} {
		if _, err := os.Stat(filepath.Join(input.ProjectDir, name)); !os.IsNotExist(err) {
			t.Fatal("failed compilation changed output", name, err)
		}
	}
}
