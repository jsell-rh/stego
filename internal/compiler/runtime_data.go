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
	state, err := recordOf(projectDir)
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

// recordOf reads the project's record, or, when the project has none, the record that the HEAD commit of the
// project's repository holds. It gives nil when there is neither; a project outside a repository has only its own.
func recordOf(projectDir string) (*State, error) {
	state, err := LoadState(filepath.Join(projectDir, recordPath))
	if err != nil {
		return nil, fmt.Errorf("reading the record to choose the data path: %w", err)
	}
	if state.LastApplied != nil {
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

// committedRecord gives the record that the HEAD commit holds at the project's folder, or nil when the project is
// not in a repository, the repository has no commit, or the commit holds no record.
func committedRecord(projectDir string) ([]byte, error) {
	top := exec.Command("git", "rev-parse", "--show-prefix")
	top.Dir = projectDir
	prefix, err := top.Output()
	if err != nil {
		return nil, nil
	}
	name := filepath.ToSlash(filepath.Join(strings.TrimSpace(string(prefix)), recordPath))
	show := exec.Command("git", "show", "HEAD:"+name)
	show.Dir = projectDir
	var stderr bytes.Buffer
	show.Stderr = &stderr
	data, err := show.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// No commit, or no record at that path in it.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the committed record: %v: %s", err, strings.TrimSpace(stderr.String()))
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
