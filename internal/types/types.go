package types

import (
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// FieldType enumerates the primitive and stego-specific field types.
type FieldType string

const (
	FieldTypeString    FieldType = "string"
	FieldTypeInt32     FieldType = "int32"
	FieldTypeInt64     FieldType = "int64"
	FieldTypeFloat     FieldType = "float"
	FieldTypeDouble    FieldType = "double"
	FieldTypeBool      FieldType = "bool"
	FieldTypeBytes     FieldType = "bytes"
	FieldTypeTimestamp  FieldType = "timestamp"
	FieldTypeEnum      FieldType = "enum"
	FieldTypeRef       FieldType = "ref"
	FieldTypeJsonb     FieldType = "jsonb"
)

// ValidFieldTypes is the set of all valid FieldType values.
var ValidFieldTypes = map[FieldType]bool{
	FieldTypeString:    true,
	FieldTypeInt32:     true,
	FieldTypeInt64:     true,
	FieldTypeFloat:     true,
	FieldTypeDouble:    true,
	FieldTypeBool:      true,
	FieldTypeBytes:     true,
	FieldTypeTimestamp: true,
	FieldTypeEnum:      true,
	FieldTypeRef:       true,
	FieldTypeJsonb:     true,
}

// Operation enumerates the CRUD+ operations.
type Operation string

const (
	OpCreate Operation = "create"
	OpRead   Operation = "read"
	OpUpdate Operation = "update"
	OpDelete Operation = "delete"
	OpList   Operation = "list"
	OpUpsert Operation = "upsert"
)

// ValidOperations is the set of all valid Operation values.
var ValidOperations = map[Operation]bool{
	OpCreate: true,
	OpRead:   true,
	OpUpdate: true,
	OpDelete: true,
	OpList:   true,
	OpUpsert: true,
}

// Field represents a single field within an entity definition.
type Field struct {
	Name           string    `yaml:"name"`
	Type           FieldType `yaml:"type"`
	MinLength      *int      `yaml:"min_length,omitempty"`
	MaxLength      *int      `yaml:"max_length,omitempty"`
	Pattern        string    `yaml:"pattern,omitempty"`
	Min            *float64  `yaml:"min,omitempty"`
	Max            *float64  `yaml:"max,omitempty"`
	Unique         bool      `yaml:"unique,omitempty"`
	UniqueComposite []string `yaml:"unique_composite,omitempty"`
	Optional       bool      `yaml:"optional,omitempty"`
	Default        any       `yaml:"default,omitempty"`
	Values         []string  `yaml:"values,omitempty"`
	Computed       bool      `yaml:"computed,omitempty"`
	FilledBy       string    `yaml:"filled_by,omitempty"`
	To             string    `yaml:"to,omitempty"` // for ref type
}

// Entity represents a domain entity with its fields.
type Entity struct {
	Name   string  `yaml:"name"`
	Fields []Field `yaml:"fields"`
}

// Port represents a named capability that a component requires or provides.
// It unmarshals from either a bare string or a mapping with a "name" key.
type Port struct {
	Name string `yaml:"name"`
}

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

// Convention captures the archetype's conventions.
type Convention struct {
	Layout        string `yaml:"layout"`
	ErrorHandling string `yaml:"error_handling"`
	Logging       string `yaml:"logging"`
	TestPattern   string `yaml:"test_pattern"`
}

// Archetype represents a curated component set with conventions.
type Archetype struct {
	Kind             string     `yaml:"kind"`
	Name             string     `yaml:"name"`
	Language         string     `yaml:"language"`
	Version          string     `yaml:"version"`
	Components       []string   `yaml:"components"`
	DefaultAuth      string     `yaml:"default_auth"`
	Conventions      Convention `yaml:"conventions"`
	CompatibleMixins []string   `yaml:"compatible_mixins"`
	Bindings         map[string]string `yaml:"bindings,omitempty"`
}

// ConfigFieldItems supports two YAML forms for a config field's items:
//   - A map of named sub-fields: items: { entity: {type: entity-ref}, ... }
//   - A single inline ConfigField: items: { enum: [create, read, ...] }
type ConfigFieldItems struct {
	// Fields holds named sub-fields when items is a map of ConfigField values.
	Fields map[string]ConfigField
	// Inline holds a single ConfigField when items is an inline object.
	Inline *ConfigField
}

// MarshalYAML produces output compatible with UnmarshalYAML: a map of named
// sub-fields when Fields is set, or a single inline ConfigField otherwise.
func (i ConfigFieldItems) MarshalYAML() (any, error) {
	if i.Fields != nil {
		return i.Fields, nil
	}
	if i.Inline != nil {
		return i.Inline, nil
	}
	return nil, nil
}

// UnmarshalYAML detects whether items is a map of named sub-fields or a single
// inline ConfigField, and deserializes accordingly.
func (i *ConfigFieldItems) UnmarshalYAML(unmarshal func(any) error) error {
	// Try map-of-named-fields first.
	var fields map[string]ConfigField
	if err := unmarshal(&fields); err == nil {
		// Disambiguate: if every value has a non-empty Type, treat as map of
		// named sub-fields. Otherwise it could be a single ConfigField that
		// has no Type (e.g. {enum: [...]}).
		allHaveType := len(fields) > 0
		for _, f := range fields {
			if f.Type == "" {
				allHaveType = false
				break
			}
		}
		if allHaveType {
			i.Fields = fields
			return nil
		}
	}
	// Fall back to single inline ConfigField.
	var single ConfigField
	if err := unmarshal(&single); err != nil {
		return err
	}
	i.Inline = &single
	return nil
}

// ConfigField represents a single field in a component's config schema.
// Items is recursive to support nested config structures.
type ConfigField struct {
	Type     string            `yaml:"type"`
	Default  any               `yaml:"default,omitempty"`
	Items    *ConfigFieldItems `yaml:"items,omitempty"`
	Optional bool              `yaml:"optional,omitempty"`
	Enum     []string          `yaml:"enum,omitempty"`
}

// Component represents a deterministic code generator with its metadata.
type Component struct {
	Kind            string                 `yaml:"kind"`
	Name            string                 `yaml:"name"`
	Version         string                 `yaml:"version"`
	Config          map[string]ConfigField `yaml:"config,omitempty"`
	Requires        []Port                 `yaml:"requires"`
	Provides        []Port                 `yaml:"provides"`
	Slots           []SlotDefinition       `yaml:"slots"`
	OutputNamespace string                 `yaml:"output_namespace,omitempty"`
}

// Mixin adds components and slots to an archetype.
type Mixin struct {
	Kind          string           `yaml:"kind"`
	Name          string           `yaml:"name"`
	Version       string           `yaml:"version"`
	AddsComponents []string        `yaml:"adds_components"`
	AddsSlots     []SlotDefinition `yaml:"adds_slots"`
	Overrides     string           `yaml:"overrides"` // "none"
}

// ConcurrencyMode for upsert operations.
type ConcurrencyMode string

const (
	ConcurrencyOptimistic ConcurrencyMode = "optimistic"
)

// ValidConcurrencyModes is the set of all valid ConcurrencyMode values.
var ValidConcurrencyModes = map[ConcurrencyMode]bool{
	ConcurrencyOptimistic: true,
}

// Collection is a scoped, operation-constrained access pattern over an entity.
// Multiple collections can reference the same entity. Each collection generates
// its own handler, routes, and wiring. The Name field is populated from the map
// key during parsing and is not serialized to YAML.
type Collection struct {
	Name        string            `yaml:"-"`
	Entity      string            `yaml:"entity"`
	Operations  []Operation       `yaml:"operations"`
	Scope       map[string]string `yaml:"scope,omitempty"`
	PathPrefix  string            `yaml:"path_prefix,omitempty"`
	UpsertKey   []string          `yaml:"upsert_key,omitempty"`
	Concurrency ConcurrencyMode   `yaml:"concurrency,omitempty"`
}

// ScopeField returns the scope field name (key) from the Scope map. For
// single-scope collections, this is the only key. Returns empty string if no
// scope is defined.
func (c Collection) ScopeField() string {
	for k := range c.Scope {
		return k
	}
	return ""
}

// ParentEntity returns the scope value (entity name) from the Scope map. For
// single-scope collections, this is the only value. Returns empty string if
// no scope is defined.
func (c Collection) ParentEntity() string {
	for _, v := range c.Scope {
		return v
	}
	return ""
}

// SlotDeclaration describes how a slot is bound in a service declaration.
type SlotDeclaration struct {
	Slot         string   `yaml:"slot"`
	Collection   string   `yaml:"collection,omitempty"`
	Gate         []string `yaml:"gate,omitempty"`
	Chain        []string `yaml:"chain,omitempty"`
	FanOut       []string `yaml:"fan-out,omitempty"`
	ShortCircuit bool     `yaml:"short_circuit,omitempty"`
}

// UnmarshalYAML detects the deprecated `entity:` key (renamed to `collection:`
// in the Collection type system migration) and returns a migration error.
func (sd *SlotDeclaration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		for i := 0; i < len(value.Content)-1; i += 2 {
			if value.Content[i].Value == "entity" {
				return fmt.Errorf("the 'entity' key in slot declarations has been renamed to 'collection' — please update your service.yaml")
			}
		}
	}
	type rawSD SlotDeclaration
	var raw rawSD
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*sd = SlotDeclaration(raw)
	return nil
}

// ServiceDeclaration is the product team's service definition.
type ServiceDeclaration struct {
	Kind        string            `yaml:"kind"`
	Name        string            `yaml:"name"`
	Archetype   string            `yaml:"archetype"`
	Language    string            `yaml:"language"`
	Entities    []Entity          `yaml:"entities"`
	Collections []Collection      `yaml:"-"`
	Slots       []SlotDeclaration `yaml:"slots"`
	Mixins      []string          `yaml:"mixins,omitempty"`
	Overrides   map[string]any    `yaml:"overrides,omitempty"`
}

// UnmarshalYAML implements custom YAML unmarshaling for ServiceDeclaration.
// It parses the `collections:` key as a named map (preserving insertion order)
// and rejects the legacy `expose:` key with a migration error.
func (sd *ServiceDeclaration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("expected a mapping node for service declaration")
	}

	// First pass: check for legacy expose: key and extract collections.
	for i := 0; i < len(value.Content)-1; i += 2 {
		key := value.Content[i].Value
		if key == "expose" {
			return fmt.Errorf("'expose' is no longer supported; use 'collections' instead — see spec for the named-map format")
		}
	}

	// Decode all standard fields using an alias to avoid recursion.
	type rawSD ServiceDeclaration
	var raw rawSD
	if err := value.Decode(&raw); err != nil {
		return err
	}

	// Parse collections: as an ordered named map.
	for i := 0; i < len(value.Content)-1; i += 2 {
		keyNode := value.Content[i]
		valNode := value.Content[i+1]
		if keyNode.Value != "collections" {
			continue
		}
		if valNode.Kind != yaml.MappingNode {
			return fmt.Errorf("'collections' must be a mapping")
		}
		for j := 0; j < len(valNode.Content)-1; j += 2 {
			collName := valNode.Content[j].Value
			var coll Collection
			if err := valNode.Content[j+1].Decode(&coll); err != nil {
				return fmt.Errorf("parsing collection %q: %w", collName, err)
			}
			coll.Name = collName
			raw.Collections = append(raw.Collections, coll)
		}
		break
	}

	*sd = ServiceDeclaration(raw)
	return nil
}

// MarshalYAML implements custom YAML marshaling for ServiceDeclaration.
// It serializes Collections as an ordered map under the `collections:` key.
func (sd ServiceDeclaration) MarshalYAML() (any, error) {
	// Build an ordered map for collections.
	collMap := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, c := range sd.Collections {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: c.Name}
		// Marshal the collection without the Name field.
		valNode := &yaml.Node{}
		valBytes, err := yaml.Marshal(c)
		if err != nil {
			return nil, fmt.Errorf("marshaling collection %q: %w", c.Name, err)
		}
		if err := yaml.Unmarshal(valBytes, valNode); err != nil {
			return nil, fmt.Errorf("re-encoding collection %q: %w", c.Name, err)
		}
		// valNode is a document node; get the inner mapping.
		if len(valNode.Content) > 0 {
			collMap.Content = append(collMap.Content, keyNode, valNode.Content[0])
		}
	}

	// Build a top-level ordered map preserving field order.
	m := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}

	addField := func(key, value string) {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
		)
	}

	addField("kind", sd.Kind)
	addField("name", sd.Name)
	addField("archetype", sd.Archetype)
	addField("language", sd.Language)

	// Entities.
	if len(sd.Entities) > 0 {
		entNode := &yaml.Node{}
		entBytes, err := yaml.Marshal(sd.Entities)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(entBytes, entNode); err != nil {
			return nil, err
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "entities"},
			entNode.Content[0],
		)
	}

	// Collections.
	if len(sd.Collections) > 0 {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "collections"},
			&collMap,
		)
	}

	// Slots.
	if len(sd.Slots) > 0 {
		sNode := &yaml.Node{}
		sBytes, err := yaml.Marshal(sd.Slots)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(sBytes, sNode); err != nil {
			return nil, err
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "slots"},
			sNode.Content[0],
		)
	}

	// Mixins.
	if len(sd.Mixins) > 0 {
		mNode := &yaml.Node{}
		mBytes, err := yaml.Marshal(sd.Mixins)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(mBytes, mNode); err != nil {
			return nil, err
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mixins"},
			mNode.Content[0],
		)
	}

	// Overrides.
	if len(sd.Overrides) > 0 {
		keys := make([]string, 0, len(sd.Overrides))
		for k := range sd.Overrides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		oNode := &yaml.Node{}
		oBytes, err := yaml.Marshal(sd.Overrides)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(oBytes, oNode); err != nil {
			return nil, err
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "overrides"},
			oNode.Content[0],
		)
	}

	return &m, nil
}

// Fill represents a human-authored implementation of a slot contract.
type Fill struct {
	Kind        string    `yaml:"kind"`
	Name        string    `yaml:"name"`
	Implements  string    `yaml:"implements"`
	Collection  string    `yaml:"collection"`
	QualifiedBy string    `yaml:"qualified_by"`
	QualifiedAt time.Time `yaml:"qualified_at"`
}

// UnmarshalYAML detects the deprecated `entity:` key (renamed to `collection:`
// in the Collection type system migration) and returns a migration error.
func (f *Fill) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		for i := 0; i < len(value.Content)-1; i += 2 {
			if value.Content[i].Value == "entity" {
				return fmt.Errorf("the 'entity' key in fill declarations has been renamed to 'collection' — please update your fill.yaml")
			}
		}
	}
	type rawFill Fill
	var raw rawFill
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*f = Fill(raw)
	return nil
}

// RegistrySource describes a single registry origin in .stego/config.yaml.
type RegistrySource struct {
	URL string `yaml:"url"`
	Ref string `yaml:"ref"`
}

// RegistryConfig represents the .stego/config.yaml file.
type RegistryConfig struct {
	Registry []RegistrySource  `yaml:"registry"`
	Pins     map[string]string `yaml:"pins,omitempty"`
}
