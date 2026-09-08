package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestApplyRejectsUnsafePathsBeforeWriting(t *testing.T) {
	for _, kind := range []string{"generated", "delete", "state"} {
		t.Run(kind, func(t *testing.T) {
			project := t.TempDir()
			outside := filepath.Join(project, "fills", "policy.go")
			if err := os.MkdirAll(filepath.Dir(outside), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(outside, []byte("human-owned source"), 0600); err != nil {
				t.Fatal(err)
			}
			plan := &Plan{
				GeneratedFiles: []gen.File{{Path: "main.go", Content: []byte("new output")}},
				NewState:       &State{LastApplied: &AppliedState{Files: map[string]string{"main.go": "hash"}}},
			}
			switch kind {
			case "generated":
				plan.GeneratedFiles = append(plan.GeneratedFiles, gen.File{Path: "../fills/policy.go", Content: []byte("replacement")})
			case "delete":
				plan.Files = []PlannedFile{{Path: "../fills/policy.go", Action: ActionDelete}}
			case "state":
				plan.NewState.LastApplied.Files["../fills/policy.go"] = "hash"
			}
			if err := Apply(plan, project, filepath.Join(project, "out")); err == nil {
				t.Fatal("unsafe path was accepted")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "human-owned source" {
				t.Fatalf("human source changed: %q, %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(project, "out")); !os.IsNotExist(err) {
				t.Fatalf("output was changed before path validation: %v", err)
			}
		})
	}
}

func TestStateRejectsCorruptionAndUnsafePaths(t *testing.T) {
	for _, input := range []string{
		"last_applied: [invalid",
		"last_applied: {unknown: value}",
		"last_applied: {files: {'../fills/policy.go': hash}}",
		"last_applied: {files: {'/tmp/source.go': hash}}",
		"last_applied: null\n---\nlast_applied: null",
	} {
		t.Run(input, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.yaml")
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadState(path); err == nil {
				t.Fatal("invalid state was treated as valid or empty")
			}
		})
	}
}

func TestApplyRejectsOutputOutsideProject(t *testing.T) {
	project, outside := t.TempDir(), t.TempDir()
	plan := &Plan{GeneratedFiles: []gen.File{{Path: "main.go"}}, NewState: &State{}}
	if err := Apply(plan, project, outside); err == nil {
		t.Fatal("external output directory was accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("external directory changed: %v, %v", entries, err)
	}
}

func TestNamespaceContractsGateCompilation(t *testing.T) {
	for _, namespace := range []string{"../fills", "/tmp/storage", "internal/api", "internal/api/nested", "internal"} {
		t.Run(namespace, func(t *testing.T) {
			project, registry := setupTestProject(t)
			path := filepath.Join(registry, "components/stub-store/component.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), "internal/storage", namespace, 1))
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			input := ReconcilerInput{
				ProjectDir: project, RegistryDir: registry,
				Generators: map[string]gen.Generator{
					"stub-api": rejectedInputGenerator{t}, "stub-store": rejectedInputGenerator{t},
				},
			}
			validation, err := Validate(input)
			if err != nil || !validation.HasErrors() || !strings.Contains(FormatValidation(validation), "[namespace]") {
				t.Fatalf("invalid namespace was accepted: result=%+v, err=%v", validation, err)
			}
			if _, err := Reconcile(input); err == nil {
				t.Fatal("generation accepted invalid namespaces")
			}
		})
	}
}
