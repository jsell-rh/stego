package parser

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

const testdataDir = "testdata"

func fixture(name string) string {
	return filepath.Join(testdataDir, name)
}

func TestParseServiceDeclaration(t *testing.T) {
	sd, err := ParseServiceDeclaration(fixture("service.yaml"))
	if err != nil {
		t.Fatalf("ParseServiceDeclaration: %v", err)
	}
	if sd.Kind != "service" {
		t.Errorf("kind = %q, want %q", sd.Kind, "service")
	}
	if sd.Name != "user-management" {
		t.Errorf("name = %q, want %q", sd.Name, "user-management")
	}
	if sd.Archetype != "rest-crud" {
		t.Errorf("archetype = %q, want %q", sd.Archetype, "rest-crud")
	}
	if sd.Language != "go" {
		t.Errorf("language = %q, want %q", sd.Language, "go")
	}

	// Entities
	if len(sd.Entities) != 2 {
		t.Fatalf("entities count = %d, want 2", len(sd.Entities))
	}
	user := sd.Entities[0]
	if user.Name != "User" {
		t.Errorf("entities[0].name = %q, want %q", user.Name, "User")
	}
	if len(user.Fields) != 3 {
		t.Errorf("entities[0].fields count = %d, want 3", len(user.Fields))
	}
	if user.Fields[0].Name != "email" || user.Fields[0].Type != types.FieldTypeString || !user.Fields[0].Unique {
		t.Errorf("entities[0].fields[0] = %+v, want email/string/unique", user.Fields[0])
	}
	if user.Fields[1].Type != types.FieldTypeEnum || len(user.Fields[1].Values) != 2 {
		t.Errorf("entities[0].fields[1] = %+v, want enum with 2 values", user.Fields[1])
	}
	if user.Fields[2].Type != types.FieldTypeRef || user.Fields[2].To != "Organization" {
		t.Errorf("entities[0].fields[2] = %+v, want ref to Organization", user.Fields[2])
	}

	// Collections
	if len(sd.Collections) != 2 {
		t.Fatalf("collections count = %d, want 2", len(sd.Collections))
	}
	if sd.Collections[0].Name != "organizations" {
		t.Errorf("collections[0].name = %q, want %q", sd.Collections[0].Name, "organizations")
	}
	if sd.Collections[0].Entity != "Organization" {
		t.Errorf("collections[0].entity = %q, want %q", sd.Collections[0].Entity, "Organization")
	}
	if len(sd.Collections[0].Operations) != 2 {
		t.Errorf("collections[0].operations count = %d, want 2", len(sd.Collections[0].Operations))
	}
	if sd.Collections[1].Name != "org-users" {
		t.Errorf("collections[1].name = %q, want %q", sd.Collections[1].Name, "org-users")
	}
	if sd.Collections[1].Scope["org_id"] != "Organization" {
		t.Errorf("collections[1].scope = %v, want {org_id: Organization}", sd.Collections[1].Scope)
	}

	// Slots
	if len(sd.Slots) != 2 {
		t.Fatalf("slots count = %d, want 2", len(sd.Slots))
	}
	if sd.Slots[0].Slot != "before_create" {
		t.Errorf("slots[0].slot = %q, want %q", sd.Slots[0].Slot, "before_create")
	}
	if len(sd.Slots[0].Gate) != 2 {
		t.Errorf("slots[0].gate count = %d, want 2", len(sd.Slots[0].Gate))
	}
	if sd.Slots[1].Slot != "on_entity_changed" {
		t.Errorf("slots[1].slot = %q, want %q", sd.Slots[1].Slot, "on_entity_changed")
	}
	if len(sd.Slots[1].FanOut) != 2 {
		t.Errorf("slots[1].fan-out count = %d, want 2", len(sd.Slots[1].FanOut))
	}

	// Mixins
	if len(sd.Mixins) != 1 || sd.Mixins[0] != "event-publisher" {
		t.Errorf("mixins = %v, want [event-publisher]", sd.Mixins)
	}

	// Overrides
	if sd.Overrides == nil {
		t.Fatal("overrides = nil, want non-nil")
	}
}

func TestParseFill(t *testing.T) {
	f, err := ParseFill(fixture("fill.yaml"))
	if err != nil {
		t.Fatalf("ParseFill: %v", err)
	}
	if f.Kind != "fill" {
		t.Errorf("kind = %q, want %q", f.Kind, "fill")
	}
	if f.Name != "admin-creation-policy" {
		t.Errorf("name = %q, want %q", f.Name, "admin-creation-policy")
	}
	if f.Implements != "rest-api.before_create" {
		t.Errorf("implements = %q, want %q", f.Implements, "rest-api.before_create")
	}
	if f.Collection != "org-users" {
		t.Errorf("collection = %q, want %q", f.Collection, "org-users")
	}
	if f.QualifiedBy != "jsell" {
		t.Errorf("qualified_by = %q, want %q", f.QualifiedBy, "jsell")
	}
}

// --- Typed parser tests ---

func TestParseServiceDeclarationTyped(t *testing.T) {
	sd, err := ParseServiceDeclaration(fixture("service.yaml"))
	if err != nil {
		t.Fatalf("ParseServiceDeclaration: %v", err)
	}
	if sd.Name != "user-management" {
		t.Errorf("name = %q, want %q", sd.Name, "user-management")
	}
}

func TestParseFillTyped(t *testing.T) {
	f, err := ParseFill(fixture("fill.yaml"))
	if err != nil {
		t.Fatalf("ParseFill: %v", err)
	}
	if f.Name != "admin-creation-policy" {
		t.Errorf("name = %q, want %q", f.Name, "admin-creation-policy")
	}
}

// --- Error handling tests ---

// document is a document with a kind, a name and a version, for the kind check
// of ReadKind.
type document struct {
	Kind    string `yaml:"kind"`
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

func documentKind(d *document) string { return d.Kind }

func TestParseFileNotFound(t *testing.T) {
	_, err := ParseServiceDeclaration("testdata/does-not-exist.yaml")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *ParseError", err)
	}
	if pe.Path != "testdata/does-not-exist.yaml" {
		t.Errorf("path = %q, want %q", pe.Path, "testdata/does-not-exist.yaml")
	}
}

func TestParseMissingKind(t *testing.T) {
	_, err := ReadKind(fixture("no_kind.yaml"), "service", documentKind)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *ParseError", err)
	}
	if pe.Path != fixture("no_kind.yaml") {
		t.Errorf("path = %q, want fixture path", pe.Path)
	}
}

func TestParseUnknownKind(t *testing.T) {
	_, err := ReadKind(fixture("bad_kind.yaml"), "service", documentKind)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *ParseError", err)
	}
}

func TestParseInvalidYAML(t *testing.T) {
	_, err := ParseServiceDeclaration(fixture("invalid.yaml"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *ParseError", err)
	}
}

// --- Round-trip tests ---
// All round-trip tests use reflect.DeepEqual to compare the entire struct,
// ensuring no fields are silently lost during marshal → unmarshal.

func TestRoundTripServiceDeclaration(t *testing.T) {
	sd, err := ParseServiceDeclaration(fixture("service.yaml"))
	if err != nil {
		t.Fatalf("ParseServiceDeclaration: %v", err)
	}
	data, err := yaml.Marshal(sd)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var sd2 types.ServiceDeclaration
	if err := yaml.Unmarshal(data, &sd2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if !reflect.DeepEqual(*sd, sd2) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  got:      %+v", *sd, sd2)
	}
}

func TestRoundTripFill(t *testing.T) {
	f, err := ParseFill(fixture("fill.yaml"))
	if err != nil {
		t.Fatalf("ParseFill: %v", err)
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var f2 types.Fill
	if err := yaml.Unmarshal(data, &f2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if !reflect.DeepEqual(*f, f2) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  got:      %+v", *f, f2)
	}
}

// --- collections: parsing and expose: rejection ---

func TestParseServiceDeclarationRejectsExpose(t *testing.T) {
	input := []byte(`
kind: service
name: test
archetype: rest-crud
language: go
entities: []
expose:
  - entity: User
    operations: [create]
`)
	_, err := ParseServiceDeclarationFromBytes(input, "test.yaml")
	if err == nil {
		t.Fatal("expected error for 'expose:' key, got nil")
	}
	if !strings.Contains(err.Error(), "expose") || !strings.Contains(err.Error(), "collections") {
		t.Errorf("error = %q, want mention of 'expose' and 'collections'", err.Error())
	}
}

func TestParseServiceDeclarationCollectionsOrder(t *testing.T) {
	input := []byte(`
kind: service
name: test
archetype: rest-crud
language: go
entities: []
collections:
  alpha:
    entity: A
    operations: [create]
  beta:
    entity: B
    operations: [read]
  gamma:
    entity: C
    operations: [list]
`)
	sd, err := ParseServiceDeclarationFromBytes(input, "test.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(sd.Collections) != 3 {
		t.Fatalf("collections count = %d, want 3", len(sd.Collections))
	}
	// Verify insertion order is preserved.
	names := []string{sd.Collections[0].Name, sd.Collections[1].Name, sd.Collections[2].Name}
	want := []string{"alpha", "beta", "gamma"}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("collections[%d].name = %q, want %q", i, n, want[i])
		}
	}
}

// --- ParseError includes file path and line context ---

func TestParseErrorContainsPath(t *testing.T) {
	_, err := ParseServiceDeclaration("testdata/does-not-exist.yaml")
	if err == nil {
		t.Fatal("expected error")
	}
	errMsg := err.Error()
	if len(errMsg) < len("testdata/does-not-exist.yaml") {
		t.Fatalf("error message too short: %q", errMsg)
	}
	// Error message must start with the file path
	want := "testdata/does-not-exist.yaml:"
	if errMsg[:len(want)] != want {
		t.Errorf("error message = %q, want prefix %q", errMsg, want)
	}
}

func TestParseErrorContainsLineContext(t *testing.T) {
	_, err := ParseServiceDeclaration(fixture("invalid.yaml"))
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *ParseError", err)
	}
	if pe.Path != fixture("invalid.yaml") {
		t.Errorf("path = %q, want %q", pe.Path, fixture("invalid.yaml"))
	}
	// The error message should contain the file path
	errMsg := pe.Error()
	if len(errMsg) == 0 {
		t.Fatal("error message is empty")
	}
}

func TestParseErrorLineInfoStruct(t *testing.T) {
	pe := &ParseError{
		Path:    "test.yaml",
		Line:    5,
		Context: "bad_field: [",
		Err:     fmt.Errorf("unmarshal failed"),
	}
	got := pe.Error()
	want := `test.yaml:5: unmarshal failed (near "bad_field: [")`
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestParseErrorLineInfoNoContext(t *testing.T) {
	pe := &ParseError{
		Path: "test.yaml",
		Line: 3,
		Err:  fmt.Errorf("unmarshal failed"),
	}
	got := pe.Error()
	want := "test.yaml:3: unmarshal failed"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestParseServiceDeclarationBasePath(t *testing.T) {
	input := []byte(`
kind: service
name: hyperfleet-api
archetype: rest-crud
language: go
base_path: /api/hyperfleet/v1
entities:
  - name: Cluster
    fields:
      - { name: name, type: string }
collections:
  clusters:
    entity: Cluster
    operations: [create, read]
`)
	sd, err := ParseServiceDeclarationFromBytes(input, "test.yaml")
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if sd.BasePath != "/api/hyperfleet/v1" {
		t.Errorf("BasePath = %q, want %q", sd.BasePath, "/api/hyperfleet/v1")
	}
}

func TestParseServiceDeclarationErrorTypeBase(t *testing.T) {
	input := []byte(`
kind: service
name: hyperfleet-api
archetype: rest-crud
language: go
error_type_base: https://api.hyperfleet.io/errors/
entities:
  - name: Cluster
    fields:
      - { name: name, type: string }
collections:
  clusters:
    entity: Cluster
    operations: [create, read]
`)
	sd, err := ParseServiceDeclarationFromBytes(input, "test.yaml")
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if sd.ErrorTypeBase != "https://api.hyperfleet.io/errors/" {
		t.Errorf("ErrorTypeBase = %q, want %q", sd.ErrorTypeBase, "https://api.hyperfleet.io/errors/")
	}
}

func TestParseServiceDeclarationBasePathOmitted(t *testing.T) {
	input := []byte(`
kind: service
name: test
archetype: rest-crud
language: go
entities:
  - name: Widget
    fields:
      - { name: name, type: string }
collections:
  widgets:
    entity: Widget
    operations: [create]
`)
	sd, err := ParseServiceDeclarationFromBytes(input, "test.yaml")
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if sd.BasePath != "" {
		t.Errorf("BasePath should be empty when omitted, got %q", sd.BasePath)
	}
	if sd.ErrorTypeBase != "" {
		t.Errorf("ErrorTypeBase should be empty when omitted, got %q", sd.ErrorTypeBase)
	}
}

func TestRoundTripServiceDeclarationWithBasePath(t *testing.T) {
	input := []byte(`
kind: service
name: test
archetype: rest-crud
language: go
base_path: /api/v1
error_type_base: https://example.com/errors/
entities:
  - name: Item
    fields:
      - { name: label, type: string }
collections:
  items:
    entity: Item
    operations: [create]
`)
	sd, err := ParseServiceDeclarationFromBytes(input, "test.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	data, err := yaml.Marshal(sd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var sd2 types.ServiceDeclaration
	if err := yaml.Unmarshal(data, &sd2); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if sd2.BasePath != "/api/v1" {
		t.Errorf("BasePath after round-trip = %q, want %q", sd2.BasePath, "/api/v1")
	}
	if sd2.ErrorTypeBase != "https://example.com/errors/" {
		t.Errorf("ErrorTypeBase after round-trip = %q, want %q", sd2.ErrorTypeBase, "https://example.com/errors/")
	}
}
