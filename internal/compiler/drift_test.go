package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
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

func TestDetectDrift_ProjectRootFile(t *testing.T) {
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
	if !result.HasDrift() {
		t.Fatal("expected drift to be detected for go.mod")
	}
	found := false
	for _, f := range result.Modified {
		if f.Path == "go.mod" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected go.mod in modified list, got:\n%s", FormatDrift(result))
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
