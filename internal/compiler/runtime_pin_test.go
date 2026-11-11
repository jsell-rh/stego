package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
)

// pinSum is the runtime pin's checksum in the registries of these tests: they generate and read module files and
// build nothing, so any go.sum hash serves.
const pinSum = "h1:2jc7mdW5JmAEJw0KYdJKSGW/vx5dqWB1LfkmGdo9GNk="

// pinBlock is the runtime block of an archetype document with the runtime library at version.
func pinBlock(version, sum string) string {
	return "runtime:\n  module: github.com/jsell-rh/stego/registry/runtime\n  version: " + version + "\n  sum: " + sum + "\n"
}

// runtimeImporter is a component's generated file that imports the package path given.
func runtimeImporter(path string) gen.File {
	return gen.File{Path: "internal/hello/hello.go", Content: []byte("package hello\n\nimport (\n\t\"context\"\n\t\"fmt\"\n\n\t\"" + path + "\"\n)\n\nvar _ = money.New\n\n// Task prints one allocation.\ntype Task struct{}\n\n// NewTask makes the task.\nfunc NewTask() *Task { return &Task{} }\n\n// Run prints and waits.\nfunc (*Task) Run(ctx context.Context) error {\n\tfmt.Println(\"run\")\n\t<-ctx.Done()\n\treturn nil\n}\n")}
}

// pinProject writes a project of the service svc on the archetype svc-arch, whose one component hello emits the
// file the generator gives, and a registry with svc-arch at version with the runtime block given. It gives the
// project folder and the registry folder.
func pinProject(t *testing.T, version, runtime string) (string, string) {
	t.Helper()
	project, registryDir := t.TempDir(), t.TempDir()
	files := map[string]string{
		"archetypes/svc-arch/archetype.yaml": "kind: archetype\nname: svc-arch\nlanguage: go\nversion: " + version + "\n" + runtime + "components: [hello]\nconventions:\n  layout: flat\n",
		"components/hello/component.yaml":    "kind: component\nname: hello\nversion: 1.0.0\nstage: ga\noutput_namespace: internal/hello\nrequires: []\nprovides: []\nslots: []\n",
	}
	for name, data := range files {
		target := filepath.Join(registryDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "service.yaml"), []byte("kind: service\nname: svc\narchetype: svc-arch\nlanguage: go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return project, registryDir
}

// pinInput is the reconciler input of a pin project whose component hello emits file.
func pinInput(project, registryDir string, file gen.File) ReconcilerInput {
	return ReconcilerInput{
		ProjectDir: project, RegistryDir: registryDir, GoVersion: "1.26.11", ModuleName: "example.com/svc",
		Generators: map[string]gen.Generator{"hello": &stubGenerator{files: []gen.File{file}, wiring: &gen.Wiring{
			Imports: []string{"internal/hello"}, Constructors: []string{"hello.NewTask()"}, BackgroundTasks: []int{0},
		}}},
	}
}

// applyPin plans and applies the input, and fails the test on an error.
func applyPin(t *testing.T, input ReconcilerInput) {
	t.Helper()
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
}

// checkModulePin reads go.mod and go.sum of the project and fails the test unless go.mod requires the runtime
// library at exactly version, with no other STEGO module and no replace directive, and go.sum holds sum.
func checkModulePin(t *testing.T, project, version, sum string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(project, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, require := range file.Require {
		if registry.IsStegoModule(require.Mod.Path) {
			if require.Mod.Path != registry.RuntimeModule || require.Mod.Version != version {
				t.Fatalf("go.mod requires %s %s, want only %s %s:\n%s", require.Mod.Path, require.Mod.Version, registry.RuntimeModule, version, data)
			}
			found++
		}
	}
	if found != 1 || len(file.Replace) != 0 {
		t.Fatalf("go.mod has %d runtime requirements and %d replace directives, want 1 and 0:\n%s", found, len(file.Replace), data)
	}
	sums, err := os.ReadFile(filepath.Join(project, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sums), registry.RuntimeModule+" "+version+" "+sum+"\n") {
		t.Fatalf("go.sum lacks the pin %s %s:\n%s", version, sum, sums)
	}
}

func TestArchetypeRuntimePin(t *testing.T) {
	t.Run("ST-SUP-INV10-T Every service of an archetype imports the one pinned runtime library and no other STEGO module", func(t *testing.T) {
		for _, tc := range []struct{ name, runtime, part string }{
			{"no pin", "", "pins no runtime library"},
			{"no module path", "runtime:\n  version: v0.1.0\n  sum: " + pinSum + "\n", "runtime.module"},
			{"no version", "runtime:\n  module: github.com/jsell-rh/stego/registry/runtime\n  sum: " + pinSum + "\n", "runtime.version"},
			{"no module checksum", "runtime:\n  module: github.com/jsell-rh/stego/registry/runtime\n  version: v0.1.0\n", "runtime.sum"},
			{"two pins", pinBlock("v0.1.0", pinSum) + pinBlock("v0.2.0", pinSum), `duplicate key "runtime"`},
		} {
			_, registryDir := pinProject(t, "1.0.0", tc.runtime)
			if _, err := registry.Load(registryDir); err == nil || !strings.Contains(err.Error(), "svc-arch") || !strings.Contains(err.Error(), tc.part) {
				t.Fatalf("the load with %s was not refused naming svc-arch and %s: %v", tc.name, tc.part, err)
			}
		}

		project, registryDir := pinProject(t, "1.0.0", pinBlock("v0.1.0", pinSum))
		applyPin(t, pinInput(project, registryDir, runtimeImporter("github.com/jsell-rh/stego/registry/runtime/money")))
		checkModulePin(t, project, "v0.1.0", pinSum)

		for path, part := range map[string]string{
			"github.com/jsell-rh/stego/registry/runtime/v2/money": "the runtime library at another version",
			"github.com/jsell-rh/stego/other/money":               "a STEGO module other than the runtime library",
		} {
			other, otherRegistry := pinProject(t, "1.0.0", pinBlock("v0.1.0", pinSum))
			_, err := Reconcile(pinInput(other, otherRegistry, runtimeImporter(path)))
			if err == nil || !strings.Contains(err.Error(), "component hello") || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), part) {
				t.Fatalf("the component that imports %s was not refused naming the import: %v", path, err)
			}
		}

		moved := strings.Replace(pinSum, "2jc7", "3kd8", 1)
		archetype := filepath.Join(registryDir, "archetypes", "svc-arch", "archetype.yaml")
		data, err := os.ReadFile(archetype)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archetype, []byte(strings.Replace(string(data), pinSum, moved, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = Reconcile(pinInput(project, registryDir, runtimeImporter("github.com/jsell-rh/stego/registry/runtime/money")))
		if err == nil || !strings.Contains(err.Error(), "svc-arch 1.0.0") || !strings.Contains(err.Error(), "must change") {
			t.Fatalf("the pin that moved within version 1.0.0 was not refused naming svc-arch and its version: %v", err)
		}
		if err := os.WriteFile(archetype, []byte(strings.Replace(strings.Replace(string(data), pinSum, moved, 1), "version: 1.0.0", "version: 1.1.0", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		applyPin(t, pinInput(project, registryDir, runtimeImporter("github.com/jsell-rh/stego/registry/runtime/money")))
		checkModulePin(t, project, "v0.1.0", moved)
	})

	t.Run("ST-GEN-INV13-T The generated module file requires the runtime library at the archetype's pin only, and a hand edit of it is drift", func(t *testing.T) {
		for _, archetype := range []struct{ version, pin, sum string }{
			{"1.0.0", "v0.1.0", pinSum},
			{"2.0.0", "v0.3.2", strings.Replace(pinSum, "2jc7", "9zz9", 1)},
		} {
			project, registryDir := pinProject(t, archetype.version, pinBlock(archetype.pin, archetype.sum))
			applyPin(t, pinInput(project, registryDir, runtimeImporter("github.com/jsell-rh/stego/registry/runtime/money")))
			checkModulePin(t, project, archetype.pin, archetype.sum)
			drift, err := DetectDrift(project, "")
			if err != nil || drift.HasDrift() {
				t.Fatalf("the applied project drifts: %v %+v", err, drift)
			}
			name := filepath.Join(project, "go.mod")
			applied, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			for edit, text := range map[string]string{
				"a pseudo-version":       strings.Replace(string(applied), registry.RuntimeModule+" "+archetype.pin, registry.RuntimeModule+" v0.0.0-20260101000000-abcdefabcdef", 1),
				"a replace":              string(applied) + "\nreplace github.com/jsell-rh/stego/registry/runtime " + archetype.pin + " => ../runtime\n",
				"another tagged version": strings.Replace(string(applied), registry.RuntimeModule+" "+archetype.pin, registry.RuntimeModule+" v1.9.9", 1),
				"a second module":        string(applied) + "\nrequire github.com/jsell-rh/stego/extra " + archetype.pin + "\n",
			} {
				if text == string(applied) {
					t.Fatalf("the edit %s changed nothing", edit)
				}
				if err := os.WriteFile(name, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
				drift, err := DetectDrift(project, "")
				if err != nil {
					t.Fatal(err)
				}
				named := false
				for _, file := range drift.Modified {
					named = named || file.Path == "go.mod"
				}
				if !named {
					t.Fatalf("drift does not name go.mod after %s: %+v", edit, drift)
				}
				if err := os.WriteFile(name, applied, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
}
