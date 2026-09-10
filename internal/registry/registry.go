// Package registry implements loading and indexing of stego registry artifacts
// (archetypes, components, mixins) from a local directory structure.
package registry

import (
	"fmt"
	"path"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// Registry holds indexed archetypes, components, and mixins loaded from a
// registry directory.
type Registry struct {
	source     *registrySnapshot
	archetypes map[string]*types.Archetype
	components map[string]*types.Component
	mixins     map[string]*types.Mixin
}

// Archetypes returns all loaded archetypes.
func (r *Registry) Archetypes() map[string]*types.Archetype {
	return r.archetypes
}

// Components returns all loaded components.
func (r *Registry) Components() map[string]*types.Component {
	return r.components
}

// Mixins returns all loaded mixins.
func (r *Registry) Mixins() map[string]*types.Mixin {
	return r.mixins
}

// Archetype looks up an archetype by name. Returns nil if not found.
func (r *Registry) Archetype(name string) *types.Archetype {
	return r.archetypes[name]
}

// Component looks up a component by name. Returns nil if not found.
func (r *Registry) Component(name string) *types.Component {
	return r.components[name]
}

// Mixin looks up a mixin by name. Returns nil if not found.
func (r *Registry) Mixin(name string) *types.Mixin {
	return r.mixins[name]
}

// Load reads a registry directory and indexes all archetypes, components, and
// mixins it contains. The expected layout is:
//
//	<dir>/archetypes/<name>/archetype.yaml
//	<dir>/components/<name>/component.yaml
//	<dir>/components/<name>/slots/*.proto
//	<dir>/mixins/<name>/mixin.yaml
func Load(dir string) (*Registry, error) {
	source, err := captureRegistry(dir, true)
	if err != nil {
		return nil, fmt.Errorf("registry directory: %w", err)
	}
	r := &Registry{source: source, archetypes: make(map[string]*types.Archetype), components: make(map[string]*types.Component), mixins: make(map[string]*types.Mixin)}
	for _, name := range r.childDirectories("archetypes") {
		file := path.Join("archetypes", name, "archetype.yaml")
		data, err := r.ReadFile(file)
		if err != nil {
			return nil, err
		}
		item, err := parser.ParseArchetypeFromBytes(data, file)
		if err != nil {
			return nil, err
		}
		if item.Name != name {
			return nil, fmt.Errorf("archetype name mismatch: directory %q but YAML name %q in %s", name, item.Name, file)
		}
		r.archetypes[name] = item
	}
	for _, name := range r.childDirectories("components") {
		file := path.Join("components", name, "component.yaml")
		data, err := r.ReadFile(file)
		if err != nil {
			return nil, err
		}
		item, err := parser.ParseComponentFromBytes(data, file)
		if err != nil {
			return nil, err
		}
		if item.Name != name {
			return nil, fmt.Errorf("component name mismatch: directory %q but YAML name %q in %s", name, item.Name, file)
		}
		if err := r.checkSlots("components", name, item.Slots); err != nil {
			return nil, err
		}
		r.components[name] = item
	}
	for _, name := range r.childDirectories("mixins") {
		file := path.Join("mixins", name, "mixin.yaml")
		data, err := r.ReadFile(file)
		if err != nil {
			return nil, err
		}
		item, err := parser.ParseMixinFromBytes(data, file)
		if err != nil {
			return nil, err
		}
		if item.Name != name {
			return nil, fmt.Errorf("mixin name mismatch: directory %q but YAML name %q in %s", name, item.Name, file)
		}
		if err := r.checkSlots("mixins", name, item.AddsSlots); err != nil {
			return nil, err
		}
		r.mixins[name] = item
	}
	return r, nil
}

func (r *Registry) checkSlots(category, name string, slots []types.SlotDefinition) error {
	for _, slot := range slots {
		file := category + "/" + name + "/slots/" + slot.Name + ".proto"
		if err := gen.ValidatePath(file); err != nil {
			return err
		}
		if _, present := r.source.files[file]; !present {
			return fmt.Errorf("%s %s slot %q: proto file missing at %s", category, name, slot.Name, file)
		}
	}
	return nil
}

// LoadConfig reads and parses a .stego/config.yaml file.
func LoadConfig(path string) (*types.RegistryConfig, error) {
	data, err := parser.ReadDocument(path)
	if err != nil {
		return nil, fmt.Errorf("reading registry config: %w", err)
	}
	var cfg types.RegistryConfig
	if err := parser.DecodeStrict(data, path, &cfg); err != nil {
		return nil, fmt.Errorf("parsing registry config %s: %w", path, err)
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("invalid registry config %s: %w", path, err)
	}
	return &cfg, nil
}

// validateConfig checks that a parsed RegistryConfig has required fields.
func validateConfig(cfg *types.RegistryConfig) error {
	if len(cfg.Registry) == 0 {
		return fmt.Errorf("at least one registry source is required")
	}
	for i, src := range cfg.Registry {
		if src.URL == "" {
			return fmt.Errorf("registry[%d]: url is required", i)
		}
		if src.Ref == "" {
			return fmt.Errorf("registry[%d]: ref is required", i)
		}
	}
	return nil
}
