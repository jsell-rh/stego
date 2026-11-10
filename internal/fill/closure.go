package fill

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
)

// allowedStandard is the fill allow list of the standard library (ST-EXT-UC01-R03). The print functions of fmt and
// the debug use of log and log/slog are held by other rules (ST-EXT-UC01-R05, ST-EXT-UC01-R06).
var allowedStandard = map[string]bool{
	"bytes": true, "cmp": true, "context": true, "crypto/sha256": true, "encoding/base64": true, "encoding/hex": true,
	"encoding/json": true, "errors": true, "fmt": true, "hash": true, "hash/crc32": true, "iter": true, "log": true,
	"log/slog": true, "maps": true, "math": true, "math/big": true, "math/bits": true, "regexp": true, "slices": true,
	"sort": true, "strconv": true, "strings": true, "time": true, "unicode": true, "unicode/utf16": true,
	"unicode/utf8": true,
}

// driverModules are the database driver modules that a fill never reaches, also when the declaration lists one
// for fills (ST-EXT-UC01-R03). A module path covers its major versions, such as github.com/jackc/pgx/v5.
var driverModules = []string{
	"github.com/lib/pq", "github.com/jackc/pgx", "github.com/jackc/pgconn", "github.com/go-sql-driver/mysql",
	"github.com/mattn/go-sqlite3", "modernc.org/sqlite", "github.com/microsoft/go-mssqldb", "github.com/sijms/go-ora",
}

// listedPackage is one package as go list -deps -json reports it.
type listedPackage struct {
	ImportPath string
	Standard   bool
	Imports    []string
	Error      *struct{ Err string }
}

// GoCommand is the go command that the import closure check runs. A test sets the pinned Go.
var GoCommand = "go"

// closureTimeout bounds one go list of a fill.
const closureTimeout = 2 * time.Minute

// commandFillFaults checks the import closure of each command fill (ST-SUP-RC32-OC03): a fault for each fill whose
// closure breaks the import rule of ST-EXT-UC01-R03, which names the fill, each refused package and the import
// chain from the fill to it.
func commandFillFaults(check compiler.FillCheck) ([]compiler.FillFault, error) {
	var faults []compiler.FillFault
	seen := map[string]bool{}
	for _, f := range check.CommandFills {
		if seen[f.Fill] {
			continue
		}
		seen[f.Fill] = true
		refused, err := checkClosure(check, f)
		if err != nil {
			return nil, err
		}
		for _, r := range refused {
			faults = append(faults, compiler.FillFault{Rule: "fill-imports", Path: f.Path, Found: f.Fill, Message: r})
		}
	}
	return faults, nil
}

// checkClosure lists the import closure of one command fill with go list -deps, without test files, in the
// service's module, and gives one sentence for each refused package. It follows every package that is built into
// the service from the fill or from a listed fill module, and each of their imports must be the slot's contract, a
// package of the fill or of a listed module, or a package of the standard library's fill allow list; a driver
// module is refused also when it is listed. It does not follow the imports that the standard library and the slot
// contract make themselves. A project with no go.mod yet, before its first generation, is checked on the fill's own
// imports only.
func checkClosure(check compiler.FillCheck, f compiler.CommandFill) ([]string, error) {
	dir := filepath.Join(check.ProjectDir, "fills", f.Fill)
	if _, err := os.Stat(dir); err != nil {
		return nil, nil
	}
	fillPackage := strings.TrimSuffix(check.Module, "/") + "/fills/" + f.Fill
	// go list sees the files of the host platform with no build tags only, so the fill's own imports are also read
	// from every Go file that is not a test file, whatever its platform suffix or build constraint.
	own, err := fileImportFaults(check, f, dir, fillPackage)
	if err != nil {
		return nil, err
	}
	var faults []string
	refused := map[string]bool{}
	if _, err := os.Stat(filepath.Join(check.ProjectDir, "go.mod")); err == nil {
		if faults, refused, err = closureFaults(check, f, fillPackage); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// A package that the closure already names with its chain is not named again for its file.
	for _, fault := range own {
		if !refused[fault.path] {
			faults = append(faults, fault.text)
		}
	}
	return faults, nil
}

// fileFault is one refused import of one file of a fill.
type fileFault struct{ path, text string }

// fileImportFaults reads the imports of every Go file of the fill that is not a test file, whatever its platform
// suffix or build constraint, and holds each to the import rule as the closure check does. A fault names the fill,
// the file and the refused package.
func fileImportFaults(check compiler.FillCheck, f compiler.CommandFill, dir, fillPackage string) ([]fileFault, error) {
	var faults []fileFault
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
			listed := listedPackage{ImportPath: path, Standard: standardLibrary(path) && path != "C"}
			if _, allowed := judgeImport(check, f.Contract, fillPackage, path, listed); !allowed {
				faults = append(faults, fileFault{path: path, text: fmt.Sprintf("fill %q may not import %s, which its file %s imports; a fill imports only its slot contract %s, the listed fill modules and the fill allow list of the standard library",
					f.Fill, path, filepath.ToSlash(relative), f.Contract)})
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading the imports of the fill %s: %w", f.Fill, err)
	}
	sort.Slice(faults, func(i, j int) bool { return faults[i].text < faults[j].text })
	return faults, nil
}

// closureFaults follows the import closure that go list -deps gives for one command fill.
func closureFaults(check compiler.FillCheck, f compiler.CommandFill, fillPackage string) ([]string, map[string]bool, error) {
	packages, err := listDeps(check.ProjectDir, f.Fill, fillPackage)
	if err != nil {
		return nil, nil, err
	}
	var roots []string
	for path := range packages {
		if underPath(path, fillPackage) {
			roots = append(roots, path)
		}
	}
	if len(roots) == 0 {
		return []string{fmt.Sprintf("fill %q has no Go package at %s", f.Fill, fillPackage)}, nil, nil
	}
	sort.Strings(roots)
	parent := map[string]string{}
	seen := map[string]bool{}
	queue := append([]string(nil), roots...)
	for _, root := range roots {
		seen[root] = true
	}
	refused := map[string]string{}
	var broken []string
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		imports := append([]string(nil), packages[path].Imports...)
		sort.Strings(imports)
		for _, imported := range imports {
			if seen[imported] {
				continue
			}
			follow, allowed := judgeImport(check, f.Contract, fillPackage, imported, packages[imported])
			if !allowed {
				if _, ok := refused[imported]; !ok {
					refused[imported] = path
				}
				continue
			}
			if follow {
				seen[imported] = true
				parent[imported] = path
				queue = append(queue, imported)
				if p := packages[imported]; p.Error != nil {
					broken = append(broken, fmt.Sprintf("fill %q reaches %s, which does not load: %s", f.Fill, imported, p.Error.Err))
				}
			}
		}
	}
	names := make([]string, 0, len(refused))
	for name := range refused {
		names = append(names, name)
	}
	sort.Strings(names)
	faults := broken
	named := map[string]bool{}
	for _, name := range names {
		named[name] = true
		chain := []string{name}
		for at := refused[name]; at != ""; at = parent[at] {
			chain = append([]string{at}, chain...)
		}
		faults = append(faults, fmt.Sprintf("fill %q may not import %s, which it reaches through %s; a fill imports only its slot contract %s, the listed fill modules and the fill allow list of the standard library",
			f.Fill, name, strings.Join(chain, " -> "), f.Contract))
	}
	return faults, named, nil
}

// judgeImport tells whether a package that a fill package or a listed module package imports is allowed, and
// whether its own imports are the fill's to check too.
func judgeImport(check compiler.FillCheck, contract, fillPackage, path string, listed listedPackage) (follow, allowed bool) {
	switch {
	case contract != "" && path == contract:
		return false, true
	case listed.Standard || (listed.ImportPath == "" && standardLibrary(path) && path != "C"):
		return false, allowedStandard[path]
	case inModules(path, driverModules):
		return false, false
	case underPath(path, fillPackage), inModules(path, check.FillModules):
		return true, true
	default:
		return false, false
	}
}

// underPath tells whether path is base or a package under it.
func underPath(path, base string) bool {
	return path == base || strings.HasPrefix(path, base+"/")
}

// inModules tells whether path is a package of one of modules: the module path itself or a path under it, so
// github.com/google/uuidx is not a package of github.com/google/uuid.
func inModules(path string, modules []string) bool {
	for _, module := range modules {
		if module != "" && underPath(path, module) {
			return true
		}
	}
	return false
}

// listDeps runs go list -e -deps -json on the fill's packages, without test files, in the service's module, with no
// workspace and a read-only module graph, so it writes no file.
func listDeps(dir, fill, fillPackage string) (map[string]listedPackage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), closureTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, GoCommand, "list", "-e", "-deps", "-json=ImportPath,Standard,Imports,Error", fillPackage+"/...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly")
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list of the fill %s failed: %w: %s", fill, err, strings.TrimSpace(stderr.String()))
	}
	packages := map[string]listedPackage{}
	decoder := json.NewDecoder(&stdout)
	for {
		var p listedPackage
		err := decoder.Decode(&p)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the go list output of the fill %s cannot be read: %w", fill, err)
		}
		if p.Error != nil && underPath(p.ImportPath, fillPackage) {
			return nil, fmt.Errorf("the fill %s does not load: %s", fill, p.Error.Err)
		}
		packages[p.ImportPath] = p
	}
	return packages, nil
}
