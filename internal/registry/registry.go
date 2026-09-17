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
	captured   []*registrySnapshot
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
	return LoadDirectories(dir)
}

// LoadDirectories combines distinct artifacts. A later source cannot replace
// an artifact or input path from an earlier source, even with identical bytes.
func LoadDirectories(dirs ...string) (*Registry, error) {
	if len(dirs) < 1 || len(dirs) > 8 {
		return nil, fmt.Errorf("registry requires one through eight sources")
	}
	source := &registrySnapshot{files: map[string]registryFile{}, directories: map[string]bool{}}
	captured := make([]*registrySnapshot, 0, len(dirs))
	var total int64
	for _, dir := range dirs {
		current, err := captureRegistryBounded(dir, true, maxRegistryBytes-total, maxRegistryFiles-len(source.files))
		if err != nil {
			return nil, fmt.Errorf("registry directory: %w", err)
		}
		for name := range current.directories {
			if source.directories[name] && (path.Dir(name) == "components" || path.Dir(name) == "archetypes" || path.Dir(name) == "mixins") {
				return nil, fmt.Errorf("registry artifact %q occurs in more than one source", name)
			}
			source.directories[name] = true
		}
		for name, file := range current.files {
			if _, exists := source.files[name]; exists {
				return nil, fmt.Errorf("registry input %q occurs in more than one source", name)
			}
			source.files[name] = file
			total += file.size
		}
		captured = append(captured, current)
	}
	source.hash = registryDigest(source.files)
	r := &Registry{source: source, captured: captured, archetypes: make(map[string]*types.Archetype), components: make(map[string]*types.Component), mixins: make(map[string]*types.Mixin)}
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
	return ParseConfig(data, path)
}

// ParseConfig validates an already captured configuration without another file read.
func ParseConfig(data []byte, path string) (*types.RegistryConfig, error) {
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
	if len(cfg.Pins) != 0 {
		return fmt.Errorf("per-component pins are not supported; pin each Git registry source with ref")
	}
	if len(cfg.Registry) == 0 {
		return fmt.Errorf("at least one registry source is required")
	}
	if len(cfg.Registry) > 8 {
		return fmt.Errorf("at most eight registry sources are allowed")
	}
	for i, src := range cfg.Registry {
		if len(src.URL) > 2048 || len(src.Ref) > 128 || len(src.Path) > 1024 || len(src.Vendor) > 1024 {
			return fmt.Errorf("registry[%d]: source setting exceeds its length limit", i)
		}
		if src.Vendor != "" {
			if err := gen.ValidatePath(src.Vendor); err != nil || !commitID.MatchString(src.Ref) {
				return fmt.Errorf("registry[%d]: vendor requires a relative path and full commit SHA", i)
			}
		}
		if src.Path != "" {
			if err := gen.ValidatePath(src.Path); err != nil {
				return fmt.Errorf("registry[%d]: invalid path: %w", i, err)
			}
		}
		if src.URL == "" {
			return fmt.Errorf("registry[%d]: url is required", i)
		}
		if src.Ref == "" {
			return fmt.Errorf("registry[%d]: ref is required", i)
		}
	}
	return nil
}
