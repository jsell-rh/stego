package types

import "time"

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
	Kind     string                 `yaml:"kind"`
	Name     string                 `yaml:"name"`
	Version  string                 `yaml:"version"`
	Config   map[string]ConfigField `yaml:"config,omitempty"`
	Requires []Port                  `yaml:"requires"`
	Provides []Port                  `yaml:"provides"`
	Slots    []SlotDefinition       `yaml:"slots"`
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

// ExposeBlock declares which entity operations are exposed.
type ExposeBlock struct {
	Entity      string          `yaml:"entity"`
	Operations  []Operation     `yaml:"operations"`
	Scope       string          `yaml:"scope,omitempty"`
	Parent      string          `yaml:"parent,omitempty"`
	PathPrefix  string          `yaml:"path_prefix,omitempty"`
	UpsertKey   []string        `yaml:"upsert_key,omitempty"`
	Concurrency ConcurrencyMode `yaml:"concurrency,omitempty"`
}

// SlotDeclaration describes how a slot is bound in a service declaration.
type SlotDeclaration struct {
	Slot         string   `yaml:"slot"`
	Entity       string   `yaml:"entity,omitempty"`
	Gate         []string `yaml:"gate,omitempty"`
	Chain        []string `yaml:"chain,omitempty"`
	FanOut       []string `yaml:"fan-out,omitempty"`
	ShortCircuit bool     `yaml:"short_circuit,omitempty"`
}

// ServiceDeclaration is the product team's service definition.
type ServiceDeclaration struct {
	Kind      string            `yaml:"kind"`
	Name      string            `yaml:"name"`
	Archetype string            `yaml:"archetype"`
	Language  string            `yaml:"language"`
	Entities  []Entity          `yaml:"entities"`
	Expose    []ExposeBlock     `yaml:"expose"`
	Slots     []SlotDeclaration `yaml:"slots"`
	Mixins    []string          `yaml:"mixins,omitempty"`
	Overrides map[string]any    `yaml:"overrides,omitempty"`
}

// Fill represents a human-authored implementation of a slot contract.
type Fill struct {
	Kind        string    `yaml:"kind"`
	Name        string    `yaml:"name"`
	Implements  string    `yaml:"implements"`
	Entity      string    `yaml:"entity"`
	QualifiedBy string    `yaml:"qualified_by"`
	QualifiedAt time.Time `yaml:"qualified_at"`
}
