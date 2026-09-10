package compiler

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

func TestDependencyFailuresPreserveProjectFiles(t *testing.T) {
	for _, failure := range []string{"tidy", "verify", "list", "diff", "source-add", "source-edit", "source-delete", "module-edit", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			input := snapshotTestInput(t)
			applyInitialSnapshot(t, input)
			write := func(name, data string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(input.ProjectDir, name), []byte(data), 0644); err != nil {
					t.Fatal(err)
				}
			}
			write("domain.go", "package domain\n")
			write("go.sum", "prior checksums\n")
			module, err := os.ReadFile(filepath.Join(input.ProjectDir, "go.mod"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			run := func(_ context.Context, _, stage string, args ...string) error {
				calls++
				if calls == 1 {
					if err := os.WriteFile(stage, append([]byte("// Resolved module.\n"), module...), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(strings.TrimSuffix(stage, ".mod")+".sum", []byte("new checksums\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "tidy" && calls == 1 || failure == "verify" && calls == 2 || failure == "list" && calls == 3 || failure == "diff" && calls == 4 {
					return errors.New("injected Go command failure")
				}
				if calls == 4 {
					switch failure {
					case "source-add":
						write("added.go", "package domain\n")
					case "source-edit":
						write("domain.go", "package domain\n// Edited.\n")
					case "source-delete":
						if err := os.Remove(filepath.Join(input.ProjectDir, "domain.go")); err != nil {
							t.Fatal(err)
						}
					case "module-edit":
						module = append(module, []byte("\n// Application edit.\n")...)
						write("go.mod", string(module))
					case "cancel":
						cancel()
					}
				}
				return nil
			}
			if err := resolveDependencies(ctx, input, run, nil); err == nil {
				t.Fatal("dependency failure was accepted")
			}
			for name, expected := range map[string][]byte{"go.mod": module, "go.sum": []byte("prior checksums\n")} {
				actual, err := os.ReadFile(filepath.Join(input.ProjectDir, name))
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("failed resolution changed %s: %v", name, err)
				}
			}
			if _, err := os.Stat(filepath.Join(input.ProjectDir, transactionPath)); !os.IsNotExist(err) {
				t.Fatalf("failed resolution prepared a transaction: %v", err)
			}
		})
	}
}

func TestDependencyResolutionUsesLocalReplacementsAndPreservesThem(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	dependency := filepath.Join(input.ProjectDir, "local")
	if err := os.Mkdir(dependency, 0755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"go.mod":   "module example.com/local\ngo 1.26.8\n",
		"local.go": "package local\n",
	} {
		if err := os.WriteFile(filepath.Join(dependency, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	modulePath := filepath.Join(input.ProjectDir, "go.mod")
	module, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatal(err)
	}
	module = append(module, []byte("\nrequire example.com/local v0.0.0\nreplace example.com/local => ./local\n")...)
	if err := os.WriteFile(modulePath, module, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input.ProjectDir, "domain.go"), []byte("package domain\nimport _ \"example.com/local\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOWORK", filepath.Join(t.TempDir(), "missing.work"))
	t.Setenv("GOFLAGS", "-mod=vendor")
	for i := 0; i < 2; i++ {
		if err := ResolveDependencies(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		plan, err := Reconcile(input)
		if err != nil || plan.hasOutputChanges() {
			t.Fatalf("resolution changed compiler output: %v", err)
		}
		if err := Apply(plan, input.ProjectDir, ""); err != nil {
			t.Fatal(err)
		}
		stable, err := Reconcile(input)
		if err != nil || stable.HasChanges() {
			t.Fatal("resolved inputs did not stabilize", err)
		}
	}
	resolved, err := os.ReadFile(modulePath)
	if err != nil || !strings.Contains(string(resolved), "replace example.com/local => ./local") {
		t.Fatalf("local replacement was not preserved: %v", err)
	}
}

func TestDependencyRecoveryCompletesBothModuleFiles(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	err := resolveDependencies(context.Background(), input, runDependencyCommand, func(point string) error {
		if point == "after_operation_0" {
			return errors.New("interrupted dependency commit")
		}
		return nil
	})
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("expected recoverable dependency update: %v", err)
	}
	if err := Recover(input.ProjectDir); err != nil {
		t.Fatal(err)
	}
	if err := ResolveDependencies(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(filepath.Join(input.ProjectDir, ".stego/state.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		if _, exists := state.LastApplied.Files[name]; exists {
			t.Fatalf("compiler claims ownership of %s", name)
		}
	}
}

func TestDependencyResolutionRequiresAppliedCode(t *testing.T) {
	input := snapshotTestInput(t)
	if err := ResolveDependencies(context.Background(), input); err == nil || !strings.Contains(err.Error(), "stego apply") {
		t.Fatalf("resolved unapplied code: %v", err)
	}
}

func TestResolvedModuleCannotLowerComponentMinimum(t *testing.T) {
	before, err := modfile.Parse("go.mod", []byte("module example.com/app\ngo 1.26.8\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "require example.com/dep v1.0.0\n"} {
		if err := validateResolvedModule(before, []byte("module example.com/app\ngo 1.26.8\n"+suffix), map[string]string{"example.com/dep": "v1.2.0"}); err == nil {
			t.Fatal("accepted missing or lower component requirement")
		}
	}
}

func TestDependencyResolutionRejectsChangedExternalReplacement(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	external := t.TempDir()
	for name, data := range map[string]string{"go.mod": "module example.com/local\ngo 1.26.8\n", "local.go": "package local\n"} {
		if err := os.WriteFile(filepath.Join(external, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	modulePath := filepath.Join(input.ProjectDir, "go.mod")
	moduleData, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatal(err)
	}
	module, err := modfile.Parse("go.mod", moduleData, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := module.AddReplace("example.com/local", "", external, ""); err != nil {
		t.Fatal(err)
	}
	moduleData, err = module.Format()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modulePath, moduleData, 0644); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, directory, stage string, args ...string) error {
		if err := runDependencyCommand(ctx, directory, stage, args...); err != nil {
			return err
		}
		if args[len(args)-1] == "-diff" {
			return os.WriteFile(filepath.Join(external, "local.go"), []byte("package local\n// Edited during resolution.\n"), 0644)
		}
		return nil
	}
	if err := resolveDependencies(context.Background(), input, run, nil); err == nil || !strings.Contains(err.Error(), "inputs changed") {
		t.Fatalf("accepted an external module edit: %v", err)
	}
	after, err := os.ReadFile(modulePath)
	if err != nil || !bytes.Equal(after, moduleData) {
		t.Fatalf("changed project module after input conflict: %v", err)
	}
}

func TestDependencyResolutionHoldsProjectLock(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	checked := false
	run := func(ctx context.Context, directory, stage string, args ...string) error {
		plan, err := Reconcile(input)
		if err != nil {
			return err
		}
		if err := Apply(plan, input.ProjectDir, ""); err == nil {
			t.Fatal("apply acquired the dependency writer's lock")
		}
		checked = true
		return runDependencyCommand(ctx, directory, stage, args...)
	}
	if err := resolveDependencies(context.Background(), input, run, nil); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("no dependency command ran")
	}
}
