package fill

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/compiler"
)

// checkImports refuses a fill whose Go files that are built into the service
// import anything but the slot contract package out/slots of the service's
// module, the Go standard library and the modules the declaration lists for
// fills (ST-EXT-INV05). A test file, whose name ends in _test.go, is not
// built into the service, so it may import any module of go.mod. The fault
// names the file and the import.
func checkImports(dir string, check compiler.FillCheck) error {
	contracts := strings.TrimSuffix(check.Module, "/") + "/out/slots"
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
			if path == contracts || standardLibrary(path) || listedModule(path, check.FillModules) {
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
