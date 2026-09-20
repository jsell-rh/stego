package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
)

func TestRuntimeConfigurationCompilation(t *testing.T) {
	project := t.TempDir()
	registry := filepath.Join(project, "registry")
	if err := os.CopyFS(registry, os.DirFS("../../registry")); err != nil {
		t.Fatal(err)
	}
	archetype := filepath.Join(registry, "archetypes/rest-crud/archetype.yaml")
	raw, err := os.ReadFile(archetype)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "components:\n", "components:\n  - runtime-configuration\n", 1))
	if err := os.WriteFile(archetype, raw, 0600); err != nil {
		t.Fatal(err)
	}
	declaration := `kind: service
name: widget
archetype: rest-crud
language: go
entities:
  - name: Widget
    fields: [{name: label, type: string}]
collections:
  widgets: {entity: Widget, operations: [create, read]}
overrides:
  runtime-configuration:
    groups:
      - name: Worker
        fields:
          - {name: Address, env: WIDGET_ADDRESS, type: string, min_length: 1, max_length: 256}
          - {name: Resync, env: WIDGET_RESYNC, type: duration, min: 1s, max: 1h, default: 30s}
`
	input := compiler.ReconcilerInput{ProjectDir: project, RegistryDir: registry, GoVersion: "1.26.8", ModuleName: "example.com/widget", Generators: defaultGenerators()}
	for name, source := range map[string]string{
		"valid":           declaration,
		"unknown field":   strings.Replace(declaration, "max_length: 256", "max_length: 256, unknown: true", 1),
		"invalid bound":   strings.Replace(declaration, "max_length: 256", "max_length: 4097", 1),
		"invalid default": strings.Replace(declaration, "default: 30s", "default: 2h", 1),
		"null default":    strings.Replace(declaration, "default: 30s", "default: null", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(project, "service.yaml"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			plan, err := compiler.Reconcile(input)
			if name != "valid" {
				if err == nil || plan != nil {
					t.Fatal("invalid configuration reached generation")
				}
				if _, err := os.Stat(filepath.Join(project, "out")); !os.IsNotExist(err) {
					t.Fatal("preflight wrote output")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, file := range plan.Files {
				if strings.HasSuffix(file.Path, "configuration/configuration.go") {
					found = true
				}
			}
			if !found {
				t.Fatal("typed settings were not generated")
			}
		})
	}
}
