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

// unknown stands for a part of a string expression that is no constant of the file's package.
const unknown = "\x00"

// sqlTexts gives the texts of a Go file that a statement can be made of: each constant string, and each string
// expression that is not part of a larger one, with each + of constants of the package folded and each other part
// as unknown. consts holds the constant expressions of the package by name.
func sqlTexts(file *ast.File, consts map[string]ast.Expr) []string {
	var eval func(e ast.Expr, depth int) (string, bool)
	eval = func(e ast.Expr, depth int) (string, bool) {
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
			return eval(e.X, depth+1)
		case *ast.Ident:
			if c, ok := consts[e.Name]; ok {
				return eval(c, depth+1)
			}
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				x, xok := eval(e.X, depth+1)
				y, yok := eval(e.Y, depth+1)
				return x + y, xok || yok
			}
		}
		return unknown, false
	}
	var texts []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if s, ok := eval(n, 0); ok && n.Op == token.ADD {
				texts = append(texts, s)
				return false
			}
		case *ast.BasicLit:
			if s, ok := eval(n, 0); ok {
				texts = append(texts, s)
			}
		}
		return true
	})
	return texts
}

// unqualifiedSQL gives each statement text of a file of tree, in the runtime library or in the generated code of an
// example, that names a table or a function of a schema manifest with no schema. It reads the Go files that are not
// tests, with their constants folded, and the .sql and .tmpl files whole, and matches across lines.
func unqualifiedSQL(tree Tree) []string {
	tables := slices.Clone(runtimeTables)
	var files []string
	for _, p := range tree.paths() {
		runtime := strings.HasPrefix(p, "registry/runtime/")
		generated := strings.HasPrefix(p, "examples/") && strings.Contains(p, "/out/")
		ext := path.Ext(p)
		if (!runtime && !generated) || strings.HasSuffix(p, "_test.go") || !slices.Contains([]string{".go", ".sql", ".tmpl"}, ext) ||
			slices.Contains(ownerDDL, p) {
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
	slices.Sort(tables)
	name := `\\?"?(` + strings.Join(tables, "|") + `)\\?"?`
	end := `(?:[^a-z0-9_.(]|$)`
	forms := []*regexp.Regexp{
		// A table after a keyword that names one.
		regexp.MustCompile(`(?is)\b(?:FROM|INTO|UPDATE|JOIN|TRUNCATE(?:\s+TABLE)?|LOCK(?:\s+TABLE)?|COPY|TABLE|USING)\s+(?:ONLY\s+)?` + name + end),
		// A table after a comma of a FROM list.
		regexp.MustCompile(`(?is)\bFROM\s+[a-z0-9_."]+(?:\s+(?:AS\s+)?[a-z_]+)?(?:\s*,\s*[a-z0-9_."]+(?:\s+(?:AS\s+)?[a-z_]+)?)*\s*,\s*` + name + end),
		// A table in a regclass cast.
		regexp.MustCompile(`(?i)'` + name + `'::(?:pg_catalog\.)?regclass`),
	}
	function := regexp.MustCompile(`(?is)(?:\b(?:SELECT|FROM|PERFORM|CALL)\s+(?:\*\s+FROM\s+)?|[,(]\s*)(` + strings.Join(runtimeFunctions, "|") + `)\s*\(`)
	var faults []string
	for _, p := range files {
		data, _ := tree.File(p)
		texts := []string{string(data)}
		if path.Ext(p) == ".go" {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, p, data, parser.SkipObjectResolution)
			if err != nil {
				faults = append(faults, fmt.Sprintf("%s: the file does not parse: %v", p, err))
				continue
			}
			consts := map[string]ast.Expr{}
			// The constants of the package: the files of the same folder.
			for _, q := range files {
				if path.Dir(q) != path.Dir(p) || path.Ext(q) != ".go" {
					continue
				}
				other, _ := tree.File(q)
				f, err := parser.ParseFile(token.NewFileSet(), q, other, parser.SkipObjectResolution)
				if err != nil {
					continue
				}
				for _, decl := range f.Decls {
					if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.CONST {
						for _, spec := range g.Specs {
							v := spec.(*ast.ValueSpec)
							for i, n := range v.Names {
								if i < len(v.Values) {
									consts[n.Name] = v.Values[i]
								}
							}
						}
					}
				}
			}
			texts = sqlTexts(file, consts)
		}
		seen := map[string]bool{}
		for _, text := range texts {
			for _, form := range forms {
				for _, m := range form.FindAllStringSubmatch(text, -1) {
					seen["the table "+m[1]] = true
				}
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
			"qualified": "const a = `SELECT public.leases.x FROM public.leases, stego_outbox.messages m WHERE stego_outbox.claim($1) AND 'public.leases'::regclass IS NOT NULL`\n// FROM leases in a comment is no statement\n",
		}
		files := map[string][]byte{"examples/e/out/internal/storage/schema_manifest.go": []byte("package storage\n\nvar m = `{Schema: \"public\", Name: \"orders\",`\n")}
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
			"after": "function claim", "paren": "function oldest", "call": "function resolve"} {
			want = append(want, "registry/runtime/"+name+"/"+name+".go: the "+what+" with no schema")
		}
		want = append(want, "examples/e/out/internal/tenancy/migrations/x.sql: the table leases with no schema")
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("the predicate gave:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}
