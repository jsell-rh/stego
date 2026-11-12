package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/fill"
)

// probeDeclaration is a rest-crud declaration with a command, a query and three observations over the entity
// Reservation, as the RC34 test records state them. It has no slot binding, because a slot fill needs signed
// evidence; the unit test of the generator proves the probe of a slot binding.
var probeDeclaration = onRuntimePath(`kind: service
name: reservations
archetype: rest-crud
language: go
base_path: /api/reservations/v1
entities:
  - name: Reservation
    fields:
      - {name: sku, type: string}
      - {name: qty, type: int64}
collections:
  reservations:
    entity: Reservation
    operations: [create, read, list]
commands:
  - name: reserve-and-charge
    writes: [Reservation]
    resource_type: reservation
    permission: edit
    resource_member: reservationId
    fill: reserve-and-charge
    route: POST /reservations/{reservationId}/charge
queries:
  - name: open-reservations
    reads: [Reservation]
    resource_type: reservation
    permission: view
    fill: open-reservations
    route: GET /reservations/open
observations:
  - name: reservation_refused
    kind: event
    severity: warn
    fields:
      - {name: sku, type: Reservation.sku}
      - {name: reason, type: "enum(short | expired)"}
    emitted_by: [reserve-and-charge]
  - name: reservations_refused
    kind: counter
    severity: warn
    fields:
      - {name: reason, type: "enum(short | expired)"}
    emitted_by: [reserve-and-charge]
  - name: reservation_wait
    kind: histogram
    severity: info
    value: "wait: ms"
    emitted_by: [open-reservations]
`)

// probeProto holds the request and the response message of the command reserve-and-charge.
const probeProto = `syntax = "proto3";

package reservations.v1;

message ReserveAndChargeRequest {
  string reservation_id = 1;
}

message ReserveAndChargeResponse {
  string reservation_id = 1;
}
`

// probeProject is a project with probeDeclaration, its fills, and a copy of the registry whose rest-crud archetype
// holds the domain-probe component.
func probeProject(t *testing.T) (compiler.ReconcilerInput, string) {
	t.Helper()
	project := t.TempDir()
	registry := filepath.Join(project, "registry")
	if err := os.CopyFS(registry, os.DirFS(shippedRegistry(t))); err != nil {
		t.Fatal(err)
	}
	archetype := filepath.Join(registry, "archetypes/rest-crud/archetype.yaml")
	data, err := os.ReadFile(archetype)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archetype, []byte(strings.Replace(string(data), "components:\n", "components:\n  - domain-probe\n", 1)), 0644); err != nil {
		t.Fatal(err)
	}
	// The component is alpha, and an archetype holds only ga components; the declaration has no experimental
	// acceptance yet (ST-DECL-EN07), so the copy states ga for this test.
	component := filepath.Join(registry, "components/domain-probe/component.yaml")
	data, err = os.ReadFile(component)
	if err != nil {
		t.Fatal(err)
	}
	staged := strings.Replace(strings.Replace(string(data), "stage: alpha\n", "stage: ga\n", 1), "sunset: 2027-04-30\n", "", 1)
	if err := os.WriteFile(component, []byte(staged), 0644); err != nil {
		t.Fatal(err)
	}
	putFile(t, project, "service.yaml", strings.Replace(probeDeclaration, "\noverrides:\n", "\noverrides:\n  command:\n    proto_files:\n      - {path: proto/reservations.proto, import_path: reservations/v1/reservations.proto}\n", 1))
	putFile(t, project, "proto/reservations.proto", probeProto)
	putFile(t, project, "fills/reserve-and-charge/fill.yaml", "kind: fill\nname: reserve-and-charge\nimplements: "+declaration.CommandSlot+"\n")
	putFile(t, project, "fills/open-reservations/fill.yaml", "kind: fill\nname: open-reservations\nimplements: "+declaration.QuerySlot+"\n")
	input := compiler.ReconcilerInput{ProjectDir: project, RegistryDir: registry, GoVersion: "1.26.11", ModuleName: "example.com/reservations",
		Generators: defaultGenerators(), OutDir: filepath.Join(project, "out"), ReadFill: fill.ReadCollection, ReadFillSlot: fill.ReadImplements}
	return input, project
}

func TestDomainProbesFromDeclaration(t *testing.T) {
	t.Parallel()
	t.Run("ST-SUP-RC34-OC03-T01 Each emitter's probe holds its own observations only, generated from service.yaml", func(t *testing.T) {
		input, _ := probeProject(t)
		plan, err := compiler.Reconcile(input)
		if err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{}
		for _, f := range plan.GeneratedFiles {
			files[f.Path] = f.Content
		}
		source, ok := files["probes/probes.go"]
		if !ok {
			var names []string
			for name := range files {
				names = append(names, name)
			}
			sort.Strings(names)
			t.Fatalf("the plan holds no probes/probes.go: %v", names)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), "probes.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][]string{}
		types := map[string]bool{}
		for _, decl := range parsed.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil {
					if id, ok := d.Recv.List[0].Type.(*ast.Ident); ok {
						got[id.Name] = append(got[id.Name], d.Name.Name)
					}
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						types[ts.Name.Name] = true
					}
				}
			}
		}
		for name, want := range map[string]string{
			"ReserveAndChargeProbe": "ReservationRefused ReservationsRefused",
			"OpenReservationsProbe": "ReservationWait",
		} {
			if !types[name] {
				t.Errorf("the probe package declares no %s", name)
			}
			methods := got[name]
			sort.Strings(methods)
			if strings.Join(methods, " ") != want {
				t.Errorf("%s has the methods %v, want %q", name, methods, want)
			}
		}
		var catalog struct {
			Observations []struct {
				Name string `json:"name"`
			} `json:"observations"`
		}
		if err := json.Unmarshal(files["probes/observations.json"], &catalog); err != nil {
			t.Fatalf("observations.json: %v", err)
		}
		var names []string
		for _, o := range catalog.Observations {
			names = append(names, o.Name)
		}
		if strings.Join(names, " ") != "reservation_refused reservation_wait reservations_refused" {
			t.Errorf("observations.json lists %v", names)
		}
	})
}
