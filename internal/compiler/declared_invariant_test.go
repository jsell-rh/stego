package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeclaredInvariantRefused(t *testing.T) {
	t.Run("ST-STD-TXN-R17-T01 Generation refuses a declared invariant that no generator emits, naming it and its entity", func(t *testing.T) {
		input := snapshotTestInput(t)
		path := filepath.Join(input.ProjectDir, "service.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, string(data)+"\ninvariants:\n  - {name: unique_label, kind: unique, on: Widget, by: [label]}\n")
		for name := range input.Generators {
			input.Generators[name] = rejectedInputGenerator{t}
		}
		// Checking accepts the invariant (ST-DECL-EN17); generation refuses it before any generator runs.
		if validation, err := Validate(input); err != nil || validation.HasErrors() {
			t.Fatalf("checking refused the invariant: %v %v", FormatValidation(validation), err)
		}
		plan, err := Reconcile(input)
		if plan != nil || err == nil || !strings.Contains(err.Error(), "unique_label (unique on Widget)") ||
			!strings.Contains(err.Error(), "ST-STD-TXN-R17") {
			t.Fatalf("Reconcile gave %v, %v; want the refusal that names the invariant unique_label on Widget", plan, err)
		}
		for _, name := range []string{"out", "go.mod", ".stego"} {
			if _, err := os.Stat(filepath.Join(input.ProjectDir, name)); !os.IsNotExist(err) {
				t.Fatal("the refused generation changed project files", name, err)
			}
		}
	})
}
