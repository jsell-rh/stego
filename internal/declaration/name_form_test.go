package declaration

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
)

// nameFaults reads ST-DECL-TD02 with one text replaced and gives its name-form faults.
func nameFaults(t *testing.T, old, new string) []ValidationError {
	t.Helper()
	data := strings.Replace(td02Fields, "%s", "      - {name: title, type: string, min_length: 1, max_length: 255}", 1)
	if old != "" {
		if !strings.Contains(data, old) {
			t.Fatalf("TD02 does not hold %q", old)
		}
		data = strings.Replace(data, old, new, 1)
	}
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	var faults []ValidationError
	for _, fault := range Validate(service).All() {
		if fault.Rule == "name-form" {
			faults = append(faults, fault)
		}
	}
	return faults
}

func TestNameForms(t *testing.T) {
	t.Run("ST-DECL-INV86-T A service, collection, entity or field name outside its form is refused with its path", func(t *testing.T) {
		cases := []struct{ name, old, new, path, found string }{
			{"a service name with a quote", "name: todo-svc", `name: "todo\"svc"`, "name", `todo"svc`},
			{"a service name in upper case", "name: todo-svc", "name: Todo-svc", "name", "Todo-svc"},
			{"a service name that starts with a hyphen", "name: todo-svc", "name: -todo", "name", "-todo"},
			{"an entity name with a backtick", "  - name: Todo", "  - name: \"To`do\"", "entities[0].name", "To`do"},
			{"an entity name in lower case", "  - name: Todo", "  - name: todo", "entities[0].name", "todo"},
			{"a field name with a semicolon", "{name: title,", `{name: "ti;tle",`, "entities[0].fields[0].name", "ti;tle"},
			{"a field name in camel case", "{name: title,", "{name: myTitle,", "entities[0].fields[0].name", "myTitle"},
			{"a collection name with a brace", "  todos:", "  \"to}dos\":", "collections.to}dos", "to}dos"},
			{"a collection name with a newline", "  todos:", "  \"to\\ndos\":", "collections.to\ndos", "to\ndos"},
		}
		for _, c := range cases {
			faults := nameFaults(t, c.old, c.new)
			if len(faults) != 1 || faults[0].Path != c.path || faults[0].Found != c.found || len(faults[0].Allowed) != 1 {
				t.Errorf("%s: got %+v, want one name-form fault at %s", c.name, faults, c.path)
			}
		}
		if faults := nameFaults(t, "", ""); len(faults) != 0 {
			t.Errorf("TD02 got name-form faults: %+v", faults)
		}
	})
}
