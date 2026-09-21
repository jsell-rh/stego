package compiler

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

type modelProbe struct {
	models  []gen.GoModel
	renders int
	change  func()
}

func (p *modelProbe) GoModels(gen.Context) ([]gen.GoModel, error) { return p.models, nil }
func (p *modelProbe) Generate(gen.Context) ([]gen.File, *gen.Wiring, error) {
	p.renders++
	if p.change != nil {
		p.change()
	}
	return nil, nil, nil
}
func TestGoModelsRejectBeforeGeneration(t *testing.T) {
	input := snapshotTestInput(t)
	probe := &modelProbe{models: []gen.GoModel{{Name: "Parcel", GoType: "*Parcel"}}}
	other := &routeProbe{}
	input.Generators["stub-api"], input.Generators["stub-store"] = other, probe
	result, err := Validate(input)
	if err != nil || !result.HasErrors() {
		t.Fatal("invalid model passed validation", result, err)
	}
	if plan, err := Reconcile(input); plan != nil || err == nil {
		t.Fatal("invalid model produced a plan", plan, err)
	}
	if probe.renders != 0 || other.renders != 0 {
		t.Fatal("a generator ran before model validation")
	}
}
func TestGoModelsRejectChangedDeclaration(t *testing.T) {
	input := snapshotTestInput(t)
	probe := &modelProbe{models: []gen.GoModel{{Name: "Parcel", GoType: "Parcel", Fields: []gen.GoModelField{{Name: "id", Selection: "ID", Type: types.FieldTypeString}}}}}
	probe.change = func() { probe.models[0].Fields[0].Selection = "OtherID" }
	input.Generators["stub-store"] = probe
	if plan, err := Reconcile(input); plan != nil || err == nil || !strings.Contains(err.Error(), "differ from its preflight declaration") {
		t.Fatal("changed declaration produced a plan", plan, err)
	}
}
func TestGoModelContextCopies(t *testing.T) {
	probe := &modelProbe{models: []gen.GoModel{{Name: "Parcel", GoType: "Parcel", Fields: []gen.GoModelField{{Name: "id", Selection: "ID", Type: types.FieldTypeString}}}}}
	input := ReconcilerInput{Generators: map[string]gen.Generator{"custom-store": probe}}
	resolved := &resolvedCompilation{Names: []string{"consumer", "custom-store"}, Contexts: map[string]gen.Context{
		"consumer": {}, "custom-store": {ModuleName: "example.com/test", OutDirName: "generated", OutputNamespace: "data/store"},
	}}
	if err := resolveGoModelSources(input, resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Contexts["consumer"].GoModelSources["custom-store"].ImportPath != "example.com/test/generated/data/store" {
		t.Fatal("source import does not use its owned namespace")
	}
	resolved.Contexts["consumer"].GoModelSources["custom-store"].Models[0].Fields[0].Selection = "Changed"
	for _, actual := range []string{probe.models[0].Fields[0].Selection, resolved.GoModelSources["custom-store"].Models[0].Fields[0].Selection, resolved.Contexts["custom-store"].GoModelSources["custom-store"].Models[0].Fields[0].Selection} {
		if actual != "ID" {
			t.Fatal("consumer changed another contract")
		}
	}
}
