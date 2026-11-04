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
	var plan *Plan
	if !t.Run("ST-DIS-UC09-R03-T02 Apply writes the current build identity into the record", func(t *testing.T) {
		if state.LastApplied.CompilerBuild == nil || *state.LastApplied.CompilerBuild != buildidentity.Current() {
			t.Fatal("compiler record missing")
		}
		// Legacy state remains readable. Its next plan adds the missing record.
		state.LastApplied.CompilerBuild = nil
		if err := SaveState(statePath, state); err != nil {
			t.Fatal(err)
		}
		plan, err = Reconcile(input)
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
	}) {
		return
	}
	if !t.Run("ST-GEN-INV04-T Apply refuses a plan that carries another compiler build", func(t *testing.T) {
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
	}) {
		return
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
	t.Run("ST-VO42-C01-T A source state is exactly unknown, clean or modified", func(t *testing.T) {
		const revision = "0123456789abcdef0123456789abcdef01234567"
		accepted := []struct{ source, vcs, revision string }{
			{"unknown", "unknown", "unknown"},
			{"clean", "git", revision},
			{"modified", "git", revision},
		}
		for _, c := range accepted {
			state.LastApplied.CompilerBuild.SourceState = c.source
			state.LastApplied.CompilerBuild.VCS = c.vcs
			state.LastApplied.CompilerBuild.Revision = c.revision
			if err := SaveState(statePath, state); err != nil {
				t.Fatalf("source state %q refused: %v", c.source, err)
			}
		}
		for _, source := range []string{"trusted", "Clean", ""} {
			state.LastApplied.CompilerBuild.SourceState = source
			state.LastApplied.CompilerBuild.VCS = "git"
			state.LastApplied.CompilerBuild.Revision = revision
			if err := SaveState(statePath, state); err == nil {
				t.Fatalf("source state %q accepted", source)
			}
		}
	})
}
