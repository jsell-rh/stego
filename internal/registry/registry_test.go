package registry_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/registry"
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
		if c.Version != "2.1.0" {
			t.Errorf("Version = %q, want %q", c.Version, "2.1.0")
		}
		if len(c.Config) != 2 {
			t.Errorf("Config count = %d, want 2", len(c.Config))
		}
		portCfg, ok := c.Config["port"]
		if !ok {
			t.Fatal("Config[port] missing")
		}
		if portCfg.Type != "int" {
			t.Errorf("Config[port].Type = %q, want %q", portCfg.Type, "int")
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
		if len(c.Slots) != 2 {
			t.Errorf("Slots count = %d, want 2", len(c.Slots))
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

	if len(cfg.Pins) != 2 {
		t.Fatalf("expected 2 pins, got %d", len(cfg.Pins))
	}
	if cfg.Pins["rest-api"] != "f4e5d6c7b8a9" {
		t.Errorf("Pins[rest-api] = %q, want %q", cfg.Pins["rest-api"], "f4e5d6c7b8a9")
	}
	if cfg.Pins["postgres-adapter"] != "3a2b1c0d" {
		t.Errorf("Pins[postgres-adapter] = %q, want %q", cfg.Pins["postgres-adapter"], "3a2b1c0d")
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
	if err := os.WriteFile(filepath.Join(archDir, "archetype.yaml"), []byte("kind: archetype\nname: bar\nversion: 1.0.0\nlanguage: go\n"), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(compDir, "component.yaml"), []byte("kind: component\nname: bar\nversion: 1.0.0\n"), 0o644); err != nil {
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
