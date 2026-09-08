package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
)

func TestFillNamesCannotEscapeApplicationDirectory(t *testing.T) {
	for _, name := range []string{"../escape", "nested/fill", "/tmp/fill", "..", ".", "bad\\name", "CON", "type", "main", "_", "1policy", "bad.name"} {
		if _, err := fillPackageName(name); err == nil {
			t.Errorf("accepted unsafe fill name %q", name)
		}
	}
}

func TestFillScaffoldRejectsExistingDirectoryAndLinks(t *testing.T) {
	project := t.TempDir()
	out := t.TempDir()
	if err := os.Symlink(out, filepath.Join(project, "fills")); err != nil {
		t.Fatal(err)
	}
	if err := writeFillScaffold(project, "policy", []byte("declaration"), []byte("source")); err == nil {
		t.Fatal("accepted a symbolic link for fills")
	}
	if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
		t.Fatalf("changed a path outside the project: %v", err)
	}
	project = t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "fills", "policy"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeFillScaffold(project, "policy", []byte("declaration"), []byte("source")); err == nil {
		t.Fatal("accepted an existing application directory")
	}
}

// This test builds the real generated service and executes a scaffold method.
// No database or HTTP server is needed for this contract check.
func TestFillWorkflowBuild(t *testing.T) {
	registry, err := filepath.Abs("../../registry")
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	t.Chdir(project)
	t.Setenv("STEGO_REGISTRY", registry)
	t.Setenv("STEGO_MODULE", "example.com/fill-workflow")
	t.Setenv("STEGO_GO_VERSION", "1.26.8")
	t.Setenv("GOWORK", "off")
	if err := runInit([]string{"--archetype", "rest-crud"}); err != nil {
		t.Fatal(err)
	}
	if err := runFillCreate([]string{"policy", "--slot", "before_create", "--collection", "widgets"}); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(project, "fills/policy/fill.go"))
	if err != nil {
		t.Fatal(err)
	}
	service := `kind: service
name: fill-workflow
archetype: rest-crud
language: go
entities:
  - name: Widget
    fields:
      - {name: label, type: string}
collections:
  widgets:
    entity: Widget
    operations: [create, read]
slots:
  - collection: widgets
    slot: before_create
    gate: [policy]
`
	if err := os.WriteFile("service.yaml", []byte(service), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runApply(nil); err != nil {
		t.Fatal(err)
	}
	contractTest := `package policy
import (
  "context"
  "errors"
  "testing"
  slots "example.com/fill-workflow/out/slots"
)
func TestScaffoldDeniesUnimplementedPolicy(t *testing.T) {
  var policy slots.BeforeCreateSlot = New()
  result, err := policy.Evaluate(context.Background(), &slots.BeforeCreateRequest{})
  if result != nil || !errors.Is(err, ErrNotImplemented) {
    t.Fatalf("unfinished policy returned success: %v, %v", result, err)
  }
}
`
	if err := os.WriteFile("fills/policy/fill_test.go", []byte(contractTest), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runDependencies(nil); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"test", "-mod=readonly", "./..."}, {"build", "-mod=readonly", "-o", filepath.Join(t.TempDir(), "service"), "./out"}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, output)
		}
	}
	resolvedModule, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	resolvedSums, err := os.ReadFile("go.sum")
	if err != nil {
		t.Fatal(err)
	}
	if err := runApply(nil); err != nil {
		t.Fatal(err)
	}
	afterApply, err := os.ReadFile("go.mod")
	if err != nil || !bytes.Equal(resolvedModule, afterApply) {
		t.Fatalf("repeated apply changed the resolved module: %v", err)
	}
	if err := runDependencies(nil); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"test", "-mod=readonly", "./..."}, {"mod", "tidy"}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("after repeated apply, go %v: %v\n%s", args, err, output)
		}
	}
	afterTidy, err := os.ReadFile("go.mod")
	if err != nil || !bytes.Equal(resolvedModule, afterTidy) {
		t.Fatalf("dependency resolution changed the module after repeated apply: %v\nbefore:\n%s\nafter:\n%s", err, resolvedModule, afterTidy)
	}
	afterSums, err := os.ReadFile("go.sum")
	if err != nil || !bytes.Equal(resolvedSums, afterSums) {
		t.Fatalf("dependency resolution changed checksums after repeated apply: %v", err)
	}
	input, err := buildReconcilerInput()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Reconcile(input)
	if err != nil || plan.HasChanges() {
		t.Fatalf("generation is not stable after dependency resolution: %v", err)
	}
	drift, err := compiler.DetectDrift(project, filepath.Join(project, "out"))
	if err != nil || drift.HasDrift() {
		t.Fatalf("resolved service has output drift: %v", err)
	}
	after, err := os.ReadFile("fills/policy/fill.go")
	if err != nil || !bytes.Equal(source, after) {
		t.Fatalf("apply changed the application fill: %v", err)
	}
}
