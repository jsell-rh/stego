package fill

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
)

// allowedStandard is the fill allow list of the standard library (ST-EXT-UC01-R03). The print functions of fmt and
// the calls of log and log/slog that change, stop or bypass the standard logger are held by other rules
// (ST-EXT-UC01-R05, ST-EXT-UC01-R07).
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

// refusedCalls are the functions of fmt, log and log/slog that a fill may not call or take as a value, with what each
// does: the print functions of fmt (ST-EXT-UC01-R05) and the functions that change, stop or bypass the standard
// logger (ST-EXT-UC01-R07).
var refusedCalls = map[string]map[string]string{
	"fmt": {"Print": "prints to standard output", "Printf": "prints to standard output", "Println": "prints to standard output"},
	"log": {
		"SetOutput": "changes the standard logger", "SetFlags": "changes the standard logger",
		"SetPrefix": "changes the standard logger", "Default": "gives the standard logger to change",
		"Writer": "gives the writer of the standard logger, which bypasses it",
		"Fatal":  "stops the service", "Fatalf": "stops the service", "Fatalln": "stops the service",
		"Panic": "stops the service", "Panicf": "stops the service", "Panicln": "stops the service",
	},
	"log/slog": {"SetDefault": "changes the standard logger", "SetLogLoggerLevel": "changes the standard logger"},
}

// stopMethods are the methods of a *log.Logger that stop the service (ST-EXT-UC01-R07). The check reads no types,
// so it refuses a method of these names on every value.
var stopMethods = map[string]bool{"Fatal": true, "Fatalf": true, "Fatalln": true, "Panic": true, "Panicf": true, "Panicln": true}

// refusedDirectives are the compiler directives that let a fill reach code that its imports do not show: a symbol
// of another package, a cgo symbol or a file that is not Go.
var refusedDirectives = []string{"//go:linkname", "//go:cgo_", "//go:embed"}

// listedPackage is one package as go list -deps -json reports it, with its Go files and the files that are not Go
// source.
type listedPackage struct {
	ImportPath        string
	Dir               string
	Standard          bool
	Imports           []string
	GoFiles           []string
	IgnoredGoFiles    []string
	CgoFiles          []string
	CFiles            []string
	CXXFiles          []string
	MFiles            []string
	HFiles            []string
	FFiles            []string
	SFiles            []string
	SwigFiles         []string
	SwigCXXFiles      []string
	SysoFiles         []string
	EmbedFiles        []string
	IgnoredOtherFiles []string
	Error             *struct{ Err string }
}

// otherFiles names the files of the package that are not plain Go source, which a fill package and a listed module
// package may not hold (ST-EXT-UC01-R03): cgo, C, C++, Objective-C, headers, Fortran, assembly, SWIG, system
// objects, embedded files, and such files of other platforms.
func (p listedPackage) otherFiles() []string {
	var names []string
	for _, files := range [][]string{p.CgoFiles, p.CFiles, p.CXXFiles, p.MFiles, p.HFiles, p.FFiles, p.SFiles,
		p.SwigFiles, p.SwigCXXFiles, p.SysoFiles, p.EmbedFiles, p.IgnoredOtherFiles} {
		names = append(names, files...)
	}
	sort.Strings(names)
	return slices.Compact(names)
}

// listedFields are the fields of listedPackage that go list writes.
const listedFields = "ImportPath,Dir,Standard,Imports,GoFiles,IgnoredGoFiles,CgoFiles,CFiles,CXXFiles,MFiles,HFiles," +
	"FFiles,SFiles,SwigFiles,SwigCXXFiles,SysoFiles,EmbedFiles,IgnoredOtherFiles,Error"

// GoCommand is the go command that the import closure check runs. A test sets the pinned Go.
var GoCommand = "go"

// closureTimeout bounds one go list of the fills.
const closureTimeout = 2 * time.Minute

// The rules that a fill fault names.
const (
	ruleImports = "fill-imports"
	ruleFiles   = "fill-files"
	ruleCalls   = "fill-calls"
)

// ruleFault is one fault of one fill: the rule it breaks, the refused package of an import fault and the sentence.
type ruleFault struct{ rule, path, text string }

// ruleTarget is one fill that the declaration names, with where it names it and the packages its slot admits.
type ruleTarget struct {
	fill, path string
	contracts  []string
}

// declaredFillFaults holds every fill that the declaration names, at every slot, to the fill rules (ST-EXT-UC01-R03,
// ST-EXT-UC01-R05, ST-EXT-UC01-R07, ST-SUP-RC32-OC03, ST-SUP-RC33-OC03): the fill of each command, of each query and
// of each worker and of each slot binding, each with the contract of its own slot. A fault names the fill, where the declaration names
// it, and the refused package, file or call.
func declaredFillFaults(check compiler.FillCheck) ([]compiler.FillFault, error) {
	var targets []ruleTarget
	seen := map[string]bool{}
	add := func(fill, path, implements string) {
		if seen[fill] || fill == "" {
			return
		}
		seen[fill] = true
		targets = append(targets, ruleTarget{fill: fill, path: path, contracts: slotContracts(check, fill, implements)})
	}
	for _, f := range check.CommandFills {
		add(f.Fill, f.Path, commandSlot)
	}
	for _, f := range check.QueryFills {
		add(f.Fill, f.Path, querySlot)
	}
	for _, f := range check.WorkerFills {
		add(f.Fill, f.Path, workerSlot)
	}
	for _, binding := range check.Bindings {
		f, err := ReadManifest(filepath.Join(check.ProjectDir, "fills", binding.Fill, ManifestFile), check)
		if err != nil {
			// The binding faults name a manifest that cannot be read.
			continue
		}
		add(binding.Fill, binding.Path, f.Implements)
	}
	var names []string
	for _, target := range targets {
		names = append(names, target.fill)
	}
	packages, err := listFills(check, names)
	if err != nil {
		return nil, err
	}
	var faults []compiler.FillFault
	for _, target := range targets {
		found, err := fillFaults(check, filepath.Join(check.ProjectDir, "fills", target.fill), target.fill, target.contracts, packages)
		if err != nil {
			return nil, err
		}
		for _, fault := range found {
			faults = append(faults, compiler.FillFault{Rule: fault.rule, Path: target.path, Found: target.fill, Message: fault.text})
		}
	}
	return faults, nil
}

// fillFaults holds one fill, in its folder dir, to the fill rules. It reads every file of the fill folder that the go tool can build,
// whatever its platform suffix or build constraint, and follows the import closure that go list -deps gives (packages;
// nil before the project's first generation, when it has no go.mod) over every package that is built into the service
// from the fill or from a listed fill module. Each import must be one of contracts (the slot's contract and the runtime
// packages it reaches), a package of the fill or of a listed module, or a package of the standard library's fill allow
// list; a driver module, cgo, a listed module with no dot in its path and a package that go list does not report as
// standard are refused. Each of those packages may hold Go source files only, with no function without a body, no
// //go:linkname, //go:cgo_ or //go:embed directive, and no use of a function that ST-EXT-UC01-R05 or ST-EXT-UC01-R07
// refuses. The imports that the standard library and the slot contract make themselves are not followed.
func fillFaults(check compiler.FillCheck, dir, fill string, contracts []string, packages map[string]listedPackage) ([]ruleFault, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, nil
	}
	fillPackage := strings.TrimSuffix(check.Module, "/") + "/fills/" + fill
	own, err := treeFaults(check, fill, dir, fillPackage, contracts, packages)
	if err != nil {
		return nil, err
	}
	if packages == nil {
		return own, nil
	}
	closure := closureFaults(check, fill, fillPackage, contracts, packages)
	// A package that the closure names with its chain is not named again for the file that imports it; the
	// chain names that file.
	named := map[string]bool{}
	for _, fault := range closure {
		if fault.path != "" {
			named[fault.path] = true
		}
	}
	faults := closure
	for _, fault := range own {
		if fault.path == "" || !named[fault.path] {
			faults = append(faults, fault)
		}
	}
	return faults, nil
}

// listFills runs go list -e -deps -json on the packages of the fills, without test files, in the service's module,
// with no workspace and a read-only module graph, so it writes no file. It gives nil when the project has no go.mod
// yet or names no fill folder that exists.
func listFills(check compiler.FillCheck, fills []string) (map[string]listedPackage, error) {
	if _, err := os.Stat(filepath.Join(check.ProjectDir, "go.mod")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var patterns []string
	for _, fill := range fills {
		if _, err := os.Stat(filepath.Join(check.ProjectDir, "fills", fill)); err == nil {
			// A folder pattern, as stego test gives go test, names the packages of the main module only, so go
			// list needs no go.sum entry of a module that the fill does not reach.
			patterns = append(patterns, "./fills/"+fill+"/...")
		}
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), closureTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, GoCommand, append([]string{"list", "-e", "-deps", "-json=" + listedFields}, patterns...)...)
	cmd.Dir = check.ProjectDir
	// The service's build settings, not the host's, so go list sees the files and imports that the build sees.
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=readonly")
	for key, value := range compiler.TargetBuildSettings() {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list of the fills %s failed: %w: %s", strings.Join(fills, ", "), err, strings.TrimSpace(stderr.String()))
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
			return nil, fmt.Errorf("the go list output of the fills cannot be read: %w", err)
		}
		packages[p.ImportPath] = p
	}
	return packages, nil
}

// treeFaults reads every file of the fill folder that the go tool can build, whatever its platform suffix or build
// constraint, and holds it to the fill rules: the folder holds Go source files and its manifest only, and each Go file
// that is not a test file imports only what the rule allows and holds no function without a body, no refused
// directive and no refused function. A fault names the fill and the file.
//
// Before the project's first generation (pending), go.mod does not fix the service's module yet, so a generated
// contract package is admitted under any module path; the closure check holds the module once go.mod exists.
func treeFaults(check compiler.FillCheck, fill, dir, fillPackage string, contracts []string, packages map[string]listedPackage) ([]ruleFault, error) {
	pending := packages == nil
	var faults []ruleFault
	byFolder := map[string][]string{}
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// The go tool builds no package from testdata or from a folder or file whose name starts with _ or ., so
		// nothing there is built into the service.
		base := entry.Name()
		if name != dir && (base == "testdata" || strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".")) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		relative, _ := filepath.Rel(dir, name)
		relative = filepath.ToSlash(relative)
		switch {
		case relative == ManifestFile, strings.HasSuffix(name, "_test.go"):
		case strings.HasSuffix(name, ".go") && entry.Type().IsRegular():
			byFolder[filepath.Dir(name)] = append(byFolder[filepath.Dir(name)], name)
		default:
			faults = append(faults, ruleFault{rule: ruleFiles, text: fmt.Sprintf("fill %q holds the file %s, which is not a Go source file; a fill holds only Go source files, its %s and test data under testdata",
				fill, relative, ManifestFile)})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading the files of the fill %s: %w", fill, err)
	}
	folders := make([]string, 0, len(byFolder))
	for folder := range byFolder {
		folders = append(folders, folder)
	}
	sort.Strings(folders)
	for _, folder := range folders {
		files, err := parseFiles(byFolder[folder])
		if err != nil {
			return nil, fmt.Errorf("reading the files of the fill %s: %w", fill, err)
		}
		for _, file := range files {
			relative, _ := filepath.Rel(dir, file.name)
			relative = filepath.ToSlash(relative)
			for _, spec := range file.ast.Imports {
				path, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return nil, err
				}
				listed := listedPackage{ImportPath: path, Standard: standardLibrary(path) && path != "C"}
				follow, allowed := judgeImport(check, contracts, fillPackage, path, listed)
				switch {
				case !allowed && !(pending && pendingContract(check, contracts, path)):
					faults = append(faults, ruleFault{rule: ruleImports, path: path, text: fmt.Sprintf("fill %q: the file %s imports %s, which a fill may not import; a fill imports only its slot contract %s, the listed fill modules and the fill allow list of the standard library",
						fill, relative, path, contracts[0])})
				case allowed && follow && !pending && packages[path].ImportPath == "":
					// A file that the service build does not build imports a package that go list did not follow,
					// so its closure is not checked.
					faults = append(faults, ruleFault{rule: ruleImports, path: path, text: fmt.Sprintf("fill %q: the file %s imports %s, which go list with the service's build settings does not report, so its imports cannot be checked",
						fill, relative, path)})
				}
			}
			for _, text := range sourceFaults(file, relative, files) {
				faults = append(faults, ruleFault{rule: ruleCalls, text: fmt.Sprintf("fill %q: the file %s", fill, text)})
			}
		}
	}
	sort.Slice(faults, func(i, j int) bool { return faults[i].text < faults[j].text })
	return faults, nil
}

// pendingContract tells whether path is a generated contract package of contracts, such as out/slots, under another
// module path than the one the check gives.
func pendingContract(check compiler.FillCheck, contracts []string, path string) bool {
	for _, contract := range contracts {
		relative, ok := strings.CutPrefix(contract, strings.TrimSuffix(check.Module, "/")+"/")
		if ok && strings.HasSuffix(path, "/"+relative) && !standardLibrary(path) {
			return true
		}
	}
	return false
}

// closureFaults follows the import closure that go list -deps gives for one fill.
func closureFaults(check compiler.FillCheck, fill, fillPackage string, contracts []string, packages map[string]listedPackage) []ruleFault {
	var roots []string
	// The roots are the fill's packages that ./... names; go list -deps also reports a package under a folder that
	// starts with _ or . or is testdata when a root imports it, and that import is refused (judgeImport).
	for path := range packages {
		if underPath(path, fillPackage) && !hiddenPath(strings.TrimPrefix(path, fillPackage)) {
			roots = append(roots, path)
		}
	}
	if len(roots) == 0 {
		return []ruleFault{{rule: ruleImports, text: fmt.Sprintf("fill %q has no Go package at %s", fill, fillPackage)}}
	}
	sort.Strings(roots)
	parent := map[string]string{}
	seen := map[string]bool{}
	queue := append([]string(nil), roots...)
	for _, root := range roots {
		seen[root] = true
	}
	refused := map[string]string{}
	var faults []ruleFault
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		p := packages[path]
		if p.Error != nil && underPath(path, fillPackage) {
			faults = append(faults, ruleFault{rule: ruleImports, text: fmt.Sprintf("fill %q does not load: its package %s: %s", fill, path, p.Error.Err)})
		}
		// The files of the fill's own packages are read from its folder (treeFaults).
		if !underPath(path, fillPackage) {
			faults = append(faults, modulePackageFaults(check, contracts, fillPackage, fill, p, packages)...)
		}
		imports := append([]string(nil), p.Imports...)
		sort.Strings(imports)
		for _, imported := range imports {
			if seen[imported] {
				continue
			}
			follow, allowed := judgeImport(check, contracts, fillPackage, imported, packages[imported])
			if !allowed {
				if _, ok := refused[imported]; !ok {
					refused[imported] = path
					// A package of the fill that the go tool builds only when it is imported by its path, under a
					// folder whose name starts with _ or . or is testdata, is not read from the fill folder; its
					// files are named here.
					if underPath(imported, fillPackage) {
						faults = append(faults, modulePackageFaults(check, contracts, fillPackage, fill, packages[imported], packages)...)
					}
				}
				continue
			}
			if follow && packages[imported].ImportPath == "" {
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
					faults = append(faults, ruleFault{rule: ruleImports, text: fmt.Sprintf("fill %q reaches %s, which does not load: %s", fill, imported, p.Error.Err)})
				}
			}
		}
	}
	names := make([]string, 0, len(refused))
	for name := range refused {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		chain := []string{name}
		for at := refused[name]; at != ""; at = parent[at] {
			chain = append([]string{at}, chain...)
		}
		// Each package of the chain shows the files of it that import the next one.
		links := make([]string, len(chain))
		for i, path := range chain {
			links[i] = path
			if i+1 < len(chain) {
				if files := importingFiles(packages[path], chain[i+1]); len(files) > 0 {
					links[i] += " (" + strings.Join(files, ", ") + ")"
				}
			}
		}
		faults = append(faults, ruleFault{rule: ruleImports, path: name, text: fmt.Sprintf("fill %q may not import %s, which it reaches through %s; a fill imports only its slot contract %s, the listed fill modules and the fill allow list of the standard library",
			fill, name, strings.Join(links, " -> "), contracts[0])})
	}
	return faults
}

// importingFiles names the Go files of the package that import path, whatever their build constraints.
func importingFiles(p listedPackage, path string) []string {
	var names []string
	fset := token.NewFileSet()
	for _, name := range append(append(append([]string(nil), p.GoFiles...), p.CgoFiles...), p.IgnoredGoFiles...) {
		file, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, spec := range file.Imports {
			if imported, err := strconv.Unquote(spec.Path.Value); err == nil && imported == path {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return slices.Compact(names)
}

// modulePackageFaults holds one package that a fill reaches, of a listed fill module or of the fill under a folder
// that the fill folder check does not read, to the rules on files and calls. The imports of its files that the
// service build leaves out (IgnoredGoFiles) are judged as well, because go list does not follow them: each must be
// allowed, and a package to follow must be one that go list reports.
func modulePackageFaults(check compiler.FillCheck, contracts []string, fillPackage, fill string, p listedPackage, packages map[string]listedPackage) []ruleFault {
	var faults []ruleFault
	if p.Dir == "" {
		return nil
	}
	if other := p.otherFiles(); len(other) > 0 {
		faults = append(faults, ruleFault{rule: ruleFiles, text: fmt.Sprintf("fill %q reaches %s, which holds the files %s that are not Go source files; a package that a fill reaches holds only Go source files",
			fill, p.ImportPath, strings.Join(other, ", "))})
	}
	ignored := map[string]bool{}
	var names []string
	for _, name := range append(append([]string(nil), p.GoFiles...), p.IgnoredGoFiles...) {
		if !strings.HasSuffix(name, "_test.go") {
			names = append(names, filepath.Join(p.Dir, name))
		}
	}
	for _, name := range p.IgnoredGoFiles {
		ignored[filepath.Join(p.Dir, name)] = true
	}
	files, err := parseFiles(names)
	if err != nil {
		return append(faults, ruleFault{rule: ruleFiles, text: fmt.Sprintf("fill %q reaches %s, whose files cannot be read: %v", fill, p.ImportPath, err)})
	}
	for _, file := range files {
		shown := p.ImportPath + "/" + filepath.Base(file.name)
		for _, text := range sourceFaults(file, shown, files) {
			faults = append(faults, ruleFault{rule: ruleCalls, text: fmt.Sprintf("fill %q reaches a package whose file %s", fill, text)})
		}
		if !ignored[file.name] {
			continue
		}
		for _, spec := range file.ast.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			listed, reported := packages[path]
			if !reported {
				listed = listedPackage{ImportPath: path, Standard: standardLibrary(path) && path != "C"}
			}
			follow, allowed := judgeImport(check, contracts, fillPackage, path, listed)
			if !allowed || (follow && !reported) {
				faults = append(faults, ruleFault{rule: ruleImports, text: fmt.Sprintf("fill %q reaches %s, whose file %s, which the service build leaves out, imports %s; a package that a fill reaches imports only what a fill may import, and only packages that go list reports",
					fill, p.ImportPath, filepath.Base(file.name), path)})
			}
		}
	}
	return faults
}

// sourceFile is one parsed Go file with its file set.
type sourceFile struct {
	name string
	fset *token.FileSet
	ast  *ast.File
}

// parseFiles parses the Go files of one package folder, with their comments.
func parseFiles(names []string) ([]sourceFile, error) {
	sort.Strings(names)
	var files []sourceFile
	for _, name := range names {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, sourceFile{name: name, fset: fset, ast: file})
	}
	return files, nil
}

// packageNames gives the names that the files of one package declare at package level.
func packageNames(files []sourceFile) map[string]bool {
	declared := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.ast.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					declared[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						for _, name := range s.Names {
							declared[name.Name] = true
						}
					case *ast.TypeSpec:
						declared[s.Name.Name] = true
					}
				}
			}
		}
	}
	return declared
}

// sourceFaults gives a sentence for each function without a body, each refused directive and each use of a refused
// function in one Go file, shown by the name given, with its line. pkg is every file of the file's package, which
// tells the names the package declares itself, so a function of the package named print is not the builtin.
func sourceFaults(file sourceFile, shown string, pkg []sourceFile) []string {
	var faults []string
	at := func(pos token.Pos) string { return fmt.Sprintf("%s:%d", shown, file.fset.Position(pos).Line) }
	declared := packageNames(pkg)
	// The names that this file gives its imports.
	imported := map[string]string{}
	for _, spec := range file.ast.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "." && refusedCalls[path] != nil {
			faults = append(faults, fmt.Sprintf("%s imports %s with a dot, which hides its functions from the check; a fill names the package of each function it uses", at(spec.Pos()), path))
		}
		imported[name] = path
	}
	for _, group := range file.ast.Comments {
		for _, comment := range group.List {
			for _, directive := range refusedDirectives {
				if strings.HasPrefix(comment.Text, directive) {
					faults = append(faults, fmt.Sprintf("%s holds the directive %s, which a fill may not use", at(comment.Pos()), strings.Fields(comment.Text)[0]))
				}
			}
		}
	}
	ast.Inspect(file.ast, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if n.Body == nil {
				faults = append(faults, fmt.Sprintf("%s declares the function %s with no body; a fill gives every function a Go body", at(n.Pos()), n.Name.Name))
			}
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok && x.Obj == nil && !declared[x.Name] {
				if path, isImport := imported[x.Name]; isImport {
					if why, refused := refusedCalls[path][n.Sel.Name]; refused {
						faults = append(faults, fmt.Sprintf("%s uses %s.%s, which %s; a fill may not use it", at(n.Pos()), path, n.Sel.Name, why))
					}
					return true
				}
			}
			if stopMethods[n.Sel.Name] {
				faults = append(faults, fmt.Sprintf("%s uses the method %s, which stops the service when it is a method of a *log.Logger; a fill may not use a method of this name", at(n.Sel.Pos()), n.Sel.Name))
			}
		case *ast.GoStmt:
			faults = append(faults, fmt.Sprintf("%s starts a goroutine with a go statement; a fill runs only in the call that the service makes", at(n.Pos())))
		case *ast.CallExpr:
			if fn, ok := n.Fun.(*ast.Ident); ok && fn.Obj == nil && (fn.Name == "print" || fn.Name == "println") && !declared[fn.Name] {
				faults = append(faults, fmt.Sprintf("%s uses the builtin %s, which writes to standard error past the standard logger; a fill may not use it", at(n.Pos()), fn.Name))
			}
		}
		return true
	})
	return faults
}

// judgeImport tells whether a package that a fill package or a listed module package imports is allowed, and
// whether its own imports are the fill's to check too. contracts are the packages that the fill's slot admits.
func judgeImport(check compiler.FillCheck, contracts []string, fillPackage, path string, listed listedPackage) (follow, allowed bool) {
	switch {
	case slices.Contains(contracts, path):
		return false, true
	case listed.Standard:
		return false, allowedStandard[path]
	case inModules(path, driverModules):
		return false, false
	case underPath(path, fillPackage):
		return true, !hiddenPath(strings.TrimPrefix(path, fillPackage))
	case inModules(path, check.FillModules):
		// A listed module path with no dot in its first element is not a module that a proxy serves, and a reader
		// can take it for a package of the standard library.
		return true, !standardLibrary(path)
	default:
		return false, false
	}
}

// hiddenPath tells whether a relative package path has an element that starts with _ or . or is testdata, which the
// go tool leaves out of a ./... pattern and the fill folder check does not read.
func hiddenPath(relative string) bool {
	for _, element := range strings.Split(relative, "/") {
		if element == "testdata" || strings.HasPrefix(element, "_") || strings.HasPrefix(element, ".") {
			return true
		}
	}
	return false
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
