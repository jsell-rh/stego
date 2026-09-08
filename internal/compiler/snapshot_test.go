package compiler

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func snapshotTestInput(t *testing.T) ReconcilerInput {
	t.Helper()
	project, registry := setupTestProject(t)
	return ReconcilerInput{
		ProjectDir: project, RegistryDir: registry, ModuleName: "example.com/snapshot", GoVersion: "1.26.8",
		Generators: map[string]gen.Generator{
			"stub-api":   &stubGenerator{files: []gen.File{{Path: "internal/api/handler.go", Content: []byte("package api\n")}}},
			"stub-store": &stubGenerator{},
		},
	}
}

func applyInitialSnapshot(t *testing.T, input ReconcilerInput) {
	t.Helper()
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
}

func TestPlanReportsModifiedOutputWhenStateStillMatches(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	path := filepath.Join(input.ProjectDir, "out/internal/api/handler.go")
	if err := os.WriteFile(path, []byte("package changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range plan.Files {
		if file.Path == "internal/api/handler.go" && file.Action == ActionUpdate {
			found = true
		}
	}
	if !found || !plan.HasChanges() {
		t.Fatalf("modified output is hidden by saved hashes: %s", FormatPlan(plan))
	}
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
	drift, err := DetectDrift(input.ProjectDir, filepath.Join(input.ProjectDir, "out"))
	if err != nil || drift.HasDrift() {
		t.Fatalf("apply did not restore the desired output: %v", err)
	}
}

func TestApplyRejectsChangesAfterPlanBeforeWriting(t *testing.T) {
	for _, name := range []string{"service.yaml", "go.mod", ".stego/state.yaml", ".stego/config.yaml", "out/internal/api/handler.go"} {
		t.Run(name, func(t *testing.T) {
			input := snapshotTestInput(t)
			applyInitialSnapshot(t, input)
			plan, err := Reconcile(input)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(input.ProjectDir, name)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			changed := []byte("changed after plan\n")
			if err := os.WriteFile(path, changed, 0644); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(input.ProjectDir, ".stego/state.yaml")
			state, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(plan, input.ProjectDir, ""); err == nil || !strings.Contains(err.Error(), "changed after planning") {
				t.Fatalf("stale plan was accepted: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, changed) {
				t.Fatalf("apply overwrote a changed file: %v", err)
			}
			afterState, err := os.ReadFile(statePath)
			if err != nil || !bytes.Equal(afterState, state) {
				t.Fatalf("apply wrote state before rejecting the stale plan: %v", err)
			}
		})
	}
}

func TestApplyRejectsOrphanChangeAfterPlan(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	input.Generators["stub-api"] = &stubGenerator{}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(input.ProjectDir, "out/internal/api/handler.go")
	if err := os.WriteFile(path, []byte("changed orphan"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan, input.ProjectDir, ""); err == nil {
		t.Fatal("deleted an orphan that changed after planning")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "changed orphan" {
		t.Fatalf("changed orphan was lost: %v", err)
	}
}

func TestApplyBindsPlanToProjectAndOutput(t *testing.T) {
	input := snapshotTestInput(t)
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := Apply(plan, other, ""); err == nil {
		t.Fatal("accepted a plan for another project")
	}
	if err := Apply(plan, input.ProjectDir, filepath.Join(input.ProjectDir, "different")); err == nil {
		t.Fatal("accepted a plan for another output directory")
	}
	entries, err := os.ReadDir(other)
	if err != nil || len(entries) != 0 {
		t.Fatalf("different project was modified: %v", err)
	}
}

func TestPlanRecordsInputOnlyStateChange(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	path := filepath.Join(input.ProjectDir, "service.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n# Declaration comment.\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.StateChanged || !plan.HasChanges() {
		t.Fatal("input change cannot update compiler state")
	}
	for _, file := range plan.Files {
		if file.Action != ActionUnchanged {
			t.Errorf("comment changed output %s", file.Path)
		}
	}
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
	plan, err = Reconcile(input)
	if err != nil || plan.HasChanges() {
		t.Fatalf("state change was not saved: %v", err)
	}
}
