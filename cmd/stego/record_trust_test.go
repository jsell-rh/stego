package main

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests start from ST-GEN-TD03 and drive stego drift through the
// binary, as a platform engineer checks a service's record.

// breakRecord sets the registry reference of the record to R2, and leaves the
// registry reference of its input manifest at R1.
func (s *smallService) breakRecord(t *testing.T) {
	t.Helper()
	record := s.read(t, ".stego/state.yaml")
	line := "registry_sha: " + s.r1 + "\n"
	if strings.Count(record, line) != 1 || !strings.Contains(record, "registry_ref: "+s.r1+"\n") {
		t.Fatalf("the record does not name R1 once as its registry reference and once in its input manifest:\n%s", record)
	}
	s.write(t, ".stego/state.yaml", strings.Replace(record, line, "registry_sha: "+s.r2+"\n", 1))
}

// generatedFiles gives the paths, under out, of the files the record lists.
func (s *smallService) generatedFiles(t *testing.T) []string {
	t.Helper()
	applied, _ := s.record(t)["last_applied"].(map[string]any)
	files, _ := applied["files"].(map[string]any)
	var paths []string
	for name := range files {
		if name != "go.mod" && name != "go.sum" {
			paths = append(paths, name)
		}
	}
	slices.Sort(paths)
	return paths
}

// reportsCurrent tells whether a drift report says that a generated file, or
// all of them, match the record.
func reportsCurrent(output string, paths []string) bool {
	if strings.Contains(output, "No drift detected") || strings.Contains(output, "match the last apply") {
		return true
	}
	for _, line := range strings.Split(output, "\n") {
		for _, path := range paths {
			if strings.Contains(line, path) && (strings.Contains(line, "current") || strings.Contains(line, "unchanged") || strings.Contains(line, "matches")) {
				return true
			}
		}
	}
	return false
}

func TestBrokenRecordIsReported(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC02-R02-T01 A record whose registry reference differs from the one its input manifest holds is reported as broken", func(t *testing.T) {
		s := newSmallService(t)
		paths := s.generatedFiles(t)
		s.breakRecord(t)
		before := tree(t, s.parent)
		stdout, stderr, err := s.stego(t, "drift")
		output := stdout + stderr
		if err == nil {
			t.Fatalf("drift of a broken record passed:\n%s", output)
		}
		if !strings.Contains(output, "broken") || !strings.Contains(output, "registry reference") {
			t.Fatalf("drift did not say that the record is broken and name the registry reference:\n%s", output)
		}
		if reportsCurrent(output, paths) {
			t.Fatalf("drift of a broken record reports generated files as current:\n%s", output)
		}
		if !maps.Equal(before, tree(t, s.parent)) {
			t.Fatal("drift changed a file")
		}
	})
}

func TestDriftFlowOfTrust(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC02-FL-T Drift names each generated file that no longer matches its hash, and a broken record is reported as broken", func(t *testing.T) {
		s := newSmallService(t)
		paths := s.generatedFiles(t)
		if len(paths) < 2 {
			t.Fatalf("the record lists %d generated files", len(paths))
		}

		before := tree(t, s.parent)
		stdout, stderr, err := s.stego(t, "drift")
		if err != nil || !reportsCurrent(stdout, nil) {
			t.Fatalf("drift of the unchanged service did not pass with no drift: %v\n%s%s", err, stdout, stderr)
		}
		if !maps.Equal(before, tree(t, s.parent)) {
			t.Fatal("drift of the unchanged service changed a file")
		}

		edited := []string{paths[0], paths[len(paths)-1]}
		originals := map[string]string{}
		for _, path := range edited {
			name := filepath.Join("out", path)
			originals[name] = s.read(t, name)
			s.write(t, name, originals[name]+"// edited by hand\n")
		}
		before = tree(t, s.parent)
		stdout, stderr, err = s.stego(t, "drift")
		if err == nil {
			t.Fatalf("drift passed with two edited files:\n%s%s", stdout, stderr)
		}
		for _, path := range paths {
			listed := strings.Contains(stdout, path)
			if wanted := slices.Contains(edited, path); listed != wanted {
				t.Errorf("drift lists %s: %v; edited by hand: %v\n%s", path, listed, wanted, stdout)
			}
		}
		if !maps.Equal(before, tree(t, s.parent)) {
			t.Fatal("drift with edited files changed a file")
		}

		for name, data := range originals {
			s.write(t, name, data)
		}
		s.breakRecord(t)
		before = tree(t, s.parent)
		stdout, stderr, err = s.stego(t, "drift")
		if err == nil || !strings.Contains(stdout+stderr, "broken") {
			t.Fatalf("drift of a broken record did not fail and say that the record is broken: %v\n%s%s", err, stdout, stderr)
		}
		if !maps.Equal(before, tree(t, s.parent)) {
			t.Fatal("drift of a broken record changed a file")
		}
	})
}
