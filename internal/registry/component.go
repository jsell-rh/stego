package registry

import "github.com/jsell-rh/stego/internal/types"

// Port represents a named capability that a component requires or provides.
// It unmarshals from either a bare string or a mapping with a "name" key.
type Port struct {
	Name string `yaml:"name"`
}

// YAMLScalarForm marks Port as a parser.ScalarForm: the strict decoder accepts
// a port given by its bare name.
func (Port) YAMLScalarForm() {}

// UnmarshalYAML allows Port to be unmarshaled from a bare string (e.g. "- auth-provider")
// or from a mapping (e.g. "- name: auth-provider").
func (p *Port) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		p.Name = s
		return nil
	}
	type raw Port
	var r raw
	if err := unmarshal(&r); err != nil {
		return err
	}
	*p = Port(r)
	return nil
}

// SlotDefinition represents a slot defined by a component.
type SlotDefinition struct {
	Name    string `yaml:"name"`
	Proto   string `yaml:"proto"`
	Default string `yaml:"default,omitempty"`
}

// Archetype represents a curated component set with conventions.
type Archetype struct {
	Kind             string            `yaml:"kind"`
	Name             string            `yaml:"name"`
	Language         string            `yaml:"language"`
	Version          string            `yaml:"version"`
	Components       []string          `yaml:"components"`
	DefaultAuth      string            `yaml:"default_auth"`
	Conventions      types.Convention  `yaml:"conventions"`
	CompatibleMixins []string          `yaml:"compatible_mixins"`
	Bindings         map[string]string `yaml:"bindings,omitempty"`
	// Runtime is the one runtime library version that every service of the
	// archetype imports (ST-SUP-INV10).
	Runtime *RuntimePin `yaml:"runtime"`
}

// Component represents a deterministic code generator with its metadata.
type Component struct {
	Kind            string                       `yaml:"kind"`
	Name            string                       `yaml:"name"`
	Version         string                       `yaml:"version"`
	Stage           types.Stage                  `yaml:"stage"`
	Sunset          string                       `yaml:"sunset,omitempty"`
	Config          map[string]types.ConfigField `yaml:"config,omitempty"`
	Requires        []Port                       `yaml:"requires"`
	Provides        []Port                       `yaml:"provides"`
	Slots           []SlotDefinition             `yaml:"slots"`
	OutputNamespace string                       `yaml:"output_namespace,omitempty"`
}

// Mixin adds components and slots to an archetype. A mixin with AliasOf is a deprecated alias: binding it binds
// the mixins it names instead, and it adds nothing of its own (ST-SUP-INV17). RequiresMixins are the mixins that
// must be bound with it (ST-SUP-INV16).
type Mixin struct {
	Kind           string           `yaml:"kind"`
	Name           string           `yaml:"name"`
	Version        string           `yaml:"version"`
	AliasOf        []string         `yaml:"alias_of,omitempty"`
	RequiresMixins []string         `yaml:"requires_mixins,omitempty"`
	AddsComponents []string         `yaml:"adds_components"`
	AddsSlots      []SlotDefinition `yaml:"adds_slots"`
	Overrides      string           `yaml:"overrides"` // "none"
}

// RegistrySource describes a single registry origin in .stego/config.yaml.
type RegistrySource struct {
	URL    string `yaml:"url"`
	Ref    string `yaml:"ref"`
	Path   string `yaml:"path,omitempty"`
	Vendor string `yaml:"vendor,omitempty"`
}

// RegistryConfig represents the .stego/config.yaml file.
type RegistryConfig struct {
	Registry []RegistrySource  `yaml:"registry"`
	Pins     map[string]string `yaml:"pins,omitempty"`
}
