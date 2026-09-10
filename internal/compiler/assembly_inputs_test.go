package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssemblyTargetChecksGateAllGenerators(t *testing.T) {
	for _, test := range []struct{ name, module, target string }{{"missing module", "", "1.26.8"}, {"invalid module", "bad module", "1.26.8"}, {"missing target", "example.com/service", ""}, {"invalid target", "example.com/service", "invalid"}} {
		t.Run(test.name, func(t *testing.T) {
			input := snapshotTestInput(t)
			input.ModuleName = test.module
			input.GoVersion = test.target
			input.Generators["stub-api"] = rejectedInputGenerator{t}
			result, err := Validate(input)
			if err != nil || !result.HasErrors() {
				t.Fatal("invalid build target passed validation", result, err)
			}
			if plan, err := Reconcile(input); plan != nil || err == nil {
				t.Fatal("invalid build target reached generation", plan, err)
			}
			if files, err := Assemble(AssemblerInput{ModuleName: test.module, GoVersion: test.target}); files != nil || err == nil {
				t.Fatal("direct assembly accepted invalid target", files, err)
			}
		})
	}
}

func TestNormalizedSlotNamesFailBeforeRendering(t *testing.T) {
	project, registry, input := setupValidateProject(t)
	component := filepath.Join(registry, "components/stub-api/component.yaml")
	data, err := os.ReadFile(component)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, component, string(data)+"  - name: before__create\n    proto: stego.components.rest_api.slots.BeforeRetry\n    default: passthrough\n")
	original := filepath.Join(registry, "components/stub-api/slots/before_create.proto")
	data, err = os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(registry, "components/stub-api/slots/before__create.proto"), strings.ReplaceAll(string(data), "BeforeCreate", "BeforeRetry"))
	for i, slot := range []string{"before_create", "before__create"} {
		name := []string{"first", "second"}[i]
		dir := filepath.Join(project, "fills", name)
		mkdirAll(t, dir)
		writeFile(t, filepath.Join(dir, "fill.yaml"), "kind: fill\nname: "+name+"\nimplements: stub-api."+slot+"\ncollection: widgets\nqualified_by: tester\nqualified_at: 2026-04-01\n")
	}
	service := filepath.Join(project, "service.yaml")
	data, err = os.ReadFile(service)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, service, string(data)+"\nslots:\n  - {slot: before_create, collection: widgets, gate: [first]}\n  - {slot: before__create, collection: widgets, gate: [second]}\n")
	input.Generators["stub-api"] = rejectedInputGenerator{t}
	result, err := Validate(input)
	if err != nil || !result.HasErrors() || !strings.Contains(FormatValidation(result), "same variable name") {
		t.Fatal("normalized slot collision passed validation", result, err)
	}
	if plan, err := Reconcile(input); plan != nil || err == nil || !strings.Contains(err.Error(), "same variable name") {
		t.Fatal("slot collision reached rendering", plan, err)
	}
}

func TestBuildTargetSyntaxIsShared(t *testing.T) {
	for _, target := range []string{"1.026", "01.26", "1.26.08", "go1.26", "1.26.8 extra"} {
		input := snapshotTestInput(t)
		input.GoVersion = target
		checked := validateBuildTarget(input.ModuleName, target)
		if checked == nil {
			t.Fatal("invalid Go target accepted", target)
		}
		if _, _, err := ProjectModuleSettings(input.ProjectDir, input.ModuleName, target); err == nil || err.Error() != checked.Error() {
			t.Fatal("project settings use a different target rule", target, err)
		}
		if result, err := Validate(input); err != nil || !result.HasErrors() || !strings.Contains(FormatValidation(result), checked.Error()) {
			t.Fatal("validation uses a different target rule", target, result, err)
		}
		if _, err := Assemble(AssemblerInput{ModuleName: input.ModuleName, GoVersion: target}); err == nil || err.Error() != checked.Error() {
			t.Fatal("assembly uses a different target rule", target, err)
		}
	}
	for _, target := range []string{"1.22", "1.26.8", "1.27rc1"} {
		if err := validateBuildTarget("example.com/service", target); err != nil {
			t.Fatal("valid Go target rejected", target, err)
		}
	}
}
