package oteltracing_test

import (
	_ "embed"
	"fmt"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpclient"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

//go:embed testdata/http_diagnostics_test.go
var httpDiagnosticTests []byte

//go:embed testdata/runtime_test.go
var runtimeTests []byte

//go:embed testdata/signals_test.go
var signalTests []byte

//go:embed testdata/service_test.go
var serviceTests []byte

//go:embed testdata/controller_test.go
var controllerTests []byte

//go:embed testdata/identity_test.go
var identityTests []byte

//go:embed testdata/client_test.go
var clientTests []byte

//go:embed testdata/http_client_test.go
var httpClientTests []byte

//go:embed testdata/http_client_transport_test.go
var httpClientTransportTests []byte

func TestGeneratedTracing(t *testing.T) {
	files, wiring, err := new(oteltracing.Generator).Generate(gen.Context{OutputNamespace: "tracing", ServiceName: "records"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := httpclient.Render("web", "example.com/records/tracing")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, client, gen.File{Path: "tracing/http_client_test.go", Content: httpClientTests}, gen.File{Path: "tracing/http_client_transport_test.go", Content: httpClientTransportTests})
	files = append(files, gen.File{Path: "tracing/client_test.go", Content: clientTests}, gen.File{Path: "tracing/http_diagnostics_test.go", Content: httpDiagnosticTests}, gen.File{Path: "tracing/runtime_test.go", Content: runtimeTests}, gen.File{Path: "tracing/signals_test.go", Content: signalTests}, gen.File{Path: "tracing/service_test.go", Content: serviceTests}, gen.File{Path: "tracing/controller_test.go", Content: controllerTests}, gen.File{Path: "tracing/identity_test.go", Content: identityTests})
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
	for _, args := range [][]string{{"mod", "tidy", "-go=1.26.0"}, {"vet", "./..."}, {"test", "-race", "-count=1", "-timeout=60s", "./..."}} {
		command := exec.Command("go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("generated tracing %v: %v\n%s", args, err, output)
		}
	}
	if os.Getenv("STEGO_BENCH_CLIENT") == "1" {
		command := exec.Command("go", "test", "-run=^$", "-bench=^Benchmark(ClientSignals|HTTPClientSignals|HTTPTracing)$", "-benchtime=200ms", "-count=3", "./...")
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("client benchmark: %v %s", err, output)
		}
		t.Logf("%s", output)
	}
	if os.Getenv("STEGO_BENCH_TRACING") == "1" {
		command := exec.Command("go", "test", "-run=^$", "-bench=^Benchmark(HTTPTracing|RequestSignals|ServiceLogs|ControllerSignals)$", "-benchtime=200ms", "-count=3", "./...")
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
