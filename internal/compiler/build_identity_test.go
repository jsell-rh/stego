package compiler

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/buildidentity"
)

func TestCompilerBuildState(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	statePath := filepath.Join(input.ProjectDir, ".stego/state.yaml")
	state, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastApplied.CompilerBuild == nil || *state.LastApplied.CompilerBuild != buildidentity.Current() {
		t.Fatal("compiler record missing")
	}
	// Legacy state remains readable. Its next plan adds the missing record.
	state.LastApplied.CompilerBuild = nil
	if err := SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	plan, err := Reconcile(input)
	if err != nil || !plan.StateChanged {
		t.Fatal("missing record was hidden", err)
	}
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
	plan, err = Reconcile(input)
	if err != nil || plan.HasChanges() {
		t.Fatal("repeated generation changed state", err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	// Public plan state cannot substitute a different compiler record.
	plan.NewState.LastApplied.CompilerBuild.GoVersion = "go1.0"
	if err := Apply(plan, input.ProjectDir, ""); err == nil || !strings.Contains(err.Error(), "compiler") {
		t.Fatal("changed build record accepted", err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed apply changed state", err)
	}
	state, err = LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state.LastApplied.CompilerBuild.GoVersion = "go1.0"
	if err := SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	plan, err = Reconcile(input)
	if err != nil || !plan.StateChanged {
		t.Fatal("compiler change was hidden", err)
	}
	for _, file := range plan.Files {
		if file.Action != ActionUnchanged {
			t.Fatal("build metadata changed stub output", file)
		}
	}
	state.LastApplied.CompilerBuild.SourceState = "trusted"
	if err := SaveState(statePath, state); err == nil {
		t.Fatal("invalid source state accepted")
	}
}
