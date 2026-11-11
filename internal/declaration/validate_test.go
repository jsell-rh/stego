package declaration

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
)

// td02Fields is ST-DECL-TD02 with its fields left for each case to give.
const td02Fields = `kind: service
name: todo-svc
archetype: rest-crud
language: go
base_path: /api/todo/v1
entities:
  - name: Todo
    fields:
%s
collections:
  todos:
    entity: Todo
    operations: [create, read, update, delete, list]
`

func defaultFaults(t *testing.T, fields ...string) []ValidationError {
	t.Helper()
	data := strings.Replace(td02Fields, "%s", "      "+strings.Join(fields, "\n      "), 1)
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	var faults []ValidationError
	for _, fault := range Validate(service).All() {
		if fault.Rule == "default-type" {
			faults = append(faults, fault)
		}
	}
	return faults
}

func TestValidateFieldDefaults(t *testing.T) {
	title := "- {name: title, type: string, min_length: 1, max_length: 255}"
	done := "- {name: done, type: bool, default: false}"
	refused := []struct {
		name, field, default_, typ string
		fields                     []string
	}{
		{"a bool with the default yes", "done", "yes", "bool", []string{title, `- {name: done, type: bool, default: "yes"}`}},
		{"a string with the default 12", "title", "12", "string", []string{"- {name: title, type: string, default: 12}", done}},
		{"an int with the default 1.5", "count", "1.5", "int32", []string{title, done, "- {name: count, type: int32, default: 1.5}"}},
		{"an enum with the default archived", "state", "archived", "enum", []string{title, done, "- {name: state, type: enum, values: [open, closed], default: archived}"}},
		{"a timestamp generation with a default", "generation", "x; panic(1)", "timestamp", []string{title, done, `- {name: generation, type: timestamp, default: "x; panic(1)"}`}},
	}
	for _, c := range refused {
		t.Run("ST-DECL-INV13-T A field default that does not suit the field's type is refused: "+c.name, func(t *testing.T) {
			faults := defaultFaults(t, c.fields...)
			if len(faults) != 1 {
				t.Fatalf("want one default fault, got %+v", faults)
			}
			fault := faults[0]
			for _, part := range []string{`"Todo"`, `"` + c.field + `"`, c.default_, `"` + c.typ + `"`} {
				if !strings.Contains(fault.Message, part) {
					t.Errorf("the fault does not name %s: %s", part, fault.Message)
				}
			}
			if fault.Found != c.default_ {
				t.Errorf("the fault found %q, want %q", fault.Found, c.default_)
			}
		})
	}
	t.Run("ST-DECL-INV13-T A field default that suits the field's type is accepted", func(t *testing.T) {
		if faults := defaultFaults(t, title, "- {name: done, type: bool, default: true}", "- {name: count, type: int32, default: 0}"); len(faults) != 0 {
			t.Fatalf("want no default fault, got %+v", faults)
		}
	})
}
