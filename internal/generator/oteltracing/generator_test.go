package oteltracing_test

import (
	_ "embed"
	"fmt"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

//go:embed testdata/runtime_test.go
var runtimeTests []byte

//go:embed testdata/signals_test.go
var signalTests []byte

func TestGeneratedTracing(t *testing.T) {
	files, wiring, err := new(oteltracing.Generator).Generate(gen.Context{OutputNamespace: "tracing", ServiceName: "records"})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, gen.File{Path: "tracing/runtime_test.go", Content: runtimeTests}, gen.File{Path: "tracing/signals_test.go", Content: signalTests})
	var module strings.Builder
	module.WriteString("module example.com/records\ngo 1.26.0\nrequire (\n")
	var names []string
	for name := range wiring.GoModRequires {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&module, "%s %s\n", name, wiring.GoModRequires[name])
	}
	module.WriteString(")\n")
	files = append(files, gen.File{Path: "go.mod", Content: []byte(module.String())})
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Content, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy", "-go=1.26.0"}, {"vet", "./..."}, {"test", "-race", "-count=1", "-timeout=45s", "./..."}} {
		command := exec.Command("go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("generated tracing %v: %v\n%s", args, err, output)
		}
	}
	if os.Getenv("STEGO_BENCH_TRACING") == "1" {
		command := exec.Command("go", "test", "-run=^$", "-bench=^Benchmark(HTTPTracing|RequestSignals)$", "-benchtime=200ms", "-count=3", "./...")
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("tracing benchmark: %v %s", err, output)
		}
		t.Logf("%s", output)
	}

}
func TestInvalidTracingInput(t *testing.T) {
	for _, ctx := range []gen.Context{{}, {OutputNamespace: "../escape", ServiceName: "records"}, {OutputNamespace: "tracing", ServiceName: "records", ComponentConfig: map[string]any{"enabled": true}}} {
		g := new(oteltracing.Generator)
		if g.ValidateContext(ctx) == nil {
			t.Fatal("invalid input accepted")
		}
		if _, _, err := g.Generate(ctx); err == nil {
			t.Fatal("invalid input rendered")
		}
	}
}
