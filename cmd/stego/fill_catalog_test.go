package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/fill"
)

// catalogRow is one entry that stego registry fills prints.
type catalogRow struct{ collection, standing, reason string }

// catalog runs stego registry fills with the arguments, expects it to exit
// 0, and gives its entries by fill name, with what it printed.
func (p *fillProject) catalog(t *testing.T, args ...string) (map[string]catalogRow, string) {
	t.Helper()
	stdout := p.must(t, "no-key", append([]string{"registry", "fills"}, args...)...)
	return catalogRows(t, stdout), stdout
}

// catalogRows reads the entries of the table that stego registry fills
// prints. A fill that shows twice fails the test.
func catalogRows(t *testing.T, stdout string) map[string]catalogRow {
	t.Helper()
	rows := map[string]catalogRow{}
	header := false
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "FILL" && fields[1] == "COLLECTION" {
			header = true
			continue
		}
		if !header || len(fields) < 3 {
			continue
		}
		if _, twice := rows[fields[0]]; twice {
			t.Fatalf("the list shows %s twice:\n%s", fields[0], stdout)
		}
		rows[fields[0]] = catalogRow{collection: fields[1], standing: fields[2], reason: strings.Join(fields[3:], " ")}
	}
	return rows
}

// restAPISlots are the nine slots of rest-api, the only component of
// rest-crud that declares slots.
var restAPISlots = []string{"after_create", "after_delete", "after_patch", "after_upsert", "before_create", "before_delete", "before_patch", "before_upsert", "validate"}

// slotsNamed gives the slot names that a refusal lists after "it declares
// the slots:".
func slotsListed(t *testing.T, stderr string) []string {
	t.Helper()
	_, list, found := strings.Cut(stderr, "it declares the slots: ")
	if !found {
		t.Fatalf("the refusal lists no slots:\n%s", stderr)
	}
	list, _, _ = strings.Cut(list, "\n")
	names := strings.Split(strings.TrimSpace(list), ", ")
	sort.Strings(names)
	return names
}

// auditHooksRegistry copies the built-in registry and adds to the active
// component set of rest-crud a fixture component audit-hooks that also
// declares a slot before_create, and gives the copy.
func auditHooksRegistry(t *testing.T, p *fillProject) string {
	t.Helper()
	registry := copyFolder(t, p.keys.registry)
	component := "kind: component\nname: audit-hooks\nversion: 1.0.0\nstage: ga\noutput_namespace: internal/audit\nrequires: []\nprovides: []\nslots:\n  - name: before_create\n    proto: audit.hooks.BeforeCreate\n    default: passthrough\n"
	contract := "syntax = \"proto3\";\npackage audit.hooks;\nimport \"stego/common/types.proto\";\n\nservice BeforeCreate {\n  rpc Evaluate(BeforeCreateRequest) returns (stego.common.SlotResult);\n}\n\nmessage BeforeCreateRequest {\n  stego.common.Identity caller = 1;\n}\n"
	if err := os.MkdirAll(filepath.Join(registry, "components/audit-hooks/slots"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, registry, "components/audit-hooks/component.yaml", component, 0644)
	writeFile(t, registry, "components/audit-hooks/slots/before_create.proto", contract, 0644)
	archetype := filepath.Join(registry, "archetypes/rest-crud/archetype.yaml")
	data, err := os.ReadFile(archetype)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), "  - health-check\n", "  - health-check\n  - audit-hooks\n", 1)
	if changed == string(data) {
		t.Fatal("the archetype rest-crud does not list health-check")
	}
	writeFile(t, registry, "archetypes/rest-crud/archetype.yaml", changed, 0644)
	return registry
}

// sevenFills are the fills of ST-EXT-TD01 that implement
// rest-api.before_create, with their collection and standing.
var sevenFills = map[string]catalogRow{
	"qualified-fill":  {collection: "org-users", standing: "qualified"},
	"stale-fill":      {collection: "org-users", standing: "stale"},
	"unsigned-fill":   {collection: "org-users", standing: "unsigned"},
	"unlisted-fill":   {collection: "org-users", standing: "unsigned"},
	"untested-fill":   {collection: "org-users", standing: "untested"},
	"untested-fill-2": {collection: "org-groups", standing: "untested"},
	"no-tests-fill":   {collection: "org-users", standing: "untested"},
}

// expectFills fails the test unless the rows are exactly the fills, with
// their collection and standing.
func expectFills(t *testing.T, rows map[string]catalogRow, want map[string]catalogRow, stdout string) {
	t.Helper()
	if len(rows) != len(want) {
		t.Fatalf("the list shows %d fills, want %d:\n%s", len(rows), len(want), stdout)
	}
	for name, row := range want {
		got, ok := rows[name]
		if !ok || got.collection != row.collection || got.standing != row.standing {
			t.Fatalf("the list shows %s as %+v, want %+v:\n%s", name, got, row, stdout)
		}
	}
}

func TestFillCatalog(t *testing.T) {
	t.Run("ST-EXT-UC03-R01-T01 The list of fills for a slot shows each fill that implements it, with its collection and standing", func(t *testing.T) {
		p := newFillProject(t)
		rows, stdout := p.catalog(t, "--slot", "before_create")
		expectFills(t, rows, sevenFills, stdout)
		if _, ok := rows["other-slot-fill"]; ok {
			t.Fatalf("the list shows other-slot-fill:\n%s", stdout)
		}
	})

	t.Run("ST-EXT-UC03-R02-T01 A slot that no component of the active set declares is refused, with the slots that the set declares", func(t *testing.T) {
		p := newFillProject(t)
		for _, slot := range []string{"befor_create", "on_entity_changed"} {
			stdout, stderr, err := p.stego(t, "no-key", "registry", "fills", "--slot", slot)
			if err == nil {
				t.Fatalf("%s: the list was not refused:\n%s", slot, stdout)
			}
			if !strings.Contains(stderr, `"`+slot+`"`) {
				t.Fatalf("%s: the refusal does not name the slot:\n%s", slot, stderr)
			}
			if got := slotsListed(t, stderr); strings.Join(got, ",") != strings.Join(restAPISlots, ",") {
				t.Fatalf("%s: the refusal lists %v, want %v", slot, got, restAPISlots)
			}
			if len(catalogRows(t, stdout)) != 0 || strings.Contains(stdout, "-fill") {
				t.Fatalf("%s: the refusal shows a fill:\n%s", slot, stdout)
			}
		}
	})

	t.Run("ST-EXT-UC03-R03-T01 A known slot with no fill gets an explicit empty answer that names stego fill create", func(t *testing.T) {
		p := newFillProject(t)
		rows, stdout := p.catalog(t, "--slot", "after_delete")
		if len(rows) != 0 || !strings.Contains(stdout, "No fill implements the slot after_delete") || !strings.Contains(stdout, "stego fill create") {
			t.Fatalf("the empty answer does not name after_delete, say that no fill implements it, and name stego fill create:\n%s", stdout)
		}
	})

	t.Run("ST-EXT-UC03-R04-T01 A fill folder whose fill manifest cannot be read is listed as unreadable with its fault", func(t *testing.T) {
		p := newFillProject(t)
		p.write(t, "fills/broken-yaml-fill/fill.yaml", "kind: fill\nname: broken-yaml-fill\nimplements: [rest-api.before_create\n")
		p.write(t, "fills/unknown-field-fill/fill.yaml", "kind: fill\nname: unknown-field-fill\nimplements: rest-api.before_create\ncollection: org-users\nowner: someone\n")
		faults := map[string]string{}
		for _, name := range []string{"broken-yaml-fill", "unknown-field-fill"} {
			_, err := fill.ParseFill(p.path("fills/" + name + "/fill.yaml"))
			if err == nil {
				t.Fatalf("the reader reads %s", name)
			}
			faults[name] = strings.Join(strings.Fields(err.Error()), " ")
		}
		for _, slot := range []string{"before_create", "after_delete"} {
			rows, stdout := p.catalog(t, "--slot", slot)
			for name, fault := range faults {
				row, ok := rows[name]
				if !ok || row.standing != "unreadable" || row.reason != fault {
					t.Fatalf("%s: the list does not show %s as unreadable with the fault %q:\n%s", slot, name, fault, stdout)
				}
			}
			if slot == "before_create" {
				readable := map[string]catalogRow{}
				for name, row := range rows {
					if row.standing != "unreadable" {
						readable[name] = row
					}
				}
				expectFills(t, readable, sevenFills, stdout)
			}
		}
	})

	t.Run("ST-EXT-UC03-R05-T01 A slot name that two components of the active set declare is refused with no component, naming both components", func(t *testing.T) {
		p := newFillProject(t)
		p.registry = auditHooksRegistry(t, p)
		p.write(t, "fills/audit-fill/fill.yaml", "kind: fill\nname: audit-fill\nimplements: audit-hooks.before_create\ncollection: org-users\n")
		stdout, stderr, err := p.stego(t, "no-key", "registry", "fills", "--slot", "before_create")
		if err == nil {
			t.Fatalf("the ambiguous slot name was not refused:\n%s", stdout)
		}
		if !strings.Contains(stderr, `"before_create"`) || !strings.Contains(stderr, ": audit-hooks, rest-api;") {
			t.Fatalf("the refusal does not name the slot and exactly the components rest-api and audit-hooks:\n%s", stderr)
		}
		if strings.Contains(stdout, "qualified-fill") || strings.Contains(stdout, "audit-fill") {
			t.Fatalf("the refusal shows a fill:\n%s", stdout)
		}
	})

	t.Run("ST-EXT-UC03-R05-T02 With the component given, the list of an ambiguous slot name shows only the fills of that component's slot", func(t *testing.T) {
		p := newFillProject(t)
		p.registry = auditHooksRegistry(t, p)
		p.write(t, "fills/audit-fill/fill.yaml", "kind: fill\nname: audit-fill\nimplements: audit-hooks.before_create\ncollection: org-users\n")
		rows, stdout := p.catalog(t, "--slot", "before_create", "--component", "audit-hooks")
		if len(rows) != 1 || rows["audit-fill"].collection != "org-users" {
			t.Fatalf("the list for audit-hooks is not exactly audit-fill:\n%s", stdout)
		}
		rows, stdout = p.catalog(t, "--slot", "before_create", "--component", "rest-api")
		expectFills(t, rows, sevenFills, stdout)
		stdout, stderr, err := p.stego(t, "no-key", "registry", "fills", "--slot", "before_create", "--component", "rest-crud-missing")
		if err == nil || !strings.Contains(stderr, `component "rest-crud-missing" is not in the active component set`) {
			t.Fatalf("the list for rest-crud-missing was not refused with its name: %v\n%s\n%s", err, stdout, stderr)
		}
	})
}

func TestFindFillFlow(t *testing.T) {
	t.Run("ST-EXT-UC03-FL-T An agent finds the fills for a slot with their standing, and each other path says what it found", func(t *testing.T) {
		p := newFillProject(t)
		rows, stdout := p.catalog(t, "--slot", "before_create")
		expectFills(t, rows, sevenFills, stdout)
		if _, ok := rows["other-slot-fill"]; ok {
			t.Fatalf("the list shows other-slot-fill:\n%s", stdout)
		}

		// The broken path.
		manifest := string(p.read(t, "fills/unsigned-fill/fill.yaml"))
		p.write(t, "fills/unsigned-fill/fill.yaml", "kind: fill\nname: [unsigned-fill\n")
		rows, stdout = p.catalog(t, "--slot", "before_create")
		if rows["unsigned-fill"].standing != "unreadable" || rows["unsigned-fill"].reason == "" {
			t.Fatalf("the list does not show unsigned-fill as unreadable with its fault:\n%s", stdout)
		}
		p.write(t, "fills/unsigned-fill/fill.yaml", manifest)
		rows, stdout = p.catalog(t, "--slot", "before_create")
		if rows["unsigned-fill"].standing != "unsigned" {
			t.Fatalf("after the repair the list does not show unsigned-fill as unsigned:\n%s", stdout)
		}

		// The empty path.
		_, stdout = p.catalog(t, "--slot", "after_delete")
		if !strings.Contains(stdout, "after_delete") || !strings.Contains(stdout, "stego fill create") {
			t.Fatalf("the empty answer does not name after_delete and stego fill create:\n%s", stdout)
		}

		// The refused path.
		stdout, stderr, err := p.stego(t, "no-key", "registry", "fills", "--slot", "befor_create")
		if err == nil {
			t.Fatalf("the unknown slot was not refused:\n%s", stdout)
		}
		if got := slotsListed(t, stderr); strings.Join(got, ",") != strings.Join(restAPISlots, ",") {
			t.Fatalf("the refusal lists %v, want %v", got, restAPISlots)
		}
	})
}
