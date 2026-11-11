package fill

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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

// checkImports refuses a fill that breaks the fill rules (ST-EXT-INV05, ST-EXT-UC01-R03, ST-EXT-UC01-R05,
// ST-EXT-UC01-R07): a Go file that is built into the service, or a package that the fill reaches through a listed fill
// module, that imports anything but the contract package of its slot and the runtime packages that the contract
// reaches (slotContracts), a listed fill module or a package of the fill allow list of the standard library, that
// holds a file that is not Go source, a function without a body or a refused directive, or that uses a refused
// function. A test file, whose name ends in _test.go, is not built into the service, so it may import any module of
// go.mod. The fault names the file and the import, the file or the call.
func checkImports(dir string, check compiler.FillCheck, fill, implements string) error {
	packages, err := listFills(check, []string{fill})
	if err != nil {
		return err
	}
	faults, err := fillFaults(check, dir, fill, slotContracts(check, fill, implements), packages)
	if err != nil {
		return err
	}
	if len(faults) == 0 {
		return nil
	}
	texts := make([]string, len(faults))
	for i, fault := range faults {
		texts[i] = fault.text
	}
	return errors.New(strings.Join(texts, "; "))
}

// standardLibrary tells if an import path is a package of the Go standard
// library: its first element has no dot.
func standardLibrary(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
