package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/fill"
	"github.com/jsell-rh/stego/internal/registry"
)

// checkInProcess runs the check of stego validate in this process on a project: the same compiler input that
// stego builds for the project, with the generators of this build and its fill readers.
func checkInProcess(t *testing.T, project string) []declaration.ValidationError {
	t.Helper()
	resolved, err := registry.ResolveRegistry(registry.ResolveOptions{ProjectDir: project})
	if err != nil {
		t.Fatal(err)
	}
	module, goVersion, err := compiler.ProjectModuleSettings(project, "", "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiler.Validate(compiler.ReconcilerInput{
		ProjectDir: project, RegistryDir: resolved.Dir, RegistryDirs: resolved.Dirs, Generators: defaultGenerators(),
		GoVersion: goVersion, ModuleName: module, RegistrySHA: resolved.Ref, OutDir: filepath.Join(project, "out"),
		ReadFill: fill.ReadCollection, ReadFillSlot: fill.ReadImplements, QualifyFills: fill.QualificationFaults,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Errors
}

// commandProject is the project of ST-DECL-TD02 with the declaration given and the fills named, each a fill of
// the command slot.
func commandProject(t *testing.T, service string, fills ...string) string {
	t.Helper()
	project := td02Project(t, service)
	for _, name := range fills {
		putFile(t, project, filepath.Join("fills", name, "fill.yaml"), "kind: fill\nname: "+name+"\nimplements: "+declaration.CommandSlot+"\n")
	}
	return project
}

// routeFaults gives the faults of the rule operation-route.
func routeFaults(faults []declaration.ValidationError) []declaration.ValidationError {
	var kept []declaration.ValidationError
	for _, fault := range faults {
		if fault.Rule == "operation-route" {
			kept = append(kept, fault)
		}
	}
	return kept
}

func TestOperationRoutes(t *testing.T) {
	command := func(route string) string {
		return td02 + "commands:\n  - name: reserve-and-charge\n    writes: [Todo]\n    fill: reserve-and-charge\n" + route
	}
	t.Run("ST-DECL-INV47-T A command on POST under the base path is accepted", func(t *testing.T) {
		project := commandProject(t, command("    route: POST /orders/{orderId}/reservations\n"), "reserve-and-charge")
		if faults := checkInProcess(t, project); len(faults) != 0 {
			t.Fatalf("want no fault, got %+v", faults)
		}
	})
	for _, c := range []struct{ name, route, names string }{
		{"the method GET", "    route: GET /orders/{orderId}/reservations\n", "GET /orders/{orderId}/reservations"},
		{"a route that a collection serves", "    route: POST /todos\n", "POST /todos"},
		{"no route", "", "no route"},
		{"a pattern that is not valid", "    route: POST /orders/{orderId\n", "POST /orders/{orderId"},
	} {
		t.Run("ST-DECL-INV47-T A command with "+c.name+" is refused", func(t *testing.T) {
			project := commandProject(t, command(c.route), "reserve-and-charge")
			faults := checkInProcess(t, project)
			routes := routeFaults(faults)
			if len(routes) != 1 || len(faults) != 1 {
				t.Fatalf("want one route fault, got %+v", faults)
			}
			for _, part := range []string{`"reserve-and-charge"`, c.names} {
				if !strings.Contains(routes[0].Message, part) {
					t.Errorf("the fault does not name %s: %s", part, routes[0].Message)
				}
			}
			if routes[0].Path != "commands[0].route" {
				t.Errorf("the fault is at %s, want commands[0].route", routes[0].Path)
			}
		})
	}
	t.Run("ST-DECL-INV47-T Two commands on one route are refused", func(t *testing.T) {
		service := command("    route: POST /reservations\n") + "  - name: release\n    writes: [Todo]\n    fill: reserve-and-charge\n    route: POST /reservations\n"
		routes := routeFaults(checkInProcess(t, commandProject(t, service, "reserve-and-charge")))
		if len(routes) != 1 || !strings.Contains(routes[0].Message, `"release"`) {
			t.Fatalf("want one route fault that names release, got %+v", routes)
		}
	})
	queryProject := func(t *testing.T, route string) string {
		service := command("    route: POST /orders/{orderId}/reservations\n") + "queries:\n  - name: open-reservations\n    reads: [Todo]\n    fill: open-reservations\n" + route
		project := commandProject(t, service, "reserve-and-charge")
		putFile(t, project, filepath.Join("fills", "open-reservations", "fill.yaml"), "kind: fill\nname: open-reservations\nimplements: "+declaration.QuerySlot+"\n")
		return project
	}
	t.Run("ST-DECL-INV47-T A command on POST and a query on GET are accepted", func(t *testing.T) {
		if faults := checkInProcess(t, queryProject(t, "    route: GET /reservations/open\n")); len(faults) != 0 {
			t.Fatalf("want no fault, got %+v", faults)
		}
	})
	for _, c := range []struct{ name, route, names string }{
		{"the method POST", "    route: POST /reservations/open\n", "POST /reservations/open"},
		{"a route that a collection serves", "    route: GET /todos\n", "GET /todos"},
	} {
		t.Run("ST-DECL-INV47-T A query with "+c.name+" is refused", func(t *testing.T) {
			faults := checkInProcess(t, queryProject(t, c.route))
			if len(faults) != 1 || faults[0].Rule != "operation-route" || faults[0].Path != "queries[0].route" {
				t.Fatalf("want one route fault of the query, got %+v", faults)
			}
			for _, part := range []string{`"open-reservations"`, c.names} {
				if !strings.Contains(faults[0].Message, part) {
					t.Errorf("the fault does not name %s: %s", part, faults[0].Message)
				}
			}
		})
	}
	t.Run("ST-DECL-INV47-T Checking reads the slot of a command's fill from the project", func(t *testing.T) {
		project := commandProject(t, command("    route: POST /reservations\n"))
		putFile(t, project, filepath.Join("fills", "reserve-and-charge", "fill.yaml"), "kind: fill\nname: reserve-and-charge\nimplements: rest-api.before_create\ncollection: todos\n")
		faults := checkInProcess(t, project)
		if len(faults) != 1 || faults[0].Rule != "command-fill" || !strings.Contains(faults[0].Message, "rest-api.before_create") {
			t.Fatalf("want one fill fault that names rest-api.before_create, got %+v", faults)
		}
		if err := os.RemoveAll(filepath.Join(project, "fills", "reserve-and-charge")); err != nil {
			t.Fatal(err)
		}
		faults = checkInProcess(t, project)
		if len(faults) != 1 || faults[0].Rule != "command-fill" || !strings.Contains(faults[0].Message, "does not exist") {
			t.Fatalf("want one fill fault for a missing fill, got %+v", faults)
		}
	})
}
