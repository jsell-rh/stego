package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
)

// projectFiles reads every file of a project folder, by its path in the folder, leaving out the apply lock, which
// any command that holds the lock may create empty.
func projectFiles(t *testing.T, project string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(project, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(project, name)
		if relative == filepath.Join(".stego", "apply.lock") {
			return nil
		}
		if entry.IsDir() {
			files[relative+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		files[relative] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// td04 is ST-DECL-TD02 with a EUR ledger (ST-DECL-TD04).
const td04 = `kind: service
name: todo-svc
archetype: rest-crud
language: go
base_path: /api/todo/v1
entities:
  - name: Todo
    fields:
      - {name: title, type: string, min_length: 1, max_length: 255}
      - {name: done, type: bool, default: false}
  - name: Wallet
    kind: ledger
    currency: EUR
    transfer_codes: [topup, purchase, refund]
    two_phase: true
    pending_timeout: 15m
    snapshot_every: 3
    accounts:
      - {name: customer_wallet, normal: credit, minimum_balance: 0, owner: customer, hot: false}
      - {name: settlement, normal: debit, hot: true}
collections:
  todos:
    entity: Todo
    operations: [create, read, update, delete, list]
  transfers:
    entity: Wallet
    operations: [create, post, void, reverse, read, list]
`

func TestCheckADeclaration(t *testing.T) {
	t.Parallel()
	t.Run("ST-DECL-UC03-R02-T01 A refusal reports every fault at once", func(t *testing.T) {
		service := strings.Replace(td02Plain, "{name: title, type: string,", "{name: title, type: foobar,", 1)
		service = strings.Replace(service, "      - {name: done, type: bool, default: false}\n", "      - {name: done, type: bool, default: false}\n      - {name: status, type: enum}\n", 1)
		service = strings.Replace(service, "operations: [create, read, update, delete, list]\n", "operations: [create, read, update, delete, list, explode]\n  things:\n    entity: Nothing\n    operations: [read]\n", 1)
		service += "slots:\n  - collection: gadgets\n    slot: before_create\n    gate: [policy]\n"
		project := td02Project(t, service)
		document, run := validateJSON(t, project)
		if run.code == 0 || document.Accepted {
			t.Fatalf("the declaration with five faults was accepted:\n%s", run.stdout)
		}
		for _, value := range []string{"foobar", "status", "Nothing", "explode", "gadgets"} {
			named := 0
			for _, fault := range document.Faults {
				if fault.Found == value || strings.Contains(fault.Message, `"`+value+`"`) {
					named++
				}
			}
			if named != 1 {
				t.Errorf("%d faults name %s, want one:\n%s", named, value, run.stdout)
			}
		}
	})

	t.Run("ST-DECL-UC03-R03-T01 Generation refuses no declaration that checking accepted, and refuses what checking refuses with the same faults", func(t *testing.T) {
		candidates := map[string]func(t *testing.T) string{
			"ST-DECL-TD02": func(t *testing.T) string { return td02Project(t, td02) },
			"ST-DECL-TD03": func(t *testing.T) string { return td03Project(t, td03Service(t)) },
			"ST-DECL-TD04": func(t *testing.T) string { return td02Project(t, td04) },
		}
		registry, _ := declaringRegistry(t)
		archetypes, err := os.ReadDir(filepath.Join(registry, "archetypes"))
		if err != nil {
			t.Fatal(err)
		}
		for _, archetype := range archetypes {
			if !archetype.IsDir() {
				continue
			}
			name := archetype.Name()
			candidates["scaffold of "+name] = func(t *testing.T) string {
				project := declaringProject(t)
				if run := stegoIn(t, project, nil, "init", "--archetype", name); run.code != 0 {
					t.Fatalf("stego init --archetype %s: %s", name, run.stderr)
				}
				return project
			}
		}
		accepted := 0
		for name, make := range candidates {
			project := make(t)
			if run := stegoIn(t, project, nil, "validate"); run.code != 0 {
				t.Logf("%s is not accepted by stego validate, so it is not a case of the test", name)
				continue
			}
			accepted++
			run := stegoIn(t, project, nil, "apply")
			if name == "ST-DECL-TD03" && run.code != 0 {
				// The registry holds spicedb-authz (ST-SUP-RC29), so the authorization of TD03 no longer fails closed
				// (ST-DECL-UC06-R07): generation must accept it.
				t.Errorf("stego apply refused ST-DECL-TD03, which the registry's spicedb-authz generates:\n%s%s", run.stdout, run.stderr)
			}
			if strings.Contains(run.stderr+run.stdout, "validation failed") || len(proseFaults(run.stdout+run.stderr)) > 0 {
				t.Errorf("stego apply refused %s, which stego validate accepted:\n%s%s", name, run.stdout, run.stderr)
			}
		}
		if accepted == 0 {
			t.Fatal("stego validate accepted none of the declarations")
		}
		faulty := map[string]string{
			"an empty length range": strings.Replace(td02, "min_length: 1, max_length: 255", "min_length: 10, max_length: 1", 1),
			"a bad pattern":         strings.Replace(td02, "min_length: 1, max_length: 255", `pattern: "["`, 1),
			"an unsupported type":   strings.Replace(td02, "type: string,", "type: unsupported,", 1),
			"a missing patchable":   strings.Replace(td02, "operations: [create, read, update, delete, list]", "operations: [create, read, update, delete, list, patch]\n    patchable: [missing]", 1),
		}
		for name, service := range faulty {
			project := td02Project(t, service)
			before := projectFiles(t, project)
			check := stegoIn(t, project, nil, "validate")
			apply := stegoIn(t, project, nil, "apply")
			checkFaults, applyFaults := proseFaults(check.stdout), proseFaults(apply.stdout+apply.stderr)
			if check.code == 0 || apply.code == 0 || len(checkFaults) == 0 {
				t.Errorf("%s: want both commands to refuse, got validate %d and apply %d", name, check.code, apply.code)
			}
			if !reflect.DeepEqual(checkFaults, applyFaults) {
				t.Errorf("%s: validate and apply give other faults:\n%s\n---\n%s", name, strings.Join(checkFaults, "\n"), strings.Join(applyFaults, "\n"))
			}
			if after := projectFiles(t, project); !reflect.DeepEqual(before, after) {
				t.Errorf("%s: stego apply changed the project after a refusal", name)
			}
		}
	})

	t.Run("ST-DECL-UC03-FL-T An author checks a declaration, sees every fault, and the project is unchanged", func(t *testing.T) {
		project := td02Project(t, td02)
		before := projectFiles(t, project)
		document, run := validateJSON(t, project)
		if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
			t.Fatalf("ST-DECL-TD02 is not accepted:\n%s%s", run.stdout, run.stderr)
		}
		prose := stegoIn(t, project, nil, "validate")
		if want := acceptedReport(t); prose.code != 0 || prose.stdout != want {
			t.Fatalf("stego validate on ST-DECL-TD02 exits %d and prints\n%q\nwant\n%q", prose.code, prose.stdout, want)
		}
		putFile(t, project, "service.yaml", threeFaults)
		document, run = validateJSON(t, project)
		if run.code == 0 || len(document.Faults) != 3 {
			t.Fatalf("want three faults:\n%s", run.stdout)
		}
		before["service.yaml"] = threeFaults
		if schema, err := os.ReadFile(filepath.Join(project, runtimeSchemaPath)); err == nil {
			before[runtimeSchemaPath] = string(schema)
		}
		if after := projectFiles(t, project); !reflect.DeepEqual(before, after) {
			t.Errorf("checking made, changed or removed a file")
		}
	})
}

// acceptedReport is the whole standard output that stego validate prints for ST-DECL-TD02 when it accepts it: the
// line that says no fault was found, then the standing of each component pin on lines of its own. The pins are
// read from the record that stego apply writes in a second project with the same declaration and registry.
func acceptedReport(t *testing.T) string {
	t.Helper()
	applied := td02Project(t, td02)
	if run := stegoIn(t, applied, nil, "apply"); run.code != 0 {
		t.Fatalf("stego apply on ST-DECL-TD02 exits %d:\n%s%s", run.code, run.stdout, run.stderr)
	}
	state, err := compiler.LoadState(filepath.Join(applied, ".stego", "state.yaml"))
	if err != nil || state.LastApplied == nil || len(state.LastApplied.Components) == 0 {
		t.Fatalf("stego apply wrote no component pins: %v", err)
	}
	names := make([]string, 0, len(state.LastApplied.Components))
	for name := range state.LastApplied.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	want := "Validation passed. No issues found.\nPin standing:\n"
	for _, name := range names {
		want += "  current " + name + " " + state.LastApplied.Components[name].SHA + "\n"
	}
	return want
}
