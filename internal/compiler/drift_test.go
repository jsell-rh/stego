package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"gopkg.in/yaml.v3"
)

func TestDetectDrift_NoState(t *testing.T) {
	projectDir := t.TempDir()
	outDir := filepath.Join(projectDir, "out")

	_, err := DetectDrift(projectDir, outDir)
	if err == nil {
		t.Fatal("expected error for missing state")
	}
	if !strings.Contains(err.Error(), "stego apply") {
		t.Errorf("expected error to mention 'stego apply', got: %v", err)
	}
}

func TestDetectDrift_NoDrift(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)
	outDir := filepath.Join(projectDir, "out")

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
	if err := Apply(plan, projectDir, outDir); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	result, err := DetectDrift(projectDir, outDir)
	if err != nil {
		t.Fatalf("DetectDrift returned error: %v", err)
	}
	if result.HasDrift() {
		t.Errorf("expected no drift, got:\n%s", FormatDrift(result))
	}
}

func TestDetectDrift_ModifiedFile(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)
	outDir := filepath.Join(projectDir, "out")

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
	if err := Apply(plan, projectDir, outDir); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Hand-edit a generated file.
	handlerPath := filepath.Join(outDir, "internal", "api", "handler.go")
	if err := os.WriteFile(handlerPath, []byte("package api\n// hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := DetectDrift(projectDir, outDir)
	if err != nil {
		t.Fatalf("DetectDrift returned error: %v", err)
	}
	if !result.HasDrift() {
		t.Fatal("expected drift to be detected")
	}
	if len(result.Modified) != 1 {
		t.Fatalf("expected 1 modified file, got %d", len(result.Modified))
	}
	if result.Modified[0].Path != "internal/api/handler.go" {
		t.Errorf("expected modified file 'internal/api/handler.go', got %q", result.Modified[0].Path)
	}
}

func TestDetectDrift_DeletedFile(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)
	outDir := filepath.Join(projectDir, "out")

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
	if err := Apply(plan, projectDir, outDir); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Delete a generated file.
	handlerPath := filepath.Join(outDir, "internal", "api", "handler.go")
	if err := os.Remove(handlerPath); err != nil {
		t.Fatal(err)
	}

	result, err := DetectDrift(projectDir, outDir)
	if err != nil {
		t.Fatalf("DetectDrift returned error: %v", err)
	}
	if !result.HasDrift() {
		t.Fatal("expected drift to be detected")
	}
	if len(result.Deleted) != 1 {
		t.Fatalf("expected 1 deleted file, got %d", len(result.Deleted))
	}
	if result.Deleted[0].Path != "internal/api/handler.go" {
		t.Errorf("expected deleted file 'internal/api/handler.go', got %q", result.Deleted[0].Path)
	}
}

func TestDetectDrift_MultipleChanges(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)
	outDir := filepath.Join(projectDir, "out")

	generators := map[string]gen.Generator{
		"stub-api": &stubGenerator{
			files: []gen.File{
				{Path: "internal/api/handler.go", Content: []byte("package api\n")},
				{Path: "internal/api/routes.go", Content: []byte("package api\n// routes\n")},
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
	if err := Apply(plan, projectDir, outDir); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Modify one file and delete another.
	handlerPath := filepath.Join(outDir, "internal", "api", "handler.go")
	if err := os.WriteFile(handlerPath, []byte("package api\n// hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	routesPath := filepath.Join(outDir, "internal", "api", "routes.go")
	if err := os.Remove(routesPath); err != nil {
		t.Fatal(err)
	}

	result, err := DetectDrift(projectDir, outDir)
	if err != nil {
		t.Fatalf("DetectDrift returned error: %v", err)
	}
	if !result.HasDrift() {
		t.Fatal("expected drift to be detected")
	}
	if len(result.Modified) != 1 {
		t.Errorf("expected 1 modified file, got %d", len(result.Modified))
	}
	if len(result.Deleted) != 1 {
		t.Errorf("expected 1 deleted file, got %d", len(result.Deleted))
	}
}

func TestDetectDrift_ApplicationModuleIsNotGenerated(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)
	outDir := filepath.Join(projectDir, "out")

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
	if err := Apply(plan, projectDir, outDir); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Modify go.mod at project root.
	goModPath := filepath.Join(projectDir, "go.mod")
	if err := os.WriteFile(goModPath, []byte("module github.com/test/svc\n// hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := DetectDrift(projectDir, outDir)
	if err != nil {
		t.Fatalf("DetectDrift returned error: %v", err)
	}
	if result.HasDrift() {
		t.Fatalf("application module edits are not output drift: %s", FormatDrift(result))
	}
}

func TestFormatDrift_NoDrift(t *testing.T) {
	r := &DriftResult{}
	got := FormatDrift(r)
	if !strings.Contains(got, "No drift detected") {
		t.Errorf("expected 'No drift detected' message, got: %s", got)
	}
}

func TestFormatDrift_WithDrift(t *testing.T) {
	r := &DriftResult{
		Modified: []DriftedFile{{Path: "internal/api/handler.go"}},
		Deleted:  []DriftedFile{{Path: "internal/api/routes.go"}},
	}
	got := FormatDrift(r)
	if !strings.Contains(got, "2 file(s)") {
		t.Errorf("expected '2 file(s)' in output, got: %s", got)
	}
	if !strings.Contains(got, "modified:") {
		t.Errorf("expected 'modified:' in output, got: %s", got)
	}
	if !strings.Contains(got, "deleted:") {
		t.Errorf("expected 'deleted:' in output, got: %s", got)
	}
}

func TestDetectDrift_InputChanged(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)
	outDir := filepath.Join(projectDir, "out")

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
	if err := Apply(plan, projectDir, outDir); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Change a recorded source input after apply.
	if err := os.WriteFile(filepath.Join(projectDir, "service.yaml"), []byte("kind: service\nname: test-service\narchetype: test-arch\nlanguage: go\n\nentities:\n  - name: Widget\n    fields:\n      - { name: label, type: string }\n      - { name: note, type: string }\n\ncollections:\n  widgets:\n    entity: Widget\n    operations: [create, read]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := DetectDrift(projectDir, outDir)
	if err != nil {
		t.Fatalf("DetectDrift returned error: %v", err)
	}
	if !result.HasDrift() {
		t.Fatal("expected input drift to be detected")
	}
	if len(result.Inputs) != 1 || result.Inputs[0].Path != "service.yaml" {
		t.Fatalf("expected drifted input 'service.yaml', got %+v", result.Inputs)
	}
	if len(result.Modified) != 0 || len(result.Deleted) != 0 {
		t.Fatalf("expected no output drift, got %+v %+v", result.Modified, result.Deleted)
	}
	if got := FormatDrift(result); !strings.Contains(got, "input file(s) changed") || !strings.Contains(got, "stego apply") {
		t.Errorf("expected input drift report, got:\n%s", got)
	}
}

func TestDetectDrift_InputDeleted(t *testing.T) {
	projectDir, registryDir := setupTestProject(t)
	outDir := filepath.Join(projectDir, "out")

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
	if err := Apply(plan, projectDir, outDir); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Delete a recorded source input after apply.
	if err := os.Remove(filepath.Join(projectDir, "service.yaml")); err != nil {
		t.Fatal(err)
	}

	result, err := DetectDrift(projectDir, outDir)
	if err != nil {
		t.Fatalf("DetectDrift returned error: %v", err)
	}
	if !result.HasDrift() {
		t.Fatal("expected input drift for deleted input")
	}
	if len(result.Inputs) != 1 || result.Inputs[0].Path != "service.yaml" {
		t.Fatalf("expected drifted input 'service.yaml', got %+v", result.Inputs)
	}
}

func TestDetectDrift_InputDeletedButModuleFilesIgnored(t *testing.T) {
	// go.mod and go.sum are application-owned: stego merges go.mod at apply
	// and go tooling may rewrite both afterwards. Their recorded pre-apply
	// hashes must not report drift.
	projectDir, registryDir := setupTestProject(t)
	outDir := filepath.Join(projectDir, "out")

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
	if err := Apply(plan, projectDir, outDir); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Rewrite both module files after apply.
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module github.com/test/svc\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "go.sum"), []byte("rewritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := DetectDrift(projectDir, outDir)
	if err != nil {
		t.Fatalf("DetectDrift returned error: %v", err)
	}
	if result.HasDrift() {
		t.Fatalf("expected module-file changes to be ignored, got:\n%s", FormatDrift(result))
	}
}

func TestDetectDrift_StateWithoutInputs(t *testing.T) {
	// States written by older compilers have no input manifest. Input
	// drift detection must stay silent for them.
	projectDir, registryDir := setupTestProject(t)
	outDir := filepath.Join(projectDir, "out")

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
	// Simulate an old state by clearing the recorded input manifest after
	// apply. Older compilers wrote state without an input manifest.
	if err := Apply(plan, projectDir, outDir); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	oldState := *plan.NewState.LastApplied
	oldState.Inputs = nil
	stateData, err := yaml.Marshal(&State{FormatVersion: StateFormatVersion, LastApplied: &oldState})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".stego", "state.yaml"), stateData, 0o644); err != nil {
		t.Fatal(err)
	}

	// Change a source that an old state never recorded.
	if err := os.WriteFile(filepath.Join(projectDir, "service.yaml"), []byte("kind: service\nname: other\narchetype: test-arch\nlanguage: go\n\nentities:\n  - name: Widget\n    fields:\n      - { name: label, type: string }\n\ncollections:\n  widgets:\n    entity: Widget\n    operations: [create, read]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := DetectDrift(projectDir, outDir)
	if err != nil {
		t.Fatalf("DetectDrift returned error: %v", err)
	}
	if result.HasDrift() {
		t.Fatalf("expected no drift for state without inputs, got:\n%s", FormatDrift(result))
	}
}
