package compiler

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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

func TestRecordIsUpgradedAndDecodedStrictly(t *testing.T) {
	t.Run("ST-GEN-INV25-T A record is upgraded on the next run and decoded strictly", func(t *testing.T) {
		input := snapshotTestInput(t)
		applyInitialSnapshot(t, input)
		statePath := filepath.Join(input.ProjectDir, ".stego/state.yaml")
		state, err := LoadState(statePath)
		if err != nil {
			t.Fatal(err)
		}

		// A record without the compiler build is accepted and completed.
		state.LastApplied.CompilerBuild = nil
		if err := SaveState(statePath, state); err != nil {
			t.Fatal(err)
		}
		plan, err := Reconcile(input)
		if err != nil || !plan.StateChanged {
			t.Fatal("a record without the compiler build was not planned for completion", err)
		}
		if err := Apply(plan, input.ProjectDir, ""); err != nil {
			t.Fatal(err)
		}
		completed, err := LoadState(statePath)
		if err != nil || completed.LastApplied.CompilerBuild == nil || *completed.LastApplied.CompilerBuild != buildidentity.Current() {
			t.Fatal("the next apply did not complete the record with the compiler build", err)
		}

		// A record of another compiler build is a record change, with no output change.
		completed.LastApplied.CompilerBuild.Revision = "0123456789abcdef0123456789abcdef01234567"
		completed.LastApplied.CompilerBuild.VCS = "git"
		completed.LastApplied.CompilerBuild.SourceState = "clean"
		if err := SaveState(statePath, completed); err != nil {
			t.Fatal(err)
		}
		plan, err = Reconcile(input)
		if err != nil || !plan.StateChanged {
			t.Fatal("a record of another compiler build is not a record change", err)
		}
		for _, file := range plan.Files {
			if file.Action != ActionUnchanged {
				t.Fatal("a change of the compiler build alone changed a generated file", file.Path)
			}
		}
		if err := Apply(plan, input.ProjectDir, ""); err != nil {
			t.Fatal(err)
		}

		// A record with an unknown field is refused, and nothing changes.
		data, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		unknown := bytes.Replace(data, []byte("last_applied:\n"), []byte("last_applied:\n    unknown_field: 1\n"), 1)
		if bytes.Equal(unknown, data) {
			t.Fatal("the record has no last_applied to change")
		}
		if err := os.WriteFile(statePath, unknown, 0o644); err != nil {
			t.Fatal(err)
		}
		before := projectFiles(t, input.ProjectDir)
		if plan, err := Reconcile(input); err == nil {
			t.Fatal("a record with an unknown field was accepted", plan)
		}
		if !reflect.DeepEqual(before, projectFiles(t, input.ProjectDir)) {
			t.Fatal("a refused record changed a file")
		}
	})
}
