package types

// SchemaChange is one change to the stored shape of an entity after the service's first release (ST-DECL-EN21):
// its ID, its kind, the entity and the field it changes, and what its kind needs to move the rows. The declaration
// keeps every schema change, also a finished one.
type SchemaChange struct {
	// ID is the change's name in lower kebab case, unique in the declaration.
	ID string `yaml:"id"`
	// Kind is add, rename, retype, require, drop or unique.
	Kind string `yaml:"kind"`
	// Entity is the entity whose table the change changes.
	Entity string `yaml:"entity"`
	// Field is the field the change is about, by its name after the change; for unique, the name of the declared
	// invariant of kind unique.
	Field string `yaml:"field,omitempty"`
	// From is, for rename, the field's name before the change, and for retype, the field's type before it.
	From string `yaml:"from,omitempty"`
	// Conversion is, for retype, the conversion from the old type to the new one, such as text_to_integer.
	Conversion string `yaml:"conversion,omitempty"`
	// InverseConversion is, for retype, the conversion from the new type back to the old one.
	InverseConversion string `yaml:"inverse_conversion,omitempty"`
	// FillValue is, for require, the value each row that exists gets; for retype, the value of a row whose old value
	// does not convert; for drop, the default of the old column at expand.
	FillValue any `yaml:"fill_value,omitempty"`
	// Definition is, for drop, the removed field's type, constraints and default, such as string, max_length 64, or
	// the removed entity's fields.
	Definition string `yaml:"definition,omitempty"`
}

// SchemaStep is one entry of the declaration's ordered schema history (ST-DECL-EN22): a schema change and the phase
// it moves to, expand or contract. The phase of a change is the phase of its last step.
type SchemaStep struct {
	Change string `yaml:"change"`
	Phase  string `yaml:"phase"`
}
