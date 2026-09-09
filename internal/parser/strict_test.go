package parser

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
)

func TestStrictServiceInput(t *testing.T) {
	const base = "kind: service\nname: test\narchetype: rest-crud\nlanguage: go\n"
	tests := []struct {
		name, suffix, want string
	}{
		{"root field", "langauge: go\n", "langauge"},
		{"entity constraint", "entities:\n  - name: Todo\n    fields:\n      - {name: title, type: string, min_lenght: 1}\n", "entities[0].fields[0].min_lenght"},
		{"collection field", "collections:\n  todos: {entity: Todo, operation: [create]}\n", "collections.todos.operation"},
		{"slot field", "slots:\n  - {slot: before_create, collection: todos, gates: [policy]}\n", "slots[0].gates"},
		{"duplicate collection", "collections:\n  todos: {entity: Todo, operations: [create]}\n  todos: {entity: Todo, operations: [read]}\n", "duplicate key"},
		{"duplicate dynamic config", "overrides:\n  auth: {header: one, header: two}\n", "duplicate key"},
		{"additional document", "---\nkind: service\n", "multiple YAML documents"},
		{"empty additional document", "---\n", "multiple YAML documents"},
		{"anchor", "overrides: {auth: &auth {header: example}}\n", "anchors and aliases"},
		{"merge", "overrides: {auth: {<<: {header: example}}}\n", "merge keys"},
		{"non-string key", "overrides: {123: value}\n", "keys must be strings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseServiceDeclarationFromBytes([]byte(base+tt.suffix), "service.yaml")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error containing %q", err, tt.want)
			}
			var pe *ParseError
			if !errors.As(err, &pe) || pe.Path != "service.yaml" || pe.Line < 1 {
				t.Fatalf("error must identify the file and line: %v", err)
			}
		})
	}
}

func TestStrictOtherDeclarations(t *testing.T) {
	tests := []struct {
		name, input, want string
		target            any
	}{
		{"archetype", "kind: archetype\nconventions: {loging: json}\n", "conventions.loging", &types.Archetype{}},
		{"component", "kind: component\nconfig: {port: {type: int, defaut: 8080}}\n", "config.port.defaut", &types.Component{}},
		{"nested component", "kind: component\nconfig: {items: {type: list, items: {name: {type: string, optonal: true}}}}\n", "items.name.optonal", &types.Component{}},
		{"inline component", "kind: component\nconfig: {items: {type: list, items: {type: string, optonal: true}}}\n", "items.optonal", &types.Component{}},
		{"port", "kind: component\nrequires: [{name: auth, optonal: true}]\n", "requires[0].optonal", &types.Component{}},
		{"mixin", "kind: mixin\nadds_slots: [{name: test, protto: test}]\n", "adds_slots[0].protto", &types.Mixin{}},
		{"fill", "kind: fill\nqualifed_by: operator\n", "qualifed_by", &types.Fill{}},
		{"config", "registry: [{url: example, reff: abc}]\n", "registry[0].reff", &types.RegistryConfig{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := DecodeStrict([]byte(tt.input), tt.name+".yaml", tt.target); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestStrictLimits(t *testing.T) {
	var service types.ServiceDeclaration
	if err := DecodeStrict([]byte(strings.Repeat(" ", MaxDocumentBytes+1)), "large.yaml", &service); err == nil {
		t.Fatal("oversized input was accepted")
	}
	input := "overrides: " + strings.Repeat("[", 130) + "0" + strings.Repeat("]", 130)
	if err := DecodeStrict([]byte(input), "deep.yaml", &service); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("got %v, want nesting error", err)
	}
	path := filepath.Join(t.TempDir(), "large.yaml")
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", MaxDocumentBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(path); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("got %v, want a file size error", err)
	}
}

func FuzzStrictServiceInput(f *testing.F) {
	f.Add([]byte("kind: service\nname: test\narchetype: rest-crud\nlanguage: go\n"))
	f.Add([]byte("kind: service\ncollections: {x: {entity: X}, x: {entity: Y}}"))
	f.Add([]byte("kind: service\nslots: [{slot: test, entity: legacy}]"))
	f.Add([]byte("kind: service\noverrides: {auth: &auth {header: example}, other: *auth}"))
	f.Add([]byte("kind: service\n---\nkind: component"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, err := ParseServiceDeclarationFromBytes(data, "fuzz.yaml")
		if err != nil {
			var pe *ParseError
			if !errors.As(err, &pe) || pe.Path != "fuzz.yaml" {
				t.Fatalf("error lost its source path: %v", err)
			}
		}
	})
}

func TestStrictPreservesOrderedCollectionsAndDynamicConfig(t *testing.T) {
	input := []byte(`kind: service
name: test
archetype: rest-crud
language: go
collections:
  second: {entity: Todo, operations: [create]}
  first: {entity: Todo, operations: [read]}
overrides:
  custom-auth:
    header: X-Token
`)
	service, err := ParseServiceDeclarationFromBytes(input, "service.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(service.Collections) != 2 || service.Collections[0].Name != "second" || service.Collections[1].Name != "first" {
		t.Fatalf("collection order changed: %+v", service.Collections)
	}
	if service.Overrides["custom-auth"].(map[string]any)["header"] != "X-Token" {
		t.Fatal("dynamic configuration changed")
	}
}

func TestStrictArtifactLimitRetainsValidation(t *testing.T) {
	var value struct {
		Name string `yaml:"name"`
	}
	data := []byte("name: example\n")
	if err := DecodeStrictWithLimit(data, "artifact", &value, len(data)-1); err == nil {
		t.Fatal("artifact limit was ignored")
	}
	if err := DecodeStrictWithLimit(data, "artifact", &value, len(data)); err != nil {
		t.Fatal(err)
	}
	if err := DecodeStrictWithLimit(data, "artifact", &value, 0); err == nil {
		t.Fatal("zero limit was accepted")
	}
	for _, invalid := range []string{"name: first\nname: second\n", "unknown: value\n"} {
		if err := DecodeStrictWithLimit([]byte(invalid), "artifact", &value, 1024); err == nil {
			t.Fatalf("artifact decoding bypassed validation: %s", invalid)
		}
	}
}

func TestResourceVersionDeclarationRequiresBoolean(t *testing.T) {
	for _, value := range []string{"true", "false", "yes", "1", "\"true\"", "null"} {
		input := "kind: service\nname: test\narchetype: test\nlanguage: go\nentities:\n - name: Record\n   versioned: " + value + "\n"
		result, err := ParseServiceDeclarationFromBytes([]byte(input), "service.yaml")
		valid := value == "true" || value == "false"
		if valid && (err != nil || result.Entities[0].Versioned != (value == "true")) {
			t.Fatalf("boolean %s: %v", value, err)
		}
		if !valid && err == nil {
			t.Fatalf("accepted non-boolean versioned: %s", value)
		}
	}
}

func TestCleanupOwnerDeclarationRequiresStrings(t *testing.T) {
	for _, value := range []string{`[provider]`, `["true"]`, `[]`, `[true]`, `[1]`, `[null]`, `null`, `provider`, `{provider: true}`} {
		input := "kind: service\nname: test\narchetype: test\nlanguage: go\nentities:\n - name: Record\n   versioned: true\n   cleanup_owners: " + value + "\n"
		_, err := ParseServiceDeclarationFromBytes([]byte(input), "service.yaml")
		valid := value == `[provider]` || value == `["true"]` || value == `[]`
		if valid && err != nil {
			t.Fatal("valid cleanup list rejected", value, err)
		}
		if !valid && err == nil {
			t.Fatal("cleanup owner type was coerced", value)
		}
	}
}
