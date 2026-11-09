package types

// DeclaredInvariant is one rule across the stored instances of entities that the declaration states and generation
// emits as a constraint of the store (ST-DECL-EN17): its name, its kind (unique, no_overlap or sum_limit) and the
// fields its kind needs.
type DeclaredInvariant struct {
	Name   string   `yaml:"name"`
	Kind   string   `yaml:"kind"`
	On     string   `yaml:"on"`
	Where  string   `yaml:"where,omitempty"`
	By     []string `yaml:"by,omitempty"`
	Range  string   `yaml:"range,omitempty"`
	Parent string   `yaml:"parent,omitempty"`
}
