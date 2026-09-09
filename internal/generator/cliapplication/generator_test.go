package cliapplication_test

import (
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/cliapplication"
	"os"
	"os/exec"
	"path/filepath"
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
func Commands()command.Application{return command.Application{ConfigEnv:"TEST_CLI_CONFIG",ConfigName:"sample",Commands:[]command.Command{{Name:[]string{"get","record"},Method:"GET",Path:"/records/{id}",ID:true,Success:[]int{200}}}}}
`
	for name, data := range map[string][]byte{"go.mod": []byte("module example.com/cli-test\n\ngo " + requirement.MinimumGoVersion() + "\n\nrequire github.com/coreos/go-oidc/v3 " + wiring.GoModRequires["github.com/coreos/go-oidc/v3"] + "\n\nrequire gopkg.in/yaml.v3 " + wiring.GoModRequires["gopkg.in/yaml.v3"] + "\n"), "app/app.go": []byte(factory)} {
		if err := os.WriteFile(filepath.Join(project, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"runtime_test.go", "output_test.go", "oauth_test.go", "apply_test.go"} {
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
}
func TestInvalidCLIConfig(t *testing.T) {
	for _, factory := range []any{nil, 42, "", "../outside", "/absolute", "out", "out/app"} {
		_, _, err := new(cliapplication.Generator).Generate(gen.Context{ModuleName: "example.com/cli", OutDirName: "out", OutputNamespace: "cli", ComponentConfig: map[string]any{"factory_package": factory}})
		if err == nil {
			t.Fatal("invalid factory accepted")
		}
	}
}
