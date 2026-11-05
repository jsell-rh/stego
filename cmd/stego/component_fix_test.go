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
		case inA == "" && strings.HasPrefix(path, "internal/api/"):
			inA = path
		case other == "" && !strings.HasPrefix(path, "internal/api/"):
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
