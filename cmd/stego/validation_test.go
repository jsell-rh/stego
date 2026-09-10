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
