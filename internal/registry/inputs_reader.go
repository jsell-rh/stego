package registry

import (
	"encoding/json"
	"sort"
)

// DeclarableInput is one input a declaration may give the active component set of one archetype (ST-SUP-RM01): a
// component, and either one setting its schema declares or one slot it declares. A component with neither has one
// row that names only the component.
type DeclarableInput struct {
	Archetype        string   `json:"archetype"`
	Component        string   `json:"component"`
	ComponentVersion string   `json:"component_version"`
	Setting          string   `json:"setting"`
	SettingType      string   `json:"setting_type"`
	AllowedValues    []string `json:"allowed_values"`
	Optional         *bool    `json:"optional"`
	Default          any      `json:"default"`
	Slot             string   `json:"slot"`
	SlotContract     string   `json:"slot_contract"`
}

// InputsReader holds the declarable inputs that one run built in memory from the active component set and hands
// them to the query handler (ST-SUP-RM01-RD). The handler reads nothing else, and nothing is kept between runs.
type InputsReader struct {
	archetype string
	rows      []DeclarableInput
}

// NewInputsReader builds the declarable inputs of an archetype from its active component set: each component in
// name order, then each setting of its schema in name order, then each slot it declares in its order. A setting's
// type is the schema's type, or string for a set of allowed values with no type. A setting is optional when the
// schema marks it optional or gives it a default.
func NewInputsReader(archetype string, components map[string]*Component) InputsReader {
	reader := InputsReader{archetype: archetype, rows: []DeclarableInput{}}
	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		component := components[name]
		row := DeclarableInput{Archetype: archetype, Component: name, ComponentVersion: component.Version}
		if len(component.Config) == 0 && len(component.Slots) == 0 {
			reader.rows = append(reader.rows, row)
			continue
		}
		settings := make([]string, 0, len(component.Config))
		for setting := range component.Config {
			settings = append(settings, setting)
		}
		sort.Strings(settings)
		for _, setting := range settings {
			field := component.Config[setting]
			input := row
			input.Setting, input.SettingType = setting, field.Type
			if input.SettingType == "" && len(field.Enum) > 0 {
				input.SettingType = "string"
			}
			input.AllowedValues = append([]string(nil), field.Enum...)
			optional := field.Optional || field.Default != nil
			input.Optional = &optional
			input.Default = field.Default
			reader.rows = append(reader.rows, input)
		}
		for _, slot := range component.Slots {
			input := row
			input.Slot, input.SlotContract = slot.Name, slot.Proto
			reader.rows = append(reader.rows, input)
		}
	}
	return reader
}

// Rows gives every declarable input in order.
func (r InputsReader) Rows() []DeclarableInput {
	return append([]DeclarableInput(nil), r.rows...)
}

// JSON gives the declarable inputs as one JSON document: the archetype and its rows.
func (r InputsReader) JSON() ([]byte, error) {
	return json.MarshalIndent(struct {
		Archetype string            `json:"archetype"`
		Rows      []DeclarableInput `json:"rows"`
	}{r.archetype, r.rows}, "", "  ")
}
