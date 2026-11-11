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
		ProjectDir: project, RegistryDir: registry, ModuleName: "example.com/snapshot", GoVersion: "1.26.11",
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
	t.Run("ST-GEN-UC03-R01-T02 A new generation run restores generated output with no hand edit", func(t *testing.T) {
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
		// The author allows the replacement of the hand edit (ST-GEN-UC03-R03).
		plan.ReplaceHandEdits = true
		if err := Apply(plan, input.ProjectDir, ""); err != nil {
			t.Fatal(err)
		}
		drift, err := DetectDrift(input.ProjectDir, filepath.Join(input.ProjectDir, "out"))
		if err != nil || drift.HasDrift() {
			t.Fatalf("apply did not restore the desired output: %v", err)
		}
	})
}

func TestApplyRejectsChangesAfterPlanBeforeWriting(t *testing.T) {
	t.Run("ST-GEN-INV05-T Apply refuses a plan when an input, the record or a generated file changed after planning", func(t *testing.T) {
		for _, name := range []string{"service.yaml", "go.mod", "go.sum", ".stego/state.yaml", ".stego/config.yaml", "out/internal/api/handler.go"} {
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
	})
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

func TestDetectDriftFillPresence(t *testing.T) {
	t.Run("ST-GEN-INV05-T The record names whether the project holds fills, so drift names fills/ when the first fill comes or the last goes, and apply refuses a plan made before the first fill", func(t *testing.T) {
		projectDir, registryDir := setupTestProject(t)
		outDir := filepath.Join(projectDir, "out")
		reconcile := func() *Plan {
			t.Helper()
			plan, err := Reconcile(ReconcilerInput{
				ProjectDir:  projectDir,
				RegistryDir: registryDir,
				Generators:  map[string]gen.Generator{"stub-api": &stubGenerator{}, "stub-store": &stubGenerator{}},
				GoVersion:   "1.22",
				ModuleName:  "github.com/test/svc",
			})
			if err != nil {
				t.Fatalf("Reconcile failed: %v", err)
			}
			return plan
		}
		plan := reconcile()
		if recorded := plan.NewState.LastApplied.Inputs.Options.HoldsFills; recorded == nil || *recorded {
			t.Fatalf("the record of a project without fills gives holds_fills %v", recorded)
		}
		if err := Apply(plan, projectDir, outDir); err != nil {
			t.Fatalf("Apply failed: %v", err)
		}
		// An empty fills folder, as stego init makes it, is not a fill.
		if err := os.MkdirAll(filepath.Join(projectDir, "fills"), 0o755); err != nil {
			t.Fatal(err)
		}
		if result, err := DetectDrift(projectDir, outDir); err != nil || result.HasDrift() {
			t.Fatalf("an empty fills folder drifts: %+v, %v", result, err)
		}

		stale := reconcile()
		if err := os.Mkdir(filepath.Join(projectDir, "fills", "first-fill"), 0o755); err != nil {
			t.Fatal(err)
		}
		result, err := DetectDrift(projectDir, outDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Inputs) != 1 || result.Inputs[0].Path != "fills/" {
			t.Fatalf("the first fill gives drifted inputs %+v, want fills/", result.Inputs)
		}
		if err := Apply(stale, projectDir, outDir); err == nil || !strings.Contains(err.Error(), "run plan again") {
			t.Fatalf("apply accepted a plan made before the first fill: %v", err)
		}

		if err := Apply(reconcile(), projectDir, outDir); err != nil {
			t.Fatalf("Apply with the fill failed: %v", err)
		}
		if result, err := DetectDrift(projectDir, outDir); err != nil || result.HasDrift() {
			t.Fatalf("drift after apply with the fill: %+v, %v", result, err)
		}
		if err := os.Remove(filepath.Join(projectDir, "fills", "first-fill")); err != nil {
			t.Fatal(err)
		}
		if result, err := DetectDrift(projectDir, outDir); err != nil || len(result.Inputs) != 1 || result.Inputs[0].Path != "fills/" {
			t.Fatalf("removing the last fill gives %+v, %v, want fills/", result, err)
		}
	})
}
