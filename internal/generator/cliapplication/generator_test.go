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
	if requirement.MinimumGoVersion() != "1.25" {
		t.Fatal("CLI file operations require Go 1.25")
	}
	project := t.TempDir()
	ctx := gen.Context{ModuleName: "example.com/cli-test", OutDirName: "out", OutputNamespace: "cli", ComponentConfig: map[string]any{"factory_package": "app"}}
	files, _, err := new(cliapplication.Generator).Generate(ctx)
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
	for name, data := range map[string][]byte{"go.mod": []byte("module example.com/cli-test\n\ngo " + requirement.MinimumGoVersion() + "\n"), "app/app.go": []byte(factory)} {
		if err := os.WriteFile(filepath.Join(project, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"runtime_test.go", "output_test.go"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "out/cli/command", name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("go", "test", "-race", "-count=1", "-mod=readonly", "-timeout=30s", "./...")
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
