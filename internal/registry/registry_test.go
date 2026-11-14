package registry_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/registry"
	"gopkg.in/yaml.v3"
)

const testdataDir = "testdata"

func TestLoad(t *testing.T) {
	reg, err := registry.Load(filepath.Join(testdataDir, "registry"))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	t.Run("loads archetypes", func(t *testing.T) {
		archetypes := reg.Archetypes()
		if len(archetypes) != 1 {
			t.Fatalf("expected 1 archetype, got %d", len(archetypes))
		}
		a := reg.Archetype("rest-crud")
		if a == nil {
			t.Fatal("Archetype(rest-crud) returned nil")
		}
		if a.Name != "rest-crud" {
			t.Errorf("Name = %q, want %q", a.Name, "rest-crud")
		}
		if a.Kind != "archetype" {
			t.Errorf("Kind = %q, want %q", a.Kind, "archetype")
		}
		if a.Language != "go" {
			t.Errorf("Language = %q, want %q", a.Language, "go")
		}
		if a.Version != "3.0.0" {
			t.Errorf("Version = %q, want %q", a.Version, "3.0.0")
		}
		if len(a.Components) != 4 {
			t.Errorf("Components count = %d, want 4", len(a.Components))
		}
		if a.DefaultAuth != "jwt-auth" {
			t.Errorf("DefaultAuth = %q, want %q", a.DefaultAuth, "jwt-auth")
		}
		if a.Conventions.Layout != "flat" {
			t.Errorf("Conventions.Layout = %q, want %q", a.Conventions.Layout, "flat")
		}
		if len(a.CompatibleMixins) != 2 {
			t.Errorf("CompatibleMixins count = %d, want 2", len(a.CompatibleMixins))
		}
		if len(a.Bindings) != 2 {
			t.Errorf("Bindings count = %d, want 2", len(a.Bindings))
		}
		if a.Bindings["storage-adapter"] != "postgres-adapter" {
			t.Errorf("Bindings[storage-adapter] = %q, want %q", a.Bindings["storage-adapter"], "postgres-adapter")
		}
	})

	t.Run("loads components", func(t *testing.T) {
		components := reg.Components()
		if len(components) != 4 {
			t.Fatalf("expected 4 components, got %d", len(components))
		}

		c := reg.Component("rest-api")
		if c == nil {
			t.Fatal("Component(rest-api) returned nil")
		}
		if c.Name != "rest-api" {
			t.Errorf("Name = %q, want %q", c.Name, "rest-api")
		}
		if c.Kind != "component" {
			t.Errorf("Kind = %q, want %q", c.Kind, "component")
		}
		if c.Version != "3.0.0" {
			t.Errorf("Version = %q, want %q", c.Version, "3.0.0")
		}
		if len(c.Config) != 1 {
			t.Errorf("Config count = %d, want 1", len(c.Config))
		}
		exposeCfg, ok := c.Config["expose"]
		if !ok {
			t.Fatal("Config[expose] missing")
		}
		if exposeCfg.Type != "list" {
			t.Errorf("Config[expose].Type = %q, want %q", exposeCfg.Type, "list")
		}
		if len(c.Requires) != 2 {
			t.Errorf("Requires count = %d, want 2", len(c.Requires))
		}
		if c.Requires[0].Name != "auth-provider" {
			t.Errorf("Requires[0].Name = %q, want %q", c.Requires[0].Name, "auth-provider")
		}
		if len(c.Provides) != 2 {
			t.Errorf("Provides count = %d, want 2", len(c.Provides))
		}
		if len(c.Slots) != 9 {
			t.Errorf("Slots count = %d, want 9", len(c.Slots))
		}
		if c.Slots[0].Name != "before_create" {
			t.Errorf("Slots[0].Name = %q, want %q", c.Slots[0].Name, "before_create")
		}

		pg := reg.Component("postgres-adapter")
		if pg == nil {
			t.Fatal("Component(postgres-adapter) returned nil")
		}
		if pg.Version != "2.0.0" {
			t.Errorf("postgres-adapter Version = %q, want %q", pg.Version, "2.0.0")
		}
		if len(pg.Provides) != 1 || pg.Provides[0].Name != "storage-adapter" {
			t.Errorf("postgres-adapter Provides unexpected: %+v", pg.Provides)
		}

		otel := reg.Component("otel-tracing")
		if otel == nil {
			t.Fatal("Component(otel-tracing) returned nil")
		}
		if otel.Version != "0.1.0" {
			t.Errorf("otel-tracing Version = %q, want %q", otel.Version, "0.1.0")
		}
		if len(otel.Provides) != 1 || otel.Provides[0].Name != "tracing" {
			t.Errorf("otel-tracing Provides unexpected: %+v", otel.Provides)
		}
		if len(otel.Slots) != 0 {
			t.Errorf("otel-tracing Slots count = %d, want 0", len(otel.Slots))
		}

		hc := reg.Component("health-check")
		if hc == nil {
			t.Fatal("Component(health-check) returned nil")
		}
		if hc.Version != "0.1.0" {
			t.Errorf("health-check Version = %q, want %q", hc.Version, "0.1.0")
		}
		if len(hc.Provides) != 1 || hc.Provides[0].Name != "health-endpoint" {
			t.Errorf("health-check Provides unexpected: %+v", hc.Provides)
		}
		if len(hc.Slots) != 0 {
			t.Errorf("health-check Slots count = %d, want 0", len(hc.Slots))
		}
	})

	t.Run("loads mixins", func(t *testing.T) {
		mixins := reg.Mixins()
		if len(mixins) != 1 {
			t.Fatalf("expected 1 mixin, got %d", len(mixins))
		}
		m := reg.Mixin("event-publisher")
		if m == nil {
			t.Fatal("Mixin(event-publisher) returned nil")
		}
		if m.Name != "event-publisher" {
			t.Errorf("Name = %q, want %q", m.Name, "event-publisher")
		}
		if m.Kind != "mixin" {
			t.Errorf("Kind = %q, want %q", m.Kind, "mixin")
		}
		if m.Version != "1.0.0" {
			t.Errorf("Version = %q, want %q", m.Version, "1.0.0")
		}
		if len(m.AddsComponents) != 1 || m.AddsComponents[0] != "kafka-producer" {
			t.Errorf("AddsComponents = %v, want [kafka-producer]", m.AddsComponents)
		}
		if len(m.AddsSlots) != 1 || m.AddsSlots[0].Name != "on_entity_changed" {
			t.Errorf("AddsSlots = %v, want [on_entity_changed]", m.AddsSlots)
		}
		if m.Overrides != "none" {
			t.Errorf("Overrides = %q, want %q", m.Overrides, "none")
		}
	})
}

func TestLookupNotFound(t *testing.T) {
	reg, err := registry.Load(filepath.Join(testdataDir, "registry"))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if a := reg.Archetype("nonexistent"); a != nil {
		t.Errorf("Archetype(nonexistent) = %v, want nil", a)
	}
	if c := reg.Component("nonexistent"); c != nil {
		t.Errorf("Component(nonexistent) = %v, want nil", c)
	}
	if m := reg.Mixin("nonexistent"); m != nil {
		t.Errorf("Mixin(nonexistent) = %v, want nil", m)
	}
}

func TestLoadEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	reg, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("Load() error on empty dir: %v", err)
	}
	if len(reg.Archetypes()) != 0 {
		t.Errorf("expected 0 archetypes, got %d", len(reg.Archetypes()))
	}
	if len(reg.Components()) != 0 {
		t.Errorf("expected 0 components, got %d", len(reg.Components()))
	}
	if len(reg.Mixins()) != 0 {
		t.Errorf("expected 0 mixins, got %d", len(reg.Mixins()))
	}
}

func TestLoadNonExistentDirectory(t *testing.T) {
	_, err := registry.Load("/nonexistent/registry/path")
	if err == nil {
		t.Fatal("expected error for non-existent registry directory, got nil")
	}
}

func TestLoadInvalidArchetype(t *testing.T) {
	dir := t.TempDir()
	archDir := filepath.Join(dir, "archetypes", "bad")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "archetype.yaml"), []byte("not: valid: yaml: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.Load(dir)
	if err == nil {
		t.Fatal("expected error loading invalid archetype, got nil")
	}
}

func TestLoadInvalidComponent(t *testing.T) {
	dir := t.TempDir()
	compDir := filepath.Join(dir, "components", "bad")
	if err := os.MkdirAll(compDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compDir, "component.yaml"), []byte("kind: component\nname: 123\nversion: [invalid]"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.Load(dir)
	if err == nil {
		t.Fatal("expected error loading invalid component, got nil")
	}
}

func TestLoadInvalidMixin(t *testing.T) {
	dir := t.TempDir()
	mixDir := filepath.Join(dir, "mixins", "bad")
	if err := os.MkdirAll(mixDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mixDir, "mixin.yaml"), []byte("not: valid: yaml: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.Load(dir)
	if err == nil {
		t.Fatal("expected error loading invalid mixin, got nil")
	}
}

func TestLoadMissingArchetypeFile(t *testing.T) {
	dir := t.TempDir()
	archDir := filepath.Join(dir, "archetypes", "missing")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Directory exists but archetype.yaml is missing.
	_, err := registry.Load(dir)
	if err == nil {
		t.Fatal("expected error for missing archetype.yaml, got nil")
	}
}

func TestLoadConfig(t *testing.T) {
	cfg, err := registry.LoadConfig(filepath.Join(testdataDir, "stego-config", "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}

	if len(cfg.Registry) != 1 {
		t.Fatalf("expected 1 registry source, got %d", len(cfg.Registry))
	}
	src := cfg.Registry[0]
	if src.URL != "git.corp.com/platform/stego-registry" {
		t.Errorf("Registry[0].URL = %q, want %q", src.URL, "git.corp.com/platform/stego-registry")
	}
	if src.Ref != "a1b2c3d4e5f6" {
		t.Errorf("Registry[0].Ref = %q, want %q", src.Ref, "a1b2c3d4e5f6")
	}

	if len(cfg.Pins) != 0 {
		t.Fatalf("unexpected component pins: %d", len(cfg.Pins))
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := registry.LoadConfig("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing config file, got nil")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("not: valid: yaml: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML config, got nil")
	}
}

func TestLoadSkipsNonDirectoryEntries(t *testing.T) {
	dir := t.TempDir()
	archDir := filepath.Join(dir, "archetypes")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a regular file in the archetypes directory (should be skipped).
	if err := os.WriteFile(filepath.Join(archDir, ".gitkeep"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(reg.Archetypes()) != 0 {
		t.Errorf("expected 0 archetypes, got %d", len(reg.Archetypes()))
	}
}

func TestLoadArchetypeNameMismatch(t *testing.T) {
	dir := t.TempDir()
	archDir := filepath.Join(dir, "archetypes", "foo")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "archetype.yaml"), []byte("kind: archetype\nruntime: {module: github.com/jsell-rh/stego/registry/runtime, version: v0.1.0, sum: 'h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='}\nname: bar\nversion: 1.0.0\nlanguage: go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.Load(dir)
	if err == nil {
		t.Fatal("expected error for name mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "name mismatch") {
		t.Errorf("expected name mismatch error, got: %v", err)
	}
}

func TestLoadComponentNameMismatch(t *testing.T) {
	dir := t.TempDir()
	compDir := filepath.Join(dir, "components", "foo")
	if err := os.MkdirAll(compDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compDir, "component.yaml"), []byte("kind: component\nname: bar\nversion: 1.0.0\nstage: ga\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.Load(dir)
	if err == nil {
		t.Fatal("expected error for name mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "name mismatch") {
		t.Errorf("expected name mismatch error, got: %v", err)
	}
}

func TestLoadMixinNameMismatch(t *testing.T) {
	dir := t.TempDir()
	mixDir := filepath.Join(dir, "mixins", "foo")
	if err := os.MkdirAll(mixDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mixDir, "mixin.yaml"), []byte("kind: mixin\nname: bar\nversion: 1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.Load(dir)
	if err == nil {
		t.Fatal("expected error for name mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "name mismatch") {
		t.Errorf("expected name mismatch error, got: %v", err)
	}
}

func TestLoadMixinMissingProtoFile(t *testing.T) {
	dir := t.TempDir()
	mixDir := filepath.Join(dir, "mixins", "my-mixin")
	if err := os.MkdirAll(mixDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `kind: mixin
name: my-mixin
version: 1.0.0
adds_slots:
  - name: on_change
    proto: stego.mixins.my_mixin.slots.OnChange
    default: noop
`
	if err := os.WriteFile(filepath.Join(mixDir, "mixin.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.Load(dir)
	if err == nil {
		t.Fatal("expected error for missing mixin proto file, got nil")
	}
	if !strings.Contains(err.Error(), "proto file missing") {
		t.Errorf("expected proto file missing error, got: %v", err)
	}
}

func TestLoadMixinWithProtoFiles(t *testing.T) {
	dir := t.TempDir()
	mixDir := filepath.Join(dir, "mixins", "my-mixin")
	slotsDir := filepath.Join(mixDir, "slots")
	if err := os.MkdirAll(slotsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `kind: mixin
name: my-mixin
version: 1.0.0
adds_slots:
  - name: on_change
    proto: stego.mixins.my_mixin.slots.OnChange
    default: noop
`
	if err := os.WriteFile(filepath.Join(mixDir, "mixin.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slotsDir, "on_change.proto"), []byte("syntax = \"proto3\";\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	m := reg.Mixin("my-mixin")
	if m == nil {
		t.Fatal("Mixin(my-mixin) returned nil")
	}
	if len(m.AddsSlots) != 1 {
		t.Errorf("AddsSlots count = %d, want 1", len(m.AddsSlots))
	}
}

func TestLoadComponentMissingProtoFile(t *testing.T) {
	dir := t.TempDir()
	compDir := filepath.Join(dir, "components", "my-comp")
	if err := os.MkdirAll(compDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Component references a slot but no proto file exists.
	yaml := `kind: component
name: my-comp
version: 1.0.0
stage: ga
slots:
  - name: before_create
    proto: stego.components.my_comp.slots.BeforeCreate
    default: passthrough
`
	if err := os.WriteFile(filepath.Join(compDir, "component.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.Load(dir)
	if err == nil {
		t.Fatal("expected error for missing proto file, got nil")
	}
	if !strings.Contains(err.Error(), "proto file missing") {
		t.Errorf("expected proto file missing error, got: %v", err)
	}
}

func TestLoadComponentWithProtoFiles(t *testing.T) {
	dir := t.TempDir()
	compDir := filepath.Join(dir, "components", "my-comp")
	slotsDir := filepath.Join(compDir, "slots")
	if err := os.MkdirAll(slotsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `kind: component
name: my-comp
version: 1.0.0
stage: ga
slots:
  - name: my_slot
    proto: stego.components.my_comp.slots.MySlot
    default: passthrough
`
	if err := os.WriteFile(filepath.Join(compDir, "component.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slotsDir, "my_slot.proto"), []byte("syntax = \"proto3\";\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	c := reg.Component("my-comp")
	if c == nil {
		t.Fatal("Component(my-comp) returned nil")
	}
	if len(c.Slots) != 1 {
		t.Errorf("Slots count = %d, want 1", len(c.Slots))
	}
}

func TestLoadConfigEmptyRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("registry: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for empty registry sources, got nil")
	}
	if !strings.Contains(err.Error(), "at least one registry source") {
		t.Errorf("expected 'at least one registry source' error, got: %v", err)
	}
}

func TestLoadConfigMissingURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("registry:\n  - ref: abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for missing url, got nil")
	}
	if !strings.Contains(err.Error(), "url is required") {
		t.Errorf("expected 'url is required' error, got: %v", err)
	}
}

func TestLoadConfigMissingRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("registry:\n  - url: git.example.com/reg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := registry.LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for missing ref, got nil")
	}
	if !strings.Contains(err.Error(), "ref is required") {
		t.Errorf("expected 'ref is required' error, got: %v", err)
	}
}

// --- Parsing of archetype, component and mixin documents ---

// parserFixture is a registry document under testdata/parser.
func parserFixture(name string) string {
	return filepath.Join(testdataDir, "parser", name)
}

func TestParseArchetype(t *testing.T) {
	a, err := registry.ParseArchetype(parserFixture("archetype.yaml"))
	if err != nil {
		t.Fatalf("ParseArchetype: %v", err)
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
	if a.Conventions.CORS != "enabled" {
		t.Errorf("conventions.cors = %q, want %q", a.Conventions.CORS, "enabled")
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
	c, err := registry.ParseComponent(parserFixture("component.yaml"))
	if err != nil {
		t.Fatalf("ParseComponent: %v", err)
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
	m, err := registry.ParseMixin(parserFixture("mixin.yaml"))
	if err != nil {
		t.Fatalf("ParseMixin: %v", err)
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

func TestParseArchetypeTyped(t *testing.T) {
	a, err := registry.ParseArchetype(parserFixture("archetype.yaml"))
	if err != nil {
		t.Fatalf("ParseArchetype: %v", err)
	}
	if a.Name != "rest-crud" {
		t.Errorf("name = %q, want %q", a.Name, "rest-crud")
	}
}

func TestParseComponentTyped(t *testing.T) {
	c, err := registry.ParseComponent(parserFixture("component.yaml"))
	if err != nil {
		t.Fatalf("ParseComponent: %v", err)
	}
	if c.Name != "rest-api" {
		t.Errorf("name = %q, want %q", c.Name, "rest-api")
	}
}

func TestParseMixinTyped(t *testing.T) {
	m, err := registry.ParseMixin(parserFixture("mixin.yaml"))
	if err != nil {
		t.Fatalf("ParseMixin: %v", err)
	}
	if m.Name != "event-publisher" {
		t.Errorf("name = %q, want %q", m.Name, "event-publisher")
	}
}

func TestParseTypedWrongKind(t *testing.T) {
	// wrong_kind.yaml has kind: mixin, parsing as archetype should fail
	_, err := registry.ParseArchetype(parserFixture("wrong_kind.yaml"))
	if err == nil {
		t.Fatal("expected error for wrong kind, got nil")
	}
	var pe *parser.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *parser.ParseError", err)
	}
}

func TestParseTypedFileNotFound(t *testing.T) {
	_, err := registry.ParseArchetype(parserFixture("does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pe *parser.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *parser.ParseError", err)
	}
}

func TestRoundTripArchetype(t *testing.T) {
	a, err := registry.ParseArchetype(parserFixture("archetype.yaml"))
	if err != nil {
		t.Fatalf("ParseArchetype: %v", err)
	}
	data, err := yaml.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var a2 registry.Archetype
	if err := yaml.Unmarshal(data, &a2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if !reflect.DeepEqual(*a, a2) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  got:      %+v", *a, a2)
	}
}

func TestRoundTripComponent(t *testing.T) {
	c, err := registry.ParseComponent(parserFixture("component.yaml"))
	if err != nil {
		t.Fatalf("ParseComponent: %v", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var c2 registry.Component
	if err := yaml.Unmarshal(data, &c2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if !reflect.DeepEqual(*c, c2) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  got:      %+v", *c, c2)
	}
}

func TestRoundTripMixin(t *testing.T) {
	m, err := registry.ParseMixin(parserFixture("mixin.yaml"))
	if err != nil {
		t.Fatalf("ParseMixin: %v", err)
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m2 registry.Mixin
	if err := yaml.Unmarshal(data, &m2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if !reflect.DeepEqual(*m, m2) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  got:      %+v", *m, m2)
	}
}

func TestParseErrorLineInfoFromRealYAML(t *testing.T) {
	// Feed a YAML file with a type mismatch (version: [1,2,3] where string expected)
	// through the real parser and verify that parseErrorWithLineInfo populates
	// Line and Context from the yaml.TypeError.
	_, err := registry.ParseArchetype(parserFixture("type_mismatch.yaml"))
	if err == nil {
		t.Fatal("expected error for type mismatch YAML, got nil")
	}
	var pe *parser.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *parser.ParseError", err)
	}
	if pe.Path != parserFixture("type_mismatch.yaml") {
		t.Errorf("path = %q, want %q", pe.Path, parserFixture("type_mismatch.yaml"))
	}
	if pe.Line <= 0 {
		t.Errorf("Line = %d, want > 0 (line info should be extracted from yaml.TypeError)", pe.Line)
	}
	if pe.Context == "" {
		t.Errorf("Context = %q, want non-empty (should contain snippet of offending line)", pe.Context)
	}
	// The error message should include the line number and context
	errMsg := pe.Error()
	if pe.Line > 0 && !strings.Contains(errMsg, fmt.Sprintf(":%d:", pe.Line)) {
		t.Errorf("Error() = %q, should contain line number %d", errMsg, pe.Line)
	}
}

func TestArchetypeUnmarshal(t *testing.T) {
	input := `
kind: archetype
runtime: {module: github.com/jsell-rh/stego/registry/runtime, version: v0.1.0, sum: 'h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='}
name: rest-crud
language: go
version: 3.0.0
components:
  - rest-api
  - postgres-adapter
  - otel-tracing
  - health-check
default_auth: jwt-auth
conventions:
  layout: flat
  error_handling: problem-details-rfc
  logging: structured-json
  test_pattern: table-driven
  cors: enabled
compatible_mixins:
  - event-publisher
  - async-worker
bindings:
  storage-adapter: postgres-adapter
  auth-provider: jwt-auth
`
	var a registry.Archetype
	if err := yaml.Unmarshal([]byte(input), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Name != "rest-crud" {
		t.Errorf("name = %q, want %q", a.Name, "rest-crud")
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
	if a.Conventions.CORS != "enabled" {
		t.Errorf("conventions.cors = %q, want %q", a.Conventions.CORS, "enabled")
	}
	if len(a.Bindings) != 2 {
		t.Errorf("bindings count = %d, want 2", len(a.Bindings))
	}
}

func TestComponentUnmarshalWithConfig(t *testing.T) {
	// Uses the spec's full component YAML, including nested items for both
	// map-of-fields (expose.items) and leaf-list (operations.items) forms.
	input := `
kind: component
name: rest-api
version: 2.1.0
stage: ga
config:
  port:
    type: int
    default: 8080
  expose:
    type: list
    items:
      entity:
        type: entity-ref
      operations:
        type: list
        items:
          enum: [create, read, update, delete, list]
      scope:
        type: field-ref
        optional: true
requires:
  - auth-provider
provides:
  - http-server
`
	var c registry.Component
	if err := yaml.Unmarshal([]byte(input), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	expose, ok := c.Config["expose"]
	if !ok {
		t.Fatal("config missing 'expose' key")
	}
	if expose.Type != "list" {
		t.Errorf("config.expose.type = %q, want %q", expose.Type, "list")
	}
	// expose.items is a map of named sub-fields
	if expose.Items == nil || expose.Items.Fields == nil {
		t.Fatal("config.expose.items.Fields is nil, want map with 3 entries")
	}
	if len(expose.Items.Fields) != 3 {
		t.Fatalf("config.expose.items.Fields count = %d, want 3", len(expose.Items.Fields))
	}
	entityItem, ok := expose.Items.Fields["entity"]
	if !ok {
		t.Fatal("config.expose.items missing 'entity'")
	}
	if entityItem.Type != "entity-ref" {
		t.Errorf("config.expose.items.entity.type = %q, want %q", entityItem.Type, "entity-ref")
	}
	scopeItem := expose.Items.Fields["scope"]
	if !scopeItem.Optional {
		t.Error("config.expose.items.scope.optional = false, want true")
	}
	// operations.items is a single inline types.ConfigField (leaf-list)
	opsItem, ok := expose.Items.Fields["operations"]
	if !ok {
		t.Fatal("config.expose.items missing 'operations'")
	}
	if opsItem.Type != "list" {
		t.Errorf("config.expose.items.operations.type = %q, want %q", opsItem.Type, "list")
	}
	if opsItem.Items == nil || opsItem.Items.Inline == nil {
		t.Fatal("config.expose.items.operations.items.Inline is nil, want inline types.ConfigField")
	}
	if len(opsItem.Items.Inline.Enum) != 5 {
		t.Errorf("operations.items.enum count = %d, want 5", len(opsItem.Items.Inline.Enum))
	}
	if opsItem.Items.Inline.Enum[0] != "create" {
		t.Errorf("operations.items.enum[0] = %q, want %q", opsItem.Items.Inline.Enum[0], "create")
	}
}

func TestComponentUnmarshal(t *testing.T) {
	input := `
kind: component
name: rest-api
version: 2.1.0
stage: ga
output_namespace: internal/api
requires:
  - auth-provider
  - storage-adapter
provides:
  - http-server
  - openapi-spec
slots:
  - name: before_create
    proto: stego.components.rest_api.slots.BeforeCreate
    default: passthrough
  - name: validate
    proto: stego.components.rest_api.slots.Validate
    default: passthrough
`
	var c registry.Component
	if err := yaml.Unmarshal([]byte(input), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Name != "rest-api" {
		t.Errorf("name = %q, want %q", c.Name, "rest-api")
	}
	if c.OutputNamespace != "internal/api" {
		t.Errorf("output_namespace = %q, want %q", c.OutputNamespace, "internal/api")
	}
	if len(c.Requires) != 2 {
		t.Errorf("requires count = %d, want 2", len(c.Requires))
	}
	if c.Requires[0].Name != "auth-provider" {
		t.Errorf("requires[0].name = %q, want %q", c.Requires[0].Name, "auth-provider")
	}
	if c.Requires[1].Name != "storage-adapter" {
		t.Errorf("requires[1].name = %q, want %q", c.Requires[1].Name, "storage-adapter")
	}
	if len(c.Provides) != 2 {
		t.Errorf("provides count = %d, want 2", len(c.Provides))
	}
	if c.Provides[0].Name != "http-server" {
		t.Errorf("provides[0].name = %q, want %q", c.Provides[0].Name, "http-server")
	}
	if len(c.Slots) != 2 {
		t.Errorf("slots count = %d, want 2", len(c.Slots))
	}
	if c.Slots[0].Default != "passthrough" {
		t.Errorf("slots[0].default = %q, want %q", c.Slots[0].Default, "passthrough")
	}
}

func TestMixinUnmarshal(t *testing.T) {
	input := `
kind: mixin
name: event-publisher
version: 1.0.0
adds_components:
  - kafka-producer
adds_slots:
  - name: on_entity_changed
    proto: stego.mixins.event_publisher.slots.OnEntityChanged
    default: noop
overrides: none
`
	var m registry.Mixin
	if err := yaml.Unmarshal([]byte(input), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Name != "event-publisher" {
		t.Errorf("name = %q, want %q", m.Name, "event-publisher")
	}
	if len(m.AddsComponents) != 1 || m.AddsComponents[0] != "kafka-producer" {
		t.Errorf("adds_components = %v, want [kafka-producer]", m.AddsComponents)
	}
	if len(m.AddsSlots) != 1 {
		t.Errorf("adds_slots count = %d, want 1", len(m.AddsSlots))
	}
	if m.Overrides != "none" {
		t.Errorf("overrides = %q, want %q", m.Overrides, "none")
	}
}

func TestStrictRegistryDocuments(t *testing.T) {
	tests := []struct {
		name, input, want string
		target            any
	}{
		{"archetype", "kind: archetype\nruntime: {module: github.com/jsell-rh/stego/registry/runtime, version: v0.1.0, sum: 'h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='}\nconventions: {loging: json}\n", "conventions.loging", &registry.Archetype{}},
		{"component", "kind: component\nconfig: {port: {type: int, defaut: 8080}}\n", "config.port.defaut", &registry.Component{}},
		{"nested component", "kind: component\nconfig: {items: {type: list, items: {name: {type: string, optonal: true}}}}\n", "items.name.optonal", &registry.Component{}},
		{"inline component", "kind: component\nconfig: {items: {type: list, items: {type: string, optonal: true}}}\n", "items.optonal", &registry.Component{}},
		{"port", "kind: component\nrequires: [{name: auth, optonal: true}]\n", "requires[0].optonal", &registry.Component{}},
		{"mixin", "kind: mixin\nadds_slots: [{name: test, protto: test}]\n", "adds_slots[0].protto", &registry.Mixin{}},
		{"config", "registry: [{url: example, reff: abc}]\n", "registry[0].reff", &registry.RegistryConfig{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := parser.DecodeStrict([]byte(tt.input), tt.name+".yaml", tt.target); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error containing %q", err, tt.want)
			}
		})
	}
}
