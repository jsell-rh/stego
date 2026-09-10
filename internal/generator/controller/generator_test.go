package controller

import (
	_ "embed"
	"fmt"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed testdata/runtime_test.go
var runtimeTests []byte

//go:embed testdata/keyed_test.go
var keyedTests []byte

//go:embed testdata/admission_test.go
var admissionTests []byte

//go:embed testdata/watch_keyed_test.go
var watchKeyedTests []byte

//go:embed testdata/sweep_test.go
var sweepTests []byte

//go:embed testdata/scan_test.go
var scanTests []byte

//go:embed testdata/stream_test.go
var streamTests []byte

//go:embed testdata/observation_test.go
var observationTests []byte

//go:embed testdata/metrics_test.go
var metricsTests []byte

//go:embed testdata/checkpoint_test.go
var checkpointTests []byte

//go:embed testdata/cycle_test.go
var cycleTests []byte

//go:embed testdata/telemetry_test.go
var telemetryTests []byte

func TestGeneratedController(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedController(t, telemetry) })
	}
}
func testGeneratedController(t *testing.T, telemetry bool) {
	ctx := gen.Context{OutputNamespace: "controller", ModuleName: "example.com/records", ServiceName: "records"}
	if telemetry {
		ctx.PeerNamespaces = map[string]string{"otel-tracing": "telemetry"}
	}
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, gen.File{Path: "controller/runtime_test.go", Content: runtimeTests}, gen.File{Path: "controller/keyed_test.go", Content: keyedTests}, gen.File{Path: "controller/sweep_test.go", Content: sweepTests})
	files = append(files, gen.File{Path: "controller/admission_test.go", Content: admissionTests})
	files = append(files, gen.File{Path: "controller/stream_test.go", Content: streamTests}, gen.File{Path: "controller/observation_test.go", Content: observationTests})
	files = append(files, gen.File{Path: "controller/scan_test.go", Content: scanTests}, gen.File{Path: "controller/watch_keyed_test.go", Content: watchKeyedTests})
	files = append(files, gen.File{Path: "controller/metrics_test.go", Content: metricsTests}, gen.File{Path: "controller/checkpoint_test.go", Content: checkpointTests})
	files = append(files, gen.File{Path: "controller/cycle_test.go", Content: cycleTests})
	var module strings.Builder
	module.WriteString("module example.com/records\n")
	if telemetry {
		module.WriteString("go 1.26.0\n")
	} else {
		module.WriteString("go 1.25.0\n")
	}
	if telemetry {
		ctx.OutputNamespace = "telemetry"
		generated, wiring, err := new(oteltracing.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		files = append(files, gen.File{Path: "controller/telemetry_test.go", Content: telemetryTests})
		var names []string
		for name := range wiring.GoModRequires {
			names = append(names, name)
		}
		sort.Strings(names)
		module.WriteString("require (\n")
		for _, name := range names {
			fmt.Fprintf(&module, "%s %s\n", name, wiring.GoModRequires[name])
		}
		module.WriteString(")\n")
	}
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
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(module.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if telemetry {
		cmd := exec.Command("go", "mod", "tidy")
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("resolve telemetry modules: %v %s", err, output)
		}
	}
	cmd := exec.Command("go", "test", "-race", "-count=1", "-timeout=45s", "./...")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated controller: %v\n%s", err, output)
	}
	if os.Getenv("STEGO_BENCH_CONTROLLER") == "1" {
		bench := exec.Command("go", "test", "-run=^$", "-bench=^Benchmark(KeyQueueWorkers|KeyAdmission|StreamScan|ControllerMetrics|KeyReconnect)$", "-benchtime=200ms", "-count=3", "./...")
		bench.Dir = project
		bench.Env = append(os.Environ(), "GOWORK=off")
		output, err := bench.CombinedOutput()
		if err != nil {
			t.Fatalf("generated controller benchmark: %v\n%s", err, output)
		}
		t.Logf("generated controller benchmark:\n%s", output)
	}
	if os.Getenv("STEGO_BENCH_OBSERVATION") == "1" {
		bench := exec.Command("go", "test", "-run=^$", "-bench=^BenchmarkObservation$", "-benchtime=200ms", "-count=3", "./...")
		bench.Dir = project
		bench.Env = append(os.Environ(), "GOWORK=off")
		output, err := bench.CombinedOutput()
		if err != nil {
			t.Fatalf("generated observation benchmark: %v\n%s", err, output)
		}
		t.Logf("generated observation benchmark:\n%s", output)
	}
}
func TestRejectInvalidGeneration(t *testing.T) {
	for _, ctx := range []gen.Context{{}, {OutputNamespace: "../escape"}, {OutputNamespace: "controller", ComponentConfig: map[string]any{"workers": 0}}, {OutputNamespace: "controller", PeerNamespaces: map[string]string{"otel-tracing": "../outside"}}, {OutputNamespace: "controller", PeerNamespaces: map[string]string{"otel-tracing": "tracing"}}} {
		if _, _, err := new(Generator).Generate(ctx); err == nil {
			t.Fatal("invalid generation accepted")
		}
	}
}

func TestNestedControllerLibraryCanBeImported(t *testing.T) {
	for _, namespace := range []string{"go-services/worker", "libs/init", "libs/string"} {
		t.Run(namespace, func(t *testing.T) {
			files, _, err := new(Generator).Generate(gen.Context{OutputNamespace: namespace})
			if err != nil {
				t.Fatal(err)
			}
			project := t.TempDir()
			files = append(files, gen.File{Path: "go.mod", Content: []byte("module example.com/controllernamespace\ngo 1.26.8\n")}, gen.File{Path: "main.go", Content: []byte("package main\nimport runtime \"example.com/controllernamespace/" + namespace + "\"\nfunc main(){_=runtime.ErrScanContract}\n")})
			for _, file := range files {
				name := filepath.Join(project, file.Path)
				if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, file.Content, 0644); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("go", "build", "-mod=readonly", "./...")
			command.Dir = project
			command.Env = append(os.Environ(), "GOWORK=off")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("nested library import failed: %v\n%s", err, output)
			}
		})
	}
}
