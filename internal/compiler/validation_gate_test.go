package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

type rejectedInputGenerator struct {
	t *testing.T
}

func (g rejectedInputGenerator) Generate(gen.Context) ([]gen.File, *gen.Wiring, error) {
	g.t.Fatal("generator ran after semantic validation rejected the service")
	return nil, nil, nil
}

func TestSemanticValidationGatesGeneration(t *testing.T) {
	tests := []struct {
		name, field, collection, want string
	}{
		{"empty length range", "{name: title, type: string, min_length: 10, max_length: 1}", "", "min_length"},
		{"invalid pattern", `{name: title, type: string, pattern: "["}`, "", "pattern"},
		{"unknown field type", "{name: title, type: unsupported}", "", "unsupported"},
		{"missing patch field", "{name: title, type: string}", "    patchable: [missing]\n", "patchable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project, registry := setupTestProject(t)
			service := "kind: service\nname: test\narchetype: test-arch\nlanguage: go\nentities:\n  - name: Widget\n    fields:\n      - " + tt.field + "\ncollections:\n  widgets:\n    entity: Widget\n    operations: [create, patch]\n" + tt.collection
			writeFile(t, filepath.Join(project, "service.yaml"), service)
			input := ReconcilerInput{
				ProjectDir: project, RegistryDir: registry,
				ModuleName: "example.com/test", GoVersion: "1.24",
				Generators: map[string]gen.Generator{
					"stub-api": rejectedInputGenerator{t}, "stub-store": rejectedInputGenerator{t},
				},
			}
			validation, err := Validate(input)
			if err != nil || !validation.HasErrors() || !strings.Contains(FormatValidation(validation), tt.want) {
				t.Fatalf("expected validation error for %s: result=%+v, err=%v", tt.want, validation, err)
			}
			plan, err := Reconcile(input)
			if plan != nil || err == nil || !strings.Contains(err.Error(), FormatValidation(validation)) {
				t.Fatalf("reconciliation did not use the validation result: plan=%+v, err=%v", plan, err)
			}
			for _, path := range []string{"out", "go.mod", ".stego"} {
				if _, err := os.Stat(filepath.Join(project, path)); !os.IsNotExist(err) {
					t.Fatalf("invalid input changed %s: %v", path, err)
				}
			}
		})
	}
}

func TestMissingGeneratorStopsAllGeneration(t *testing.T) {
	for _, state := range []string{"missing", "nil", "typed nil"} {
		t.Run(state, func(t *testing.T) {
			input := snapshotTestInput(t)
			input.Generators["stub-api"] = rejectedInputGenerator{t}
			switch state {
			case "missing":
				delete(input.Generators, "stub-store")
			case "nil":
				input.Generators["stub-store"] = nil
			case "typed nil":
				input.Generators["stub-store"] = (*stubGenerator)(nil)
			}
			validation, err := Validate(input)
			if err != nil || !validation.HasErrors() || !strings.Contains(FormatValidation(validation), `component "stub-store" has no generator`) {
				t.Fatalf("missing backend passed validation: %+v, %v", validation, err)
			}
			plan, err := Reconcile(input)
			if plan != nil || err == nil || !strings.Contains(err.Error(), FormatValidation(validation)) {
				t.Fatalf("missing backend did not stop compilation: %+v, %v", plan, err)
			}
			for _, name := range []string{"out", "go.mod", ".stego"} {
				if _, err := os.Stat(filepath.Join(input.ProjectDir, name)); !os.IsNotExist(err) {
					t.Fatalf("missing generator changed %s: %v", name, err)
				}
			}
		})
	}
}
