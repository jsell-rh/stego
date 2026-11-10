package compiler

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// The query component, whose generator carries the reads of a service on the runtime data path with the command
// component's (ST-SUP-RC33), and the PostgreSQL adapter, whose store the two replace.
const (
	queryComponent   = "query"
	adapterComponent = "postgres-adapter"
)

// recordPath is the record of the project, from the project root.
const recordPath = ".stego/state.yaml"

// runtimeDataPath tells whether the compiler switches the runtime data path on for the service (gen.Context
// RuntimeData, ST-SUP-RC01-OC41). The record states the data path of the last run (data_path); a record before
// format version 4 is of a release on the GORM store, because the runtime data path came with format version 4. The
// record is the project's record, or, when the project has none, the record that the HEAD commit of the project's
// repository holds, so a record that is not checked out still holds the service on its path. Until the check
// against a base ref (ST-GEN-UC14) gives plan another record, that is the record at the base ref (ST-SUP-Q91).
//   - A service with no record starts on the runtime data path when it has both the command and the query
//     generators, which replace the store, and on the store otherwise.
//   - A record on the store keeps the store, also when the generators are added: the move to the runtime data path
//     at a declaration version bump waits for ST-GEN-UC10.
//   - A record on the runtime data path stays on it, and a service that has lost one of the generators is refused:
//     it would move back onto the store, whose tables its database does not hold.
func runtimeDataPath(projectDir string, names []string) (bool, error) {
	generators := slices.Contains(names, commandComponent) && slices.Contains(names, queryComponent)
	state, err := recordOf(projectDir, generators)
	if err != nil {
		return false, err
	}
	if state == nil || state.LastApplied == nil {
		return generators, nil
	}
	return dataPathOf(state.LastApplied, generators)
}

// dataPathOf gives the data path that a run with or without the command and query generators takes after the run
// that applied recorded.
func dataPathOf(recorded *AppliedState, generators bool) (bool, error) {
	switch recorded.DataPath {
	case "", DataPathStore:
		return false, nil
	case DataPathRuntime:
		if !generators {
			return false, fmt.Errorf("the record states the runtime data path, and the service no longer has both the command and the query generators: the run would move it back onto the GORM store, whose tables its database does not hold; add the command and query components again (ST-SUP-RC01-OC41, ST-SUP-Q91)")
		}
		return true, nil
	default:
		return false, fmt.Errorf("the record states the data path %q, which is not %s or %s", recorded.DataPath, DataPathRuntime, DataPathStore)
	}
}

// recordOf reads the project's record, or, when the project has none and the service has the command and query
// generators, the record that the HEAD commit of the project's repository holds. It gives nil when there is
// neither. A service without the generators is on the store with or without a committed record, so its run reads no
// repository.
func recordOf(projectDir string, generators bool) (*State, error) {
	state, err := LoadState(filepath.Join(projectDir, recordPath))
	if err != nil {
		return nil, fmt.Errorf("reading the record to choose the data path: %w", err)
	}
	if state.LastApplied != nil || !generators {
		return state, nil
	}
	data, err := committedRecord(projectDir)
	if err != nil || data == nil {
		return nil, err
	}
	committed, err := decodeState(data, "HEAD:"+recordPath)
	if err != nil {
		return nil, fmt.Errorf("reading the committed record to choose the data path: %w", err)
	}
	return committed, nil
}

// committedRecord gives the record that the HEAD commit holds at the project's folder, or nil when the project's
// repository has no commit or its HEAD commit holds no record. It refuses a project that is not in a git repository,
// and a failure of git: the run cannot tell then whether a record of a release is committed (ST-SUP-Q91).
func committedRecord(projectDir string) ([]byte, error) {
	git := func(args ...string) ([]byte, int, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = projectDir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out, exit.ExitCode(), fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
		}
		return out, 0, err
	}
	refused := func(err error) error {
		return fmt.Errorf("the project has no record %s, and the run cannot read the record of the HEAD commit to choose the data path, which a service with the command and query generators needs: %v; run in the service's git repository, or commit the record (ST-SUP-RC01-OC41, ST-SUP-Q91)", recordPath, err)
	}
	prefix, _, err := git("rev-parse", "--show-prefix")
	if err != nil {
		return nil, refused(err)
	}
	if _, code, err := git("rev-parse", "--verify", "--quiet", "HEAD^{commit}"); code == 1 {
		// A repository with no commit holds no record.
		return nil, nil
	} else if err != nil {
		return nil, refused(err)
	}
	name := "HEAD:" + filepath.ToSlash(filepath.Join(strings.TrimSpace(string(prefix)), recordPath))
	kind, code, err := git("cat-file", "-t", name)
	if code != 0 && err != nil {
		if _, _, other := git("ls-tree", "HEAD", "--", filepath.ToSlash(filepath.Join(strings.TrimSpace(string(prefix)), recordPath))); other == nil {
			// The HEAD commit holds no record at that path.
			return nil, nil
		}
		return nil, refused(err)
	}
	if err != nil {
		return nil, refused(err)
	}
	if strings.TrimSpace(string(kind)) != "blob" {
		return nil, refused(fmt.Errorf("%s is not a file", name))
	}
	data, _, err := git("show", name)
	if err != nil {
		return nil, refused(err)
	}
	return data, nil
}

// dataPathName is the value of data_path for a run.
func dataPathName(runtime bool) string {
	if runtime {
		return DataPathRuntime
	}
	return DataPathStore
}

// checkDataPathSnapshot refuses a run whose data path differs from the one that the record in the snapshot of the
// project's inputs gives: the record changed between the choice of the data path and the snapshot.
func checkDataPathSnapshot(snapshot *State, names []string, runtime bool) error {
	if snapshot == nil || snapshot.LastApplied == nil {
		return nil
	}
	generators := slices.Contains(names, commandComponent) && slices.Contains(names, queryComponent)
	want, err := dataPathOf(snapshot.LastApplied, generators)
	if err != nil {
		return err
	}
	if want != runtime {
		return fmt.Errorf("the record %s changed during compilation: it gives the data path %s, and the run chose %s", recordPath, dataPathName(want), dataPathName(runtime))
	}
	return nil
}
