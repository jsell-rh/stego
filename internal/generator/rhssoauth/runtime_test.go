package rhssoauth

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
)

func TestGeneratedSSOTrustPolicy(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedSSOTrustPolicy(t, telemetry) })
	}
}
func testGeneratedSSOTrustPolicy(t *testing.T, telemetry bool) {
	project := t.TempDir()
	settings := gen.Context{OutputNamespace: "auth", ModuleName: "example.com/sso-test"}
	if telemetry {
		settings.PeerNamespaces = map[string]string{"otel-tracing": "tracing"}
	}
	files, wiring, err := new(Generator).Generate(settings)
	if err != nil {
		t.Fatal(err)
	}
	if telemetry {
		peerFiles, peerWiring, err := new(oteltracing.Generator).Generate(gen.Context{OutputNamespace: "tracing", ServiceName: "sso-test"})
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, peerFiles...)
		for name, version := range peerWiring.GoModRequires {
			wiring.GoModRequires[name] = version
		}
		tests, err := os.ReadFile("testdata/startup_telemetry_test.go")
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, gen.File{Path: "auth/startup_telemetry_test.go", Content: tests})
	}
	for _, file := range files {
		target := filepath.Join(project, file.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, file.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tests, err := os.ReadFile("testdata/trust_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "auth/trust_test.go"), tests, 0600); err != nil {
		t.Fatal(err)
	}
	module := "module example.com/sso-test\ngo 1.26.8\n"
	names := make([]string, 0, len(wiring.GoModRequires))
	for name := range wiring.GoModRequires {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		module += fmt.Sprintf("require %s %s\n", name, wiring.GoModRequires[name])
	}
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(module), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-p=1", "-race", "-count=1", "-mod=readonly", "-timeout=45s", "./..."}} {
		command := exec.CommandContext(ctx, "go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=1", "GOMEMLIMIT=384MiB")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("SSO runtime: %v\n%s", err, output)
		}
	}
}
