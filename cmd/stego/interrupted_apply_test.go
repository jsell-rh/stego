package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// These tests start from ST-GEN-TD03 and stop stego apply at a named point of
// its journal. A kill by timing does not reach each point, so they run a test
// build of stego (the build tag stegotesthook), which kills itself with
// SIGKILL at the point that STEGO_TEST_STOP_APPLY names. The product build
// holds no such hook.

var hookFixture onceFixture
var hookPath string

// stegoHookBinary builds the test build of cmd/stego once, from the same
// source snapshot as stegoBinary.
func stegoHookBinary(t *testing.T) string {
	t.Helper()
	stegoBinary(t)
	hookFixture.get(t, func() (string, error) {
		hookPath = filepath.Join(fixtureRoot, "bin", "stego-hook")
		build := exec.Command(pinnedGo(t), "build", "-mod=readonly", "-trimpath", "-tags", "stegotesthook", "-o", hookPath, "./cmd/stego")
		build.Dir = filepath.Join(fixtureRoot, "stego-source")
		build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local")
		if output, err := build.CombinedOutput(); err != nil {
			return "", fmt.Errorf("build the test build of stego: %v\n%s", err, output)
		}
		return "", nil
	})
	return hookPath
}

// stopApply runs stego apply from the test build, which kills itself at the
// journal point, and fails the test unless the run was killed.
func (s *smallService) stopApply(t *testing.T, point string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, stegoHookBinary(t), "apply")
	command.Dir, command.Env = s.service, append(slices.Clone(s.env), "STEGO_TEST_STOP_APPLY="+point)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("apply did not stop at %s: %v\n%s", point, err, output)
	}
}

// reset restores the service folder to its commit.
func (s *smallService) reset(t *testing.T) {
	t.Helper()
	git(t, s.service, "checkout", "-q", "--", ".")
	git(t, s.service, "clean", "-q", "-fdx")
}

// tagDeclaration adds an entity Tag with its collection, and Note loses its
// field title and its collection, so that the plan creates, updates and
// deletes files.
const tagDeclaration = `kind: service
name: notes
archetype: rest-crud
language: go
entities:
  - name: Note
    fields: []
  - name: Tag
    fields: [{name: label, type: string}]
collections:
  tags: {entity: Tag, operations: [create, read]}
`

// journal reads the pending journal with a YAML parser.
func (s *smallService) journal(t *testing.T) (operations []string) {
	t.Helper()
	var journal struct {
		Operations []struct {
			Path string `yaml:"path"`
		} `yaml:"operations"`
	}
	if err := yaml.Unmarshal([]byte(s.read(t, ".stego/transaction.yaml")), &journal); err != nil {
		t.Fatal(err)
	}
	for _, operation := range journal.Operations {
		operations = append(operations, operation.Path)
	}
	return operations
}

// appliedResult applies the tag declaration without a stop, gives the
// record and the tree of the service folder that the apply leaves, and
// restores the service folder to its commit.
func (s *smallService) appliedResult(t *testing.T) (string, map[string]string) {
	t.Helper()
	s.write(t, "service.yaml", tagDeclaration)
	s.must(t, "apply")
	record, files := s.read(t, ".stego/state.yaml"), tree(t, s.service)
	s.reset(t)
	return record, files
}

// matchesRecord checks that every file the record lists has its recorded
// hash, and that out holds no other file.
func (s *smallService) matchesRecord(t *testing.T) {
	t.Helper()
	files := tree(t, s.service)
	applied, _ := s.record(t)["last_applied"].(map[string]any)
	listed, _ := applied["files"].(map[string]any)
	for name, hash := range listed {
		path := "out/" + name
		if name == "go.mod" || name == "go.sum" {
			path = name
		}
		if files[path] != hash {
			t.Errorf("%s does not match the hash its record holds", path)
		}
	}
	for path := range files {
		if name, ok := strings.CutPrefix(path, "out/"); ok && listed[name] == nil {
			t.Errorf("%s is in out, and the record does not list it", path)
		}
	}
}

func TestRecoverCompletesInterruptedApply(t *testing.T) {
	t.Run("ST-GEN-UC06-R01-T01 Recover completes an interrupted apply, so the files and the record match the record apply was writing", func(t *testing.T) {
		s := newSmallService(t)
		committed := tree(t, s.service)
		wanted, wantedFiles := s.appliedResult(t)
		deleted := 0
		for path := range committed {
			if _, kept := wantedFiles[path]; !kept {
				deleted++
			}
		}
		if deleted == 0 {
			t.Fatal("the changed declaration deletes no file, so the test cannot show a delete")
		}
		for _, point := range []string{"prepared", "after_operation_0", "before_record"} {
			s.write(t, "service.yaml", tagDeclaration)
			s.stopApply(t, point)
			stdout := s.must(t, "recover")
			if !strings.Contains(stdout, "No pending apply remains") {
				t.Errorf("recover after %s did not say that no pending apply remains:\n%s", point, stdout)
			}
			if record := s.read(t, ".stego/state.yaml"); record != wanted {
				t.Errorf("after a stop at %s and recover, the record is not the one apply was writing", point)
			}
			s.matchesRecord(t)
			for path := range committed {
				if _, kept := wantedFiles[path]; !kept {
					if _, err := os.Stat(filepath.Join(s.service, path)); !os.IsNotExist(err) {
						t.Errorf("after a stop at %s and recover, %s is not deleted", point, path)
					}
				}
			}
			s.must(t, "drift")
			s.reset(t)
		}
	})
}

func TestRecoverRefusesConflict(t *testing.T) {
	t.Run("ST-GEN-UC06-R02-T01 Recover refuses, and changes nothing, when a journaled file matches neither of its states", func(t *testing.T) {
		s := newSmallService(t)
		s.write(t, "service.yaml", tagDeclaration)
		s.stopApply(t, "after_operation_0")
		operations := s.journal(t)
		if len(operations) < 2 {
			t.Fatalf("the journal holds %d operations", len(operations))
		}
		conflict := operations[0]
		s.write(t, conflict, "// a third content, written during the interruption\n")
		before := tree(t, s.service)
		stdout, stderr, err := s.stego(t, "recover")
		if err == nil || !strings.Contains(stdout+stderr, conflict) {
			t.Fatalf("recover did not refuse and name %s: %v\n%s%s", conflict, err, stdout, stderr)
		}
		if !maps.Equal(before, tree(t, s.service)) {
			t.Fatal("a refused recovery changed a file")
		}
		if _, err := os.Stat(filepath.Join(s.service, ".stego/transaction.yaml")); err != nil {
			t.Fatalf("the journal is not pending after a refused recovery: %v", err)
		}
	})
}

func TestPendingJournalStopsEveryRun(t *testing.T) {
	t.Run("ST-GEN-UC06-R03-T01 Plan, apply and drift each refuse while a journal is pending, change nothing and name recover", func(t *testing.T) {
		s := newSmallService(t)
		s.write(t, "service.yaml", tagDeclaration)
		s.stopApply(t, "after_operation_0")
		before := tree(t, s.service)
		for _, command := range []string{"plan", "apply", "drift"} {
			stdout, stderr, err := s.stego(t, command)
			if err == nil || !strings.Contains(stdout+stderr, "stego recover") {
				t.Errorf("stego %s did not refuse and name stego recover while a journal is pending: %v\n%s%s", command, err, stdout, stderr)
			}
			if !maps.Equal(before, tree(t, s.service)) {
				t.Fatalf("stego %s changed a file while a journal is pending", command)
			}
		}
	})
}

func TestInterruptedApplyFlow(t *testing.T) {
	t.Run("ST-GEN-UC06-FL-T After an interrupted apply, drift names recover, and recover makes the files and the record agree", func(t *testing.T) {
		s := newSmallService(t)
		s.write(t, "service.yaml", tagDeclaration)
		s.stopApply(t, "after_operation_0")
		if stdout, stderr, err := s.stego(t, "drift"); err == nil || !strings.Contains(stdout+stderr, "stego recover") {
			t.Fatalf("drift did not fail and name stego recover: %v\n%s%s", err, stdout, stderr)
		}
		s.must(t, "recover")
		s.matchesRecord(t)
		s.must(t, "drift")

		s.write(t, "service.yaml", smallServiceDeclaration)
		s.stopApply(t, "after_operation_0")
		conflict := s.journal(t)[0]
		s.write(t, conflict, "// a third content\n")
		before := tree(t, s.service)
		if stdout, stderr, err := s.stego(t, "recover"); err == nil || !strings.Contains(stdout+stderr, conflict) {
			t.Fatalf("recover did not refuse and name %s: %v\n%s%s", conflict, err, stdout, stderr)
		}
		if !maps.Equal(before, tree(t, s.service)) {
			t.Fatal("a refused recovery changed a file")
		}
	})
}

func TestRecordFlow(t *testing.T) {
	t.Run("ST-GEN-UC01-FL-T An apply writes the code and its record together, and one commit holds both", func(t *testing.T) {
		s := newSmallService(t)
		s.write(t, "service.yaml", secondEntityDeclaration)
		s.must(t, "apply")
		s.matchesRecord(t)
		git(t, s.service, "add", "-A")
		git(t, s.service, "commit", "-q", "-m", "Tag")
		if status := git(t, s.service, "status", "--porcelain"); status != "" {
			t.Fatalf("one commit leaves the service unclean:\n%s", status)
		}
		committed := strings.Split(git(t, s.service, "ls-files"), "\n")
		applied, _ := s.record(t)["last_applied"].(map[string]any)
		files, _ := applied["files"].(map[string]any)
		for name := range files {
			path := "out/" + name
			if name == "go.mod" || name == "go.sum" {
				path = name
			}
			if !slices.Contains(committed, path) {
				t.Errorf("the commit does not hold %s, which the record lists", path)
			}
		}

		s.write(t, "service.yaml", smallServiceDeclaration)
		s.stopApply(t, "prepared")
		before := tree(t, s.service)
		for _, command := range []string{"plan", "apply", "drift"} {
			if stdout, stderr, err := s.stego(t, command); err == nil || !strings.Contains(stdout+stderr, "stego recover") {
				t.Errorf("stego %s did not refuse and name stego recover: %v\n%s%s", command, err, stdout, stderr)
			}
		}
		if !maps.Equal(before, tree(t, s.service)) {
			t.Fatal("a refused run changed a file")
		}
		s.must(t, "recover")
		if _, err := os.Stat(filepath.Join(s.service, ".stego/transaction.yaml")); !os.IsNotExist(err) {
			t.Fatalf("a journal remains after recover: %v", err)
		}
		s.matchesRecord(t)
	})
}

func TestApplySavesWholeOrNothing(t *testing.T) {
	t.Run("ST-GEN-A1-SR01-T01 A crash at any point of apply leaves the old record with the old files, or the new record with the new files after the next run", func(t *testing.T) {
		s := newSmallService(t)
		oldRecord, oldFiles := s.read(t, ".stego/state.yaml"), tree(t, s.service)
		newRecord, _ := s.appliedResult(t)
		s.write(t, "service.yaml", tagDeclaration)
		s.stopApply(t, "prepared")
		operations := s.journal(t)
		s.reset(t)
		points := []string{"before_prepare", "prepared"}
		for index := range operations {
			points = append(points, fmt.Sprintf("after_operation_%d", index))
		}
		points = append(points, "before_record", "before_complete")
		for _, point := range points {
			s.write(t, "service.yaml", tagDeclaration)
			s.stopApply(t, point)
			if point == "before_prepare" {
				files := tree(t, s.service)
				delete(files, "service.yaml")
				old := maps.Clone(oldFiles)
				delete(old, "service.yaml")
				if s.read(t, ".stego/state.yaml") != oldRecord || !maps.Equal(files, old) {
					t.Errorf("a stop before the journal is written did not leave the old record and the old files")
				}
			} else {
				s.must(t, "recover")
				if got := s.read(t, ".stego/state.yaml"); got != newRecord {
					t.Errorf("a stop at %s and recovery did not leave the new record:\n%s\n----\n%s", point, got, newRecord)
				}
			}
			s.matchesRecord(t)
			s.reset(t)
		}
	})

	t.Run("ST-GEN-A1-SR01-T02 Apply saves the record after every generated file", func(t *testing.T) {
		s := newSmallService(t)
		s.write(t, "service.yaml", tagDeclaration)
		s.stopApply(t, "prepared")
		operations := s.journal(t)
		s.reset(t)
		written := 0
		for _, path := range operations {
			if strings.HasPrefix(path, "out/") {
				written++
			}
		}
		if written < 3 {
			t.Fatalf("the plan writes %d generated files, fewer than three", written)
		}
		log := filepath.Join(t.TempDir(), "apply.log")
		s.write(t, "service.yaml", tagDeclaration)
		command := exec.Command(stegoHookBinary(t), "apply")
		command.Dir, command.Env = s.service, append(slices.Clone(s.env), "STEGO_TEST_APPLY_LOG="+log)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("apply: %v\n%s", err, output)
		}
		data, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		// Each after_operation_<n> line marks that operation n of the journal
		// wrote its file; the journal gives the path of each.
		var order []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var index int
			if _, err := fmt.Sscanf(line, "after_operation_%d", &index); err == nil {
				order = append(order, operations[index])
			}
		}
		if len(order) != len(operations) || order[len(order)-1] != ".stego/state.yaml" {
			t.Fatalf("the record is not the last write of the run: %v", order)
		}
		if !bytes.Contains(data, []byte("before_record\n")) {
			t.Fatal("the run passed no point before the record")
		}
	})
}
