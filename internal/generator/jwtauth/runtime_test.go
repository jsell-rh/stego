package jwtauth

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
)

func TestGeneratedAuthenticationRuntime(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedAuthenticationRuntime(t, telemetry) })
	}
}
func testGeneratedAuthenticationRuntime(t *testing.T, telemetry bool) {
	project := t.TempDir()
	ctx := gen.Context{OutputNamespace: "auth", ModuleName: "example.com/auth-test"}
	if telemetry {
		ctx.PeerNamespaces = map[string]string{"otel-tracing": "tracing"}
	}
	files, wiring, err := (&Generator{}).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !wiring.ConstructorReturnsError[0] {
		t.Fatal("authentication startup errors must be handled")
	}
	if err := os.Mkdir(filepath.Join(project, "auth"), 0755); err != nil {
		t.Fatal(err)
	}
	if telemetry {
		peerFiles, peerWiring, err := new(oteltracing.Generator).Generate(gen.Context{OutputNamespace: "tracing", ServiceName: "auth-test"})
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, peerFiles...)
		for name, version := range peerWiring.GoModRequires {
			wiring.GoModRequires[name] = version
		}
		tests, err := os.ReadFile("testdata/key_source_telemetry_test.go")
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, gen.File{Path: "auth/key_source_telemetry_test.go", Content: tests})
	}
	for _, file := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(project, file.Path)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, file.Path), file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	tests, err := os.ReadFile("testdata/auth_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "auth/auth_test.go"), tests, 0644); err != nil {
		t.Fatal(err)
	}
	grants, err := os.ReadFile("testdata/grants_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "auth/grants_test.go"), grants, 0644); err != nil {
		t.Fatal(err)
	}
	keySourceTests, err := os.ReadFile("testdata/key_source_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "auth/key_source_test.go"), keySourceTests, 0644); err != nil {
		t.Fatal(err)
	}
	module := "module example.com/auth-test\ngo 1.26.8\nrequire (\n"
	names := make([]string, 0, len(wiring.GoModRequires))
	for name := range wiring.GoModRequires {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		module += name + " " + wiring.GoModRequires[name] + "\n"
	}
	module += ")\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(module), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-mod=readonly", "./..."}} {
		command := exec.Command("go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("generated authentication checks: %v\n%s", err, output)
		}
	}
	if os.Getenv("STEGO_BENCH_AUTH") == "1" {
		command := exec.Command("go", "test", "-run", "^$", "-bench", "^Benchmark(Verify|GrantPolicy)$", "-benchtime=1s", "-benchmem", "./...")
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("authentication benchmark: %v\n%s", err, output)
		}
		t.Log(string(output))
	}
}

func TestAuthenticationHeaderMustBeValid(t *testing.T) {
	for _, header := range []string{"Bad Header", "Authorization\nInjected: yes", "Bad:Header", "Ünicode", "Bad\x00Header"} {
		_, _, err := (&Generator{}).Generate(gen.Context{ComponentConfig: map[string]any{"header": header}})
		if err == nil {
			t.Errorf("accepted invalid header %q", header)
		}
	}
}

func TestRolesClaimConfiguration(t *testing.T) {
	for _, value := range []any{42, nil, ".roles", "roles.", "realm..roles", "realm/roles"} {
		if _, _, err := new(Generator).Generate(gen.Context{ComponentConfig: map[string]any{"roles_claim": value}}); err == nil {
			t.Fatalf("invalid roles_claim was accepted: %v", value)
		}
	}
	for _, value := range []string{"", "roles", "realm_access.roles"} {
		if _, _, err := new(Generator).Generate(gen.Context{ComponentConfig: map[string]any{"roles_claim": value}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuthenticationTelemetryPeerValidation(t *testing.T) {
	for _, ctx := range []gen.Context{
		{OutputNamespace: "auth", PeerNamespaces: map[string]string{"otel-tracing": "tracing"}},
		{ModuleName: "example.com/test", OutputNamespace: "auth", PeerNamespaces: map[string]string{"otel-tracing": "../outside"}},
	} {
		if _, _, err := new(Generator).Generate(ctx); err == nil {
			t.Fatal("invalid telemetry peer accepted")
		}
	}
}
