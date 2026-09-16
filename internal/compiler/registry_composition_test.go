package compiler

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

func TestComponentNamespacesDoNotChangeCommonMetadata(t *testing.T) {
	original := &types.Component{Name: "sample", OutputNamespace: "internal/api"}
	active := map[string]*types.Component{"sample": original}
	if failures := applyComponentNamespaces(map[string]string{"sample": "application"}, active); len(failures) != 0 {
		t.Fatal(failures)
	}
	if original.OutputNamespace != "internal/api" || active["sample"].OutputNamespace != "application" || original == active["sample"] {
		t.Fatal("application namespace changed registry metadata")
	}
	for _, override := range []map[string]string{{"missing": "application"}, {"sample": ""}, {"sample": "../escape"}, {"sample": "contracts"}} {
		selected := map[string]*types.Component{"sample": original}
		failures := applyComponentNamespaces(override, selected)
		failures = append(failures, validateComponentNamespaces(selected)...)
		if len(failures) == 0 {
			t.Fatal("invalid namespace override accepted", override)
		}
	}
	service := types.ServiceDeclaration{Kind: "service", Name: "sample", Archetype: "application", Language: "go", ComponentNamespaces: map[string]string{"sample": "application"}}
	data, err := yaml.Marshal(service)
	if err != nil {
		t.Fatal(err)
	}
	var decoded types.ServiceDeclaration
	if err := yaml.Unmarshal(data, &decoded); err != nil || decoded.ComponentNamespaces["sample"] != "application" {
		t.Fatal("namespace declaration did not survive encoding", err)
	}
}

func TestComposedRegistryRejectsLocalChangeBeforeApply(t *testing.T) {
	input := snapshotTestInput(t)
	local := t.TempDir()
	// An application extension is captured even when this service does not use it.
	path := filepath.Join(local, "archetypes", "extension", "archetype.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("kind: archetype\nname: extension\nlanguage: go\nversion: 1.0.0\ncomponents: [stub-api]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	input.RegistryDirs = []string{input.RegistryDir, local}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed after plan"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan, input.ProjectDir, ""); err == nil {
		t.Fatal("changed local extension was applied")
	}
	if _, err := os.Stat(filepath.Join(input.ProjectDir, "out/internal/api/handler.go")); !os.IsNotExist(err) {
		t.Fatal("stale plan wrote application output")
	}
}

func TestComponentNamespaceDeclarationReachesGeneration(t *testing.T) {
	input := snapshotTestInput(t)
	service := filepath.Join(input.ProjectDir, "service.yaml")
	data, err := os.ReadFile(service)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\ncomponent_namespaces:\n  stub-api: application\n")...)
	if err := os.WriteFile(service, data, 0600); err != nil {
		t.Fatal(err)
	}
	input.Generators["stub-api"] = &stubGenerator{files: []gen.File{{Path: "application/handler.go", Content: []byte("package application\n")}}}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(input.ProjectDir, "out/application/handler.go")); err != nil {
		t.Fatal(err)
	}
}
