package main

import (
	"strings"
	"testing"
)

// sixChanges is ST-DECL-TD02 with one schema change of each kind on Todo, each with an expand step, and the rename
// with a contract step too. Todo declares the fields as they are after the changes. The declaration format has no
// integer type, so priority is an int64.
var sixChanges = strings.Replace(td02, "      - {name: title, type: string, min_length: 1, max_length: 255}\n", `      - {name: summary, type: string, min_length: 1, max_length: 255}
      - {name: note, type: string, optional: true}
      - {name: priority, type: int64}
      - {name: owner, type: string}
`, 1) + `invariants:
  - {name: unique_summary, kind: unique, on: Todo, by: [summary]}
schema_changes:
  - {id: todo-note-add, kind: add, entity: Todo, field: note}
  - {id: todo-title-rename, kind: rename, entity: Todo, field: summary, from: title}
  - {id: todo-priority-retype, kind: retype, entity: Todo, field: priority, from: string, conversion: text_to_integer, inverse_conversion: integer_to_text}
  - {id: todo-owner-require, kind: require, entity: Todo, field: owner, fill_value: nobody}
  - {id: todo-legacy-drop, kind: drop, entity: Todo, field: legacy, definition: "legacy: string, max_length 64"}
  - {id: todo-summary-unique, kind: unique, entity: Todo, field: unique_summary}
schema_history:
  - {change: todo-note-add, phase: expand}
  - {change: todo-title-rename, phase: expand}
  - {change: todo-priority-retype, phase: expand}
  - {change: todo-owner-require, phase: expand}
  - {change: todo-legacy-drop, phase: expand}
  - {change: todo-summary-unique, phase: expand}
  - {change: todo-title-rename, phase: contract}
`

func TestDeclareSchemaChanges(t *testing.T) {
	t.Run("ST-DECL-UC09-R06-T01 A schema change of each kind with its steps is accepted, and a finished change stays accepted", func(t *testing.T) {
		document, run := validateJSON(t, td02Project(t, sixChanges))
		if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
			t.Fatalf("stego validate refused the six changes (exit %d):\n%s\n%s", run.code, run.stdout, run.stderr)
		}
		phases := map[string]any{}
		for _, setting := range document.Settings {
			if strings.HasPrefix(setting.Path, "schema_changes[") {
				phases[setting.Path] = setting.Value
			}
		}
		want := map[string]any{
			"schema_changes[0].phase": "expand", "schema_changes[1].phase": "contract", "schema_changes[2].phase": "expand",
			"schema_changes[3].phase": "expand", "schema_changes[4].phase": "expand", "schema_changes[5].phase": "expand",
		}
		for path, phase := range want {
			if phases[path] != phase {
				t.Errorf("checking reports %s as %v, want %v", path, phases[path], phase)
			}
		}
		if len(phases) != len(want) {
			t.Errorf("checking reports the phases %v, want six", phases)
		}
	})
}
