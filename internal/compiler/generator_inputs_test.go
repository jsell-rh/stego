package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

type sourceGenerator struct{}

func (sourceGenerator) InputFiles(map[string]any) ([]string, error) {
	return []string{"api/record.proto"}, nil
}
func (sourceGenerator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	return []gen.File{{Path: "internal/api/contract.txt", Content: ctx.Inputs["api/record.proto"]}}, nil, nil
}

func TestGeneratorSourceSnapshotAndRegeneration(t *testing.T) {
	input := snapshotTestInput(t)
	input.Generators["stub-api"] = sourceGenerator{}
	source := filepath.Join(input.ProjectDir, "api/record.proto")
	if err := os.Mkdir(filepath.Dir(source), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, source, "first")
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, source, "second")
	if err := Apply(plan, input.ProjectDir, ""); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed input passed apply: %v", err)
	}
	plan, err = Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source)
	if err != nil || string(data) != "second" {
		t.Fatalf("apply changed source: %s %v", data, err)
	}
	generated := filepath.Join(input.ProjectDir, "out/internal/api/contract.txt")
	data, err = os.ReadFile(generated)
	if err != nil || !strings.Contains(string(data), "second") {
		t.Fatalf("wrong generated input: %s %v", data, err)
	}
	writeFile(t, source, "third")
	plan, err = Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(generated)
	if err != nil || !strings.Contains(string(data), "third") {
		t.Fatalf("source change did not regenerate: %s %v", data, err)
	}
}

func TestGeneratorInputsRejectUnsafeFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "source.proto"), "source")
	if err := os.Symlink(filepath.Join(root, "source.proto"), filepath.Join(root, "linked.proto")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "large.proto"), strings.Repeat("x", (1<<20)+1))
	for _, names := range [][]string{{"../outside.proto"}, {"/absolute.proto"}, {"out/file.proto"}, {".stego/state.yaml"}, {"linked.proto"}, {"absent.proto"}, {"large.proto"}, {"source.proto", "source.proto"}} {
		if _, err := captureGeneratorInputs(root, "out", names, map[string]fileSnapshot{}); err == nil {
			t.Fatalf("unsafe inputs accepted: %v", names)
		}
	}
}
