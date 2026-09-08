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
