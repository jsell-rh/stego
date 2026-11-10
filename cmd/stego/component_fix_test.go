package main

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests start from ST-GEN-TD03 and take the fix of component A at R2
// into the service through the stego binary.

// editTwoGeneratedFiles edits by hand one generated file in component A's
// namespace and one in another component's, and gives their paths under out.
func (s *smallService) editTwoGeneratedFiles(t *testing.T) []string {
	t.Helper()
	var inA, other string
	for _, path := range s.generatedFiles(t) {
		switch {
		case inA == "" && strings.HasPrefix(path, componentANamespace):
			inA = path
		case other == "" && !strings.HasPrefix(path, componentANamespace):
			other = path
		}
	}
	if inA == "" || other == "" {
		t.Fatal("the service has no generated file in component A's namespace and in another's")
	}
	for _, path := range []string{inA, other} {
		name := filepath.Join("out", path)
		s.write(t, name, s.read(t, name)+"// edited by hand\n")
	}
	return []string{inA, other}
}

func TestHandEditStopsApply(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC03-R03-T01 Apply names every hand-edited generated file and writes nothing", func(t *testing.T) {
		s := newSmallService(t)
		edited := s.editTwoGeneratedFiles(t)
		s.pin(t, s.r2)
		before := tree(t, s.service)
		stdout, stderr, err := s.stego(t, "apply")
		if err == nil {
			t.Fatalf("apply over two hand-edited files passed:\n%s%s", stdout, stderr)
		}
		for _, path := range edited {
			if !strings.Contains(stdout+stderr, path) {
				t.Errorf("apply did not name the hand-edited file %s:\n%s%s", path, stdout, stderr)
			}
		}
		if !maps.Equal(before, tree(t, s.service)) {
			t.Fatal("a refused apply changed a file")
		}
		for _, name := range []string{".stego/transaction.yaml", ".stego/plan.yaml"} {
			if _, err := os.Stat(filepath.Join(s.service, name)); !os.IsNotExist(err) {
				t.Errorf("a refused apply left %s: %v", name, err)
			}
		}
	})

	t.Run("ST-GEN-UC03-R03-T02 Apply replaces hand-edited generated files when the author allows it on the command", func(t *testing.T) {
		s := newSmallService(t)
		edited := s.editTwoGeneratedFiles(t)
		s.pin(t, s.r2)
		s.must(t, "apply", "--replace-hand-edits")
		for _, path := range edited {
			if strings.HasSuffix(s.read(t, filepath.Join("out", path)), "// edited by hand\n") {
				t.Errorf("apply kept the hand edit of %s", path)
			}
		}
		s.matchesRecord(t)
		s.must(t, "drift")
	})
}

func TestFixLandsByRegeneration(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC03-R01-T01 A component fix shows as an update in the next plan, and apply lands it with no hand edit", func(t *testing.T) {
		s := newSmallService(t)
		middleware := filepath.Join("out", componentANamespace, "middleware.go")
		if strings.Contains(s.read(t, middleware), "X-Notes-Token") {
			t.Fatal("the service already carries the fix")
		}
		s.pin(t, s.r2)
		plan := s.must(t, "plan")
		if !strings.Contains(plan, "update:   "+componentANamespace+"middleware.go") {
			t.Fatalf("the plan does not show the fixed file as an update:\n%s", plan)
		}
		s.must(t, "apply", "--saved-plan")
		if !strings.Contains(s.read(t, middleware), "X-Notes-Token") {
			t.Fatal("apply did not land the fix")
		}
		s.matchesRecord(t)
		s.must(t, "drift")
	})

	t.Run("ST-GEN-UC03-R01-T02 A new generation run restores generated output with no hand edit", func(t *testing.T) {
		s := newSmallService(t)
		middleware := filepath.Join("out", componentANamespace, "middleware.go")
		s.write(t, middleware, s.read(t, middleware)+"// edited by hand\n")
		plan := s.must(t, "plan")
		if !strings.Contains(plan, "update:   "+componentANamespace+"middleware.go") {
			t.Fatalf("the plan does not name the edited file as an update:\n%s", plan)
		}
		s.must(t, "apply", "--saved-plan", "--replace-hand-edits")
		s.matchesRecord(t)
		s.must(t, "drift")
	})
}

func TestRegistryChangeWithoutOutputChange(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC03-R02-T01 A registry change that does not affect output changes no generated file", func(t *testing.T) {
		s := newSmallService(t)
		definition := filepath.Join(s.registry, "components", "health-check", "component.yaml")
		data, err := os.ReadFile(definition)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Dir(definition), "component.yaml", "# The health check of the service.\n"+string(data), 0o644)
		git(t, s.registry, "commit", "-q", "-am", "a comment")
		r3 := git(t, s.registry, "rev-parse", "HEAD")
		before := tree(t, s.service)
		recordBefore, _ := s.record(t)["last_applied"].(map[string]any)
		s.pin(t, r3)
		s.must(t, "apply")
		after := tree(t, s.service)
		for path, hash := range after {
			if strings.HasPrefix(path, "out/") && before[path] != hash {
				t.Errorf("a registry change that does not affect output changed %s", path)
			}
		}
		recordAfter, _ := s.record(t)["last_applied"].(map[string]any)
		if recordAfter["registry_content_sha256"] == recordBefore["registry_content_sha256"] {
			t.Fatal("the record did not gain the new registry content digest")
		}
		if plan := s.must(t, "plan"); !strings.Contains(plan, "No changes") {
			t.Fatalf("a repeated plan changes something:\n%s", plan)
		}
	})
}
