package cliapplication_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/jsell-rh/stego/internal/buildidentity"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/cliapplication"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestGeneratedCLI(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) { testGeneratedCLI(t, enabled) })
	}
}
func testGeneratedCLI(t *testing.T, enabled bool) {
	var requirement gen.GoVersionRequirement = new(cliapplication.Generator)
	if requirement.MinimumGoVersion() != "1.25.0" {
		t.Fatal("CLI dependencies require Go 1.25.0")
	}
	project := t.TempDir()
	ctx := gen.Context{ModuleName: "example.com/cli-test", OutDirName: "out", OutputNamespace: "cli", ComponentConfig: map[string]any{"factory_package": "app"}}
	if enabled {
		ctx.PeerNamespaces = map[string]string{"otel-tracing": "tracing"}
	}
	files, wiring, err := new(cliapplication.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		telemetryFiles, telemetryWiring, err := new(oteltracing.Generator).Generate(gen.Context{OutputNamespace: "tracing", ServiceName: "records"})
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, telemetryFiles...)
		for name, version := range telemetryWiring.GoModRequires {
			wiring.GoModRequires[name] = version
		}
	}
	for _, file := range files {
		path := filepath.Join(project, "out", file.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(project, "app"), 0755); err != nil {
		t.Fatal(err)
	}
	factory := `package app
import command "example.com/cli-test/out/cli/command"
func Commands()command.Application{return command.Application{VersionCommand:true,ConfigEnv:"TEST_CLI_CONFIG",ConfigName:"sample",Commands:[]command.Command{{Name:[]string{"get","record"},Method:"GET",Path:"/records/{id}",ID:true,Success:[]int{200}}}}}
`
	version := requirement.MinimumGoVersion()
	if enabled {
		version = "1.26.0"
	}
	var module strings.Builder
	fmt.Fprintf(&module, "module example.com/cli-test\ngo %s\nrequire (\n", version)
	names := []string{}
	for name := range wiring.GoModRequires {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&module, "%s %s\n", name, wiring.GoModRequires[name])
	}
	module.WriteString(")\n")
	for name, data := range map[string][]byte{"go.mod": []byte(module.String()), "app/app.go": []byte(factory)} {
		if err := os.WriteFile(filepath.Join(project, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"runtime_test.go", "output_test.go", "oauth_test.go", "apply_test.go", "apply_immutable_test.go", "relative_time_test.go", "identity_test.go", "version_test.go"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "out/cli/command", name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = project
	tidy.Env = append(os.Environ(), "GOWORK=off")
	if output, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("CLI dependencies: %v\n%s", err, output)
	}
	command := exec.Command("go", "test", "-race", "-count=1", "-mod=readonly", "-timeout=60s", "./...")
	command.Dir = project
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated CLI: %v\n%s", err, output)
	}
	if os.Getenv("STEGO_BENCH_IMMUTABLE_APPLY") == "1" {
		bench := exec.Command("go", "test", "-run=^$", "-bench=^BenchmarkImmutableApplyComparison$", "-benchmem", "-benchtime=200ms", "-count=3", "./out/cli/command")
		bench.Dir = project
		bench.Env = append(os.Environ(), "GOWORK=off")
		output, err := bench.CombinedOutput()
		if err != nil {
			t.Fatalf("immutable apply benchmark: %v\n%s", err, output)
		}
		t.Logf("immutable apply benchmark:\n%s", output)
	}
	testVersionBuilds(t, project)
	if enabled {
		testTelemetryProcess(t, project)
	}
}
func TestInvalidCLIConfig(t *testing.T) {
	for _, factory := range []any{nil, 42, "", "../outside", "/absolute", "out", "out/app"} {
		_, _, err := new(cliapplication.Generator).Generate(gen.Context{ModuleName: "example.com/cli", OutDirName: "out", OutputNamespace: "cli", ComponentConfig: map[string]any{"factory_package": factory}})
		if err == nil {
			t.Fatal("invalid factory accepted")
		}
	}
}

// Build real executables so the test checks Go revision stamping, not test data.
func testVersionBuilds(t *testing.T, project string) {
	t.Helper()
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", args[0], err, out)
		}
		return out
	}
	run("git", "init", "-q")
	run("git", "add", ".")
	run("git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "Build fixture")
	revision := strings.TrimSpace(string(run("git", "rev-parse", "HEAD")))
	binary := filepath.Join(t.TempDir(), "cli")
	build := func(stamp, want string) {
		t.Helper()
		run("go", "build", "-trimpath", "-buildvcs="+stamp, "-mod=readonly", "-o", binary, "./out/cli/cmd")
		command := exec.Command(binary, "version")
		command.Dir = t.TempDir()
		command.Env = append(os.Environ(), "TEST_CLI_CONFIG=/missing/config.json")
		out, err := command.Output()
		if err != nil {
			t.Fatalf("version: %v %s", err, out)
		}
		var report struct {
			Application buildidentity.Record `json:"application"`
			Compiler    buildidentity.Record `json:"compiler"`
		}
		if err := json.Unmarshal(out, &report); err != nil {
			t.Fatal(err)
		}
		if report.Application.SourceState != want || report.Compiler != buildidentity.Current() {
			t.Fatal(report)
		}
		if want != "unknown" && report.Application.Revision != revision {
			t.Fatal(report)
		}
		if want == "unknown" && report.Application.Revision != "unknown" {
			t.Fatal(report)
		}
		repeat := exec.Command(binary, "version")
		repeat.Dir = command.Dir
		repeat.Env = command.Env
		again, err := repeat.Output()
		if err != nil || !bytes.Equal(out, again) {
			t.Fatal("unstable version output", err)
		}
	}
	build("true", "clean")
	path := filepath.Join(project, "app/app.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n// Changed source.\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	build("true", "modified")
	build("false", "unknown")
}
