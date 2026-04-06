// Package registry implements loading and indexing of stego registry artifacts
// (archetypes, components, mixins) from a local directory structure.
package registry

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/stego-project/stego/internal/parser"
	"github.com/stego-project/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// Registry holds indexed archetypes, components, and mixins loaded from a
// registry directory.
type Registry struct {
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
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("registry directory: %w", err)
	}

	r := &Registry{
		archetypes: make(map[string]*types.Archetype),
		components: make(map[string]*types.Component),
		mixins:     make(map[string]*types.Mixin),
	}

	if err := r.loadArchetypes(filepath.Join(dir, "archetypes")); err != nil {
		return nil, err
	}
	if err := r.loadComponents(filepath.Join(dir, "components")); err != nil {
		return nil, err
	}
	if err := r.loadMixins(filepath.Join(dir, "mixins")); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *Registry) loadArchetypes(dir string) error {
	entries, err := readDirIfExists(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirName := entry.Name()
		path := filepath.Join(dir, dirName, "archetype.yaml")
		a, err := parser.ParseArchetype(path)
		if err != nil {
			return fmt.Errorf("loading archetype %s: %w", dirName, err)
		}
		if a.Name != dirName {
			return fmt.Errorf("archetype name mismatch: directory %q but YAML name %q in %s", dirName, a.Name, path)
		}
		r.archetypes[a.Name] = a
	}
	return nil
}

func (r *Registry) loadComponents(dir string) error {
	entries, err := readDirIfExists(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirName := entry.Name()
		path := filepath.Join(dir, dirName, "component.yaml")
		c, err := parser.ParseComponent(path)
		if err != nil {
			return fmt.Errorf("loading component %s: %w", dirName, err)
		}
		if c.Name != dirName {
			return fmt.Errorf("component name mismatch: directory %q but YAML name %q in %s", dirName, c.Name, path)
		}
		// Verify that slot proto files exist on disk.
		for _, slot := range c.Slots {
			protoPath := filepath.Join(dir, dirName, "slots", slot.Name+".proto")
			if _, err := os.Stat(protoPath); err != nil {
				return fmt.Errorf("component %s slot %q: proto file missing at %s", dirName, slot.Name, protoPath)
			}
		}
		r.components[c.Name] = c
	}
	return nil
}

func (r *Registry) loadMixins(dir string) error {
	entries, err := readDirIfExists(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirName := entry.Name()
		path := filepath.Join(dir, dirName, "mixin.yaml")
		m, err := parser.ParseMixin(path)
		if err != nil {
			return fmt.Errorf("loading mixin %s: %w", dirName, err)
		}
		if m.Name != dirName {
			return fmt.Errorf("mixin name mismatch: directory %q but YAML name %q in %s", dirName, m.Name, path)
		}
		// Verify that adds_slots proto files exist on disk.
		for _, slot := range m.AddsSlots {
			protoPath := filepath.Join(dir, dirName, "slots", slot.Name+".proto")
			if _, err := os.Stat(protoPath); err != nil {
				return fmt.Errorf("mixin %s slot %q: proto file missing at %s", dirName, slot.Name, protoPath)
			}
		}
		r.mixins[m.Name] = m
	}
	return nil
}

// readDirIfExists returns directory entries, or an empty slice if the directory
// does not exist.
func readDirIfExists(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return entries, err
}

// LoadConfig reads and parses a .stego/config.yaml file.
func LoadConfig(path string) (*types.RegistryConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading registry config: %w", err)
	}
	var cfg types.RegistryConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
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
