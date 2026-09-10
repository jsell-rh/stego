package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandsRejectInvalidConstraintsBeforeWriting(t *testing.T) {
	registry, err := filepath.Abs("../../registry")
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	t.Chdir(project)
	t.Setenv("STEGO_REGISTRY", registry)
	files := map[string]string{
		"service.yaml": `kind: service
name: invalid
archetype: rest-crud
language: go
entities:
  - name: Todo
    fields:
      - {name: title, type: string, min_length: 10, max_length: 1}
collections:
  todos: {entity: Todo, operations: [create]}
`,
		"out/main.go":       "existing output\n",
		".stego/state.yaml": "last_applied: null\n",
	}
	for path, data := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []struct {
		name string
		run  func([]string) error
	}{{"validate", runValidate}, {"plan", runPlan}, {"apply", runApply}} {
		t.Run(command.name, func(t *testing.T) {
			err := command.run(nil)
			if err == nil || !strings.Contains(err.Error(), "validation failed") {
				t.Fatalf("expected validation failure, got %v", err)
			}
			for path, expected := range files {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != expected {
					t.Fatalf("command changed %s: data=%q err=%v", path, data, err)
				}
			}
			if _, err := os.Stat("go.mod"); !os.IsNotExist(err) {
				t.Fatalf("command wrote go.mod: %v", err)
			}
		})
	}
}

func TestCommandsRejectInvalidApplicationFactories(t *testing.T) {
	builtin, err := filepath.Abs("../../registry")
	if err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{"cli-application", "http-application", "grpc-application"} {
		t.Run(component, func(t *testing.T) {
			registry := filepath.Join(t.TempDir(), "registry")
			if err := os.CopyFS(registry, os.DirFS(builtin)); err != nil {
				t.Fatal(err)
			}
			arch := filepath.Join(registry, "archetypes/application")
			if err := os.MkdirAll(arch, 0755); err != nil {
				t.Fatal(err)
			}
			declaration := "kind: archetype\nname: application\nlanguage: go\nversion: 1.0.0\ncomponents: [" + component + ", postgres-adapter]\ndefault_auth: jwt-auth\nbindings:\n  storage-adapter: postgres-adapter\n  auth-provider: jwt-auth\n"
			if err := os.WriteFile(filepath.Join(arch, "archetype.yaml"), []byte(declaration), 0600); err != nil {
				t.Fatal(err)
			}
			project := t.TempDir()
			t.Chdir(project)
			t.Setenv("STEGO_REGISTRY", registry)
			t.Setenv("STEGO_MODULE", "example.com/preflight")
			t.Setenv("STEGO_GO_VERSION", "1.26.8")
			service := "kind: service\nname: preflight\narchetype: application\nlanguage: go\noverrides:\n  " + component + ":\n    factory_package: out/domain\n"
			if component == "grpc-application" {
				service += "    proto_files: [{path: api.proto, import_path: api.proto}]\n"
			}
			files := map[string]string{"service.yaml": service, "api.proto": "syntax=\"proto3\";package example;message Record{}", "out/retained.txt": "retained output", ".stego/state.yaml": "last_applied: null\n", "go.mod": "module example.com/preflight\ngo 1.26.8\n", "go.sum": ""}
			for name, data := range files {
				if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, command := range []struct {
				name string
				run  func([]string) error
			}{{"validate", runValidate}, {"plan", runPlan}, {"apply", runApply}} {
				if err := command.run(nil); err == nil || !strings.Contains(err.Error(), "validation failed") {
					t.Fatal(command.name, "accepted invalid factory", err)
				}
				for name, want := range files {
					got, err := os.ReadFile(name)
					if err != nil || string(got) != want {
						t.Fatal(command.name, "changed", name, err)
					}
				}
			}
		})
	}
}

func TestCommandsRejectInvalidGoPackageNamespaces(t *testing.T) {
	for _, namespace := range []string{"bad-name", "workers/type", "workers/_", "workers/main", "workers/9worker", "workers/café", "workers/bad name"} {
		t.Run(namespace, func(t *testing.T) {
			registry := t.TempDir()
			files := map[string]string{
				"archetypes/application/archetype.yaml": "kind: archetype\nname: application\nlanguage: go\nversion: 1.0.0\ncomponents: [controller]\n",
				"components/controller/component.yaml":  "kind: component\nname: controller\nversion: 1.0.0\noutput_namespace: " + namespace + "\n",
			}
			for name, data := range files {
				name = filepath.Join(registry, name)
				if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			project := t.TempDir()
			t.Chdir(project)
			t.Setenv("STEGO_REGISTRY", registry)
			t.Setenv("STEGO_MODULE", "example.com/namespace")
			t.Setenv("STEGO_GO_VERSION", "1.26.8")
			files = map[string]string{"service.yaml": "kind: service\nname: namespace\narchetype: application\nlanguage: go\n", "out/retained.txt": "retained output", ".stego/state.yaml": "last_applied: null\n", "go.mod": "module example.com/namespace\ngo 1.26.8\n", "go.sum": ""}
			for name, data := range files {
				if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, command := range []struct {
				name string
				run  func([]string) error
			}{{"validate", runValidate}, {"plan", runPlan}, {"apply", runApply}} {
				if err := command.run(nil); err == nil || !strings.Contains(err.Error(), "validation failed") {
					t.Fatal(command.name, "accepted invalid Go package namespace", namespace, err)
				}
				for name, want := range files {
					got, err := os.ReadFile(name)
					if err != nil || string(got) != want {
						t.Fatal(command.name, "changed", name, err)
					}
				}
			}
		})
	}
}

func TestCommandsRejectNormalizedSlotCollisions(t *testing.T) {
	builtin, err := filepath.Abs("../../registry")
	if err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(t.TempDir(), "registry")
	if err := os.CopyFS(registry, os.DirFS(builtin)); err != nil {
		t.Fatal(err)
	}
	component := filepath.Join(registry, "components/rest-api/component.yaml")
	data, err := os.ReadFile(component)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(component, append(data, []byte("  - name: before__create\n    proto: stego.components.rest_api.slots.BeforeRetry\n    default: passthrough\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(registry, "components/rest-api/slots/before_create.proto"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, "components/rest-api/slots/before__create.proto"), []byte(strings.ReplaceAll(string(data), "BeforeCreate", "BeforeRetry")), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("STEGO_REGISTRY", registry)
	t.Setenv("STEGO_MODULE", "example.com/slots")
	t.Setenv("STEGO_GO_VERSION", "1.26.8")
	files := map[string]string{
		"service.yaml":     "kind: service\nname: slots\narchetype: rest-crud\nlanguage: go\nentities:\n  - name: Widget\n    fields: [{name: label, type: string}]\ncollections:\n  widgets: {entity: Widget, operations: [create]}\nslots:\n  - {slot: before_create, collection: widgets, gate: [first]}\n  - {slot: before__create, collection: widgets, gate: [second]}\n",
		"out/retained.txt": "retained output", ".stego/state.yaml": "last_applied: null\n", "go.mod": "module example.com/slots\ngo 1.26.8\n", "go.sum": "",
	}
	for i, slot := range []string{"before_create", "before__create"} {
		name := []string{"first", "second"}[i]
		files["fills/"+name+"/fill.yaml"] = "kind: fill\nname: " + name + "\nimplements: rest-api." + slot + "\ncollection: widgets\nqualified_by: tester\nqualified_at: 2026-04-01\n"
	}
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []struct {
		name string
		run  func([]string) error
	}{{"validate", runValidate}, {"plan", runPlan}, {"apply", runApply}} {
		if err := command.run(nil); err == nil || !strings.Contains(err.Error(), "validation failed") {
			t.Fatal(command.name, "accepted colliding slot names", err)
		}
		for name, want := range files {
			got, err := os.ReadFile(name)
			if err != nil || string(got) != want {
				t.Fatal(command.name, "changed", name, err)
			}
		}
	}
}

func TestCommandsRejectUnsupportedDependencyTarget(t *testing.T) {
	registry, err := filepath.Abs("../../registry")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("STEGO_REGISTRY", registry)
	t.Setenv("STEGO_MODULE", "example.com/target")
	t.Setenv("STEGO_GO_VERSION", "1.24.9")
	files := map[string]string{
		"service.yaml":      "kind: service\nname: target\narchetype: rest-crud\nlanguage: go\nentities:\n  - name: Record\n    fields: [{name: title, type: string}]\ncollections:\n  records: {entity: Record, operations: [create, read]}\n",
		"go.mod":            "module example.com/target\ngo 1.24.9\n",
		"go.sum":            "",
		"out/retained.txt":  "retained output",
		".stego/state.yaml": "last_applied: null\n",
	}
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []struct {
		name string
		run  func([]string) error
	}{{"validate", runValidate}, {"plan", runPlan}, {"apply", runApply}} {
		if err := command.run(nil); err == nil || !strings.Contains(err.Error(), "validation failed") {
			t.Fatal(command.name, "accepted an unsupported dependency target", err)
		}
		for name, want := range files {
			got, err := os.ReadFile(name)
			if err != nil || string(got) != want {
				t.Fatal(command.name, "changed", name, err)
			}
		}
	}
	t.Setenv("STEGO_GO_VERSION", "1.25.0")
	if err := runValidate(nil); err == nil {
		t.Fatal("target below the tracing dependency minimum was accepted")
	}
	t.Setenv("STEGO_GO_VERSION", "1.26.0")
	if err := runValidate(nil); err != nil {
		t.Fatal("supported target rejected", err)
	}
	if err := runPlan(nil); err != nil {
		t.Fatal("supported target cannot produce a plan", err)
	}
}
