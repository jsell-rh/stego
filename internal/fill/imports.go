package fill

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/compiler"
)

// runtimeModule is the module of the runtime library, whose packages a slot contract reaches.
const runtimeModule = "github.com/jsell-rh/stego/registry/runtime"

// querySlot and commandSlot are the slots whose contracts are not the package out/slots.
const (
	querySlot   = "query.query"
	commandSlot = "command.command"
)

// slotContracts gives the packages that a fill of the slot implements may import besides the standard library and
// the listed fill modules (ST-EXT-INV05): its slot's contract package, out/slots for a protobuf slot, out/queries for
// the query slot and the command generator's package for the command slot, and the packages of the runtime library
// that the contract package imports.
func slotContracts(check compiler.FillCheck, fill, implements string) []string {
	contract := strings.TrimSuffix(check.Module, "/") + "/out/slots"
	switch implements {
	case querySlot:
		if check.QueryContract != "" {
			contract = check.QueryContract
		}
	case commandSlot:
		for _, f := range check.CommandFills {
			if f.Fill == fill {
				contract = f.Contract
			}
		}
	}
	return append([]string{contract}, contractRuntimeImports(check, contract)...)
}

// contractRuntimeImports gives the packages of the runtime library that the Go files of the contract package
// import, as generated in the project; none when the package is not generated yet.
func contractRuntimeImports(check compiler.FillCheck, contract string) []string {
	relative, ok := strings.CutPrefix(contract, strings.TrimSuffix(check.Module, "/")+"/")
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(check.ProjectDir, filepath.FromSlash(relative)))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(check.ProjectDir, filepath.FromSlash(relative), entry.Name()), nil, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err == nil && strings.HasPrefix(path, runtimeModule+"/") && !seen[path] {
				seen[path] = true
				out = append(out, path)
			}
		}
	}
	sort.Strings(out)
	return out
}

// checkImports refuses a fill whose Go files that are built into the service import anything but the contract
// package of its slot and the runtime packages that the contract reaches (slotContracts), the Go standard library and
// the modules the declaration lists for fills (ST-EXT-INV05). A test file, whose name ends in _test.go, is not built
// into the service, so it may import any module of go.mod. The fault names the file and the import.
func checkImports(dir string, check compiler.FillCheck, fill, implements string) error {
	admitted := slotContracts(check, fill, implements)
	contracts := admitted[0]
	var faults []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(dir, name)
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if slices.Contains(admitted, path) || standardLibrary(path) || listedModule(path, check.FillModules) {
				continue
			}
			faults = append(faults, fmt.Sprintf("the file %s imports %s", filepath.ToSlash(relative), path))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(faults) == 0 {
		return nil
	}
	sort.Strings(faults)
	allowed := append([]string{contracts, "the standard library"}, check.FillModules...)
	return fmt.Errorf("%s, which a fill may not import; a fill imports only %s", strings.Join(faults, "; "), strings.Join(allowed, ", "))
}

// standardLibrary tells if an import path is a package of the Go standard
// library: its first element has no dot.
func standardLibrary(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// listedModule tells if an import path is a package of a listed module: the
// module path itself, or a path under it. A path that only starts with the
// module path's text, such as github.com/google/uuidx for
// github.com/google/uuid, is not.
func listedModule(path string, modules []string) bool {
	for _, module := range modules {
		if path == module || strings.HasPrefix(path, module+"/") {
			return true
		}
	}
	return false
}
