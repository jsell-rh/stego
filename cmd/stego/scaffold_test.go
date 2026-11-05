package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/fill"
	"github.com/jsell-rh/stego/internal/parser"
	"gopkg.in/yaml.v3"
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
	t.Run("ST-EXT-UC01-R01-T01 A scaffolded fill compiles against its slot contract, and its test runs in the generated service", func(t *testing.T) {
		registry, err := filepath.Abs("../../registry")
		if err != nil {
			t.Fatal(err)
		}
		project := t.TempDir()
		t.Chdir(project)
		t.Setenv("STEGO_REGISTRY", registry)
		t.Setenv("STEGO_MODULE", "example.com/fill-workflow")
		t.Setenv("STEGO_GO_VERSION", "1.26.10")
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
		declaration := `kind: service
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
`
		binding := `slots:
  - collection: widgets
    slot: before_create
    gate: [policy]
`
		// The first apply, with no fill bound, writes the slot contracts that
		// the fill compiles against.
		if err := os.WriteFile("service.yaml", []byte(declaration), 0644); err != nil {
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
		// stego test records the fill's evidence, and the service author signs
		// it with a test key that SIGNERS lists, so that apply accepts the
		// bound fill.
		if err := runTest(nil); err != nil {
			t.Fatal(err)
		}
		keys := testFillKeys(t)
		if err := os.WriteFile(fill.SignersFile, []byte(keys.signers), 0644); err != nil {
			t.Fatal(err)
		}
		for _, entry := range keys.goEnv {
			name, value, _ := strings.Cut(entry, "=")
			t.Setenv(name, value)
		}
		t.Setenv("HOME", keys.homes["listed-ssh"])
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(keys.homes["listed-ssh"], ".config"))
		if err := runFillSign([]string{"policy"}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("service.yaml", []byte(declaration+binding), 0644); err != nil {
			t.Fatal(err)
		}
		if err := runApply(nil); err != nil {
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
	})
}

// --- Reading of the fill manifest (internal/fill) ---

// fillFixture is a fill manifest under testdata/fill.
func fillFixture(name string) string {
	return filepath.Join("testdata", "fill", name)
}

func TestParseFill(t *testing.T) {
	f, err := fill.ParseFill(fillFixture("fill.yaml"))
	if err != nil {
		t.Fatalf("ParseFill: %v", err)
	}
	if f.Kind != "fill" {
		t.Errorf("kind = %q, want %q", f.Kind, "fill")
	}
	if f.Name != "admin-creation-policy" {
		t.Errorf("name = %q, want %q", f.Name, "admin-creation-policy")
	}
	if f.Implements != "rest-api.before_create" {
		t.Errorf("implements = %q, want %q", f.Implements, "rest-api.before_create")
	}
	if f.Collection != "org-users" {
		t.Errorf("collection = %q, want %q", f.Collection, "org-users")
	}
}

func TestParseFillTyped(t *testing.T) {
	f, err := fill.ParseFill(fillFixture("fill.yaml"))
	if err != nil {
		t.Fatalf("ParseFill: %v", err)
	}
	if f.Name != "admin-creation-policy" {
		t.Errorf("name = %q, want %q", f.Name, "admin-creation-policy")
	}
}

func TestRoundTripFill(t *testing.T) {
	f, err := fill.ParseFill(fillFixture("fill.yaml"))
	if err != nil {
		t.Fatalf("ParseFill: %v", err)
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var f2 fill.Fill
	if err := yaml.Unmarshal(data, &f2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if !reflect.DeepEqual(*f, f2) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  got:      %+v", *f, f2)
	}
}

func TestFillRejectsDeprecatedEntityKey(t *testing.T) {
	input := `
kind: fill
name: my-policy
implements: rest-api.before_create
entity: User
`
	var f fill.Fill
	err := yaml.Unmarshal([]byte(input), &f)
	if err == nil {
		t.Fatal("expected error for deprecated 'entity' key in fill declaration, got nil")
	}
	if !strings.Contains(err.Error(), "renamed to 'collection'") {
		t.Errorf("error message should mention renaming to 'collection', got: %v", err)
	}
}

func TestFillUnmarshal(t *testing.T) {
	input := `
kind: fill
name: admin-creation-policy
implements: rest-api.before_create
collection: org-users
`
	var f fill.Fill
	if err := yaml.Unmarshal([]byte(input), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Name != "admin-creation-policy" {
		t.Errorf("name = %q, want %q", f.Name, "admin-creation-policy")
	}
	if f.Implements != "rest-api.before_create" {
		t.Errorf("implements = %q, want %q", f.Implements, "rest-api.before_create")
	}
	if f.Collection != "org-users" {
		t.Errorf("collection = %q, want %q", f.Collection, "org-users")
	}
}

func TestStrictFillManifest(t *testing.T) {
	err := parser.DecodeStrict([]byte("kind: fill\nqualifed_by: operator\n"), "fill.yaml", &fill.Fill{})
	if err == nil || !strings.Contains(err.Error(), "qualifed_by") {
		t.Fatalf("got %v, want error containing %q", err, "qualifed_by")
	}
}
