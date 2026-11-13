package main

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// generatedMigrations gives each file under a folder named migrations of the generated output of the project, by
// its path from the project, with its content.
func generatedMigrations(t *testing.T, project string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(filepath.Join(project, "out"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Base(filepath.Dir(path)) != "migrations" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(project, path)
		files[relative] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[filepath.Join("out", "internal", "storage", "migrations", "001_initial.sql")]; !ok {
		t.Fatalf("stego apply generated no 001_initial.sql: %v", slices.Sorted(maps.Keys(files)))
	}
	return files
}

// provisionerController is the controller provisioner of the composition tests (sources scan and watch, resync 10m,
// replicas 2, lease TTL 15s, tenants each), with the field given left out.
func provisionerController(without string) string {
	var kept []string
	for _, field := range []string{"name: provisioner", "sources: [scan, watch]", "resync: 10m", "replicas: 2", "lease_ttl: 15s", "tenants: each"} {
		if !strings.HasPrefix(field, without+":") {
			kept = append(kept, field)
		}
	}
	return "controllers:\n  - {" + strings.Join(kept, ", ") + "}\n"
}

// td02WithTimes is ST-DECL-TD02 with the two time fields starts_at and ends_at, which bound the range of a
// no_overlap invariant, and the field owner_id, which scopes it.
var td02WithTimes = strings.Replace(td02, "      - {name: done, type: bool, default: false}\n",
	"      - {name: done, type: bool, default: false}\n      - {name: starts_at, type: timestamp}\n      - {name: ends_at, type: timestamp}\n      - {name: owner_id, type: string}\n", 1)

func TestDeclareComposition(t *testing.T) {
	t.Parallel()
	t.Run("ST-DECL-UC09-R01-T01 Modes that are a subset of api, worker and controller are accepted", func(t *testing.T) {
		for _, modes := range []string{"[api]", "[api, worker]", "[api, worker, controller]"} {
			document, run := validateJSON(t, td02Project(t, td02+"modes: "+modes+"\n"))
			if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
				t.Errorf("the modes %s were refused: exit %d\n%s%s", modes, run.code, run.stdout, run.stderr)
			}
		}
	})
	t.Run("ST-DECL-UC09-R01-T03 An unknown mode is refused, naming the mode", func(t *testing.T) {
		fault := refusedFault(t, td02+"modes: [api, batch]\n", "modes[1]")
		if fault.Found != "batch" || !slices.Equal(fault.Allowed, []string{"api", "worker", "controller"}) || !strings.Contains(fault.Message, `"batch"`) {
			t.Errorf("want a fault that names batch and allows api, worker and controller, got %+v", fault)
		}
	})

	t.Run("ST-DECL-UC09-R02-T01 kafka-events without durable-events is refused, naming the missing mixin", func(t *testing.T) {
		refusedNaming(t, td02+"mixins: [kafka-events]\n", "mixins[0]", "kafka-events", "durable-events")
	})

	for _, c := range []struct{ id, without, path, name string }{
		{"ST-DECL-UC09-R03-T01 A controller with no lease TTL is refused, naming the controller and the field", "lease_ttl", "controllers[0].lease_ttl", "lease TTL"},
		{"ST-DECL-UC09-R03-T02 A controller with no sources is refused, naming the controller and the field", "sources", "controllers[0].sources", "sources"},
		{"ST-DECL-UC09-R03-T03 A controller with no resync is refused, naming the controller and the field", "resync", "controllers[0].resync", "resync"},
		{"ST-DECL-UC09-R03-T04 A controller with no replicas is refused, naming the controller and the field", "replicas", "controllers[0].replicas", "replicas"},
	} {
		t.Run(c.id, func(t *testing.T) {
			refusedNaming(t, td02+provisionerController(c.without), c.path, `"provisioner"`, c.name)
		})
	}

	t.Run("ST-DECL-UC09-R04-T01 A declared invariant of each known kind with its fields is accepted", func(t *testing.T) {
		service := td02WithTimes + `invariants:
  - {name: unique_open_title, kind: unique, on: Todo, by: [title], where: done = false}
  - {name: no_double_booking, kind: no_overlap, on: Todo, range: 'starts_at, ends_at', by: [owner_id]}
  - {name: within_budget, kind: sum_limit, on: Todo, parent: budget.limit}
`
		document, run := validateJSON(t, td02Project(t, service))
		if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
			t.Fatalf("the three declared invariants were refused: exit %d\n%s%s", run.code, run.stdout, run.stderr)
		}
	})
	t.Run("ST-DECL-UC09-R04-T02 A declared invariant of another kind is refused, naming the invariant and the kind", func(t *testing.T) {
		service := td02 + "invariants:\n  - {name: one_open, kind: exclusive, on: Todo}\n"
		refusedNaming(t, service, "invariants[0].kind", `"one_open"`, `"exclusive"`)
		if fault := refusedFault(t, service, "invariants[0].kind"); !slices.Equal(fault.Allowed, []string{"unique", "no_overlap", "sum_limit"}) {
			t.Errorf("want unique, no_overlap and sum_limit allowed, got %+v", fault)
		}
	})
	t.Run("ST-DECL-UC09-R04-T03 A declared invariant that lacks a field its kind needs is refused, naming the invariant and the field", func(t *testing.T) {
		for _, c := range []struct{ invariant, field string }{
			{"{name: unique_title, kind: unique, on: Todo}", "by"},
			{"{name: no_double_booking, kind: no_overlap, on: Todo, by: [owner_id]}", "range"},
			{"{name: within_budget, kind: sum_limit, on: Todo}", "parent"},
		} {
			name := strings.TrimPrefix(strings.Split(c.invariant, ",")[0], "{name: ")
			refusedNaming(t, td02WithTimes+"invariants:\n  - "+c.invariant+"\n", "invariants[0]."+c.field, `"`+name+`"`, "needs "+c.field)
		}
	})

	t.Run("ST-DECL-UC09-R04-T04 A rule with no known emission stays a slot, and generation emits no store constraint for it", func(t *testing.T) {
		plain := td02Project(t, td02)
		putFile(t, plain, "go.mod", "module example.com/fills\n\ngo 1.26.11\n")
		bound := td02Project(t, td02+"slots:\n  - collection: todos\n    slot: before_create\n    gate: [policy]\n")
		putQualifiedPolicy(t, bound)
		for _, project := range []string{plain, bound} {
			for _, command := range []string{"validate", "apply"} {
				if run := stegoIn(t, project, nil, command); run.code != 0 {
					t.Fatalf("stego %s exited %d:\n%s%s", command, run.code, run.stdout, run.stderr)
				}
			}
		}
		want, got := generatedMigrations(t, plain), generatedMigrations(t, bound)
		if len(got) != len(want) {
			t.Errorf("the binding changes the migration files: want %d, got %d", len(want), len(got))
		}
		for name, content := range want {
			if got[name] != content {
				t.Errorf("the binding changes the migration %s", name)
			}
		}
	})
}
