package compiler

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"

	"github.com/jsell-rh/stego/internal/registry"
)

// The query component, whose generator carries the reads of a service on the runtime data path with the command
// component's (ST-SUP-RC33), and the PostgreSQL adapter, whose store the two replace.
const (
	queryComponent   = "query"
	adapterComponent = "postgres-adapter"
)

// runtimeDataPath tells whether the compiler switches the runtime data path on for the service (gen.Context
// RuntimeData, ST-SUP-RC01-OC41): the command and the query generators are both resolved, so they replace the
// store, and the record at the base ref is not of a release on the GORM store. The base ref of plan and apply is
// the project's own record, .stego/state.yaml, until the check against a base ref (ST-GEN-UC14) gives plan another
// one. A project with no record has had no release, so it starts on the runtime data path (ST-GEN-UC14-R02). A
// record that holds the store file of the PostgreSQL adapter is of a release on the GORM store, whose database has
// the GORM tables: that service keeps the store until a declaration version bump moves it (ST-GEN-UC10). A record
// with no store file is of a release on the runtime data path, or of one with no tables, and stays on it.
func runtimeDataPath(projectDir string, names []string, components map[string]*registry.Component) (bool, error) {
	if !slices.Contains(names, commandComponent) || !slices.Contains(names, queryComponent) {
		return false, nil
	}
	state, err := LoadState(filepath.Join(projectDir, ".stego", "state.yaml"))
	if err != nil {
		return false, fmt.Errorf("reading the record to choose the data path: %w", err)
	}
	if state.LastApplied == nil {
		return true, nil
	}
	adapter := components[adapterComponent]
	if adapter == nil || adapter.OutputNamespace == "" {
		return true, nil
	}
	_, store := state.LastApplied.Files[path.Join(adapter.OutputNamespace, "store.go")]
	return !store, nil
}
