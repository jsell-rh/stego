package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

type sourceGenerator struct{ limit int64 }

func (g sourceGenerator) InputFiles(map[string]any) ([]gen.InputFile, error) {
	inputs := gen.SourceInputs([]string{"api/record.proto"})
	if g.limit != 0 {
		inputs[0].MaxBytes = g.limit
	}
	return inputs, nil
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
		if _, err := captureGeneratorInputs(root, "out", gen.SourceInputs(names), map[string]fileSnapshot{}); err == nil {
			t.Fatalf("unsafe inputs accepted: %v", names)
		}
	}
}

func TestGeneratorInputBudgets(t *testing.T) {
	root := t.TempDir()
	const size = (1 << 20) + 1
	writeFile(t, filepath.Join(root, "assets.zip"), strings.Repeat("a", size))
	files := []gen.InputFile{{Path: "assets.zip", MaxBytes: size}}
	got, err := captureGeneratorInputs(root, "out", files, map[string]fileSnapshot{})
	if err != nil || len(got["assets.zip"]) != size {
		t.Fatal("declared larger input failed", err)
	}
	if _, err := captureGeneratorInputs(root, "out", gen.SourceInputs([]string{"assets.zip"}), map[string]fileSnapshot{}); err == nil {
		t.Fatal("the ordinary source limit was increased")
	}
	files[0].MaxBytes--
	if _, err := captureGeneratorInputs(root, "out", files, map[string]fileSnapshot{}); err == nil {
		t.Fatal("declared input limit was ignored")
	}
	for _, limit := range []int64{-1, 0, gen.MaxInputBytes + 1} {
		_, err := captureGeneratorInputs(filepath.Join(root, "absent"), "out", []gen.InputFile{{Path: "input", MaxBytes: limit}}, map[string]fileSnapshot{})
		if err == nil || !strings.Contains(err.Error(), "invalid size limit") {
			t.Fatal("invalid limit reached filesystem access", limit, err)
		}
	}
	files = nil
	for _, name := range []string{"a", "b", "c"} {
		file, err := os.Create(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		err = file.Truncate(3 << 20)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
		files = append(files, gen.InputFile{Path: name, MaxBytes: gen.MaxInputBytes})
	}
	if _, err := captureGeneratorInputs(root, "out", files, map[string]fileSnapshot{}); err == nil || !strings.Contains(err.Error(), "exceed 8 MiB") {
		t.Fatal("larger per-file limits escaped the aggregate bound", err)
	}
}

func TestGeneratorLargeInputRetainsSnapshotCheck(t *testing.T) {
	input := snapshotTestInput(t)
	input.Generators["stub-api"] = sourceGenerator{limit: gen.MaxInputBytes}
	source := filepath.Join(input.ProjectDir, "api/record.proto")
	if err := os.Mkdir(filepath.Dir(source), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, source, strings.Repeat("a", (1<<20)+1))
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, source, strings.Repeat("b", (1<<20)+1))
	if err := Apply(plan, input.ProjectDir, ""); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatal("large changed input passed apply", err)
	}
}
