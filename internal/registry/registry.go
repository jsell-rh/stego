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
//	<dir>/mixins/<name>/mixin.yaml
func Load(dir string) (*Registry, error) {
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
		path := filepath.Join(dir, entry.Name(), "archetype.yaml")
		a, err := parser.ParseArchetype(path)
		if err != nil {
			return fmt.Errorf("loading archetype %s: %w", entry.Name(), err)
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
		path := filepath.Join(dir, entry.Name(), "component.yaml")
		c, err := parser.ParseComponent(path)
		if err != nil {
			return fmt.Errorf("loading component %s: %w", entry.Name(), err)
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
		path := filepath.Join(dir, entry.Name(), "mixin.yaml")
		m, err := parser.ParseMixin(path)
		if err != nil {
			return fmt.Errorf("loading mixin %s: %w", entry.Name(), err)
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
	return &cfg, nil
}
