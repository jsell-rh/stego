package parser

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stego-project/stego/internal/types"
	"gopkg.in/yaml.v3"
)

const testdataDir = "testdata"

func fixture(name string) string {
	return filepath.Join(testdataDir, name)
}

// --- Unified Parse dispatcher tests ---

func TestParseArchetype(t *testing.T) {
	v, err := Parse(fixture("archetype.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a, ok := v.(*types.Archetype)
	if !ok {
		t.Fatalf("got %T, want *types.Archetype", v)
	}
	if a.Kind != "archetype" {
		t.Errorf("kind = %q, want %q", a.Kind, "archetype")
	}
	if a.Name != "rest-crud" {
		t.Errorf("name = %q, want %q", a.Name, "rest-crud")
	}
	if a.Language != "go" {
		t.Errorf("language = %q, want %q", a.Language, "go")
	}
	if a.Version != "3.0.0" {
		t.Errorf("version = %q, want %q", a.Version, "3.0.0")
	}
	if len(a.Components) != 4 {
		t.Errorf("components count = %d, want 4", len(a.Components))
	}
	if a.DefaultAuth != "jwt-auth" {
		t.Errorf("default_auth = %q, want %q", a.DefaultAuth, "jwt-auth")
	}
	if a.Conventions.Layout != "flat" {
		t.Errorf("conventions.layout = %q, want %q", a.Conventions.Layout, "flat")
	}
	if a.Conventions.ErrorHandling != "problem-details-rfc" {
		t.Errorf("conventions.error_handling = %q, want %q", a.Conventions.ErrorHandling, "problem-details-rfc")
	}
	if a.Conventions.Logging != "structured-json" {
		t.Errorf("conventions.logging = %q, want %q", a.Conventions.Logging, "structured-json")
	}
	if a.Conventions.TestPattern != "table-driven" {
		t.Errorf("conventions.test_pattern = %q, want %q", a.Conventions.TestPattern, "table-driven")
	}
	if len(a.CompatibleMixins) != 2 {
		t.Errorf("compatible_mixins count = %d, want 2", len(a.CompatibleMixins))
	}
	if len(a.Bindings) != 2 {
		t.Errorf("bindings count = %d, want 2", len(a.Bindings))
	}
	if a.Bindings["storage-adapter"] != "postgres-adapter" {
		t.Errorf("bindings[storage-adapter] = %q, want %q", a.Bindings["storage-adapter"], "postgres-adapter")
	}
}

func TestParseComponent(t *testing.T) {
	v, err := Parse(fixture("component.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, ok := v.(*types.Component)
	if !ok {
		t.Fatalf("got %T, want *types.Component", v)
	}
	if c.Kind != "component" {
		t.Errorf("kind = %q, want %q", c.Kind, "component")
	}
	if c.Name != "rest-api" {
		t.Errorf("name = %q, want %q", c.Name, "rest-api")
	}
	if c.Version != "2.1.0" {
		t.Errorf("version = %q, want %q", c.Version, "2.1.0")
	}
	if len(c.Requires) != 2 {
		t.Errorf("requires count = %d, want 2", len(c.Requires))
	}
	if c.Requires[0].Name != "auth-provider" {
		t.Errorf("requires[0] = %q, want %q", c.Requires[0].Name, "auth-provider")
	}
	if len(c.Provides) != 2 {
		t.Errorf("provides count = %d, want 2", len(c.Provides))
	}
	if len(c.Slots) != 2 {
		t.Errorf("slots count = %d, want 2", len(c.Slots))
	}
	if c.Slots[0].Name != "before_create" {
		t.Errorf("slots[0].name = %q, want %q", c.Slots[0].Name, "before_create")
	}
	if c.Slots[0].Proto != "stego.components.rest_api.slots.BeforeCreate" {
		t.Errorf("slots[0].proto = %q, want full proto path", c.Slots[0].Proto)
	}
	if c.Slots[0].Default != "passthrough" {
		t.Errorf("slots[0].default = %q, want %q", c.Slots[0].Default, "passthrough")
	}

	// Verify nested config: expose.items (map-of-fields) and operations.items (inline)
	expose, ok := c.Config["expose"]
	if !ok {
		t.Fatal("config missing 'expose'")
	}
	if expose.Type != "list" {
		t.Errorf("config.expose.type = %q, want %q", expose.Type, "list")
	}
	if expose.Items == nil || expose.Items.Fields == nil {
		t.Fatal("config.expose.items.Fields is nil")
	}
	if len(expose.Items.Fields) != 3 {
		t.Fatalf("config.expose.items.Fields count = %d, want 3", len(expose.Items.Fields))
	}
	entityCfg := expose.Items.Fields["entity"]
	if entityCfg.Type != "entity-ref" {
		t.Errorf("expose.items.entity.type = %q, want %q", entityCfg.Type, "entity-ref")
	}
	scopeCfg := expose.Items.Fields["scope"]
	if !scopeCfg.Optional {
		t.Error("expose.items.scope.optional = false, want true")
	}
	opsCfg := expose.Items.Fields["operations"]
	if opsCfg.Items == nil || opsCfg.Items.Inline == nil {
		t.Fatal("operations.items.Inline is nil")
	}
	if len(opsCfg.Items.Inline.Enum) != 5 {
		t.Errorf("operations.items.enum count = %d, want 5", len(opsCfg.Items.Inline.Enum))
	}

	portCfg, ok := c.Config["port"]
	if !ok {
		t.Fatal("config missing 'port'")
	}
	if portCfg.Type != "int" {
		t.Errorf("config.port.type = %q, want %q", portCfg.Type, "int")
	}
}

func TestParseMixin(t *testing.T) {
	v, err := Parse(fixture("mixin.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m, ok := v.(*types.Mixin)
	if !ok {
		t.Fatalf("got %T, want *types.Mixin", v)
	}
	if m.Kind != "mixin" {
		t.Errorf("kind = %q, want %q", m.Kind, "mixin")
	}
	if m.Name != "event-publisher" {
		t.Errorf("name = %q, want %q", m.Name, "event-publisher")
	}
	if m.Version != "1.0.0" {
		t.Errorf("version = %q, want %q", m.Version, "1.0.0")
	}
	if len(m.AddsComponents) != 1 || m.AddsComponents[0] != "kafka-producer" {
		t.Errorf("adds_components = %v, want [kafka-producer]", m.AddsComponents)
	}
	if len(m.AddsSlots) != 1 {
		t.Errorf("adds_slots count = %d, want 1", len(m.AddsSlots))
	}
	if m.AddsSlots[0].Name != "on_entity_changed" {
		t.Errorf("adds_slots[0].name = %q, want %q", m.AddsSlots[0].Name, "on_entity_changed")
	}
	if m.AddsSlots[0].Default != "noop" {
		t.Errorf("adds_slots[0].default = %q, want %q", m.AddsSlots[0].Default, "noop")
	}
	if m.Overrides != "none" {
		t.Errorf("overrides = %q, want %q", m.Overrides, "none")
	}
}

func TestParseServiceDeclaration(t *testing.T) {
	v, err := Parse(fixture("service.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sd, ok := v.(*types.ServiceDeclaration)
	if !ok {
		t.Fatalf("got %T, want *types.ServiceDeclaration", v)
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

	// Expose
	if len(sd.Expose) != 2 {
		t.Fatalf("expose count = %d, want 2", len(sd.Expose))
	}
	if sd.Expose[0].Entity != "Organization" {
		t.Errorf("expose[0].entity = %q, want %q", sd.Expose[0].Entity, "Organization")
	}
	if len(sd.Expose[0].Operations) != 2 {
		t.Errorf("expose[0].operations count = %d, want 2", len(sd.Expose[0].Operations))
	}
	if sd.Expose[1].Scope != "org_id" {
		t.Errorf("expose[1].scope = %q, want %q", sd.Expose[1].Scope, "org_id")
	}
	if sd.Expose[1].Parent != "Organization" {
		t.Errorf("expose[1].parent = %q, want %q", sd.Expose[1].Parent, "Organization")
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
	v, err := Parse(fixture("fill.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f, ok := v.(*types.Fill)
	if !ok {
		t.Fatalf("got %T, want *types.Fill", v)
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
	if f.Entity != "User" {
		t.Errorf("entity = %q, want %q", f.Entity, "User")
	}
	if f.QualifiedBy != "jsell" {
		t.Errorf("qualified_by = %q, want %q", f.QualifiedBy, "jsell")
	}
}

// --- Typed parser tests ---

func TestParseArchetypeTyped(t *testing.T) {
	a, err := ParseArchetype(fixture("archetype.yaml"))
	if err != nil {
		t.Fatalf("ParseArchetype: %v", err)
	}
	if a.Name != "rest-crud" {
		t.Errorf("name = %q, want %q", a.Name, "rest-crud")
	}
}

func TestParseComponentTyped(t *testing.T) {
	c, err := ParseComponent(fixture("component.yaml"))
	if err != nil {
		t.Fatalf("ParseComponent: %v", err)
	}
	if c.Name != "rest-api" {
		t.Errorf("name = %q, want %q", c.Name, "rest-api")
	}
}

func TestParseMixinTyped(t *testing.T) {
	m, err := ParseMixin(fixture("mixin.yaml"))
	if err != nil {
		t.Fatalf("ParseMixin: %v", err)
	}
	if m.Name != "event-publisher" {
		t.Errorf("name = %q, want %q", m.Name, "event-publisher")
	}
}

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

func TestParseFileNotFound(t *testing.T) {
	_, err := Parse("testdata/does-not-exist.yaml")
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
	_, err := Parse(fixture("no_kind.yaml"))
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
	_, err := Parse(fixture("bad_kind.yaml"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *ParseError", err)
	}
}

func TestParseInvalidYAML(t *testing.T) {
	_, err := Parse(fixture("invalid.yaml"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *ParseError", err)
	}
}

func TestParseTypedWrongKind(t *testing.T) {
	// wrong_kind.yaml has kind: mixin, parsing as archetype should fail
	_, err := ParseArchetype(fixture("wrong_kind.yaml"))
	if err == nil {
		t.Fatal("expected error for wrong kind, got nil")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *ParseError", err)
	}
}

func TestParseTypedFileNotFound(t *testing.T) {
	_, err := ParseArchetype("testdata/does-not-exist.yaml")
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

func TestRoundTripArchetype(t *testing.T) {
	a, err := ParseArchetype(fixture("archetype.yaml"))
	if err != nil {
		t.Fatalf("ParseArchetype: %v", err)
	}
	data, err := yaml.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var a2 types.Archetype
	if err := yaml.Unmarshal(data, &a2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if !reflect.DeepEqual(*a, a2) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  got:      %+v", *a, a2)
	}
}

func TestRoundTripComponent(t *testing.T) {
	c, err := ParseComponent(fixture("component.yaml"))
	if err != nil {
		t.Fatalf("ParseComponent: %v", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var c2 types.Component
	if err := yaml.Unmarshal(data, &c2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if !reflect.DeepEqual(*c, c2) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  got:      %+v", *c, c2)
	}
}

func TestRoundTripMixin(t *testing.T) {
	m, err := ParseMixin(fixture("mixin.yaml"))
	if err != nil {
		t.Fatalf("ParseMixin: %v", err)
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m2 types.Mixin
	if err := yaml.Unmarshal(data, &m2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if !reflect.DeepEqual(*m, m2) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  got:      %+v", *m, m2)
	}
}

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

// --- ParseError includes file path and line context ---

func TestParseErrorContainsPath(t *testing.T) {
	_, err := Parse("testdata/does-not-exist.yaml")
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
	_, err := Parse(fixture("invalid.yaml"))
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
