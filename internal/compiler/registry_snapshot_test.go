package compiler

import (
	"bytes"
	"context"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestApplyRejectsRegistryChangeAfterPlan(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	input.Generators["stub-api"] = &stubGenerator{files: []gen.File{{Path: "internal/api/handler.go", Content: []byte("package api\n// New output.\n")}}}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(input.ProjectDir, ".stego/state.yaml")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(input.RegistryDir, "components/stub-api/component.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n# Registry changed after planning.\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan, input.ProjectDir, ""); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Errorf("apply accepted a changed registry: %v", err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed apply changed state", err)
	}
}

type registryEditingGenerator struct{ path string }

func (g registryEditingGenerator) Generate(gen.Context) ([]gen.File, *gen.Wiring, error) {
	data, err := os.ReadFile(g.path)
	if err != nil {
		return nil, nil, err
	}
	return nil, nil, os.WriteFile(g.path, append(data, []byte("\n# Registry changed during generation.\n")...), 0600)
}
func TestReconcileRejectsRegistryChangeDuringGeneration(t *testing.T) {
	input := snapshotTestInput(t)
	input.Generators["stub-api"] = registryEditingGenerator{path: filepath.Join(input.RegistryDir, "components/stub-store/component.yaml")}
	plan, err := Reconcile(input)
	if err == nil || plan != nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("generation accepted a changed registry: %v", err)
	}
}

func TestRegistryContentChangeUpdatesOnlyState(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	before, err := LoadState(filepath.Join(input.ProjectDir, ".stego/state.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(before.LastApplied.RegistryContentSHA256) != 64 {
		t.Fatal("registry identity was not saved")
	}
	path := filepath.Join(input.RegistryDir, "components/stub-api/component.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n# Content without output changes.\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.StateChanged || !plan.HasChanges() || plan.NewState.LastApplied.RegistryContentSHA256 == before.LastApplied.RegistryContentSHA256 {
		t.Fatal("registry-only change was hidden")
	}
	for _, file := range plan.Files {
		if file.Action != ActionUnchanged {
			t.Fatal("comment changed generated output", file)
		}
	}
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
	next, err := Reconcile(input)
	if err != nil || next.HasChanges() {
		t.Fatal("repeated plan changed state", err)
	}
	// Old state records remain readable, but require a new content identity.
	saved, err := LoadState(filepath.Join(input.ProjectDir, ".stego/state.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	saved.LastApplied.RegistryContentSHA256 = ""
	if err := SaveState(filepath.Join(input.ProjectDir, ".stego/state.yaml"), saved); err != nil {
		t.Fatal(err)
	}
	migrated, err := Reconcile(input)
	if err != nil || !migrated.StateChanged || len(migrated.NewState.LastApplied.RegistryContentSHA256) != 64 {
		t.Fatal("legacy state did not gain provenance", err)
	}
}

func TestRegistryChangeAfterContendedApply(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(input.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := lockProject(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := Apply(plan, input.ProjectDir, ""); err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatal("apply bypassed the lock", err)
	}
	if err := os.WriteFile(filepath.Join(input.RegistryDir, "extra.proto"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if err := Apply(plan, input.ProjectDir, ""); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatal("retry accepted the changed registry", err)
	}
}

func TestDependenciesRejectRegistryEditBeforeCommit(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	modulePath := filepath.Join(input.ProjectDir, "go.mod")
	before, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, directory, stage string, args ...string) error {
		if err := runDependencyCommand(ctx, directory, stage, args...); err != nil {
			return err
		}
		if args[len(args)-1] == "-diff" {
			return os.WriteFile(filepath.Join(input.RegistryDir, "new.proto"), nil, 0600)
		}
		return nil
	}
	if err := resolveDependencies(context.Background(), input, run, nil); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatal("dependencies used a stale registry", err)
	}
	after, err := os.ReadFile(modulePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("registry rejection changed module", err)
	}
}

func TestSlotGenerationUsesCapturedRegistryFiles(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{
		"components/sample/component.yaml":    "kind: component\nname: sample\nversion: 1.0.0\nslots:\n  - name: check\n    proto: sample.Check\n",
		"components/sample/slots/check.proto": "syntax = \"proto3\";\npackage sample;\nimport \"stego/common/types.proto\";\nservice Check {\n  rpc Evaluate(Request) returns (common.Result);\n}\nmessage Request {\n  string id = 1;\n}\n",
		"common/types.proto":                  "syntax = \"proto3\";\npackage common;\nmessage Result {\n  bool ok = 1;\n}\n",
	} {
		file := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := registry.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	components := map[string]*types.Component{"sample": reg.Component("sample")}
	declaration := &types.ServiceDeclaration{Slots: []types.SlotDeclaration{{Slot: "check", Gate: []string{"policy"}}}}
	before, err := generateSlotFiles("slots", components, declaration, reg)
	if err != nil || len(before) == 0 {
		t.Fatal("valid slot fixture", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	after, err := generateSlotFiles("slots", components, declaration, reg)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("slot generation reread disk", err)
	}
}

func TestApplyRejectsChangedRegistryIdentityInPlan(t *testing.T) {
	input := snapshotTestInput(t)
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	plan.NewState.LastApplied.RegistryContentSHA256 = strings.Repeat("a", 64)
	if err := Apply(plan, input.ProjectDir, ""); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatal("changed identity was accepted", err)
	}
	if _, err := os.Stat(filepath.Join(input.ProjectDir, "out")); !os.IsNotExist(err) {
		t.Fatal("invalid plan wrote output", err)
	}
}

func TestSlotImportErrorsAreNotIgnored(t *testing.T) {
	for _, failure := range []string{"missing", "malformed", "traversal"} {
		t.Run(failure, func(t *testing.T) {
			project := t.TempDir()
			root := filepath.Join(project, "registry")
			if err := os.CopyFS(root, os.DirFS("../../registry")); err != nil {
				t.Fatal(err)
			}
			common := filepath.Join(root, "common/types.proto")
			switch failure {
			case "missing":
				if err := os.Remove(common); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(common, []byte("message Broken {\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "traversal":
				data, err := os.ReadFile(common)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(project, "outside.proto"), data, 0600); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(root, "components/rest-api/slots/before_create.proto")
				data, err = os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				data = bytes.ReplaceAll(data, []byte("stego/common/types.proto"), []byte("../outside.proto"))
				if err := os.WriteFile(file, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			reg, err := registry.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			components := map[string]*types.Component{"rest-api": reg.Component("rest-api")}
			declaration := &types.ServiceDeclaration{Slots: []types.SlotDeclaration{{Slot: "before_create", Gate: []string{"policy"}}}}
			files, err := generateSlotFiles("slots", components, declaration, reg)
			if err == nil || len(files) != 0 || !strings.Contains(err.Error(), "import") {
				t.Fatal("invalid import produced slot files", err)
			}
		})
	}
}
