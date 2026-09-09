package types

import "testing"

func TestObservationDeclarationsRejectAmbiguousOwnership(t *testing.T) {
	valid := func() Entity {
		return Entity{Name: "Record", Versioned: true, GenerationFields: []string{"desired"}, Observations: map[string][]string{"health": {"status"}}, Fields: []Field{{Name: "desired", Type: FieldTypeString}, {Name: "status", Type: FieldTypeString, Optional: true}}}
	}
	for _, test := range []struct {
		name   string
		change func(*Entity)
	}{
		{"no revisions", func(e *Entity) { e.Versioned = false }},
		{"no inputs", func(e *Entity) { e.GenerationFields = nil }},
		{"unknown input", func(e *Entity) { e.GenerationFields = []string{"unknown"} }},
		{"repeated input", func(e *Entity) { e.GenerationFields = []string{"desired", "desired"} }},
		{"empty group", func(e *Entity) { e.Observations["health"] = nil }},
		{"unsafe group", func(e *Entity) { e.Observations["not/a/group"] = []string{"status"} }},
		{"input overlap", func(e *Entity) { e.GenerationFields = append(e.GenerationFields, "status") }},
		{"group overlap", func(e *Entity) { e.Observations["other"] = []string{"status"} }},
		{"unknown field", func(e *Entity) { e.Observations["health"] = []string{"unknown"} }},
		{"required field", func(e *Entity) { e.Fields[1].Optional = false }},
		{"default observation", func(e *Entity) { e.Fields[1].Default = "Healthy" }},
		{"computed observation", func(e *Entity) { e.Fields[1].Computed = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			entity := valid()
			test.change(&entity)
			if len(ValidateVersioned([]Entity{entity})) == 0 {
				t.Fatal("invalid observation declaration accepted")
			}
		})
	}
	if problems := ValidateVersioned([]Entity{valid()}); len(problems) != 0 {
		t.Fatal(problems)
	}
	for _, collection := range []Collection{{Entity: "Record", UpsertKey: []string{"status"}}, {Entity: "Record", Patchable: []string{"status"}}} {
		if len(ValidateVersioned([]Entity{valid()}, []Collection{collection})) == 0 {
			t.Fatal("collection can mutate an observation")
		}
	}
}

func TestUnobservedValuesAreValidated(t *testing.T) {
	for _, test := range []struct {
		name, value string
		change      func(*Entity)
		valid       bool
	}{
		{name: "empty", valid: true},
		{name: "literal", value: "Pending \\ ' ?", valid: true},
		{name: "non observation", change: func(e *Entity) { e.Observations = nil }},
		{name: "non string", change: func(e *Entity) { e.Fields[1].Type = FieldTypeInt64 }},
		{name: "invalid utf8", value: string([]byte{0xff})},
		{name: "nul", value: "a\x00b"},
		{name: "pattern", value: "Pending", change: func(e *Entity) { e.Fields[1].Pattern = "^Healthy$" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := Entity{Name: "Record", Versioned: true, GenerationFields: []string{"desired"}, Observations: map[string][]string{"worker": {"status"}}, Fields: []Field{{Name: "desired", Type: FieldTypeString}, {Name: "status", Type: FieldTypeString, Optional: true, Unobserved: &test.value}}}
			if test.change != nil {
				test.change(&e)
			}
			problems := ValidateVersioned([]Entity{e})
			if (len(problems) == 0) != test.valid {
				t.Fatal(problems)
			}
		})
	}
}

func TestCleanupOwnerContracts(t *testing.T) {
	for _, test := range []struct {
		name      string
		owners    []string
		versioned bool
		field     string
	}{
		{"not versioned", []string{"worker"}, false, ""},
		{"duplicate", []string{"worker", "worker"}, true, ""},
		{"empty", []string{""}, true, ""},
		{"unsafe", []string{"worker/status"}, true, ""},
		{"metadata", []string{"worker"}, true, "cleanup_state"},
		{"method", []string{"worker"}, true, "cleanup_complete"},
	} {
		e := Entity{Name: "Record", Versioned: test.versioned, CleanupOwners: test.owners}
		if test.field != "" {
			e.Fields = []Field{{Name: test.field, Type: FieldTypeString}}
		}
		if len(ValidateVersioned([]Entity{e})) == 0 {
			t.Fatal("invalid cleanup contract accepted", test.name)
		}
	}
	if result := ValidateVersioned([]Entity{{Name: "Record", Versioned: true, CleanupOwners: []string{"worker", "identity"}}}); len(result) != 0 {
		t.Fatal(result)
	}
}

func TestCleanupTargetsRequireDeclaredOwnersAndRequiredInputFields(t *testing.T) {
	for _, tc := range []struct {
		name    string
		targets map[string]string
		field   Field
		valid   bool
	}{
		{"string", map[string]string{"worker": "target"}, Field{Name: "target", Type: FieldTypeString}, true},
		{"reference", map[string]string{"worker": "target"}, Field{Name: "target", Type: FieldTypeRef}, true},
		{"unknown owner", map[string]string{"other": "target"}, Field{Name: "target", Type: FieldTypeString}, false},
		{"unknown field", map[string]string{"worker": "missing"}, Field{Name: "target", Type: FieldTypeString}, false},
		{"optional", map[string]string{"worker": "target"}, Field{Name: "target", Type: FieldTypeString, Optional: true}, false},
		{"number", map[string]string{"worker": "target"}, Field{Name: "target", Type: FieldTypeInt64}, false},
		{"reserved", map[string]string{"worker": "cleanup_targets"}, Field{Name: "cleanup_targets", Type: FieldTypeString}, false},
	} {
		e := Entity{Name: "Placement", Versioned: true, CleanupOwners: []string{"worker"}, CleanupTargets: tc.targets, Fields: []Field{tc.field}}
		if result := ValidateVersioned([]Entity{e}); (len(result) == 0) != tc.valid {
			t.Fatal(tc.name, result)
		}
	}
}
