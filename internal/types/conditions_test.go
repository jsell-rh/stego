package types

import "testing"

func TestConditionDeclarationsRequireBoundedOwnedGenerations(t *testing.T) {
	valid := Entity{Name: "Widget", Versioned: true, GenerationFields: []string{"name"}, Conditions: map[string][]string{"worker": {"Ready"}}, Fields: []Field{{Name: "name", Type: FieldTypeString}}}
	if errs := ValidateVersioned([]Entity{valid}); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, change := range []func(*Entity){
		func(e *Entity) { e.Versioned = false }, func(e *Entity) { e.GenerationFields = nil },
		func(e *Entity) { e.Conditions = map[string][]string{"invalid-owner": {"Ready"}} },
		func(e *Entity) { e.Conditions = map[string][]string{"worker": {}} },
		func(e *Entity) { e.Conditions = map[string][]string{"worker": {"Ready", "Ready"}} },
		func(e *Entity) { e.Conditions = map[string][]string{"worker": {"ready"}} },
		func(e *Entity) { e.Fields = append(e.Fields, Field{Name: "condition_state", Type: FieldTypeString}) },
		func(e *Entity) {
			e.Conditions = nil
			e.Fields = append(e.Fields, Field{Name: "stego_conditions", Type: FieldTypeString})
		},
	} {
		candidate := valid
		change(&candidate)
		if errs := ValidateVersioned([]Entity{candidate}); len(errs) == 0 {
			t.Fatal("invalid condition declaration accepted", candidate)
		}
	}
}
