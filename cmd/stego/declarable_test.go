package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/registry"
)

// declarableReport is the structured report of what may be declared for an archetype.
type declarableReport struct {
	Archetype string                     `json:"archetype"`
	Rows      []registry.DeclarableInput `json:"rows"`
}

// reportFor runs the report for the archetype as structured data in a project with the configuration of
// ST-DECL-TD01.
func reportFor(t *testing.T, archetype string) (declarableReport, stegoRun) {
	t.Helper()
	run := stegoIn(t, declaringProject(t), nil, "registry", "inspect", "--archetype", archetype, "--json")
	var report declarableReport
	if run.code == 0 {
		decoder := json.NewDecoder(strings.NewReader(run.stdout))
		if err := decoder.Decode(&report); err != nil {
			t.Fatalf("the report is not one JSON document: %v\n%s", err, run.stdout)
		}
		var extra json.RawMessage
		if err := decoder.Decode(&extra); err == nil {
			t.Fatalf("the report printed more than one document:\n%s", run.stdout)
		}
	}
	return report, run
}

// fixtureFile reads one YAML file of the fixture registry into a map.
func fixtureFile(t *testing.T, path ...string) map[string]any {
	t.Helper()
	registryDir, _ := declaringRegistry(t)
	data, err := os.ReadFile(filepath.Join(append([]string{registryDir}, path...)...))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func stringList(value any) []string {
	var list []string
	items, _ := value.([]any)
	for _, item := range items {
		switch item := item.(type) {
		case string:
			list = append(list, item)
		case map[string]any:
			if name, ok := item["name"].(string); ok {
				list = append(list, name)
			}
		}
	}
	return list
}

// expectedInputs reads, from the fixture registry's own files, what the report must hold for an archetype: the
// archetype's components and default auth, and for each port that one of them requires the component the
// archetype binds to it, until no more are added; each with its settings by type and allowed values, and its
// slots.
func expectedInputs(t *testing.T, archetype string) map[string]string {
	t.Helper()
	definition := fixtureFile(t, "archetypes", archetype, "archetype.yaml")
	bindings, _ := definition["bindings"].(map[string]any)
	queue := stringList(definition["components"])
	if auth, ok := definition["default_auth"].(string); ok && auth != "" {
		queue = append(queue, auth)
	}
	expected := map[string]string{}
	seen := map[string]bool{}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		component := fixtureFile(t, "components", name, "component.yaml")
		expected[name] = fmt.Sprintf("component %s %v", name, component["version"])
		config, _ := component["config"].(map[string]any)
		for setting, value := range config {
			field, _ := value.(map[string]any)
			kind, _ := field["type"].(string)
			if kind == "" && field["enum"] != nil {
				kind = "string"
			}
			expected[name+" setting "+setting] = fmt.Sprintf("%s %v", kind, stringList(field["enum"]))
		}
		for _, slot := range stringList(component["slots"]) {
			expected[name+" slot "+slot] = "slot"
		}
		for _, port := range stringList(component["requires"]) {
			if target, ok := bindings[port].(string); ok {
				queue = append(queue, target)
			}
		}
	}
	return expected
}

// reportedInputs gives what a report holds, in the form of expectedInputs.
func reportedInputs(report declarableReport) map[string]string {
	reported := map[string]string{}
	for _, row := range report.Rows {
		reported[row.Component] = fmt.Sprintf("component %s %s", row.Component, row.ComponentVersion)
		if row.Setting != "" {
			allowed := row.AllowedValues
			if allowed == nil {
				allowed = []string{}
			}
			reported[row.Component+" setting "+row.Setting] = fmt.Sprintf("%s %v", row.SettingType, allowed)
		}
		if row.Slot != "" {
			reported[row.Component+" slot "+row.Slot] = "slot"
		}
	}
	return reported
}

func compareInputs(t *testing.T, expected, reported map[string]string) {
	t.Helper()
	var differences []string
	for key, want := range expected {
		if got, ok := reported[key]; !ok {
			differences = append(differences, "missing: "+key)
		} else if got != want && !(strings.HasSuffix(want, " []") && strings.HasSuffix(got, " []")) {
			differences = append(differences, fmt.Sprintf("differs: %s: report %q, registry %q", key, got, want))
		}
	}
	for key := range reported {
		if _, ok := expected[key]; !ok {
			differences = append(differences, "one more: "+key)
		}
	}
	sort.Strings(differences)
	if len(differences) > 0 {
		t.Fatalf("the report and the fixture registry differ:\n%s", strings.Join(differences, "\n"))
	}
}

// valueOfType gives a value of a setting's reported type: its first allowed value, or a value of the type.
func valueOfType(row registry.DeclarableInput) any {
	if len(row.AllowedValues) > 0 {
		return row.AllowedValues[0]
	}
	switch row.SettingType {
	case "bool":
		return true
	case "int":
		return 1
	case "float", "number":
		return 1.5
	case "list":
		return []any{}
	case "entity-ref":
		return "Todo"
	case "field-ref":
		return "title"
	}
	return "value"
}

// withSetting gives ST-DECL-TD02 with one setting of one component given a value.
func withSetting(t *testing.T, component, setting string, value any) string {
	t.Helper()
	overrides, err := yaml.Marshal(map[string]any{"overrides": map[string]any{component: map[string]any{setting: value}}})
	if err != nil {
		t.Fatal(err)
	}
	return td02 + string(overrides)
}

// undeclaredSettingFaults gives the faults that say a setting or its component is not declared.
func undeclaredSettingFaults(t *testing.T, project string) []string {
	t.Helper()
	document, _ := validateJSON(t, project)
	var faults []string
	for _, fault := range document.Faults {
		if fault.Rule == "undeclared-field" || strings.Contains(fault.Message, "unknown configuration field") || strings.Contains(fault.Message, "component is not active") {
			faults = append(faults, fault.Prose())
		}
	}
	return faults
}

func TestLearnWhatMayBeDeclared(t *testing.T) {
	t.Parallel()
	t.Run("ST-DECL-UC02-R01-T01 The report names every component of the active set, each setting with its type, and each slot", func(t *testing.T) {
		report, run := reportFor(t, "rest-crud")
		if run.code != 0 {
			t.Fatalf("the report failed: %s", run.stderr)
		}
		compareInputs(t, expectedInputs(t, "rest-crud"), reportedInputs(report))
	})

	t.Run("ST-DECL-UC02-R02-T01 The report is one structured document with the fields of the declarable inputs", func(t *testing.T) {
		report, run := reportFor(t, "rest-crud")
		if run.code != 0 || len(report.Rows) == 0 {
			t.Fatalf("want exit 0 and rows, got exit %d: %s", run.code, run.stderr)
		}
		var raw struct {
			Rows []map[string]json.RawMessage `json:"rows"`
		}
		if err := json.Unmarshal([]byte(run.stdout), &raw); err != nil {
			t.Fatal(err)
		}
		fields := []string{"archetype", "component", "component_version", "setting", "setting_type", "allowed_values", "optional", "default", "slot", "slot_contract"}
		for i, row := range raw.Rows {
			for _, field := range fields {
				if _, ok := row[field]; !ok {
					t.Errorf("row %d does not hold %s", i, field)
				}
			}
			if len(row) != len(fields) {
				t.Errorf("row %d holds %d fields, want %d", i, len(row), len(fields))
			}
		}
	})

	t.Run("ST-DECL-UC02-R03-T01 No setting the report names is refused as undeclared", func(t *testing.T) {
		report, _ := reportFor(t, "rest-crud")
		settings := 0
		for _, row := range report.Rows {
			if row.Setting == "" {
				continue
			}
			settings++
			project := td02Project(t, withSetting(t, row.Component, row.Setting, valueOfType(row)))
			if faults := undeclaredSettingFaults(t, project); len(faults) > 0 {
				t.Errorf("%s.%s is refused as undeclared: %v", row.Component, row.Setting, faults)
			}
		}
		if settings == 0 {
			t.Fatal("the report names no setting")
		}
	})

	t.Run("ST-DECL-UC02-R03-T02 No slot the report names is refused as undeclared", func(t *testing.T) {
		report, _ := reportFor(t, "rest-crud")
		slots := 0
		for _, row := range report.Rows {
			if row.Slot == "" {
				continue
			}
			slots++
			project := td02Project(t, td02+fmt.Sprintf("slots:\n  - collection: todos\n    slot: %s\n    gate: [policy]\n", row.Slot))
			if run := stegoIn(t, project, nil, "fill", "create", "policy", "--slot", row.Slot, "--collection", "todos", "--component", row.Component); run.code != 0 {
				t.Fatalf("stego fill create for %s: %s", row.Slot, run.stderr)
			}
			document, _ := validateJSON(t, project)
			for _, fault := range document.Faults {
				if fault.Rule == "slot-name" || fault.Rule == "undeclared-field" {
					t.Errorf("the slot %s is refused as undeclared: %s", row.Slot, fault.Prose())
				}
			}
		}
		if slots == 0 {
			t.Fatal("the report names no slot")
		}
	})

	t.Run("ST-DECL-UC02-R04-T01 Every allowed value the report shows is accepted", func(t *testing.T) {
		report, _ := reportFor(t, "rest-crud")
		values := 0
		for _, row := range report.Rows {
			for _, value := range row.AllowedValues {
				values++
				project := td02Project(t, withSetting(t, row.Component, row.Setting, value))
				document, _ := validateJSON(t, project)
				for _, fault := range document.Faults {
					if strings.HasPrefix(fault.Path, "overrides."+row.Component+"."+row.Setting) {
						t.Errorf("the allowed value %s of %s.%s is refused: %s", value, row.Component, row.Setting, fault.Prose())
					}
				}
			}
		}
		if values == 0 {
			t.Fatal("the report shows no allowed value")
		}
	})

	t.Run("ST-DECL-UC02-R05-T01 The report refuses an unknown archetype, names the archetypes held, and reports nothing else", func(t *testing.T) {
		_, run := reportFor(t, "no-such-archetype")
		if run.code == 0 {
			t.Fatal("the report accepted an unknown archetype")
		}
		for _, name := range append([]string{"no-such-archetype"}, declaredArchetypes(t)...) {
			if !strings.Contains(run.stderr, name) {
				t.Errorf("the refusal does not name %s: %s", name, run.stderr)
			}
		}
		if strings.TrimSpace(run.stdout) != "" {
			t.Errorf("the refusal printed a report:\n%s", run.stdout)
		}
	})

	t.Run("ST-DECL-UC02-FL-T An agent reads, as structured data, every component, setting and slot it may declare for rest-crud", func(t *testing.T) {
		report, run := reportFor(t, "rest-crud")
		if run.code != 0 || report.Archetype != "rest-crud" {
			t.Fatalf("want exit 0 and the report of rest-crud, got exit %d: %s", run.code, run.stderr)
		}
		compareInputs(t, expectedInputs(t, "rest-crud"), reportedInputs(report))
	})
}
