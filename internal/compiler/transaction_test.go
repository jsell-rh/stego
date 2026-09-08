package compiler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
	"gopkg.in/yaml.v3"
)

func transactionGenerators(next bool) map[string]gen.Generator {
	files := []gen.File{
		{Path: "internal/api/a.go", Content: []byte("package api\n// Old version.\n")},
		{Path: "internal/api/orphan.go", Content: []byte("package api\n// Removed in the next version.\n")},
	}
	if next {
		files = []gen.File{
			{Path: "internal/api/a.go", Content: []byte("package api\n// New version.\n")},
			{Path: "internal/api/b.go", Content: []byte("package api\n// New file.\n")},
		}
	}
	return map[string]gen.Generator{"stub-api": &stubGenerator{files: files}, "stub-store": &stubGenerator{}}
}

func transactionInput(t *testing.T) ReconcilerInput {
	t.Helper()
	input := snapshotTestInput(t)
	input.Generators = transactionGenerators(false)
	applyInitialSnapshot(t, input)
	input.Generators = transactionGenerators(true)
	return input
}

func prepareInterruptedApply(t *testing.T, input ReconcilerInput, point string) {
	t.Helper()
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	err = applyWithFault(plan, input.ProjectDir, "", func(actual string) error {
		if actual == point {
			return fmt.Errorf("injected failure at %s", point)
		}
		return nil
	})
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("expected pending transaction at %s, got %v", point, err)
	}
}

func checkRecovered(t *testing.T, input ReconcilerInput) {
	t.Helper()
	if err := Recover(input.ProjectDir); err != nil {
		t.Fatal(err)
	}
	if err := Recover(input.ProjectDir); err != nil {
		t.Fatalf("repeat recovery failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(input.ProjectDir, transactionPath)); !os.IsNotExist(err) {
		t.Fatalf("transaction record remains: %v", err)
	}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.HasChanges() {
		t.Fatalf("recovery did not complete the planned output: %s", FormatPlan(plan))
	}
	drift, err := DetectDrift(input.ProjectDir, filepath.Join(input.ProjectDir, "out"))
	if err != nil || drift.HasDrift() {
		t.Fatalf("recovery left output drift: %v", err)
	}
	if _, err := os.Stat(filepath.Join(input.ProjectDir, "out/internal/api/orphan.go")); !os.IsNotExist(err) {
		t.Fatalf("orphan remains after recovery: %v", err)
	}
}

func TestRecoverCompletesEveryInterruptedPhase(t *testing.T) {
	for _, point := range []string{"prepared", "after_operation_0", "after_operation_1", "after_operation_2", "after_operation_3", "before_complete"} {
		t.Run(point, func(t *testing.T) {
			input := transactionInput(t)
			prepareInterruptedApply(t, input, point)
			if _, err := Reconcile(input); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("plan ignored a pending transaction: %v", err)
			}
			if _, err := DetectDrift(input.ProjectDir, filepath.Join(input.ProjectDir, "out")); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("drift ignored a pending transaction: %v", err)
			}
			checkRecovered(t, input)
		})
	}
}

func TestFailureBeforePrepareKeepsPriorOutput(t *testing.T) {
	input := transactionInput(t)
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	err = applyWithFault(plan, input.ProjectDir, "", func(point string) error {
		if point == "before_prepare" {
			return errors.New("prepare failed")
		}
		return nil
	})
	if err == nil || errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("unexpected prepare result: %v", err)
	}
	drift, err := DetectDrift(input.ProjectDir, filepath.Join(input.ProjectDir, "out"))
	if err != nil || drift.HasDrift() {
		t.Fatalf("prepare failure changed output: %v", err)
	}
	if _, err := os.Stat(filepath.Join(input.ProjectDir, transactionPath)); !os.IsNotExist(err) {
		t.Fatal("unprepared transaction was published")
	}
}

func TestRecoveryConflictDoesNotOverwriteFiles(t *testing.T) {
	input := transactionInput(t)
	prepareInterruptedApply(t, input, "prepared")
	path := filepath.Join(input.ProjectDir, "out/internal/api/b.go")
	if err := os.WriteFile(path, []byte("application edit"), 0644); err != nil {
		t.Fatal(err)
	}
	err := Recover(input.ProjectDir)
	if err == nil || !strings.Contains(err.Error(), "recovery conflict") {
		t.Fatalf("recovery accepted a conflicting file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "application edit" {
		t.Fatalf("recovery overwrote the edit: %v", err)
	}
	old, err := os.ReadFile(filepath.Join(input.ProjectDir, "out/internal/api/a.go"))
	if err != nil || !strings.Contains(string(old), "Old version") {
		t.Fatalf("recovery wrote before checking all files: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	checkRecovered(t, input)
}

func TestRecoveryRejectsCorruptRecordsBeforeWriting(t *testing.T) {
	for _, kind := range []string{"hash", "path", "output", "duplicate", "missing-state", "unknown-key"} {
		t.Run(kind, func(t *testing.T) {
			input := transactionInput(t)
			prepareInterruptedApply(t, input, "prepared")
			path := filepath.Join(input.ProjectDir, transactionPath)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var tx transaction
			if err := parser.DecodeStrictWithLimit(original, path, &tx, maxTransactionBytes); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "hash":
				tx.Operations[0].Data = "Y29ycnVwdA=="
			case "path":
				tx.Operations[0].Path = "../fills/policy.go"
			case "output":
				tx.OutputDir = "fills"
			case "duplicate":
				tx.Operations = append(tx.Operations[:1], tx.Operations...)
			case "missing-state":
				tx.Operations = tx.Operations[:len(tx.Operations)-1]
			}
			corrupt, err := yaml.Marshal(tx)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "unknown-key" {
				corrupt = append([]byte("unexpected: true\n"), corrupt...)
			}
			if err := os.WriteFile(path, corrupt, 0600); err != nil {
				t.Fatal(err)
			}
			if err := Recover(input.ProjectDir); err == nil {
				t.Fatal("corrupt transaction was accepted")
			}
			old, err := os.ReadFile(filepath.Join(input.ProjectDir, "out/internal/api/a.go"))
			if err != nil || !strings.Contains(string(old), "Old version") {
				t.Fatalf("corrupt record changed output: %v", err)
			}
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			checkRecovered(t, input)
		})
	}
}

func TestRecoveryPreservesNewSourceEdits(t *testing.T) {
	input := transactionInput(t)
	prepareInterruptedApply(t, input, "prepared")
	path := filepath.Join(input.ProjectDir, "service.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := append(data, []byte("\n# Edit made after interruption.\n")...)
	if err := os.WriteFile(path, changed, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Recover(input.ProjectDir); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, changed) {
		t.Fatalf("recovery changed source: %v", err)
	}
	plan, err := Reconcile(input)
	if err != nil || !plan.StateChanged {
		t.Fatalf("next plan did not detect the source edit: %v", err)
	}
}

func TestApplyChecksCompletedFilesBeforeClearingTransaction(t *testing.T) {
	input := transactionInput(t)
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	err = applyWithFault(plan, input.ProjectDir, "", func(point string) error {
		if point == "before_complete" {
			return os.WriteFile(filepath.Join(input.ProjectDir, "out/internal/api/a.go"), []byte("changed during apply"), 0644)
		}
		return nil
	})
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("completed-file change was not detected: %v", err)
	}
	if err := Recover(input.ProjectDir); err == nil {
		t.Fatal("recovery overwrote a conflicting completed file")
	}
}

func TestApplyPreservesExistingFilePermissions(t *testing.T) {
	input := transactionInput(t)
	path := filepath.Join(input.ProjectDir, "out/internal/api/a.go")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() {
		t.Fatalf("apply changed file permissions: %v to %v", before.Mode(), after.Mode())
	}
}

func TestInconsistentFileLayoutsFailBeforeApply(t *testing.T) {
	for _, names := range [][]string{
		{"internal/api/file.go", "internal/api/FILE.go"},
		{"internal/api/file", "internal/api/file/child.go"},
	} {
		input := snapshotTestInput(t)
		var files []gen.File
		for _, name := range names {
			files = append(files, gen.File{Path: name, Content: []byte("package api\n")})
		}
		input.Generators["stub-api"] = &stubGenerator{files: files}
		if _, err := Reconcile(input); err == nil {
			t.Fatalf("accepted conflicting output paths %v", names)
		}
		if _, err := os.Stat(filepath.Join(input.ProjectDir, "out")); !os.IsNotExist(err) {
			t.Fatal("invalid layout changed output")
		}
	}
}

func TestRecoveryAfterProcessExit(t *testing.T) {
	for _, point := range []string{"prepared", "after_operation_1", "before_complete"} {
		t.Run(point, func(t *testing.T) {
			input := transactionInput(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTransactionProcessHelper$")
			command.Env = append(os.Environ(), "STEGO_TEST_TRANSACTION_PROJECT="+input.ProjectDir, "STEGO_TEST_TRANSACTION_REGISTRY="+input.RegistryDir, "STEGO_TEST_TRANSACTION_POINT="+point)
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 99 {
				t.Fatalf("unexpected child exit: %v\n%s", err, output)
			}
			checkRecovered(t, input)
		})
	}
}

func TestTransactionProcessHelper(t *testing.T) {
	project := os.Getenv("STEGO_TEST_TRANSACTION_PROJECT")
	if project == "" {
		return
	}
	input := ReconcilerInput{
		ProjectDir: project, RegistryDir: os.Getenv("STEGO_TEST_TRANSACTION_REGISTRY"),
		ModuleName: "example.com/snapshot", GoVersion: "1.26.8", Generators: transactionGenerators(true),
	}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	err = applyWithFault(plan, project, "", func(point string) error {
		if point == os.Getenv("STEGO_TEST_TRANSACTION_POINT") {
			os.Exit(99)
		}
		return nil
	})
	t.Fatalf("crash point was not reached: %v", err)
}
