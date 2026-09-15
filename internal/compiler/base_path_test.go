package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBasePathSemanticGateStopsGenerators(t *testing.T) {
	input := snapshotTestInput(t)
	path := filepath.Join(input.ProjectDir, "service.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data)+"\nbase_path: /api/{id}\n")
	for name := range input.Generators {
		input.Generators[name] = rejectedInputGenerator{t}
	}
	validation, err := Validate(input)
	if err != nil || !validation.HasErrors() || !strings.Contains(FormatValidation(validation), "base_path") {
		t.Fatal("invalid prefix passed semantic validation", validation, err)
	}
	if plan, err := Reconcile(input); plan != nil || err == nil || !strings.Contains(err.Error(), "base_path") {
		t.Fatal("invalid prefix reached rendering", plan, err)
	}
	for _, name := range []string{"out", "go.mod", ".stego"} {
		if _, err := os.Stat(filepath.Join(input.ProjectDir, name)); !os.IsNotExist(err) {
			t.Fatal("rejected prefix changed project files", name, err)
		}
	}
}
