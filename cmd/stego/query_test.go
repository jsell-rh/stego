package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/fill"
)

// queryDeclaration is a pooled rest-crud declaration with the entity Note, the get query get-note and the list
// query list-notes, each with its own fill.
const queryDeclaration = `kind: service
name: notes
archetype: rest-crud
language: go
base_path: /api/notes/v1
tenancy:
  kind: pooled
  key: tenant_id
  claim: tenant
entities:
  - name: Note
    fields:
      - {name: title, type: string}
collections:
  notes:
    entity: Note
    operations: [create]
queries:
  - name: get-note
    reads: [Note]
    resource_member: noteId
    fill: get-note
    route: GET /notes/{noteId}
  - name: list-notes
    reads: [Note]
    fill: list-notes
    route: GET /notes
`

// queryProject is a project with queryDeclaration and its fills, and a copy of the registry whose rest-crud
// archetype holds the tenancy, domain-probe and query components. The fill of get-note implements the slot slot.
func queryProject(t *testing.T, slot string) compiler.ReconcilerInput {
	t.Helper()
	project := t.TempDir()
	registry := filepath.Join(project, "registry")
	if err := os.CopyFS(registry, os.DirFS("../../registry")); err != nil {
		t.Fatal(err)
	}
	archetype := filepath.Join(registry, "archetypes/rest-crud/archetype.yaml")
	data, err := os.ReadFile(archetype)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archetype, []byte(strings.Replace(string(data), "components:\n", "components:\n  - tenancy\n  - domain-probe\n  - query\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	// The components are alpha, and an archetype holds only ga components; the copy states ga for this test.
	for _, name := range []string{"tenancy", "domain-probe", "query"} {
		component := filepath.Join(registry, "components", name, "component.yaml")
		data, err := os.ReadFile(component)
		if err != nil {
			t.Fatal(err)
		}
		staged := strings.Replace(strings.Replace(string(data), "stage: alpha\n", "stage: ga\n", 1), "sunset: 2027-04-30\n", "", 1)
		staged = strings.Replace(staged, "sunset: \"2027-04-30\"\n", "", 1)
		if err := os.WriteFile(component, []byte(staged), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	putFile(t, project, "service.yaml", queryDeclaration)
	putFile(t, project, "fills/get-note/fill.yaml", "kind: fill\nname: get-note\nimplements: "+slot+"\n")
	putFile(t, project, "fills/list-notes/fill.yaml", "kind: fill\nname: list-notes\nimplements: "+declaration.QuerySlot+"\n")
	return compiler.ReconcilerInput{ProjectDir: project, RegistryDir: registry, GoVersion: "1.26.10", ModuleName: "example.com/notes",
		Generators: defaultGenerators(), OutDir: filepath.Join(project, "out"), ReadFill: fill.ReadCollection, ReadFillSlot: fill.ReadImplements}
}

func TestQueriesFromDeclaration(t *testing.T) {
	t.Run("ST-SUP-RC33-OC02-T01 One handler is emitted per query, bound to its fill, generated from service.yaml", func(t *testing.T) {
		plan, err := compiler.Reconcile(queryProject(t, declaration.QuerySlot))
		if err != nil {
			t.Fatal(err)
		}
		files := map[string]string{}
		var names []string
		for _, f := range plan.GeneratedFiles {
			files[f.Path] = string(f.Content)
			names = append(names, f.Path)
		}
		sort.Strings(names)
		serve, ok := files["queries/serve/serve.go"]
		if !ok || files["queries/queries.go"] == "" {
			t.Fatalf("the plan holds no query contract and handlers: %v", names)
		}
		for _, want := range []string{
			`fillGetNote "example.com/notes/fills/get-note"`, `fillListNotes "example.com/notes/fills/list-notes"`,
			`mux.Handle("GET /api/notes/v1/notes/{noteId}", h.cfg.Verifier.Middleware(h.getNote))`,
			`mux.Handle("GET /api/notes/v1/notes", h.cfg.Verifier.Middleware(h.listNotes))`,
			"return fillGetNote.Fill(ctx, req, h)", "Fill:   fillListNotes.Fill,",
		} {
			if !strings.Contains(serve, want) {
				t.Errorf("the generated handlers lack %q:\n%s", want, serve)
			}
		}
		if n := strings.Count(serve, "query.Serve("); n != 2 {
			t.Errorf("the generated handlers serve %d queries, want 2", n)
		}
	})

	t.Run("ST-SUP-RC33-OC02-T02 A query bound to a fill of another slot is refused, generated from service.yaml", func(t *testing.T) {
		input := queryProject(t, declaration.CommandSlot)
		_, err := compiler.Reconcile(input)
		if err == nil || !strings.Contains(err.Error(), "get-note") {
			t.Fatalf("Reconcile with a query fill of the command slot: %v, want a fault that names the query and the fill", err)
		}
		if _, statErr := os.Stat(filepath.Join(input.ProjectDir, "out")); !os.IsNotExist(statErr) {
			t.Fatalf("the refused generation wrote out: %v", statErr)
		}
	})
}
