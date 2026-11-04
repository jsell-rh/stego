package main

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
	"golang.org/x/mod/modfile"
)

// These tests run stego deps in process, as a service author does, against a
// project that stego apply made from the built-in registry. Go's module tools
// fetch through the module proxy that the environment names.

func dependencyProject(t *testing.T) {
	t.Helper()
	generatingProject(t, generatingDeclaration)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	mustRun(t, runApply)
}

func moduleFiles(t *testing.T) (string, string) {
	t.Helper()
	module, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile("go.sum")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(module), string(sums)
}

func noPendingTransaction(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(".stego/transaction.yaml"); !os.IsNotExist(err) {
		t.Fatalf("a resolution left an apply journal: %v", err)
	}
}

func TestDependenciesChangeModuleFilesOnlyOnSuccess(t *testing.T) {
	t.Run("ST-BLD-UC01-R01-T01 A failed resolution leaves go.mod and go.sum as they were", func(t *testing.T) {
		dependencyProject(t)
		mustRun(t, runDependencies)
		module, sums := moduleFiles(t)
		// A source file that imports a module the cache does not hold makes
		// Go's module tools fail.
		writeProjectFile(t, "extra/extra.go", "package extra\n\nimport _ \"example.invalid/stego-missing/module\"\n")
		printed, err := capture(t, runDependencies)
		if err == nil {
			t.Fatalf("a failed resolution reported success:\n%s", printed)
		}
		if after, afterSums := moduleFiles(t); after != module || afterSums != sums {
			t.Fatal("a failed resolution changed go.mod or go.sum")
		}
		noPendingTransaction(t)
	})
	t.Run("ST-BLD-UC01-R01-T02 An interrupted resolution leaves the module files as they were, or as resolved", func(t *testing.T) {
		dependencyProject(t)
		original, originalSums := moduleFiles(t)
		// Find the resolved pair once; each round then starts again from the
		// original pair.
		mustRun(t, runDependencies)
		resolved, resolvedSums := moduleFiles(t)
		if resolved == original && resolvedSums == originalSums {
			t.Fatal("the fixture resolves to the same module files; the test shows nothing")
		}
		// Keep the interrupt from ending the test process, at every delay.
		guard := make(chan os.Signal, 1)
		signal.Notify(guard, os.Interrupt)
		defer signal.Stop(guard)
		interrupted := 0
		for _, delay := range []time.Duration{0, time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond} {
			writeProjectFile(t, "go.mod", original)
			if originalSums == "" {
				if err := os.Remove("go.sum"); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			} else {
				writeProjectFile(t, "go.sum", originalSums)
			}
			mustRun(t, runApply)
			timer := time.AfterFunc(delay, func() { _ = syscall.Kill(os.Getpid(), syscall.SIGINT) })
			_, err := capture(t, runDependencies)
			timer.Stop()
			time.Sleep(10 * time.Millisecond)
			for len(guard) > 0 {
				<-guard
			}
			if err != nil {
				interrupted++
			}
			if errors.Is(err, compiler.ErrRecoveryRequired) {
				mustRun(t, runRecover)
			}
			noPendingTransaction(t)
			module, sums := moduleFiles(t)
			if (module != original || sums != originalSums) && (module != resolved || sums != resolvedSums) {
				t.Fatalf("after an interrupt at %s, go.mod and go.sum are neither the old pair nor the resolved pair: %v", delay, err)
			}
		}
		if interrupted == 0 {
			t.Fatal("no interrupt reached a resolution; the test shows nothing")
		}
		t.Logf("%d of the resolutions were interrupted", interrupted)
	})
}

func TestDependenciesFollowTheCapturedSource(t *testing.T) {
	t.Run("ST-BLD-UC01-R02-T01 A saved overlay flag and ambient GOFLAGS cannot change what resolution reads", func(t *testing.T) {
		dependencyProject(t)
		mustRun(t, runDependencies)
		project, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		dependency := t.TempDir()
		writeProjectFile(t, filepath.Join(dependency, "go.mod"), "module example.com/overlay\ngo 1.26.10\n")
		writeProjectFile(t, filepath.Join(dependency, "overlay.go"), "package overlay\n")
		raw, err := os.ReadFile("go.mod")
		if err != nil {
			t.Fatal(err)
		}
		module, err := modfile.Parse("go.mod", raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := module.AddReplace("example.com/overlay", "", dependency, ""); err != nil {
			t.Fatal(err)
		}
		if raw, err = module.Format(); err != nil {
			t.Fatal(err)
		}
		writeProjectFile(t, "go.mod", string(raw))
		const source = "package extra\n"
		writeProjectFile(t, "extra/extra.go", source)
		outside := t.TempDir()
		virtual := filepath.Join(outside, "virtual.go")
		writeProjectFile(t, virtual, "package extra\n\nimport _ \"example.com/overlay\"\n")
		encoded, err := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(project, "extra/extra.go"): virtual}})
		if err != nil {
			t.Fatal(err)
		}
		overlay := filepath.Join(outside, "overlay.json")
		writeProjectFile(t, overlay, string(encoded))
		config := filepath.Join(outside, "go.env")
		writeProjectFile(t, config, "GOFLAGS=-overlay="+overlay+"\nGOPROXY=off\n")
		t.Setenv("GOENV", config)
		t.Setenv("GOPROXY", "")
		t.Setenv("GOFLAGS", "-stego-invalid-ambient-flag")
		mustRun(t, runDependencies)
		raw, err = os.ReadFile("go.mod")
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := modfile.Parse("go.mod", raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, requirement := range resolved.Require {
			if requirement.Mod.Path == "example.com/overlay" {
				t.Fatal("a saved overlay flag reached the module requirements")
			}
		}
		if actual, err := os.ReadFile("extra/extra.go"); err != nil || string(actual) != source {
			t.Fatal("resolution changed the source", err)
		}
	})
	t.Run("ST-BLD-UC01-R02-T02 Resolution ignores an ambient vendor flag and a missing go.work", func(t *testing.T) {
		dependencyProject(t)
		writeProjectFile(t, "local/go.mod", "module example.com/local\ngo 1.26.10\n")
		writeProjectFile(t, "local/local.go", "package local\n")
		raw, err := os.ReadFile("go.mod")
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, []byte("\nrequire example.com/local v0.0.0\nreplace example.com/local => ./local\n")...)
		writeProjectFile(t, "go.mod", string(raw))
		writeProjectFile(t, "extra/extra.go", "package extra\n\nimport _ \"example.com/local\"\n")
		t.Setenv("GOWORK", filepath.Join(t.TempDir(), "missing.work"))
		t.Setenv("GOFLAGS", "-mod=vendor")
		var first string
		for i := 0; i < 2; i++ {
			mustRun(t, runDependencies)
			module, _ := moduleFiles(t)
			if i == 0 {
				first = module
			} else if module != first {
				t.Fatal("two resolutions of the same source gave different go.mod files")
			}
		}
		if !strings.Contains(first, "replace example.com/local => ./local") {
			t.Fatal("resolution dropped the local replacement")
		}
	})
}

func TestDependenciesWaitForGeneration(t *testing.T) {
	t.Run("ST-BLD-UC01-R03-T01 Resolution refuses out-of-date generated code and names apply", func(t *testing.T) {
		generatingProject(t, generatingDeclaration)
		before := projectTree(t)
		if _, err := capture(t, runDependencies); err == nil || !strings.Contains(err.Error(), "stego apply") {
			t.Fatalf("resolution ran before the first apply: %v", err)
		}
		if !maps.Equal(before, projectTree(t)) {
			t.Fatal("a refused resolution changed the project")
		}
		mustRun(t, runApply)
		writeProjectFile(t, "service.yaml", strings.Replace(generatingDeclaration, "body", "text", 1))
		before = projectTree(t)
		if _, err := capture(t, runDependencies); err == nil || !strings.Contains(err.Error(), "stego apply") {
			t.Fatalf("resolution ran on generated code that is out of date: %v", err)
		}
		if !maps.Equal(before, projectTree(t)) {
			t.Fatal("a refused resolution changed the project")
		}
	})
}
