package cliapplication_test

import (
	"bytes"
	"encoding/json"
	"github.com/jsell-rh/stego/internal/buildidentity"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/cliapplication"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedCLI(t *testing.T) {
	var requirement gen.GoVersionRequirement = new(cliapplication.Generator)
	if requirement.MinimumGoVersion() != "1.25.0" {
		t.Fatal("CLI dependencies require Go 1.25.0")
	}
	project := t.TempDir()
	ctx := gen.Context{ModuleName: "example.com/cli-test", OutDirName: "out", OutputNamespace: "cli", ComponentConfig: map[string]any{"factory_package": "app"}}
	files, wiring, err := new(cliapplication.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
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
	for name, data := range map[string][]byte{"go.mod": []byte("module example.com/cli-test\n\ngo " + requirement.MinimumGoVersion() + "\n\nrequire github.com/coreos/go-oidc/v3 " + wiring.GoModRequires["github.com/coreos/go-oidc/v3"] + "\n\nrequire gopkg.in/yaml.v3 " + wiring.GoModRequires["gopkg.in/yaml.v3"] + "\n"), "app/app.go": []byte(factory)} {
		if err := os.WriteFile(filepath.Join(project, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"runtime_test.go", "output_test.go", "oauth_test.go", "apply_test.go", "relative_time_test.go", "identity_test.go", "version_test.go"} {
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
	testVersionBuilds(t, project)
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
		out, err := command.CombinedOutput()
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
