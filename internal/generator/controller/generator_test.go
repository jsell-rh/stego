package controller

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed testdata/runtime_test.go
var runtimeTests []byte

//go:embed testdata/keyed_test.go
var keyedTests []byte

//go:embed testdata/watch_keyed_test.go
var watchKeyedTests []byte

//go:embed testdata/sweep_test.go
var sweepTests []byte

//go:embed testdata/scan_test.go
var scanTests []byte

func TestGeneratedController(t *testing.T) {
	files, _, err := new(Generator).Generate(gen.Context{OutputNamespace: "controller"})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, gen.File{Path: "controller/runtime_test.go", Content: runtimeTests}, gen.File{Path: "controller/keyed_test.go", Content: keyedTests}, gen.File{Path: "controller/sweep_test.go", Content: sweepTests})
	files = append(files, gen.File{Path: "controller/scan_test.go", Content: scanTests}, gen.File{Path: "controller/watch_keyed_test.go", Content: watchKeyedTests})
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module example.com/records\ngo 1.25.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-race", "-count=1", "-timeout=30s", "./...")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated controller: %v\n%s", err, output)
	}
	if os.Getenv("STEGO_BENCH_CONTROLLER") == "1" {
		bench := exec.Command("go", "test", "-run=^$", "-bench=^BenchmarkKeyQueueWorkers$", "-benchtime=200ms", "-count=3", "./...")
		bench.Dir = project
		bench.Env = append(os.Environ(), "GOWORK=off")
		output, err := bench.CombinedOutput()
		if err != nil {
			t.Fatalf("generated controller benchmark: %v\n%s", err, output)
		}
		t.Logf("generated controller benchmark:\n%s", output)
	}
}
func TestRejectInvalidGeneration(t *testing.T) {
	for _, ctx := range []gen.Context{{}, {OutputNamespace: "../escape"}, {OutputNamespace: "controller", ComponentConfig: map[string]any{"workers": 0}}} {
		if _, _, err := new(Generator).Generate(ctx); err == nil {
			t.Fatal("invalid generation accepted")
		}
	}
}
