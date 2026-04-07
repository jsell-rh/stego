package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// stubGenerator returns predetermined files and wiring.
type stubGenerator struct {
	files  []gen.File
	wiring *gen.Wiring
	err    error
}

func (g *stubGenerator) Generate(_ gen.Context) ([]gen.File, *gen.Wiring, error) {
	return g.files, g.wiring, g.err
}

// setupTestProject creates a temporary project directory with a service.yaml
// and a minimal registry, returning the project dir and registry dir.
func setupTestProject(t *testing.T) (projectDir, registryDir string) {
	t.Helper()
	projectDir = t.TempDir()
	registryDir = t.TempDir()

	// Create service.yaml.
	serviceYAML := `kind: service
name: test-service
archetype: test-arch
language: go

entities:
  - name: Widget
    fields:
      - { name: label, type: string }

expose:
  - entity: Widget
    operations: [create, read]
`
	if err := os.WriteFile(filepath.Join(projectDir, "service.yaml"), []byte(serviceYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create registry: archetype.
	archDir := filepath.Join(registryDir, "archetypes", "test-arch")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatal(err)
	}
	archYAML := `kind: archetype
name: test-arch
language: go
version: 1.0.0
components:
  - stub-api
  - stub-store
conventions:
  layout: flat
  error_handling: problem-details-rfc
  logging: structured-json
  test_pattern: table-driven
bindings:
  storage-adapter: stub-store
`
	if err := os.WriteFile(filepath.Join(archDir, "archetype.yaml"), []byte(archYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create registry: components.
	for _, comp := range []struct {
		name, yaml string
	}{
		{
			name: "stub-api",
			yaml: `kind: component
name: stub-api
version: 1.0.0
output_namespace: internal/api
requires:
  - storage-adapter
provides:
  - http-server
slots: []
`,
		},
		{
			name: "stub-store",
			yaml: `kind: component
name: stub-store
version: 1.0.0
output_namespace: internal/storage
requires: []
provides:
  - storage-adapter
slots: []
`,
		},
	} {
		compDir := filepath.Join(registryDir, "components", comp.name)
		if err := os.MkdirAll(compDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(compDir, "component.yaml"), []byte(comp.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return projectDir, registryDir
}

func TestReconcile_PlanShowsGenerateForNewProject(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)

	generators := map[string]gen.Generator{
		"stub-api": &stubGenerator{
			files: []gen.File{
				{Path: "internal/api/handler.go", Content: []byte("package api\n")},
			},
			wiring: &gen.Wiring{
				Imports:      []string{"internal/api"},
				Constructors: []string{"api.NewHandler()"},
				Routes:       []string{`mux.Handle("/widgets", handler)`},
			},
		},
		"stub-store": &stubGenerator{
			files: []gen.File{
				{Path: "internal/storage/store.go", Content: []byte("package storage\n")},
			},
			wiring: &gen.Wiring{
				Imports:      []string{"internal/storage"},
				Constructors: []string{"storage.NewStore(db)"},
				NeedsDB:      true,
			},
		},
	}

	plan, err := Reconcile(ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  generators,
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if !plan.HasChanges() {
		t.Fatal("expected plan to have changes for a new project")
	}

	// Verify all files are marked as "generate".
	for _, f := range plan.Files {
		if f.Action != ActionGenerate {
			t.Errorf("file %s: expected action %q, got %q", f.Path, ActionGenerate, f.Action)
		}
	}

	// Verify expected files are in the plan.
	expectedPaths := map[string]bool{
		"internal/api/handler.go":      false,
		"internal/storage/store.go":    false,
		"cmd/main.go":                  false,
		"go.mod":                       false,
	}
	for _, f := range plan.Files {
		if _, ok := expectedPaths[f.Path]; ok {
			expectedPaths[f.Path] = true
		}
	}
	for p, found := range expectedPaths {
		if !found {
			t.Errorf("expected file %s not found in plan", p)
		}
	}

	// Verify state would be saved.
	if plan.NewState == nil || plan.NewState.LastApplied == nil {
		t.Fatal("expected NewState.LastApplied to be set")
	}
	if plan.NewState.LastApplied.ServiceHash == "" {
		t.Error("expected non-empty service hash")
	}
}

func TestReconcile_ApplyWritesFiles(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)

	generators := map[string]gen.Generator{
		"stub-api": &stubGenerator{
			files: []gen.File{
				{Path: "internal/api/handler.go", Content: []byte("package api\n")},
			},
			wiring: &gen.Wiring{
				Imports:      []string{"internal/api"},
				Constructors: []string{"api.NewHandler()"},
				Routes:       []string{`mux.Handle("/widgets", handler)`},
			},
		},
		"stub-store": &stubGenerator{},
	}

	plan, err := Reconcile(ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  generators,
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	outDir := filepath.Join(projectDir, "out")
	if err := Apply(plan, projectDir, outDir); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Verify files were written.
	handlerPath := filepath.Join(outDir, "internal", "api", "handler.go")
	data, err := os.ReadFile(handlerPath)
	if err != nil {
		t.Fatalf("handler.go not written: %v", err)
	}
	if !strings.Contains(string(data), gen.Header) {
		t.Error("handler.go missing generated header")
	}

	// Verify main.go was written.
	mainPath := filepath.Join(outDir, "cmd", "main.go")
	data, err = os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("main.go not written: %v", err)
	}
	if !strings.Contains(string(data), "package main") {
		t.Error("main.go missing package main")
	}

	// Verify go.mod was written.
	goModPath := filepath.Join(outDir, "go.mod")
	data, err = os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("go.mod not written: %v", err)
	}
	if !strings.Contains(string(data), "github.com/test/svc") {
		t.Error("go.mod missing module name")
	}

	// Verify state was saved.
	statePath := filepath.Join(projectDir, ".stego", "state.yaml")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state.yaml not written: %v", err)
	}
}

func TestReconcile_SubsequentPlanNoChanges(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)

	generators := map[string]gen.Generator{
		"stub-api": &stubGenerator{
			files: []gen.File{
				{Path: "internal/api/handler.go", Content: []byte("package api\n")},
			},
			wiring: &gen.Wiring{
				Imports:      []string{"internal/api"},
				Constructors: []string{"api.NewHandler()"},
				Routes:       []string{`mux.Handle("/widgets", handler)`},
			},
		},
		"stub-store": &stubGenerator{},
	}

	reconcilerInput := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  generators,
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}

	// First apply.
	plan1, err := Reconcile(reconcilerInput)
	if err != nil {
		t.Fatalf("first Reconcile failed: %v", err)
	}
	outDir := filepath.Join(projectDir, "out")
	if err := Apply(plan1, projectDir, outDir); err != nil {
		t.Fatalf("first Apply failed: %v", err)
	}

	// Second plan — no changes expected.
	plan2, err := Reconcile(reconcilerInput)
	if err != nil {
		t.Fatalf("second Reconcile failed: %v", err)
	}

	if plan2.HasChanges() {
		var changes []string
		for _, f := range plan2.Files {
			if f.Action != ActionUnchanged {
				changes = append(changes, f.Path+" ("+string(f.Action)+")")
			}
		}
		t.Errorf("expected no changes on second plan, but got changes: %s", strings.Join(changes, ", "))
	}

	formatted := FormatPlan(plan2)
	if !strings.Contains(formatted, "No changes") {
		t.Errorf("expected 'No changes' in plan output, got: %s", formatted)
	}
}

func TestReconcile_PlanShowsUpdateOnChange(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)

	generators := map[string]gen.Generator{
		"stub-api": &stubGenerator{
			files: []gen.File{
				{Path: "internal/api/handler.go", Content: []byte("package api\n// v1\n")},
			},
			wiring: &gen.Wiring{
				Imports:      []string{"internal/api"},
				Constructors: []string{"api.NewHandler()"},
				Routes:       []string{`mux.Handle("/widgets", handler)`},
			},
		},
		"stub-store": &stubGenerator{},
	}

	reconcilerInput := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  generators,
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}

	// First apply.
	plan1, err := Reconcile(reconcilerInput)
	if err != nil {
		t.Fatalf("first Reconcile failed: %v", err)
	}
	outDir := filepath.Join(projectDir, "out")
	if err := Apply(plan1, projectDir, outDir); err != nil {
		t.Fatalf("first Apply failed: %v", err)
	}

	// Change the generator output.
	generators["stub-api"] = &stubGenerator{
		files: []gen.File{
			{Path: "internal/api/handler.go", Content: []byte("package api\n// v2\n")},
		},
		wiring: &gen.Wiring{
			Imports:      []string{"internal/api"},
			Constructors: []string{"api.NewHandler()"},
			Routes:       []string{`mux.Handle("/widgets", handler)`},
		},
	}

	// Second plan — should show update.
	plan2, err := Reconcile(reconcilerInput)
	if err != nil {
		t.Fatalf("second Reconcile failed: %v", err)
	}

	if !plan2.HasChanges() {
		t.Fatal("expected plan to show changes after generator output changed")
	}

	foundUpdate := false
	for _, f := range plan2.Files {
		if f.Path == "internal/api/handler.go" && f.Action == ActionUpdate {
			foundUpdate = true
		}
	}
	if !foundUpdate {
		t.Error("expected handler.go to show as 'update'")
	}
}

func TestReconcile_ArchetypeNotFound(t *testing.T) {
	projectDir := t.TempDir()
	registryDir := t.TempDir()

	// Service.yaml referencing non-existent archetype.
	serviceYAML := `kind: service
name: test-service
archetype: nonexistent
language: go
entities: []
expose: []
`
	if err := os.WriteFile(filepath.Join(projectDir, "service.yaml"), []byte(serviceYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Empty registry.
	for _, dir := range []string{"archetypes", "components", "mixins"} {
		if err := os.MkdirAll(filepath.Join(registryDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Reconcile(ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	})
	if err == nil {
		t.Fatal("expected error for missing archetype")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestReconcile_MissingServiceYAML(t *testing.T) {
	projectDir := t.TempDir()
	registryDir := t.TempDir()

	_, err := Reconcile(ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	})
	if err == nil {
		t.Fatal("expected error for missing service.yaml")
	}
}

func TestReconcile_StateTracksComponentVersions(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)

	generators := map[string]gen.Generator{
		"stub-api":   &stubGenerator{},
		"stub-store": &stubGenerator{},
	}

	plan, err := Reconcile(ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  generators,
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	state := plan.NewState
	if state.LastApplied == nil {
		t.Fatal("expected LastApplied to be set")
	}

	// Check component versions are tracked.
	apiState, ok := state.LastApplied.Components["stub-api"]
	if !ok {
		t.Fatal("expected stub-api in component state")
	}
	if apiState.Version != "1.0.0" {
		t.Errorf("expected stub-api version 1.0.0, got %s", apiState.Version)
	}

	storeState, ok := state.LastApplied.Components["stub-store"]
	if !ok {
		t.Fatal("expected stub-store in component state")
	}
	if storeState.Version != "1.0.0" {
		t.Errorf("expected stub-store version 1.0.0, got %s", storeState.Version)
	}
}

func TestReconcile_ValidatesServiceAgainstRegistry(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)

	// Modify service.yaml to reference a non-existent component (via archetype
	// that lists it).
	archDir := filepath.Join(registryDir, "archetypes", "test-arch")
	archYAML := `kind: archetype
name: test-arch
language: go
version: 1.0.0
components:
  - stub-api
  - missing-component
conventions:
  layout: flat
  error_handling: problem-details-rfc
  logging: structured-json
  test_pattern: table-driven
bindings:
  storage-adapter: stub-api
`
	if err := os.WriteFile(filepath.Join(archDir, "archetype.yaml"), []byte(archYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Reconcile(ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	})
	if err == nil {
		t.Fatal("expected error for missing component in archetype")
	}
	if !strings.Contains(err.Error(), "missing-component") {
		t.Errorf("expected error to mention missing-component, got: %v", err)
	}
}

func TestReconcile_NamespaceViolation(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)

	generators := map[string]gen.Generator{
		"stub-api": &stubGenerator{
			files: []gen.File{
				// File outside the component's namespace.
				{Path: "cmd/bad.go", Content: []byte("package main\n")},
			},
		},
		"stub-store": &stubGenerator{},
	}

	_, err := Reconcile(ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  generators,
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	})
	if err == nil {
		t.Fatal("expected namespace violation error")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("expected 'namespace' in error, got: %v", err)
	}
}

func TestFormatPlan_NoChanges(t *testing.T) {
	plan := &Plan{
		Files: []PlannedFile{
			{Path: "a.go", Action: ActionUnchanged},
			{Path: "b.go", Action: ActionUnchanged},
		},
	}
	output := FormatPlan(plan)
	if !strings.Contains(output, "No changes") {
		t.Errorf("expected 'No changes', got: %s", output)
	}
}

func TestFormatPlan_MixedActions(t *testing.T) {
	plan := &Plan{
		Files: []PlannedFile{
			{Path: "new.go", Action: ActionGenerate},
			{Path: "changed.go", Action: ActionUpdate},
			{Path: "same.go", Action: ActionUnchanged},
		},
	}
	output := FormatPlan(plan)
	if !strings.Contains(output, "generate: new.go") {
		t.Errorf("expected 'generate: new.go' in output, got: %s", output)
	}
	if !strings.Contains(output, "update:   changed.go") {
		t.Errorf("expected 'update: changed.go' in output, got: %s", output)
	}
	if !strings.Contains(output, "unchanged: 1") {
		t.Errorf("expected 'unchanged: 1' in output, got: %s", output)
	}
}

func TestCollectComponentNames_WithMixin(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)

	// Add a mixin to the registry.
	mixinDir := filepath.Join(registryDir, "mixins", "test-mixin")
	if err := os.MkdirAll(mixinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mixinYAML := `kind: mixin
name: test-mixin
version: 1.0.0
adds_components:
  - stub-api
overrides: none
`
	if err := os.WriteFile(filepath.Join(mixinDir, "mixin.yaml"), []byte(mixinYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create service.yaml with mixin.
	serviceYAML := `kind: service
name: test-service
archetype: test-arch
language: go
entities: []
expose: []
mixins:
  - test-mixin
`
	if err := os.WriteFile(filepath.Join(projectDir, "service.yaml"), []byte(serviceYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := registry.Load(registryDir)
	if err != nil {
		t.Fatal(err)
	}
	svcDecl, err := parser.ParseServiceDeclaration(filepath.Join(projectDir, "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	archetype := reg.Archetype("test-arch")
	names, err := collectComponentNames(archetype, svcDecl, reg)
	if err != nil {
		t.Fatalf("collectComponentNames failed: %v", err)
	}

	// stub-api appears in both archetype and mixin — should be deduplicated.
	count := 0
	for _, n := range names {
		if n == "stub-api" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected stub-api exactly once, got %d times in %v", count, names)
	}
}

func TestReconcile_EntityFieldChangesShown(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)

	generators := map[string]gen.Generator{
		"stub-api":   &stubGenerator{},
		"stub-store": &stubGenerator{},
	}

	reconcilerInput := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  generators,
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}

	// First apply.
	plan1, err := Reconcile(reconcilerInput)
	if err != nil {
		t.Fatalf("first Reconcile failed: %v", err)
	}
	outDir := filepath.Join(projectDir, "out")
	if err := Apply(plan1, projectDir, outDir); err != nil {
		t.Fatalf("first Apply failed: %v", err)
	}

	// Update service.yaml: add a field to Widget.
	serviceYAML := `kind: service
name: test-service
archetype: test-arch
language: go

entities:
  - name: Widget
    fields:
      - { name: label, type: string }
      - { name: color, type: string }

expose:
  - entity: Widget
    operations: [create, read]
`
	if err := os.WriteFile(filepath.Join(projectDir, "service.yaml"), []byte(serviceYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second plan — should show entity field change.
	plan2, err := Reconcile(reconcilerInput)
	if err != nil {
		t.Fatalf("second Reconcile failed: %v", err)
	}

	if len(plan2.EntityChanges) == 0 {
		t.Fatal("expected entity changes after adding a field")
	}

	found := false
	for _, ec := range plan2.EntityChanges {
		if ec.Entity == "Widget" {
			for _, f := range ec.Added {
				if f == "color" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("expected to find 'color' field addition for Widget")
	}

	formatted := FormatPlan(plan2)
	if !strings.Contains(formatted, "entities.Widget") {
		t.Errorf("expected entity change in plan output, got: %s", formatted)
	}
	if !strings.Contains(formatted, "+ field: color") {
		t.Errorf("expected '+ field: color' in plan output, got: %s", formatted)
	}
}

func TestComputeEntityChanges_FieldRemoved(t *testing.T) {
	entities := []types.Entity{
		{Name: "User", Fields: []types.Field{{Name: "email"}}},
	}
	existingState := &State{
		LastApplied: &AppliedState{
			Entities: map[string][]string{
				"User": {"email", "name"},
			},
		},
	}

	changes := computeEntityChanges(entities, existingState)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if len(changes[0].Removed) != 1 || changes[0].Removed[0] != "name" {
		t.Errorf("expected 'name' removed, got %v", changes[0].Removed)
	}
}

func TestComputeEntityChanges_NewEntity(t *testing.T) {
	entities := []types.Entity{
		{Name: "Widget", Fields: []types.Field{{Name: "label"}, {Name: "size"}}},
	}
	existingState := &State{
		LastApplied: &AppliedState{
			Entities: map[string][]string{},
		},
	}

	changes := computeEntityChanges(entities, existingState)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Entity != "Widget" {
		t.Errorf("expected Widget, got %s", changes[0].Entity)
	}
	if len(changes[0].Added) != 2 {
		t.Errorf("expected 2 added fields, got %d", len(changes[0].Added))
	}
}

func TestComputeEntityChanges_NoState(t *testing.T) {
	entities := []types.Entity{
		{Name: "Widget", Fields: []types.Field{{Name: "label"}}},
	}
	changes := computeEntityChanges(entities, &State{})
	if len(changes) != 0 {
		t.Errorf("expected no changes with no previous state, got %d", len(changes))
	}
}

func TestComputeEntityChanges_DeletedEntity(t *testing.T) {
	// Current entities have only Widget; old state had Widget and Gadget.
	entities := []types.Entity{
		{Name: "Widget", Fields: []types.Field{{Name: "label"}}},
	}
	existingState := &State{
		LastApplied: &AppliedState{
			Entities: map[string][]string{
				"Widget": {"label"},
				"Gadget": {"size", "color"},
			},
		},
	}

	changes := computeEntityChanges(entities, existingState)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change (Gadget deleted), got %d: %+v", len(changes), changes)
	}
	if changes[0].Entity != "Gadget" {
		t.Errorf("expected deleted entity Gadget, got %s", changes[0].Entity)
	}
	if len(changes[0].Removed) != 2 {
		t.Errorf("expected 2 removed fields, got %d", len(changes[0].Removed))
	}
}

func TestReconcile_RegistrySHAInState(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)

	generators := map[string]gen.Generator{
		"stub-api":   &stubGenerator{},
		"stub-store": &stubGenerator{},
	}

	plan, err := Reconcile(ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  generators,
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
		RegistrySHA: "a1b2c3d4e5f6",
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	state := plan.NewState
	if state.LastApplied == nil {
		t.Fatal("expected LastApplied to be set")
	}
	if state.LastApplied.RegistrySHA != "a1b2c3d4e5f6" {
		t.Errorf("expected RegistrySHA = %q, got %q", "a1b2c3d4e5f6", state.LastApplied.RegistrySHA)
	}

	// Check component SHAs are also populated.
	apiState, ok := state.LastApplied.Components["stub-api"]
	if !ok {
		t.Fatal("expected stub-api in component state")
	}
	if apiState.SHA != "a1b2c3d4e5f6" {
		t.Errorf("expected stub-api SHA = %q, got %q", "a1b2c3d4e5f6", apiState.SHA)
	}
}

func TestHasChanges_IncludesEntityChanges(t *testing.T) {
	// Plan with no file changes but entity changes should still report changes.
	plan := &Plan{
		Files: []PlannedFile{
			{Path: "a.go", Action: ActionUnchanged},
		},
		EntityChanges: []EntityChange{
			{Entity: "Widget", Added: []string{"color"}},
		},
	}
	if !plan.HasChanges() {
		t.Error("expected HasChanges() = true when entity changes exist")
	}
}

func TestReconcile_OrphanedFilesDetectedAndRemoved(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)

	// First apply with two component files.
	generators := map[string]gen.Generator{
		"stub-api": &stubGenerator{
			files: []gen.File{
				{Path: "internal/api/handler.go", Content: []byte("package api\n")},
				{Path: "internal/api/handler_widget.go", Content: []byte("package api\n// widget\n")},
			},
			wiring: &gen.Wiring{
				Imports:      []string{"internal/api"},
				Constructors: []string{"api.NewHandler()"},
				Routes:       []string{`mux.Handle("/widgets", handler)`},
			},
		},
		"stub-store": &stubGenerator{},
	}

	reconcilerInput := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  generators,
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}

	plan1, err := Reconcile(reconcilerInput)
	if err != nil {
		t.Fatalf("first Reconcile failed: %v", err)
	}
	outDir := filepath.Join(projectDir, "out")
	if err := Apply(plan1, projectDir, outDir); err != nil {
		t.Fatalf("first Apply failed: %v", err)
	}

	// Verify the widget handler was written.
	widgetPath := filepath.Join(outDir, "internal", "api", "handler_widget.go")
	if _, err := os.Stat(widgetPath); err != nil {
		t.Fatalf("handler_widget.go should exist after first apply: %v", err)
	}

	// Second apply: remove handler_widget.go from generator output.
	generators["stub-api"] = &stubGenerator{
		files: []gen.File{
			{Path: "internal/api/handler.go", Content: []byte("package api\n")},
		},
		wiring: &gen.Wiring{
			Imports:      []string{"internal/api"},
			Constructors: []string{"api.NewHandler()"},
			Routes:       []string{`mux.Handle("/widgets", handler)`},
		},
	}

	plan2, err := Reconcile(reconcilerInput)
	if err != nil {
		t.Fatalf("second Reconcile failed: %v", err)
	}

	// Plan should show the orphaned file as a delete action.
	if !plan2.HasChanges() {
		t.Fatal("expected plan to have changes when a file is orphaned")
	}

	foundDelete := false
	for _, f := range plan2.Files {
		if f.Path == "internal/api/handler_widget.go" && f.Action == ActionDelete {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Error("expected handler_widget.go to show as 'delete' in plan")
	}

	// Apply should remove the orphaned file from disk.
	if err := Apply(plan2, projectDir, outDir); err != nil {
		t.Fatalf("second Apply failed: %v", err)
	}

	if _, err := os.Stat(widgetPath); !os.IsNotExist(err) {
		t.Error("expected handler_widget.go to be removed from disk after apply")
	}
}

func TestComputePlan_UsesOutDir(t *testing.T) {
	// Create a temporary directory structure with a non-default output dir.
	tmpDir := t.TempDir()
	customOutDir := filepath.Join(tmpDir, "custom-out")
	if err := os.MkdirAll(customOutDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a file at the custom output path.
	filePath := filepath.Join(customOutDir, "test.go")
	fileContent := []byte("package test\n")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, fileContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// The generated file has the same content as on disk.
	genFile := gen.File{Path: "test.go", Content: fileContent}

	plan := computePlan(
		[]gen.File{genFile},
		&State{},
		[]byte("service: test"),
		nil,
		map[string]*types.Component{},
		customOutDir,
		"",
	)

	// Since the file exists on disk at customOutDir with the same hash,
	// it should be unchanged (not generate).
	for _, f := range plan.Files {
		if f.Path == "test.go" {
			// File exists on disk with same content — disk check uses the
			// file's rendered bytes (via gen.File.Bytes()).
			renderedHash := HashBytes(genFile.Bytes())
			diskHash := HashBytes(fileContent)
			if renderedHash == diskHash {
				if f.Action != ActionUnchanged {
					t.Errorf("expected unchanged for test.go when disk content matches, got %s", f.Action)
				}
			} else {
				// Rendered bytes include header for .go files, so content differs.
				if f.Action != ActionUpdate {
					t.Errorf("expected update for test.go (header added by Bytes()), got %s", f.Action)
				}
			}
			return
		}
	}
	t.Error("test.go not found in plan")
}

func TestComputeEntityChanges_DeletedEntitiesSorted(t *testing.T) {
	// Current: no entities. Old state: Zebra, Alpha, Mango all deleted.
	entities := []types.Entity{}
	existingState := &State{
		LastApplied: &AppliedState{
			Entities: map[string][]string{
				"Zebra": {"stripe"},
				"Alpha": {"first"},
				"Mango": {"sweet"},
			},
		},
	}

	changes := computeEntityChanges(entities, existingState)
	if len(changes) != 3 {
		t.Fatalf("expected 3 deleted entities, got %d", len(changes))
	}

	// Verify alphabetical order.
	expectedOrder := []string{"Alpha", "Mango", "Zebra"}
	for i, expected := range expectedOrder {
		if changes[i].Entity != expected {
			t.Errorf("deleted entity at position %d: expected %q, got %q", i, expected, changes[i].Entity)
		}
	}
}

func TestFormatPlan_DeleteAction(t *testing.T) {
	plan := &Plan{
		Files: []PlannedFile{
			{Path: "keep.go", Action: ActionUnchanged},
			{Path: "orphan.go", Action: ActionDelete},
		},
	}
	output := FormatPlan(plan)
	if !strings.Contains(output, "delete:   orphan.go") {
		t.Errorf("expected 'delete: orphan.go' in output, got: %s", output)
	}
	if !strings.Contains(output, "1 to delete") {
		t.Errorf("expected '1 to delete' in summary, got: %s", output)
	}
}

func TestResolveComponentConfig_MergesDefaults(t *testing.T) {
	comp := &types.Component{
		Config: map[string]types.ConfigField{
			"port":    {Default: 8080},
			"timeout": {Default: 30},
		},
	}
	svcDecl := &types.ServiceDeclaration{}

	config := resolveComponentConfig(comp, svcDecl)
	if config["port"] != 8080 {
		t.Errorf("expected port=8080, got %v", config["port"])
	}
	if config["timeout"] != 30 {
		t.Errorf("expected timeout=30, got %v", config["timeout"])
	}
}

func TestResolveComponentConfig_OverridesDefaults(t *testing.T) {
	comp := &types.Component{
		Name: "my-comp",
		Config: map[string]types.ConfigField{
			"port": {Default: 8080},
		},
	}
	svcDecl := &types.ServiceDeclaration{
		Overrides: map[string]any{
			"my-comp": map[string]any{
				"port": 9090,
			},
		},
	}

	config := resolveComponentConfig(comp, svcDecl)
	if config["port"] != 9090 {
		t.Errorf("expected port=9090 (overridden), got %v", config["port"])
	}
}
