package assure

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// runtimeTables are the tables of the schema manifests of the runtime library (ST-SUP-RP15-LC01), and
// runtimeFunctions its functions that a statement calls.
var (
	runtimeTables = []string{"messages", "audit_event", "audit_chain_heads", "audit_anchor_boundaries", "audit_retention_checkpoints",
		"idempotency_keys", "authz_floor", "authz_revocations", "authz_revocation_tuples", "leases", "stego_tenants"}
	runtimeFunctions = []string{"claim", "move", "release", "purge_parked", "purge_done", "blocked", "oldest", "resolve",
		"audit_maintain", "idempotency_keys_purge", "authz_revocation_state"}
)

// declaredTable is a table of the schema manifest that a generated service declares.
var declaredTable = regexp.MustCompile(`\{Schema: "[a-z_][a-z0-9_]*", Name: "([a-z_][a-z0-9_]*)"`)

// ownerDDL are the files whose statements only the owner role runs, in the migration job, and which make the
// objects in the schema of the migration (ST-STD-TXN-R19 names the statements of the runtime, worker and relay
// roles).
var ownerDDL = []string{"registry/runtime/idempotency/migration.go"}

// unknown stands for a part of a string expression that is no constant the scanner knows, and argument for an
// argument of a format that it does not know.
const (
	unknown  = "\x00"
	argument = "\x01"
)

// goPackages holds the string constants and package-level string variables of each folder of Go files that the
// scanner reads, and the folder of each module by its path, so a selector such as audit.EventsTable is folded too.
type goPackages struct {
	values  map[string]map[string]ast.Expr // folder -> name -> expression
	modules map[string]string              // module path -> folder
}

// index reads the constants and string variables of each Go file of paths, and the module of each go.mod of tree.
func index(tree Tree, paths []string) goPackages {
	g := goPackages{values: map[string]map[string]ast.Expr{}, modules: map[string]string{}}
	module := regexp.MustCompile(`(?m)^module\s+(\S+)`)
	for _, p := range tree.paths() {
		if path.Base(p) == "go.mod" {
			data, _ := tree.File(p)
			if m := module.FindSubmatch(data); m != nil {
				g.modules[string(m[1])] = path.Dir(p)
			}
		}
	}
	for _, p := range paths {
		if path.Ext(p) != ".go" {
			continue
		}
		data, _ := tree.File(p)
		f, err := parser.ParseFile(token.NewFileSet(), p, data, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		dir := path.Dir(p)
		if g.values[dir] == nil {
			g.values[dir] = map[string]ast.Expr{}
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				v := spec.(*ast.ValueSpec)
				for i, n := range v.Names {
					if i < len(v.Values) {
						g.values[dir][n.Name] = v.Values[i]
					}
				}
			}
		}
	}
	return g
}

// folder gives the folder of the package of an import path, or "" when no module of the tree holds it.
func (g goPackages) folder(importPath string) string {
	for module, dir := range g.modules {
		if importPath == module {
			return dir
		}
		if rest, ok := strings.CutPrefix(importPath, module+"/"); ok {
			return path.Join(dir, rest)
		}
	}
	return ""
}

// sqlTexts gives the texts of the Go file p that a statement can be made of: each string expression that is not
// part of a larger one, with each + folded over the constants and string variables of its package and of the
// packages it imports, and each fmt.Sprintf with its known arguments in place of their verbs and argument for each
// other one; each other part is unknown.
func sqlTexts(file *ast.File, p string, g goPackages) []string {
	imports := map[string]string{}
	for _, spec := range file.Imports {
		importPath, _ := strconv.Unquote(spec.Path.Value)
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = g.folder(importPath)
	}
	verb := regexp.MustCompile(`%[-+# 0]*[0-9]*(?:\.[0-9]+)?[a-zA-Z%]`)
	var eval func(e ast.Expr, dir string, depth int) (string, bool)
	eval = func(e ast.Expr, dir string, depth int) (string, bool) {
		if depth > 32 {
			return unknown, false
		}
		switch e := e.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				s, err := strconv.Unquote(e.Value)
				return s, err == nil
			}
		case *ast.ParenExpr:
			return eval(e.X, dir, depth+1)
		case *ast.Ident:
			if c, ok := g.values[dir][e.Name]; ok {
				return eval(c, dir, depth+1)
			}
		case *ast.SelectorExpr:
			if x, ok := e.X.(*ast.Ident); ok && dir == path.Dir(p) {
				if other := imports[x.Name]; other != "" {
					if c, ok := g.values[other][e.Sel.Name]; ok {
						return eval(c, other, depth+1)
					}
				}
			}
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				x, xok := eval(e.X, dir, depth+1)
				y, yok := eval(e.Y, dir, depth+1)
				return x + y, xok || yok
			}
		case *ast.CallExpr:
			if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sprintf" && len(e.Args) > 0 {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" {
					format, fok := eval(e.Args[0], dir, depth+1)
					if !fok {
						return unknown, false
					}
					args := e.Args[1:]
					i := 0
					return verb.ReplaceAllStringFunc(format, func(v string) string {
						if v == "%%" {
							return "%"
						}
						if i >= len(args) {
							return argument
						}
						a, ok := eval(args[i], dir, depth+1)
						i++
						if !ok || strings.Contains(a, unknown) {
							return argument
						}
						return a
					}), true
				}
			}
		}
		return unknown, false
	}
	dir := path.Dir(p)
	var texts []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr, *ast.CallExpr:
			if s, ok := eval(n.(ast.Expr), dir, 0); ok {
				texts = append(texts, s)
				if _, call := n.(*ast.CallExpr); !call {
					return false
				}
			}
		case *ast.BasicLit:
			if s, ok := eval(n, dir, 0); ok {
				texts = append(texts, s)
			}
		}
		return true
	})
	return texts
}

// unqualifiedSQL gives each statement text of a file of tree, in the runtime library, in the generated code of an
// example, or in a template of a generator, that names a table or a function of a schema manifest with no schema,
// or a table that a format argument names. It reads the Go files that are not tests, with their constants folded,
// and the .sql and .tmpl files whole, and matches across lines. The files of ownerDDL give their constants to the
// fold, and are not read for statements.
func unqualifiedSQL(tree Tree) []string {
	tables := slices.Clone(runtimeTables)
	var files, read []string
	for _, p := range tree.paths() {
		runtime := strings.HasPrefix(p, "registry/runtime/")
		generated := strings.HasPrefix(p, "examples/") && strings.Contains(p, "/out/")
		template := path.Ext(p) == ".tmpl" && (strings.HasPrefix(p, "internal/generator/") || strings.HasPrefix(p, "internal/compiler/"))
		ext := path.Ext(p)
		if (!runtime && !generated && !template) || strings.HasSuffix(p, "_test.go") || !slices.Contains([]string{".go", ".sql", ".tmpl"}, ext) {
			continue
		}
		read = append(read, p)
		if slices.Contains(ownerDDL, p) {
			continue
		}
		files = append(files, p)
		if strings.HasSuffix(p, "/schema_manifest.go") {
			data, _ := tree.File(p)
			for _, m := range declaredTable.FindAllStringSubmatch(string(data), -1) {
				if !slices.Contains(tables, m[1]) {
					tables = append(tables, m[1])
				}
			}
		}
	}
	g := index(tree, read)
	slices.Sort(tables)
	name := `\\?"?(` + strings.Join(tables, "|") + `)\\?"?`
	end := `(?:[^a-z0-9_.(]|$)`
	keyword := `\b(?:FROM|INTO|UPDATE|JOIN|TRUNCATE(?:\s+TABLE)?|LOCK(?:\s+TABLE)?|COPY|TABLE|USING)\s+(?:ONLY\s+)?`
	forms := []*regexp.Regexp{
		// A table after a keyword that names one.
		regexp.MustCompile(`(?is)` + keyword + name + end),
		// A table after a comma of a FROM list.
		regexp.MustCompile(`(?is)\bFROM\s+[a-z0-9_."]+(?:\s+(?:AS\s+)?[a-z_]+)?(?:\s*,\s*[a-z0-9_."]+(?:\s+(?:AS\s+)?[a-z_]+)?)*\s*,\s*` + name + end),
		// A table in a regclass cast.
		regexp.MustCompile(`(?i)'` + name + `'::(?:pg_catalog\.)?regclass`),
	}
	formatted := regexp.MustCompile(`(?is)` + keyword + `"?` + argument)
	function := regexp.MustCompile(`(?is)(?:\b(?:SELECT|FROM|PERFORM|CALL)\s+(?:\*\s+FROM\s+)?|[,(]\s*)(` + strings.Join(runtimeFunctions, "|") + `)\s*\(`)
	var faults []string
	for _, p := range files {
		data, _ := tree.File(p)
		texts := []string{string(data)}
		if path.Ext(p) == ".go" {
			file, err := parser.ParseFile(token.NewFileSet(), p, data, parser.SkipObjectResolution)
			if err != nil {
				faults = append(faults, fmt.Sprintf("%s: the file does not parse: %v", p, err))
				continue
			}
			texts = sqlTexts(file, p, g)
		}
		seen := map[string]bool{}
		for _, text := range texts {
			for _, form := range forms {
				for _, m := range form.FindAllStringSubmatch(text, -1) {
					seen["the table "+m[1]] = true
				}
			}
			if formatted.MatchString(text) {
				seen["the table that a format argument names"] = true
			}
			for _, m := range function.FindAllStringSubmatch(text, -1) {
				seen["the function "+m[1]] = true
			}
		}
		for _, what := range slices.Sorted(func(yield func(string) bool) {
			for k := range seen {
				if !yield(k) {
					return
				}
			}
		}) {
			faults = append(faults, fmt.Sprintf("%s: %s with no schema", p, what))
		}
	}
	return faults
}

func TestQualifiedSQL(t *testing.T) {
	t.Run("ST-STD-TXN-R19-T01 No statement of the runtime library or of a generated service names a table or a function of a schema manifest with no schema", func(t *testing.T) {
		root, err := RepositoryRoot(".")
		if err != nil {
			t.Fatal(err)
		}
		tree, err := ReadCommittedTree(root, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if faults := unqualifiedSQL(tree); len(faults) > 0 {
			t.Fatalf("%d statements name a table or a function with no schema:\n%s", len(faults), strings.Join(faults, "\n"))
		}
		// The predicate finds each form that it refuses, in a file of its own, and no qualified form.
		forms := map[string]string{
			"from":      "const a = `SELECT * FROM leases WHERE name = $1`",
			"folded":    "const t = \"leases\"\nconst a = `SELECT x\n FROM ` + t + ` WHERE y`",
			"into":      "const a = `INSERT INTO \"orders\" (id) VALUES ($1)`",
			"update":    "func f() { _ = \"UPDATE \" + \"messages SET x = 1\" }",
			"join":      "const a = `SELECT 1 FROM public.orders o JOIN leases l ON true`",
			"truncate":  "const a = `TRUNCATE leases`",
			"lock":      "const a = `LOCK TABLE leases IN SHARE MODE`",
			"copy":      "const a = `COPY leases TO STDOUT`",
			"table":     "const a = `TABLE leases`",
			"using":     "const a = `DELETE FROM public.orders USING leases WHERE true`",
			"comma":     "const a = `SELECT 1 FROM public.orders o, leases l WHERE true`",
			"regclass":  "const a = `SELECT 'leases'::regclass`",
			"selected":  "const a = `SELECT idempotency_keys_purge($1)`",
			"after":     "const a = `SELECT 1, claim($1)`",
			"paren":     "const a = `SELECT coalesce((oldest($1)), 0)`",
			"call":      "const a = `CALL resolve($1)`",
			"sql":       "",
			"selector":  "import \"github.com/jsell-rh/stego/registry/runtime/names\"\n\nconst a = `SELECT 1 FROM ` + names.Leases",
			"variable":  "var t = \"leases\"\n\nvar a = `DELETE FROM ` + t",
			"sprintf":   "import \"fmt\"\n\nfunc f() string { return fmt.Sprintf(\"SELECT 1 FROM %s WHERE %s\", \"leases\", \"x\") }",
			"argument":  "import \"fmt\"\n\nfunc f(t string) string { return fmt.Sprintf(\"UPDATE %s SET x = 1\", t) }",
			"owner":     "import \"github.com/jsell-rh/stego/registry/runtime/idempotency\"\n\nconst a = `SELECT 1 FROM ` + idempotency.Keys",
			"qualified": "import \"fmt\"\n\nfunc f(t string) string { return fmt.Sprintf(\"UPDATE public.%s SET x = 1\", t) }\n\nconst a = `SELECT public.leases.x FROM public.leases, stego_outbox.messages m WHERE stego_outbox.claim($1) AND 'public.leases'::regclass IS NOT NULL`\n// FROM leases in a comment is no statement\n",
		}
		files := map[string][]byte{"examples/e/out/internal/storage/schema_manifest.go": []byte("package storage\n\nvar m = `{Schema: \"public\", Name: \"orders\",`\n"),
			"registry/runtime/go.mod":                   []byte("module github.com/jsell-rh/stego/registry/runtime\n"),
			"registry/runtime/names/names.go":           []byte("package names\n\nconst Leases = \"leases\"\n"),
			"registry/runtime/idempotency/migration.go": []byte("package idempotency\n\nconst Keys = \"idempotency_keys\"\n\nconst ddl = `ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY`\n"),
			"internal/generator/g/query.go.tmpl":        []byte("const q = `SELECT 1 FROM leases WHERE {{.Key}}`\n"),
		}
		for name, body := range forms {
			if name == "sql" {
				files["examples/e/out/internal/tenancy/migrations/x.sql"] = []byte("LOCK TABLE leases;\n")
				continue
			}
			files["registry/runtime/"+name+"/"+name+".go"] = []byte("package " + name + "\n\n" + body + "\n")
		}
		got := unqualifiedSQL(NewTree(files))
		var want []string
		for name, what := range map[string]string{"from": "table leases", "folded": "table leases", "into": "table orders", "update": "table messages",
			"join": "table leases", "truncate": "table leases", "lock": "table leases", "copy": "table leases", "table": "table leases",
			"using": "table leases", "comma": "table leases", "regclass": "table leases", "selected": "function idempotency_keys_purge",
			"after": "function claim", "paren": "function oldest", "call": "function resolve", "selector": "table leases", "variable": "table leases",
			"sprintf": "table leases", "argument": "table that a format argument names", "owner": "table idempotency_keys"} {
			want = append(want, "registry/runtime/"+name+"/"+name+".go: the "+what+" with no schema")
		}
		want = append(want, "examples/e/out/internal/tenancy/migrations/x.sql: the table leases with no schema",
			"internal/generator/g/query.go.tmpl: the table leases with no schema")
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("the predicate gave:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}
