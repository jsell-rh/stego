package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DriftedFile represents a generated file that has been modified or deleted
// since the last apply.
type DriftedFile struct {
	Path string
}

// DriftResult holds the results of drift detection.
type DriftResult struct {
	Modified []DriftedFile
	Deleted  []DriftedFile
}

// HasDrift returns true if any generated files have been modified or deleted.
func (r *DriftResult) HasDrift() bool {
	return len(r.Modified) > 0 || len(r.Deleted) > 0
}

// DetectDrift compares generated files on disk against the hashes recorded in
// .stego/state.yaml to detect hand-edits to generated files.
func DetectDrift(projectDir, outDir string) (*DriftResult, error) {
	statePath := filepath.Join(projectDir, ".stego", "state.yaml")
	state, err := LoadState(statePath)
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}
	if state.LastApplied == nil {
		return nil, fmt.Errorf("no state file found — run 'stego apply' first")
	}
	if len(state.LastApplied.Files) == 0 {
		return &DriftResult{}, nil
	}

	result := &DriftResult{}

	// Collect and sort file paths for deterministic output.
	var paths []string
	for path := range state.LastApplied.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		expectedHash := state.LastApplied.Files[path]
		baseDir := fileBaseDir(path, outDir, projectDir)
		fullPath := filepath.Join(baseDir, path)

		data, err := os.ReadFile(fullPath)
		if os.IsNotExist(err) {
			result.Deleted = append(result.Deleted, DriftedFile{Path: path})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", fullPath, err)
		}

		actualHash := HashBytes(data)
		if actualHash != expectedHash {
			result.Modified = append(result.Modified, DriftedFile{Path: path})
		}
	}

	return result, nil
}

// FormatDrift produces a human-readable drift report.
func FormatDrift(r *DriftResult) string {
	if !r.HasDrift() {
		return "No drift detected. All generated files match the last apply."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Drift detected in %d file(s):\n\n", len(r.Modified)+len(r.Deleted))
	for _, f := range r.Modified {
		fmt.Fprintf(&sb, "  modified: %s\n", f.Path)
	}
	for _, f := range r.Deleted {
		fmt.Fprintf(&sb, "  deleted:  %s\n", f.Path)
	}
	return sb.String()
}
