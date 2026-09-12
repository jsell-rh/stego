package kubernetesclient

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
)

//go:embed testdata/client_test.go
var runtimeTests []byte

//go:embed testdata/rotation_test.go
var rotationTests []byte

//go:embed testdata/rotation_telemetry_test.go
var rotationTelemetryTests []byte

func TestGeneratedKubernetesClient(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedKubernetesClient(t, telemetry) })
	}
}
func testGeneratedKubernetesClient(t *testing.T, telemetry bool) {
	ctx := gen.Context{ModuleName: "example.com/widget", OutDirName: "out", OutputNamespace: "kubernetes", PeerNamespaces: map[string]string{"http-application": "application", "jwt-auth": "auth"}, StorageContract: "example.com/widget/out/contracts/storage", AuthPackage: "example.com/widget/out/auth", ComponentConfig: map[string]any{"factory_package": "sample"}}
	if telemetry {
		ctx.PeerNamespaces["otel-tracing"] = "tracing"
	}
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx.OutputNamespace = "application"
	httpFiles, _, err := new(httpapplication.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range httpFiles {
		if file.Path == "application/client/client.go" {
			files = append(files, file)
		}
	}
	files = append(files, gen.File{Path: "kubernetes/client_test.go", Content: runtimeTests})
	files = append(files, gen.File{Path: "kubernetes/rotation_test.go", Content: rotationTests})
	var module strings.Builder
	module.WriteString("module example.com/widget\ngo 1.26.8\n")
	if telemetry {
		ctx.OutputNamespace = "tracing"
		ctx.ServiceName = "widget"
		ctx.ComponentConfig = nil
		generated, wiring, err := new(oteltracing.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		files = append(files, gen.File{Path: "kubernetes/rotation_telemetry_test.go", Content: rotationTelemetryTests})
		names := make([]string, 0, len(wiring.GoModRequires))
		for name := range wiring.GoModRequires {
			names = append(names, name)
		}
		sort.Strings(names)
		module.WriteString("require (\n")
		for _, name := range names {
			fmt.Fprintf(&module, "%s %s\n", name, wiring.GoModRequires[name])
		}
		module.WriteString(")\n")
	} else {
		files = append(files, gen.File{Path: "kubernetes/rotation_telemetry_test.go", Content: []byte("package kubernetes\nimport (\"context\";\"testing\")\nconst rotationTelemetryEnabled=false\nfunc rotationTelemetry(t *testing.T,ctx context.Context)(context.Context,func()){return ctx,func(){}}\n")})
	}
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, "out", file.Path)
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
			t.Fatalf("telemetry dependencies: %v\n%s", err, output)
		}
	}
	budget := "30s"
	if telemetry && os.Getenv("STEGO_KUBERNETES_LIVE_ROTATION") == "1" {
		budget = "15m"
	}
	cmd := exec.Command("go", "test", "-v", "-race", "-count=1", "-timeout="+budget, "./...")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if !telemetry {
		cmd.Env = append(cmd.Env, "STEGO_KUBERNETES_LIVE_ROTATION=0")
	}
	output, runErr := cmd.CombinedOutput()
	if dir := os.Getenv("STEGO_KUBERNETES_ROTATION_ARTIFACTS"); dir != "" {
		if !telemetry {
			dir = filepath.Join(dir, "without-telemetry")
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "runtime.log"), output, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runErr; err != nil {
		t.Fatalf("generated Kubernetes client: %v\n%s", err, output)
	}
}
func TestMissingHTTPClientIsRejected(t *testing.T) {
	if _, _, err := new(Generator).Generate(gen.Context{ModuleName: "example.com/widget", OutputNamespace: "kubernetes"}); err == nil {
		t.Fatal("missing transport accepted")
	}
}
