package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/parser"
)

// DriftedFile represents a generated file that has been modified or deleted
// since the last apply.
type DriftedFile struct {
	Path string
}

// DriftedInput represents a project source input recorded in the input
// manifest whose current disk state no longer matches the last apply.
type DriftedInput struct {
	Path string
}

// DriftResult holds the results of drift detection.
type DriftResult struct {
	Modified []DriftedFile
	Deleted  []DriftedFile
	Inputs   []DriftedInput
}

// HasDrift returns true if any generated files have been modified or deleted,
// or if any recorded source input changed since the last apply.
func (r *DriftResult) HasDrift() bool {
	return len(r.Modified) > 0 || len(r.Deleted) > 0 || len(r.Inputs) > 0
}

// DetectDrift compares generated files on disk against the hashes recorded in
// .stego/state.yaml to detect hand-edits to generated files, and compares the
// recorded input manifest against disk to detect changed source inputs.
func DetectDrift(projectDir, outDir string) (*DriftResult, error) {
	if err := checkPendingProject(projectDir); err != nil {
		return nil, err
	}
	if outDir == "" {
		outDir = filepath.Join(projectDir, "out")
	}
	relative, err := outputRelative(projectDir, outDir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, snapshot, err := readSnapshot(root, ".stego/state.yaml", parser.MaxDocumentBytes, true)
	if err != nil {
		return nil, err
	}
	if !snapshot.Exists {
		return nil, fmt.Errorf("no state file found; run 'stego apply' first")
	}
	state, err := decodeState(data, ".stego/state.yaml")
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}
	if state.LastApplied == nil {
		return nil, fmt.Errorf("no state file found — run 'stego apply' first")
	}

	result := &DriftResult{}

	if len(state.LastApplied.Files) > 0 {
		// Collect and sort file paths for deterministic output.
		var paths []string
		for path := range state.LastApplied.Files {
			if isProjectRootFile(path) {
				continue // Application-owned, including state from older releases.
			}
			paths = append(paths, path)
		}
		sort.Strings(paths)

		for _, path := range paths {
			expectedHash := state.LastApplied.Files[path]
			_, snapshot, err := readSnapshot(root, projectFilePath(relative, path), maxTrackedFileBytes, false)
			if err != nil {
				return nil, err
			}
			if !snapshot.Exists {
				result.Deleted = append(result.Deleted, DriftedFile{Path: path})
				continue
			}
			if snapshot.Hash != expectedHash {
				result.Modified = append(result.Modified, DriftedFile{Path: path})
			}
		}
	}

	// Compare recorded source inputs (service declaration, generator inputs,
	// module file) against disk. A changed input means the outputs no longer
	// reflect the sources; only a new apply restores agreement.
	if state.LastApplied.Inputs != nil && len(state.LastApplied.Inputs.Files) > 0 {
		var inputPaths []string
		for name := range state.LastApplied.Inputs.Files {
			inputPaths = append(inputPaths, name)
		}
		sort.Strings(inputPaths)
		for _, name := range inputPaths {
			if isProjectRootFile(name) {
				continue // Module files are application-owned; stego merges go.mod at apply and go tooling may update both.
			}
			recorded := state.LastApplied.Inputs.Files[name]
			_, snapshot, err := readSnapshot(root, name, maxTrackedFileBytes, false)
			if err != nil {
				return nil, err
			}
			changed := snapshot.Exists != recorded.Exists ||
				snapshot.Hash != recorded.SHA256 ||
				uint32(snapshot.Mode.Perm()) != recorded.Mode
			if changed {
				result.Inputs = append(result.Inputs, DriftedInput{Path: name})
			}
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
	if len(r.Modified)+len(r.Deleted) > 0 {
		fmt.Fprintf(&sb, "Drift detected in %d file(s):\n\n", len(r.Modified)+len(r.Deleted))
		for _, f := range r.Modified {
			fmt.Fprintf(&sb, "  modified: %s\n", f.Path)
		}
		for _, f := range r.Deleted {
			fmt.Fprintf(&sb, "  deleted:  %s\n", f.Path)
		}
	}
	if len(r.Inputs) > 0 {
		fmt.Fprintf(&sb, "\n%d input file(s) changed since the last apply:\n\n", len(r.Inputs))
		for _, f := range r.Inputs {
			fmt.Fprintf(&sb, "  input: %s\n", f.Path)
		}
		fmt.Fprintf(&sb, "\nRun 'stego apply' to regenerate outputs from the current inputs.\n")
	}
	return sb.String()
}
