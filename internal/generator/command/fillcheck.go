// Package command is the generator of the command component (ST-SUP-RC32). This file holds the check of a command
// fill's import closure, which generation runs before it writes a file and which the query generator runs on a query
// fill (ST-SUP-RC32-OC03).
package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// FillCheck is what CheckFill needs of one fill.
type FillCheck struct {
	// Fill is the fill's name, which each fault names.
	Fill string
	// Dir is the root of the service's module, in which go list runs.
	Dir string
	// Package is the import path of the fill's package. The packages under it are the fill's too.
	Package string
	// SlotContract is the import path of the slot's contract package: the fill may import it, and its own imports
	// are not the fill's.
	SlotContract string
	// FillModules are the modules that the declaration lists for fills (ST-DECL-EN08). Their packages are checked
	// as the fill's own are.
	FillModules []string
	// GoTool is the go command to run, or "go" when it is empty.
	GoTool string
}

// allowedStandard is the fill allow list of the standard library (ST-EXT-UC01-R03). The fmt print functions and the
// debug use of log and log/slog are held by other rules (ST-EXT-UC01-R05, ST-EXT-UC01-R06).
var allowedStandard = map[string]bool{
	"bytes": true, "cmp": true, "context": true, "crypto/sha256": true, "encoding/base64": true, "encoding/hex": true,
	"encoding/json": true, "errors": true, "fmt": true, "hash": true, "hash/crc32": true, "iter": true, "log": true,
	"log/slog": true, "maps": true, "math": true, "math/big": true, "math/bits": true, "regexp": true, "slices": true,
	"sort": true, "strconv": true, "strings": true, "time": true, "unicode": true, "unicode/utf16": true,
	"unicode/utf8": true,
}

// driverModules are the database driver modules that a fill never reaches, also when the declaration lists one for
// fills (ST-EXT-UC01-R03). A module path covers its major versions, such as github.com/jackc/pgx/v5.
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

// CheckFill refuses a fill whose import closure, as go list -deps reports it, breaks the import rule of
// ST-EXT-UC01-R03 (ST-SUP-RC32-OC03). It follows every package that is built into the service from the fill or from
// a listed fill module, and each of their imports must be the slot's contract, a package of the fill or of a listed
// module, or a package of the standard library's fill allow list; a driver module is refused also when it is listed.
// It does not follow the imports that the standard library and the slot's contract make themselves, and it does not
// read test files. The fault names the fill, each refused package and the import chain from the fill to it.
func CheckFill(ctx context.Context, check FillCheck) error {
	if check.Fill == "" || check.Dir == "" || check.Package == "" {
		return errors.New("command: a fill check needs the fill's name, the module folder and the fill's package")
	}
	packages, err := listDeps(ctx, check)
	if err != nil {
		return err
	}
	var roots []string
	for path := range packages {
		if underPath(path, check.Package) {
			roots = append(roots, path)
		}
	}
	if len(roots) == 0 {
		return fmt.Errorf("command: the fill %s has no Go package at %s", check.Fill, check.Package)
	}
	sort.Strings(roots)
	parent := map[string]string{}
	seen := map[string]bool{}
	queue := append([]string(nil), roots...)
	for _, root := range roots {
		seen[root] = true
	}
	refused := map[string]string{}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		imports := append([]string(nil), packages[path].Imports...)
		sort.Strings(imports)
		for _, imported := range imports {
			if seen[imported] {
				continue
			}
			follow, allowed := judge(check, imported, packages[imported])
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
			}
		}
	}
	if len(refused) == 0 {
		return nil
	}
	names := make([]string, 0, len(refused))
	for name := range refused {
		names = append(names, name)
	}
	sort.Strings(names)
	faults := make([]string, 0, len(names))
	for _, name := range names {
		chain := []string{name}
		for at := refused[name]; at != ""; at = parent[at] {
			chain = append([]string{at}, chain...)
		}
		faults = append(faults, fmt.Sprintf("the fill %s may not import %s, which it reaches through %s",
			check.Fill, name, strings.Join(chain, " -> ")))
	}
	return fmt.Errorf("command: %s; a fill imports only its slot contract %s, the listed fill modules and the "+
		"fill allow list of the standard library", strings.Join(faults, "; "), check.SlotContract)
}

// judge tells whether a package that a fill package or a listed module package imports is allowed, and whether its
// own imports are the fill's to check too.
func judge(check FillCheck, path string, listed listedPackage) (follow, allowed bool) {
	switch {
	case check.SlotContract != "" && path == check.SlotContract:
		return false, true
	case listed.Standard:
		return false, allowedStandard[path]
	case inModules(path, driverModules):
		return false, false
	case underPath(path, check.Package), inModules(path, check.FillModules):
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

// listDeps runs go list -deps -json on the fill's packages, without test files, in the service's module, with no
// workspace and no inherited build flags.
func listDeps(ctx context.Context, check FillCheck) (map[string]listedPackage, error) {
	tool := check.GoTool
	if tool == "" {
		tool = "go"
	}
	cmd := exec.CommandContext(ctx, tool, "list", "-e", "-deps", "-json=ImportPath,Standard,Imports,Error",
		check.Package+"/...")
	cmd.Dir = check.Dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS= ")
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("command: go list of the fill %s failed: %w: %s", check.Fill, err,
			strings.TrimSpace(stderr.String()))
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
			return nil, fmt.Errorf("command: the go list output of the fill %s cannot be read: %w", check.Fill, err)
		}
		if p.Error != nil && underPath(p.ImportPath, check.Package) {
			return nil, fmt.Errorf("command: the fill %s does not load: %s", check.Fill, p.Error.Err)
		}
		packages[p.ImportPath] = p
	}
	return packages, nil
}
