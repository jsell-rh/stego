package declaration

import (
	"fmt"
	"strings"
	"testing"
)

// todoFields gives ST-DECL-TD02 with the fields of Todo given, the modes given (none when empty), the schema
// changes and the schema history.
func todoFields(fields []string, modes string, changes []string, steps ...string) string {
	service := strings.Replace(td02, "      - {name: title, type: string, min_length: 1, max_length: 255}\n      - {name: done, type: bool, default: false}\n", "      - "+strings.Join(fields, "\n      - ")+"\n", 1)
	if modes != "" {
		service += "modes: " + modes + "\n"
	}
	if len(changes) > 0 {
		service += "schema_changes:\n  - " + strings.Join(changes, "\n  - ") + "\n"
	}
	if len(steps) > 0 {
		var items []string
		for _, step := range steps {
			change, phase, _ := strings.Cut(step, " ")
			items = append(items, fmt.Sprintf("{change: %s, phase: %s}", change, phase))
		}
		service += "schema_history:\n  - " + strings.Join(items, "\n  - ") + "\n"
	}
	return service
}

var (
	// renamed are the fields of Todo after the rename of title to summary.
	renamed = []string{"{name: summary, type: string, min_length: 1, max_length: 255}", "{name: done, type: bool, default: false}"}
	rename  = "{id: todo-title-rename, kind: rename, entity: Todo, field: summary, from: title}"
)

func schemaFaults(t *testing.T, service string, rules ...string) []ValidationError {
	t.Helper()
	return compositionFaults(t, service, Facts{}, rules...)
}

func TestSchemaChangeKind(t *testing.T) {
	t.Run("ST-DECL-INV57-T A rename with its expand step is accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, todoFields(renamed, "", []string{rename}, "todo-title-rename expand")))
	})
	priority := append(append([]string(nil), renamed...), "{name: priority, type: int64}", "{name: owner, type: string}")
	for _, c := range []struct{ name, change, names string }{
		{"the kind move", "{id: todo-title-rename, kind: move, entity: Todo, field: summary, from: title}", `"move"`},
		{"the ID TodoRename", "{id: TodoRename, kind: rename, entity: Todo, field: summary, from: title}", `"TodoRename"`},
		{"a rename with no From", "{id: todo-title-rename, kind: rename, entity: Todo, field: summary}", "no from"},
		{"a rename with a Fill value", "{id: todo-title-rename, kind: rename, entity: Todo, field: summary, from: title, fill_value: x}", "fill_value"},
		{"a retype with no Inverse conversion", "{id: todo-title-rename, kind: retype, entity: Todo, field: priority, from: string, conversion: text_to_integer}", "no inverse_conversion"},
		{"a require with no Fill value", "{id: todo-title-rename, kind: require, entity: Todo, field: owner}", "no fill_value"},
		{"a drop with no Definition", "{id: todo-title-rename, kind: drop, entity: Todo, field: legacy}", "no definition"},
	} {
		t.Run("ST-DECL-INV57-T A schema change with "+c.name+" is refused", func(t *testing.T) {
			faults := schemaFaults(t, todoFields(priority, "", []string{c.change}, strings.SplitN(c.change, ",", 2)[0][5:]+" expand"), "schema-change-kind")
			wantFault(t, faults, c.names)
		})
	}
	t.Run("ST-DECL-INV57-T A second change with the ID todo-title-rename is refused", func(t *testing.T) {
		service := todoFields(renamed, "", []string{rename, "{id: todo-title-rename, kind: add, entity: Todo, field: done}"}, "todo-title-rename expand")
		wantFault(t, schemaFaults(t, service, "schema-change-kind"), `"todo-title-rename"`)
	})
}

func TestSchemaChangeEntity(t *testing.T) {
	titled := []string{"{name: summary, type: string}", "{name: title, type: string}", "{name: done, type: bool, default: false}"}
	t.Run("ST-DECL-INV58-T A rename that agrees with Todo is accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, todoFields(renamed, "", []string{rename}, "todo-title-rename expand")))
	})
	for _, c := range []struct {
		name   string
		fields []string
		change string
		field  string
	}{
		{"the rename while Todo still declares title", titled, rename, "summary"},
		{"a rename to a field that Todo does not declare", renamed, "{id: todo-title-rename, kind: rename, entity: Todo, field: headline, from: title}", "headline"},
		{"a drop of done while Todo still declares done", renamed, "{id: todo-title-rename, kind: drop, entity: Todo, field: done, definition: bool}", "done"},
		{"a drop with no field of Todo", renamed, "{id: todo-title-rename, kind: drop, entity: Todo, definition: \"summary: string\"}", ""},
		{"a unique that names no declared invariant", renamed, "{id: todo-title-rename, kind: unique, entity: Todo, field: unique_summary}", "unique_summary"},
		{"a unique that names a declared invariant of kind no_overlap", renamed, "{id: todo-title-rename, kind: unique, entity: Todo, field: summary_window}", "summary_window"},
		{"a retype from the type that the field has now", renamed, "{id: todo-title-rename, kind: retype, entity: Todo, field: summary, from: string, conversion: text_to_text, inverse_conversion: text_to_text}", "summary"},
	} {
		t.Run("ST-DECL-INV58-T "+c.name+" is refused", func(t *testing.T) {
			service := todoFields(c.fields, "", []string{c.change}, "todo-title-rename expand") + "invariants:\n  - {name: summary_window, kind: no_overlap, on: Todo, range: during, by: [summary]}\n"
			faults := schemaFaults(t, service, "schema-change-entity")
			wantFault(t, faults, `"todo-title-rename"`, `"Todo"`, `"`+c.field+`"`)
		})
	}
	t.Run("ST-DECL-INV58-T A unique that names a declared invariant of kind unique is accepted", func(t *testing.T) {
		service := todoFields(renamed, "", []string{"{id: todo-unique-summary, kind: unique, entity: Todo, field: unique_summary}"}, "todo-unique-summary expand") +
			"invariants:\n  - {name: unique_summary, kind: unique, on: Todo, by: [summary]}\n"
		wantNoFault(t, schemaFaults(t, service))
	})
}

func TestSchemaSteps(t *testing.T) {
	note := "{id: note-add, kind: add, entity: Todo, field: note}"
	noted := append(append([]string(nil), renamed...), "{name: note, type: string, optional: true}")
	t.Run("ST-DECL-INV59-T The steps expand then contract are accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, todoFields(renamed, "", []string{rename}, "todo-title-rename expand", "todo-title-rename contract")))
	})
	for _, c := range []struct {
		name    string
		fields  []string
		changes []string
		steps   []string
		names   string
	}{
		{"a contract step alone", renamed, []string{rename}, []string{"todo-title-rename contract"}, `"todo-title-rename"`},
		{"the steps contract then expand", renamed, []string{rename}, []string{"todo-title-rename contract", "todo-title-rename expand"}, `"todo-title-rename"`},
		{"two expand steps", renamed, []string{rename}, []string{"todo-title-rename expand", "todo-title-rename expand"}, `"todo-title-rename"`},
		{"two contract steps", renamed, []string{rename}, []string{"todo-title-rename expand", "todo-title-rename contract", "todo-title-rename contract"}, `"todo-title-rename"`},
		{"a step for the change todo-unknown", renamed, []string{rename}, []string{"todo-title-rename expand", "todo-unknown expand"}, `"todo-unknown"`},
		{"the change with no step", renamed, []string{rename}, nil, `"todo-title-rename"`},
		{"an add change with an expand and a contract step", noted, []string{note}, []string{"note-add expand", "note-add contract"}, `"note-add"`},
	} {
		t.Run("ST-DECL-INV59-T "+c.name+" are refused", func(t *testing.T) {
			wantFault(t, schemaFaults(t, todoFields(c.fields, "", c.changes, c.steps...), "schema-step"), c.names)
		})
	}
}

func TestSchemaChangeOpen(t *testing.T) {
	fields := []string{"{name: summary, type: int64}", "{name: done, type: bool, default: false}"}
	retype := "{id: summary-retype, kind: retype, entity: Todo, field: summary, from: string, conversion: text_to_integer, inverse_conversion: integer_to_text}"
	t.Run("ST-DECL-INV60-T A second open change on summary is refused", func(t *testing.T) {
		faults := schemaFaults(t, todoFields(fields, "", []string{rename, retype}, "todo-title-rename expand", "summary-retype expand"), "schema-change-open")
		wantFault(t, faults, `"todo-title-rename"`, `"summary-retype"`, "summary")
	})
	t.Run("ST-DECL-INV60-T A change after a finished one is accepted", func(t *testing.T) {
		service := todoFields(fields, "", []string{rename, retype}, "todo-title-rename expand", "todo-title-rename contract", "summary-retype expand")
		wantNoFault(t, schemaFaults(t, service))
	})
}

func TestSchemaChangeMode(t *testing.T) {
	noted := append(append([]string(nil), renamed...), "{name: note, type: string, optional: true}")
	t.Run("ST-DECL-INV61-T A rename at expand with the modes api is refused, and checking adds no mode", func(t *testing.T) {
		service := todoFields(renamed, "[api]", []string{rename}, "todo-title-rename expand")
		wantFault(t, schemaFaults(t, service, "schema-change-mode"), `"todo-title-rename"`, "controller")
		declared := readDeclaration(t, service)
		ValidateComposition(declared, Facts{})
		if strings.Join(declared.Modes, ",") != "api" {
			t.Errorf("checking changed the modes to %v", declared.Modes)
		}
	})
	for _, c := range []struct{ name, service string }{
		{"a rename at expand with no modes", todoFields(renamed, "", []string{rename}, "todo-title-rename expand")},
		{"a rename at contract with the modes api", todoFields(renamed, "[api]", []string{rename}, "todo-title-rename expand", "todo-title-rename contract")},
		{"only an add at expand with the modes api", todoFields(noted, "[api]", []string{"{id: note-add, kind: add, entity: Todo, field: note}"}, "note-add expand")},
	} {
		t.Run("ST-DECL-INV61-T "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, schemaFaults(t, c.service))
		})
	}
}

func TestSchemaChangeValues(t *testing.T) {
	with := func(owner, change string) string {
		fields := append(append([]string(nil), renamed...), owner)
		return todoFields(fields, "", []string{change}, strings.SplitN(change, ",", 2)[0][5:]+" expand")
	}
	required := "{name: owner, type: string, max_length: 20}"
	t.Run("ST-DECL-INV62-T A require of owner with the fill value nobody is accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, with(required, "{id: owner-require, kind: require, entity: Todo, field: owner, fill_value: nobody}")))
	})
	for _, c := range []struct{ name, owner, change, names string }{
		{"a require of a field with a default", "{name: owner, type: string, max_length: 20, default: x}", "{id: owner-require, kind: require, entity: Todo, field: owner, fill_value: nobody}", `"owner-require"`},
		{"a fill value longer than 20", required, "{id: owner-require, kind: require, entity: Todo, field: owner, fill_value: " + strings.Repeat("n", 21) + "}", "max_length 20"},
		{"a drop of legacy whose Definition is int64 with the fill value abc", required, "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: int64, fill_value: abc}", "abc"},
	} {
		t.Run("ST-DECL-INV62-T "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, schemaFaults(t, with(c.owner, c.change), "schema-change-value"), c.names)
		})
	}
}

func TestSchemaChangeCount(t *testing.T) {
	many := func(contract bool) string {
		fields := append([]string(nil), renamed...)
		changes := []string{rename}
		steps := []string{"todo-title-rename expand"}
		if contract {
			steps = append(steps, "todo-title-rename contract")
		}
		for i := 1; i <= 127; i++ {
			fields = append(fields, fmt.Sprintf("{name: note_%d, type: string, optional: true}", i))
			changes = append(changes, fmt.Sprintf("{id: note-%d-add, kind: add, entity: Todo, field: note_%d}", i, i))
			steps = append(steps, fmt.Sprintf("note-%d-add expand", i))
		}
		return todoFields(fields, "", changes, steps...)
	}
	t.Run("ST-DECL-INV64-T 128 open schema changes are refused", func(t *testing.T) {
		wantFault(t, schemaFaults(t, many(false), "schema-change-count"), "128")
	})
	t.Run("ST-DECL-INV64-T 127 open schema changes, with the rename at contract, are accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, many(true)))
	})
}
