// Package registry implements loading and indexing of stego registry artifacts
// (archetypes, components, mixins) from a local directory structure.
package registry

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
)

// Registry holds indexed archetypes, components, and mixins loaded from a
// registry directory.
type Registry struct {
	source     *registrySnapshot
	captured   []*registrySnapshot
	archetypes map[string]*Archetype
	components map[string]*Component
	mixins     map[string]*Mixin
	sources    []SourceParts
}

// Archetypes returns all loaded archetypes.
func (r *Registry) Archetypes() map[string]*Archetype {
	return r.archetypes
}

// Components returns all loaded components.
func (r *Registry) Components() map[string]*Component {
	return r.components
}

// Mixins returns all loaded mixins.
func (r *Registry) Mixins() map[string]*Mixin {
	return r.mixins
}

// Archetype looks up an archetype by name. Returns nil if not found.
func (r *Registry) Archetype(name string) *Archetype {
	return r.archetypes[name]
}

// Component looks up a component by name. Returns nil if not found.
func (r *Registry) Component(name string) *Component {
	return r.components[name]
}

// Mixin looks up a mixin by name. Returns nil if not found.
func (r *Registry) Mixin(name string) *Mixin {
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
// A refusal names each source by its folder.
func LoadDirectories(dirs ...string) (*Registry, error) {
	sources := make([]Source, len(dirs))
	for i, dir := range dirs {
		sources[i] = Source{Name: dir, Dir: dir}
	}
	return LoadSources(sources...)
}

// Source is one registry source to load: the folder it was resolved to and
// the name a refusal gives it, such as its location and its commit.
type Source struct {
	Name string
	Dir  string
}

// partCategories are the folders whose children are parts of the registry:
// one part per child folder, which no two sources may both supply.
var partCategories = map[string]string{"components": "component", "archetypes": "archetype", "mixins": "mixin", "ports": "port"}

// LoadSources captures every source before it validates any, then refuses a
// part or an input path that two sources supply, even as identical copies,
// naming the part and both sources, and indexes every archetype, component
// and mixin.
func LoadSources(sources ...Source) (*Registry, error) {
	if len(sources) < 1 || len(sources) > maxRegistrySources {
		return nil, fmt.Errorf("registry composition has %d sources; the limit is %d", len(sources), maxRegistrySources)
	}
	captured := make([]*registrySnapshot, 0, len(sources))
	var total int64
	files := 0
	for _, src := range sources {
		current, err := captureRegistryBounded(src.Dir, true, maxRegistryBytes-total, maxRegistryFiles-files)
		if err != nil {
			return nil, fmt.Errorf("registry source %s: %w", src.Name, err)
		}
		for _, file := range current.files {
			total += file.size
		}
		files += len(current.files)
		captured = append(captured, current)
	}
	source := &registrySnapshot{files: map[string]registryFile{}, directories: map[string]bool{}}
	partOwner := map[string]int{}
	fileOwner := map[string]int{}
	for i, current := range captured {
		for _, name := range slices.Sorted(maps.Keys(current.directories)) {
			if kind, isPart := partCategories[path.Dir(name)]; isPart {
				if first, exists := partOwner[name]; exists {
					return nil, fmt.Errorf("registry %s %q occurs in more than one source: it is supplied by both %s and %s; a later source may add parts but not replace one", kind, path.Base(name), sources[first].Name, sources[i].Name)
				}
				partOwner[name] = i
			}
			source.directories[name] = true
		}
		for _, name := range slices.Sorted(maps.Keys(current.files)) {
			if first, exists := fileOwner[name]; exists {
				return nil, fmt.Errorf("registry input %s occurs in more than one source: it is supplied by both %s and %s", name, sources[first].Name, sources[i].Name)
			}
			fileOwner[name] = i
			source.files[name] = current.files[name]
		}
	}
	source.hash = registryDigest(source.files)
	r := &Registry{source: source, captured: captured, archetypes: make(map[string]*Archetype), components: make(map[string]*Component), mixins: make(map[string]*Mixin)}
	for i, src := range sources {
		parts := SourceParts{Name: src.Name, Trust: TrustOf(i)}
		for name, owner := range partOwner {
			if owner == i {
				parts.Parts = append(parts.Parts, partCategories[path.Dir(name)]+" "+path.Base(name))
			}
		}
		slices.Sort(parts.Parts)
		r.sources = append(r.sources, parts)
	}
	for _, name := range r.childDirectories("archetypes") {
		file := path.Join("archetypes", name, "archetype.yaml")
		data, err := r.ReadFile(file)
		if err != nil {
			return nil, err
		}
		item, err := ParseArchetypeFromBytes(data, file)
		if err != nil {
			return nil, err
		}
		if item.Name != name {
			return nil, fmt.Errorf("archetype name mismatch: directory %q but YAML name %q in %s", name, item.Name, file)
		}
		if err := checkRuntimePin(item); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		r.archetypes[name] = item
	}
	for _, name := range r.childDirectories("components") {
		file := path.Join("components", name, "component.yaml")
		data, err := r.ReadFile(file)
		if err != nil {
			return nil, err
		}
		item, err := ParseComponentFromBytes(data, file)
		if err != nil {
			return nil, err
		}
		if item.Name != name {
			return nil, fmt.Errorf("component name mismatch: directory %q but YAML name %q in %s", name, item.Name, file)
		}
		if err := r.checkSlots("components", name, item.Slots); err != nil {
			return nil, err
		}
		if err := checkStage(item); err != nil {
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
		item, err := ParseMixinFromBytes(data, file)
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
	if err := checkGoldenPaths(r); err != nil {
		return nil, err
	}
	if err := checkExperimentPorts(r); err != nil {
		return nil, err
	}
	return r, nil
}

// SourceParts is one source of the active registry: its name, its trust and
// the parts it adds, each as its kind and its name.
type SourceParts struct {
	Name  string
	Trust Trust
	Parts []string
}

// Sources gives each source of the active registry with the parts it adds,
// in the order of composition.
func (r *Registry) Sources() []SourceParts {
	return r.sources
}

func (r *Registry) checkSlots(category, name string, slots []SlotDefinition) error {
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
func LoadConfig(path string) (*RegistryConfig, error) {
	data, err := parser.ReadDocument(path)
	if err != nil {
		return nil, fmt.Errorf("reading registry config: %w", err)
	}
	return ParseConfig(data, path)
}

// ParseConfig validates an already captured configuration without another file read.
func ParseConfig(data []byte, path string) (*RegistryConfig, error) {
	var cfg RegistryConfig
	if err := parser.DecodeStrict(data, path, &cfg); err != nil {
		return nil, fmt.Errorf("parsing registry config %s: %w", path, err)
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("invalid registry config %s: %w", path, err)
	}
	return &cfg, nil
}

// validateConfig checks that a parsed RegistryConfig has required fields.
func validateConfig(cfg *RegistryConfig) error {
	if len(cfg.Pins) != 0 {
		return fmt.Errorf("per-component pins are not supported; pin each Git registry source with ref; remove the pins entry for %s", strings.Join(slices.Sorted(maps.Keys(cfg.Pins)), ", "))
	}
	if len(cfg.Registry) == 0 {
		return fmt.Errorf("at least one registry source is required")
	}
	if len(cfg.Registry) > maxRegistrySources {
		return fmt.Errorf("%d registry sources are configured; the limit is %d", len(cfg.Registry), maxRegistrySources)
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
