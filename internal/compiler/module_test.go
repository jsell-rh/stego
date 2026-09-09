package compiler

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"golang.org/x/mod/modfile"
)

func TestModuleRequirementsUseHighestVersion(t *testing.T) {
	for _, versions := range [][]string{{"v1.12.0", "v1.9.0"}, {"v1.9.0", "v1.12.0"}} {
		var wirings []ComponentWiring
		for _, version := range versions {
			wirings = append(wirings, ComponentWiring{Name: version, Wiring: &gen.Wiring{
				GoModRequires: map[string]string{"example.com/dependency": version},
			}})
		}
		result, err := moduleRequirements(wirings)
		if err != nil || result["example.com/dependency"] != "v1.12.0" {
			t.Fatalf("requirements = %v, error = %v", result, err)
		}
	}
}

func TestModuleRequirementsRejectInvalidInput(t *testing.T) {
	for _, requirement := range []struct{ path, version string }{
		{"example.com/mod", "latest"},
		{"example.com/mod", "v1.2"},
		{"example.com/mod", "v2.0.0"},
		{"example.com/mod\nreplace x => y", "v1.0.0"},
		{"example.com/mod", "v1.0.0\nreplace x => y"},
	} {
		_, err := Assemble(AssemblerInput{
			ModuleName: "example.com/service", GoVersion: "1.26.8",
			Wirings: []ComponentWiring{{Name: "invalid", Wiring: &gen.Wiring{
				GoModRequires: map[string]string{requirement.path: requirement.version},
			}}},
		})
		if err == nil {
			t.Errorf("accepted requirement %+v", requirement)
		}
	}
}

func TestModuleMergePreservesApplicationChoices(t *testing.T) {
	project := t.TempDir()
	original := []byte(`module example.com/service

go 1.26.8

toolchain go1.26.8

// Application dependencies.
require (
	example.com/shared v1.12.0
	example.com/business v1.0.0
)

replace example.com/business => ./business
exclude example.com/shared v1.8.0
`)
	if err := os.WriteFile(filepath.Join(project, "go.mod"), original, 0644); err != nil {
		t.Fatal(err)
	}
	request := gen.File{Path: "go.mod", Content: []byte("module example.com/service\ngo 1.22\nrequire example.com/shared v1.9.0\n")}
	result, err := mergeProjectModule(project, request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Bytes(), original) {
		t.Fatalf("module changed with no unmet requirement:\n%s", result.Bytes())
	}
	request.Content = []byte("module example.com/service\ngo 1.26.8\nrequire (\nexample.com/shared v1.13.0\nexample.com/new v0.1.0\n)\n")
	result, err = mergeProjectModule(project, request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := modfile.Parse("go.mod", result.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	for _, req := range parsed.Require {
		versions[req.Mod.Path] = req.Mod.Version
	}
	if versions["example.com/shared"] != "v1.13.0" || versions["example.com/business"] != "v1.0.0" || versions["example.com/new"] != "v0.1.0" {
		t.Fatalf("lost or incorrect dependency: %v", versions)
	}
	if len(parsed.Replace) != 1 || len(parsed.Exclude) != 1 || parsed.Toolchain.Name != "go1.26.8" || !strings.Contains(string(result.Bytes()), "// Application dependencies.") {
		t.Fatalf("application settings were lost:\n%s", result.Bytes())
	}
	onDisk, err := os.ReadFile(filepath.Join(project, "go.mod"))
	if err != nil || !bytes.Equal(onDisk, original) {
		t.Fatalf("planning changed the module: %v", err)
	}
}

func TestModuleMergeRejectsInvalidProject(t *testing.T) {
	for _, data := range []string{
		"module example.com/other\ngo 1.26.8\n",
		"go 1.26.8\n",
		"module example.com/service\ninvalid directive\n",
		"module example.com/service\nrequire (\nexample.com/dep v1.0.0\nexample.com/dep v1.1.0\n)\n",
	} {
		project := t.TempDir()
		if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := mergeProjectModule(project, gen.File{Path: "go.mod", Content: []byte("module example.com/service\ngo 1.26.8\n")})
		if err == nil {
			t.Errorf("accepted invalid project module %q", data)
		}
	}
}

func TestProjectModuleSettings(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module example.com/actual\ngo 1.26.8\n"), 0644); err != nil {
		t.Fatal(err)
	}
	name, version, err := ProjectModuleSettings(project, "", "")
	if err != nil || name != "example.com/actual" || version != "1.26.8" {
		t.Fatalf("settings = %q, %q, %v", name, version, err)
	}
	if _, _, err := ProjectModuleSettings(project, "example.com/wrong", ""); err == nil {
		t.Fatal("accepted a conflicting module setting")
	}
}

func TestReconcileRetainsApplicationModuleAcrossApplies(t *testing.T) {
	project, registry := setupTestProject(t)
	input := ReconcilerInput{ProjectDir: project, RegistryDir: registry, ModuleName: "example.com/service", GoVersion: "1.26.8",
		Generators: map[string]gen.Generator{"stub-api": &stubGenerator{}, "stub-store": &stubGenerator{}},
	}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan, project, ""); err != nil {
		t.Fatal(err)
	}
	module := []byte("module example.com/service\ngo 1.26.8\nrequire example.com/domain v1.0.0\n// Maintained by the application.\n")
	if err := os.WriteFile(filepath.Join(project, "go.mod"), module, 0644); err != nil {
		t.Fatal(err)
	}
	plan, err = Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.HasChanges() {
		t.Fatalf("application dependency edit caused output changes: %s", FormatPlan(plan))
	}
	if err := Apply(plan, project, ""); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(project, "go.mod"))
	if err != nil || !bytes.Equal(actual, module) {
		t.Fatalf("apply replaced application dependencies: %v\n%s", err, actual)
	}
	if _, tracked := plan.NewState.LastApplied.Files["go.mod"]; tracked {
		t.Fatal("go.mod is still tracked as generated output")
	}
}

func TestRuntimeDependenciesRequirePatchedUnicodeNormalization(t *testing.T) {
	for _, dependency := range []struct{ path, version string }{
		{"github.com/jackc/pgx/v5", "v5.11.0"}, {"gorm.io/gorm", "v1.25.12"},
		{"google.golang.org/grpc", "v1.82.1"}, {"golang.org/x/text", "v0.36.0"},
	} {
		for _, newer := range []bool{false, true} {
			requirements := map[string]string{dependency.path: dependency.version}
			want := "v0.40.0"
			if newer {
				requirements["golang.org/x/text"] = "v0.41.0"
				want = "v0.41.0"
			}
			result, err := moduleRequirements([]ComponentWiring{{Name: "runtime", Wiring: &gen.Wiring{GoModRequires: requirements}}})
			if err != nil || result["golang.org/x/text"] != want {
				t.Fatalf("runtime minimum for %s: %v %v", dependency.path, result, err)
			}
		}
	}
	result, err := moduleRequirements([]ComponentWiring{{Name: "independent", Wiring: &gen.Wiring{GoModRequires: map[string]string{"example.com/module": "v1.0.0"}}}})
	if err != nil || result["golang.org/x/text"] != "" {
		t.Fatal("unrelated service acquired a runtime dependency", result, err)
	}
}
