package assure

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The rules of STEGO's own code standard (ST-STD-CODE) that the code-standard predicates hold a tree to.
const (
	RuleOneDomainPerPackage = "ST-STD-CODE-R01"
	RuleNamedAsRecordNames  = "ST-STD-CODE-R02"
	RuleImportsFollowOrder  = "ST-STD-CODE-R03"
	RuleTestNamesWhatItDoes = "ST-STD-CODE-R04"
	RuleNoFileWithoutRecord = "ST-STD-CODE-R05"
	RuleStubIsNotBuilt      = "ST-STD-CODE-R06"
)

// CodeFault is one fault of a tree against STEGO's own code standard (ST-STD-CODE): the rule it breaks, the file
// or package it is in, from the repository's root, and a sentence that tells why. The same tree and spec always
// give the same faults, so a gate can compare the faults of two commits.
type CodeFault struct {
	Rule   string
	Path   string
	Reason string
}

func (f CodeFault) String() string {
	return f.Path + ": " + f.Reason + " (" + f.Rule + ")"
}

// CodeFaults runs the six predicates of the code standard over the tree at a commit and the spec at the same
// commit, and returns every fault, sorted by rule, then by file or package, then by reason. It is the one entry
// the gates call: ST-ASR-SP05-PR packageOfTwoDomains (ST-STD-CODE-R01), ST-ASR-SP06-PR identifierMissing (R02),
// ST-ASR-SP07-PR importAgainstOrder (R03), ST-ASR-SP08-PR testWithoutID (R04), ST-ASR-SP09-PR fileWithoutRecord
// (R05) and ST-ASR-SP10-PR stubBuilt (R06).
func CodeFaults(spec Spec, tree Tree) []CodeFault {
	var faults []CodeFault
	faults = append(faults, packageOfTwoDomains(spec, tree)...)
	faults = append(faults, identifierMissing(spec, tree)...)
	faults = append(faults, importAgainstOrder(spec, tree)...)
	faults = append(faults, testWithoutID(spec, tree)...)
	faults = append(faults, fileWithoutRecord(spec, tree)...)
	faults = append(faults, stubBuilt(spec, tree)...)
	sortFaults(faults)
	return faults
}

func sortFaults(faults []CodeFault) {
	sort.Slice(faults, func(i, j int) bool {
		a, b := faults[i], faults[j]
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Reason < b.Reason
	})
}

// Tree is the files of one commit's tree, which the code-standard predicates read: each file's path from the
// repository's root, with forward slashes, and its bytes.
type Tree struct {
	files map[string][]byte
}

// NewTree makes a tree of the files given, by their paths from the root.
func NewTree(files map[string][]byte) Tree {
	tree := Tree{files: make(map[string][]byte, len(files))}
	for name, data := range files {
		tree.files[path.Clean(name)] = data
	}
	return tree
}

// File is the bytes of the file at the path, and whether the tree holds it.
func (t Tree) File(name string) ([]byte, bool) {
	data, ok := t.files[name]
	return data, ok
}

// paths lists the tree's files, in order.
func (t Tree) paths() []string {
	names := make([]string, 0, len(t.files))
	for name := range t.files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ReadCommittedTree reads the tree of the commit from the git objects of the repository at root, never from the
// working tree: every regular file, executable or not, with its bytes. A symbolic link or a submodule is not a
// file of the tree. Replace refs are off, so every object is the one its ID names.
func ReadCommittedTree(root, commit string) (Tree, error) {
	listing, err := git(root, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return Tree{}, err
	}
	var names, ids []string
	for _, entry := range strings.Split(string(listing), "\x00") {
		info, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(info)
		if !ok || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			continue
		}
		names = append(names, name)
		ids = append(ids, fields[2])
	}
	blobs, err := gitBlobs(root, ids)
	if err != nil {
		return Tree{}, err
	}
	tree := Tree{files: make(map[string][]byte, len(names))}
	for i, name := range names {
		tree.files[name] = blobs[i]
	}
	return tree, nil
}

// gitBlobs reads the blobs of the IDs given, in their order, with one git cat-file --batch.
func gitBlobs(root string, ids []string) ([][]byte, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	command := exec.Command("git", "-C", root, "cat-file", "--batch")
	command.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
	command.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file --batch: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	reader := bufio.NewReader(bytes.NewReader(out))
	blobs := make([][]byte, len(ids))
	for i, id := range ids {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("git cat-file --batch: no object for %s", id)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != id || fields[1] != "blob" {
			return nil, fmt.Errorf("git cat-file --batch: %q is not blob %s", strings.TrimSpace(header), id)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("git cat-file --batch: %q: %w", strings.TrimSpace(header), err)
		}
		data := make([]byte, size+1)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, fmt.Errorf("git cat-file --batch: blob %s: %w", id, err)
		}
		blobs[i] = data[:size]
	}
	return blobs, nil
}

// TechnicalRecord is one record of the spec's technical stage that names a file or a folder: its ID, its kind,
// its title, and the path it names from the product's root.
type TechnicalRecord struct {
	ID    string
	Kind  string
	Title string
	File  string
}

// latheTechnicalKinds are Lathe's own kinds of the technical stage that have a file column, in its schema 2. A
// product's own kinds of that stage are read from its product_kind table.
func latheTechnicalKinds() []string {
	return []string{
		"adapter", "command_handler", "data_model", "endpoint_handler", "entry_point", "event_handler", "event_type",
		"predicate", "projection", "query_handler", "reader", "repository", "rule", "service", "table", "tooling",
		"ui_component", "value_type",
	}
}

// readTechnicalStage reads, from the spec's tables by name, the domains with their titles and every technical
// record that names a file or folder, in ID order.
func readTechnicalStage(tables map[string]SpecFile) (map[string]string, []TechnicalRecord, error) {
	type row struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		File  string `json:"file"`
		Name  string `json:"name"`
		Stage string `json:"stage"`
	}
	rows := func(table string) ([]row, error) {
		file, ok := tables[table]
		if !ok {
			return nil, nil
		}
		var read []row
		scanner := bufio.NewScanner(bytes.NewReader(file.Data))
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
				continue
			}
			var r row
			if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
				return nil, fmt.Errorf("%s line %d: %w", file.Path, line, err)
			}
			read = append(read, r)
		}
		return read, scanner.Err()
	}
	domainRows, err := rows("domain")
	if err != nil {
		return nil, nil, err
	}
	domains := make(map[string]string, len(domainRows))
	for _, r := range domainRows {
		domains[r.ID] = r.Title
	}
	kinds := latheTechnicalKinds()
	productKinds, err := rows("product_kind")
	if err != nil {
		return nil, nil, err
	}
	for _, r := range productKinds {
		if r.Stage == "technical-specification" {
			kinds = append(kinds, r.Name)
		}
	}
	sort.Strings(kinds)
	var records []TechnicalRecord
	for _, kind := range kinds {
		kindRows, err := rows(kind)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range kindRows {
			if r.File != "" {
				records = append(records, TechnicalRecord{ID: r.ID, Kind: kind, Title: r.Title, File: normalPath(r.File)})
			}
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return domains, records, nil
}

// normalPath is a path as records write it: from the root, with no leading ./ and no trailing slash.
func normalPath(name string) string {
	return strings.TrimSuffix(path.Clean(strings.TrimPrefix(name, "./")), "/")
}

// domainOf is the domain a record belongs to: the domain whose ID starts the record's ID. A shared value type
// (ST-VOnn), an integration (ST-INTnn) or another record of the product as a whole belongs to no domain.
func (s Spec) domainOf(id string) string {
	found := ""
	for domain := range s.Domains {
		if strings.HasPrefix(id, domain+"-") && len(domain) > len(found) {
			found = domain
		}
	}
	return found
}

// goPackageOf is the folder of the Go package that holds the file, when the file is Go source, not a test, in a
// package under internal or cmd. A folder named testdata, or one whose name starts with a dot or an underscore,
// holds no package.
func goPackageOf(name string) (string, bool) {
	if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
		return "", false
	}
	if !strings.HasPrefix(name, "internal/") && !strings.HasPrefix(name, "cmd/") {
		return "", false
	}
	dir := path.Dir(name)
	for _, part := range strings.Split(dir, "/") {
		if part == "testdata" || strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
			return "", false
		}
	}
	return dir, true
}

// packageDomains lists, for each Go package under internal or cmd, the domains whose technical records name a
// file the tree holds in it, in order.
func packageDomains(spec Spec, tree Tree) map[string][]string {
	sets := make(map[string]map[string]bool)
	for _, record := range spec.Technical {
		dir, ok := goPackageOf(record.File)
		if !ok {
			continue
		}
		if _, held := tree.File(record.File); !held {
			continue
		}
		domain := spec.domainOf(record.ID)
		if domain == "" {
			continue
		}
		if sets[dir] == nil {
			sets[dir] = make(map[string]bool)
		}
		sets[dir][domain] = true
	}
	domains := make(map[string][]string, len(sets))
	for dir, set := range sets {
		for domain := range set {
			domains[dir] = append(domains[dir], domain)
		}
		sort.Strings(domains[dir])
	}
	return domains
}

// packageOfTwoDomains is the predicate of ST-ASR-SP05 (ST-STD-CODE-R01): a Go package under internal or cmd holds
// a file named by a technical record of one domain and a file named by a technical record of another. A shared
// value type counts for no domain. It finds one fault per such package, with its domains.
func packageOfTwoDomains(spec Spec, tree Tree) []CodeFault {
	var faults []CodeFault
	for dir, domains := range packageDomains(spec, tree) {
		if len(domains) > 1 {
			faults = append(faults, CodeFault{
				Rule:   RuleOneDomainPerPackage,
				Path:   dir,
				Reason: "the package holds files that the records of more than one domain name: " + strings.Join(domains, ", "),
			})
		}
	}
	sortFaults(faults)
	return faults
}

// codeTitledKinds are the kinds of technical record whose title is the package-qualified identifier the file
// declares: those whose title Lathe defines as the record's name in code, and the table records that
// ST-ASR-SP06-T names.
func codeTitledKinds() map[string]bool {
	return map[string]bool{
		"adapter": true, "command_handler": true, "data_model": true, "endpoint_handler": true, "event_handler": true,
		"event_type": true, "predicate": true, "projection": true, "query_handler": true, "reader": true,
		"repository": true, "rule": true, "service": true, "table": true, "ui_component": true, "value_type": true,
	}
}

// codeKinds are the kinds of technical record that name a function which must read its inputs: predicates,
// handlers and services (ST-STD-CODE-R06).
func codeKinds() map[string]bool {
	return map[string]bool{
		"command_handler": true, "endpoint_handler": true, "event_handler": true, "predicate": true,
		"query_handler": true, "service": true,
	}
}

// goFiles parses the Go files of the tree on demand, each at most once.
type goFiles struct {
	tree   Tree
	fset   *token.FileSet
	parsed map[string]*ast.File
	errs   map[string]error
}

func newGoFiles(tree Tree) *goFiles {
	return &goFiles{tree: tree, fset: token.NewFileSet(), parsed: make(map[string]*ast.File), errs: make(map[string]error)}
}

// parse parses the Go file at the path; the file must be in the tree.
func (g *goFiles) parse(name string) (*ast.File, error) {
	if file, ok := g.parsed[name]; ok {
		return file, g.errs[name]
	}
	data, _ := g.tree.File(name)
	file, err := parser.ParseFile(g.fset, name, data, parser.SkipObjectResolution)
	g.parsed[name], g.errs[name] = file, err
	return file, err
}

// qualifiedName splits a title such as assure.Decision or assure.Decision.Commit into its package name and the
// one or two identifiers after it. It refuses any other title.
func qualifiedName(title string) (string, []string, bool) {
	parts := strings.Split(title, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return "", nil, false
	}
	for _, part := range parts {
		if !token.IsIdentifier(part) {
			return "", nil, false
		}
	}
	return parts[0], parts[1:], true
}

// receiverName is the name of the type a method's receiver has, such as Tree for (t *Tree) or (t Tree[K]).
func receiverName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return ""
	}
	expr := decl.Recv.List[0].Type
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.Ident:
			return e.Name
		default:
			return ""
		}
	}
}

// declares tells if the file declares the identifiers at its top level: a type, function, variable or constant
// Name, or a method's receiver type Name; or, for Type.Member, a type Type that the file declares, or whose
// methods it declares, with a method, struct field or interface method Member that the file declares.
func declares(file *ast.File, names []string) bool {
	typeName := names[0]
	typeDeclared, memberDeclared := false, false
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			receiver := receiverName(d)
			if len(names) == 1 && ((receiver == "" && d.Name.Name == typeName) || receiver == typeName) {
				return true
			}
			if receiver == typeName {
				typeDeclared = true
				if len(names) == 2 && d.Name.Name == names[1] {
					memberDeclared = true
				}
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.Name != typeName {
						continue
					}
					if len(names) == 1 {
						return true
					}
					typeDeclared = true
					if hasMember(s.Type, names[1]) {
						memberDeclared = true
					}
				case *ast.ValueSpec:
					for _, name := range s.Names {
						if len(names) == 1 && name.Name == typeName {
							return true
						}
					}
				}
			}
		}
	}
	return typeDeclared && memberDeclared
}

// hasMember tells if a struct type has the field, or an interface type the method.
func hasMember(expr ast.Expr, member string) bool {
	var fields *ast.FieldList
	switch t := expr.(type) {
	case *ast.StructType:
		fields = t.Fields
	case *ast.InterfaceType:
		fields = t.Methods
	default:
		return false
	}
	for _, field := range fields.List {
		for _, name := range field.Names {
			if name.Name == member {
				return true
			}
		}
	}
	return false
}

// identifierMissing is the predicate of ST-ASR-SP06 (ST-STD-CODE-R02): a technical record names a Go file that
// exists and the package-qualified identifier in the record's title is not declared in that file. A title that
// is not an identifier of the file's package, such as a file name, is not declared either. It reads the records
// whose title is their name in code, and the table records.
func identifierMissing(spec Spec, tree Tree) []CodeFault {
	kinds := codeTitledKinds()
	files := newGoFiles(tree)
	var faults []CodeFault
	for _, record := range spec.Technical {
		if !kinds[record.Kind] || !strings.HasSuffix(record.File, ".go") {
			continue
		}
		if _, held := tree.File(record.File); !held {
			continue
		}
		fault := func(format string, args ...any) {
			faults = append(faults, CodeFault{Rule: RuleNamedAsRecordNames, Path: record.File, Reason: fmt.Sprintf(format, args...)})
		}
		file, err := files.parse(record.File)
		if err != nil {
			fault("%s names %s, which does not parse, so it declares nothing", record.ID, record.Title)
			continue
		}
		pkg, names, ok := qualifiedName(record.Title)
		if !ok || pkg != file.Name.Name {
			fault("%s is titled %q, which is not an identifier of package %s", record.ID, record.Title, file.Name.Name)
			continue
		}
		if !declares(file, names) {
			fault("%s names %s, which the file does not declare", record.ID, record.Title)
		}
	}
	sortFaults(faults)
	return faults
}

// importsAllowed tells if a package of the domain titled from may import a package of the domain titled to, along
// the order of ST-STD-CODE-R03: Assuring and Declaring import no other domain; Supplying imports Declaring;
// Generating imports Declaring and Supplying; Extending imports Declaring and Generating; Building imports
// Generating; Distributing and Delivering import Building. The order is read as written: it is not transitive.
func importsAllowed(from, to string) bool {
	switch from {
	case "Supplying":
		return to == "Declaring"
	case "Generating":
		return to == "Declaring" || to == "Supplying"
	case "Extending":
		return to == "Declaring" || to == "Generating"
	case "Building":
		return to == "Generating"
	case "Distributing", "Delivering":
		return to == "Building"
	}
	return false
}

// modulePath is the module path go.mod at the tree's root declares, or "" when the tree has no go.mod or it
// declares none.
func modulePath(tree Tree) string {
	data, ok := tree.File("go.mod")
	if !ok {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "//")
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			if unquoted, err := strconv.Unquote(fields[1]); err == nil {
				return unquoted
			}
			return fields[1]
		}
	}
	return ""
}

// importAgainstOrder is the predicate of ST-ASR-SP07 (ST-STD-CODE-R03): a package of one domain imports a package
// of a domain the order does not allow. It reads the imports of every Go file that is not a test in a package
// under internal or cmd, resolves them against go.mod's module path, and gives each package the domains whose
// records name its files. A cmd package may import any domain; a package no domain's record names, a shared value
// type and the standard library belong to no domain. It finds one fault per import against the order, with both
// packages and the domains of each pair the order does not allow.
func importAgainstOrder(spec Spec, tree Tree) []CodeFault {
	module := modulePath(tree)
	if module == "" {
		return nil
	}
	domains := packageDomains(spec, tree)
	title := func(domain string) string {
		if t := spec.Domains[domain]; t != "" {
			return t
		}
		return domain
	}
	imports := make(map[string]map[string]bool)
	var faults []CodeFault
	fset := token.NewFileSet()
	for _, name := range tree.paths() {
		dir, ok := goPackageOf(name)
		if !ok || strings.HasPrefix(dir, "cmd/") || len(domains[dir]) == 0 {
			continue
		}
		data, _ := tree.File(name)
		file, err := parser.ParseFile(fset, name, data, parser.ImportsOnly)
		if err != nil {
			faults = append(faults, CodeFault{Rule: RuleImportsFollowOrder, Path: name, Reason: "its imports cannot be read: " + err.Error()})
			continue
		}
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil || !strings.HasPrefix(imported, module+"/") {
				continue
			}
			if imports[dir] == nil {
				imports[dir] = make(map[string]bool)
			}
			imports[dir][strings.TrimPrefix(imported, module+"/")] = true
		}
	}
	for dir, targets := range imports {
		for target := range targets {
			var against []string
			for _, from := range domains[dir] {
				for _, to := range domains[target] {
					if from != to && !importsAllowed(title(from), title(to)) {
						against = append(against, fmt.Sprintf("%s (%s) may not import %s (%s)", title(from), from, title(to), to))
					}
				}
			}
			if len(against) > 0 {
				faults = append(faults, CodeFault{
					Rule:   RuleImportsFollowOrder,
					Path:   dir,
					Reason: "the package imports " + target + " against the domain order: " + strings.Join(against, "; "),
				})
			}
		}
	}
	sortFaults(faults)
	return faults
}

// testGlobs are the globs of the product's test files that lathe.config.json at the tree's root names, or Lathe's
// default when it names none.
func testGlobs(tree Tree) []string {
	var config struct {
		Tests []string `json:"tests"`
	}
	if data, ok := tree.File("lathe.config.json"); ok && json.Unmarshal(data, &config) == nil && len(config.Tests) > 0 {
		return config.Tests
	}
	return []string{"**/*.test.{ts,tsx,js,mjs}", "**/*.spec.{ts,tsx,js}"}
}

// matchesGlob tells if the path matches the glob: ** matches any number of folders, {a,b} either text, and each
// other part of the glob matches one part of the path as path.Match does.
func matchesGlob(glob, name string) bool {
	if open := strings.Index(glob, "{"); open >= 0 {
		if end := strings.Index(glob[open:], "}"); end > 0 {
			for _, choice := range strings.Split(glob[open+1:open+end], ",") {
				if matchesGlob(glob[:open]+choice+glob[open+end+1:], name) {
					return true
				}
			}
			return false
		}
	}
	return matchParts(strings.Split(strings.TrimPrefix(glob, "./"), "/"), strings.Split(name, "/"))
}

func matchParts(glob, name []string) bool {
	if len(glob) == 0 {
		return len(name) == 0
	}
	if glob[0] == "**" {
		for skip := 0; skip <= len(name); skip++ {
			if matchParts(glob[1:], name[skip:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	if ok, err := path.Match(glob[0], name[0]); err != nil || !ok {
		return false
	}
	return matchParts(glob[1:], name[1:])
}

// productFiles lists the tree's files that belong to the product, in order: not in a folder Lathe never reads
// (node_modules, dist, build, out, coverage, or one whose name starts with a dot) and not in the folder of another
// product, one with its own lathe.config.json.
func productFiles(tree Tree) []string {
	others := make(map[string]bool)
	for _, name := range tree.paths() {
		if path.Base(name) == "lathe.config.json" && path.Dir(name) != "." {
			others[path.Dir(name)] = true
		}
	}
	var files []string
	for _, name := range tree.paths() {
		parts := strings.Split(name, "/")
		skip := false
		for i, part := range parts[:len(parts)-1] {
			switch {
			case part == "node_modules", part == "dist", part == "build", part == "out", part == "coverage",
				strings.HasPrefix(part, "."), others[strings.Join(parts[:i+1], "/")]:
				skip = true
			}
		}
		if !skip {
			files = append(files, name)
		}
	}
	return files
}

// carriesID tells if the text holds the ID as a whole word: not part of a longer ID or name. An underscore may
// follow it, as go test writes a space in a test's name.
func carriesID(text []byte, id string) bool {
	for at := 0; ; {
		i := bytes.Index(text[at:], []byte(id))
		if i < 0 {
			return false
		}
		start, end := at+i, at+i+len(id)
		if (start == 0 || !idByte(text[start-1])) && (end == len(text) || !idByte(text[end])) {
			return true
		}
		at = start + 1
	}
}

func idByte(b byte) bool {
	return b == '-' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

// testWithoutID is the predicate of ST-ASR-SP08 (ST-STD-CODE-R04): a test file names no test record's ID, or an
// automated test record's file does not carry the record's ID. The test files are those that the test globs of
// lathe.config.json match under the product's root; a file names a test record when it holds, as a whole word,
// the ID of a test record of the spec.
func testWithoutID(spec Spec, tree Tree) []CodeFault {
	// A word that has the form of a test record's ID, such as ST-ASR-SP05-T or ST-STD-CODE-R01-T01.
	testRecordID := regexp.MustCompile(`ST-[A-Z0-9]+(?:-[A-Z0-9]+)*-T[0-9]*`)
	var faults []CodeFault
	globs := testGlobs(tree)
	for _, name := range productFiles(tree) {
		matched := false
		for _, glob := range globs {
			if matchesGlob(glob, name) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		data, _ := tree.File(name)
		names := false
		for _, found := range testRecordID.FindAll(data, -1) {
			if _, ok := spec.Tests[string(found)]; ok && carriesID(data, string(found)) {
				names = true
				break
			}
		}
		if !names {
			faults = append(faults, CodeFault{Rule: RuleTestNamesWhatItDoes, Path: name, Reason: "the test file names no test record's ID"})
		}
	}
	for _, id := range spec.automated() {
		record := spec.Tests[id]
		data, held := tree.File(normalPath(record.File))
		switch {
		case record.File == "":
			faults = append(faults, CodeFault{Rule: RuleTestNamesWhatItDoes, Path: id, Reason: "automated test record " + id + " names no file"})
		case !held:
			faults = append(faults, CodeFault{Rule: RuleTestNamesWhatItDoes, Path: record.File, Reason: "automated test record " + id + " names this file, which the tree does not hold"})
		case !carriesID(data, id):
			faults = append(faults, CodeFault{Rule: RuleTestNamesWhatItDoes, Path: record.File, Reason: "the file does not carry the ID of automated test record " + id})
		}
	}
	sortFaults(faults)
	return faults
}

// sourceExtensions are the file extensions of source files, as Lathe's code-file-untraced reads them.
func sourceExtensions() map[string]bool {
	extensions := make(map[string]bool)
	for _, extension := range strings.Split("ts tsx js jsx mjs cjs mts cts vue svelte py rb go rs java kt swift cs php", " ") {
		extensions["."+extension] = true
	}
	return extensions
}

// generatedHead tells if a file's first five lines say it is generated: @generated, or generated and do not
// edit, as Lathe reads them.
func generatedHead() func(data []byte) bool {
	generatedWord := regexp.MustCompile(`(?i)\bgenerated\b`)
	doNotEdit := regexp.MustCompile(`(?i)\bdo not edit\b`)
	return func(data []byte) bool {
		if len(data) > 1024 {
			data = data[:1024]
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) > 5 {
			lines = lines[:5]
		}
		head := strings.Join(lines, "\n")
		return strings.Contains(head, "@generated") || generatedWord.MatchString(head) && doNotEdit.MatchString(head)
	}
}

// fileWithoutRecord is the predicate of ST-ASR-SP09 (ST-STD-CODE-R05): a source file under internal, cmd, scripts
// or registry is named by no technical record, directly or through a folder a record names. A source file is one
// Lathe's code-file-untraced reads: a file with a source extension that is not a test (the test globs of
// lathe.config.json, or a folder named test, tests, __tests__, __mocks__ or fixtures), not a tool's config, and not
// generated.
func fileWithoutRecord(spec Spec, tree Tree) []CodeFault {
	named := make(map[string]bool)
	for _, record := range spec.Technical {
		named[record.File] = true
	}
	extensions := sourceExtensions()
	globs := testGlobs(tree)
	generated := generatedHead()
	// The name of a tool's config file or a type declaration file, which is not a source file.
	configName := regexp.MustCompile(`(^|\.)config\.[cm]?[jt]sx?$|^\.[^/]*rc(\.[cm]?[jt]s)?$|\.d\.[cm]?ts$`)
	var faults []CodeFault
	for _, name := range productFiles(tree) {
		top, _, _ := strings.Cut(name, "/")
		if top != "internal" && top != "cmd" && top != "scripts" && top != "registry" || !extensions[path.Ext(name)] {
			continue
		}
		if !sourceFile(name, globs, configName) {
			continue
		}
		if data, _ := tree.File(name); generated(data) {
			continue
		}
		traced := named["."] || named[""]
		for at := name; at != "." && !traced; at = path.Dir(at) {
			traced = named[at]
		}
		if !traced {
			faults = append(faults, CodeFault{Rule: RuleNoFileWithoutRecord, Path: name, Reason: "no technical record names the file, directly or through a folder"})
		}
	}
	sortFaults(faults)
	return faults
}

// sourceFile tells if the file is not a test, a test's helper or a tool's config.
func sourceFile(name string, testGlobs []string, configName *regexp.Regexp) bool {
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		switch part {
		case "test", "tests", "__tests__", "__mocks__", "fixtures":
			return false
		}
	}
	if configName.MatchString(parts[len(parts)-1]) {
		return false
	}
	for _, glob := range testGlobs {
		if matchesGlob(glob, name) {
			return false
		}
	}
	return true
}

// stubBuilt is the predicate of ST-ASR-SP10 (ST-STD-CODE-R06): a function that a predicate, handler or service
// record names exists, and its body only returns constants or only panics, so that it reads no input. The record's
// title names the function, as pkg.Func or pkg.Type.Method, in the record's file. A body is a stub when it is
// empty, or when each of its statements is a return of constants or a panic, and none of them reads a parameter or
// the receiver. A constant is a literal, a name that is not a parameter or the receiver, a composite literal of
// constants, an operator on constants, or an error that errors.New or fmt.Errorf makes of constants; any other call
// reads something, so it is not a constant.
func stubBuilt(spec Spec, tree Tree) []CodeFault {
	kinds := codeKinds()
	files := newGoFiles(tree)
	var faults []CodeFault
	for _, record := range spec.Technical {
		if !kinds[record.Kind] || !strings.HasSuffix(record.File, ".go") {
			continue
		}
		if _, held := tree.File(record.File); !held {
			continue
		}
		file, err := files.parse(record.File)
		if err != nil {
			continue
		}
		pkg, names, ok := qualifiedName(record.Title)
		if !ok || pkg != file.Name.Name {
			continue
		}
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil || !namesFunction(function, names) {
				continue
			}
			if stub(function) {
				faults = append(faults, CodeFault{
					Rule:   RuleStubIsNotBuilt,
					Path:   record.File,
					Reason: fmt.Sprintf("%s names %s, whose body only returns constants or panics, so it reads no input", record.ID, record.Title),
				})
			}
		}
	}
	sortFaults(faults)
	return faults
}

// namesFunction tells if the names, Func or Type.Method, name the function.
func namesFunction(function *ast.FuncDecl, names []string) bool {
	receiver := receiverName(function)
	if len(names) == 1 {
		return receiver == "" && function.Name.Name == names[0]
	}
	return receiver == names[0] && function.Name.Name == names[1]
}

// stub tells if the function's body only returns constants or only panics, and reads no parameter or receiver.
func stub(function *ast.FuncDecl) bool {
	inputs := make(map[string]bool)
	for _, list := range []*ast.FieldList{function.Recv, function.Type.Params} {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			for _, name := range field.Names {
				if name.Name != "_" {
					inputs[name.Name] = true
				}
			}
		}
	}
	for _, statement := range function.Body.List {
		switch s := statement.(type) {
		case *ast.ReturnStmt:
			for _, result := range s.Results {
				if !constant(result, inputs) {
					return false
				}
			}
		case *ast.ExprStmt:
			call, ok := s.X.(*ast.CallExpr)
			if !ok || !isIdent(call.Fun, "panic") || readsInput(call, inputs) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isIdent(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == name
}

// readsInput tells if the expression names a parameter or the receiver.
func readsInput(node ast.Node, inputs map[string]bool) bool {
	reads := false
	ast.Inspect(node, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.SelectorExpr:
			ast.Inspect(e.X, func(inner ast.Node) bool {
				if ident, ok := inner.(*ast.Ident); ok && inputs[ident.Name] {
					reads = true
				}
				return !reads
			})
			return false
		case *ast.Ident:
			if inputs[e.Name] {
				reads = true
			}
		}
		return !reads
	})
	return reads
}

// constant tells if the expression is a constant that reads no input: see stubBuilt.
func constant(expr ast.Expr, inputs map[string]bool) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		return !inputs[e.Name]
	case *ast.SelectorExpr:
		return !readsInput(e, inputs)
	case *ast.ParenExpr:
		return constant(e.X, inputs)
	case *ast.UnaryExpr:
		return constant(e.X, inputs)
	case *ast.BinaryExpr:
		return constant(e.X, inputs) && constant(e.Y, inputs)
	case *ast.KeyValueExpr:
		return constant(e.Value, inputs)
	case *ast.CompositeLit:
		for _, element := range e.Elts {
			if !constant(element, inputs) {
				return false
			}
		}
		return true
	case *ast.CallExpr:
		selector, ok := e.Fun.(*ast.SelectorExpr)
		if !ok || !(isIdent(selector.X, "errors") && selector.Sel.Name == "New" || isIdent(selector.X, "fmt") && selector.Sel.Name == "Errorf") {
			return false
		}
		for _, arg := range e.Args {
			if !constant(arg, inputs) {
				return false
			}
		}
		return true
	}
	return false
}
