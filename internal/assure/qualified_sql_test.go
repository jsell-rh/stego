package assure

import (
	"fmt"
	"regexp"
	"slices"
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

// unqualifiedSQL gives each place of a file of tree, in the runtime library or in the generated code of an example,
// where a statement names a table or a function of a schema manifest with no schema: a table after FROM, INTO,
// UPDATE or JOIN, or a function after SELECT, FROM or PERFORM. A comment line, and a file of a test, are not read.
func unqualifiedSQL(tree Tree) []string {
	var files []string
	tables := slices.Clone(runtimeTables)
	for _, path := range tree.paths() {
		runtime := strings.HasPrefix(path, "registry/runtime/")
		generated := strings.HasPrefix(path, "examples/") && strings.Contains(path, "/out/")
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || (!runtime && !generated) {
			continue
		}
		files = append(files, path)
		if strings.HasSuffix(path, "/schema_manifest.go") {
			data, _ := tree.File(path)
			for _, m := range declaredTable.FindAllStringSubmatch(string(data), -1) {
				if !slices.Contains(tables, m[1]) {
					tables = append(tables, m[1])
				}
			}
		}
	}
	slices.Sort(tables)
	table := regexp.MustCompile(`(?i)\b(FROM|INTO|UPDATE|JOIN)\s+(?:ONLY\s+)?\\?"?(` + strings.Join(tables, "|") + `)\\?"?(?:[^a-z0-9_.]|$)`)
	function := regexp.MustCompile(`(?i)\b(SELECT|FROM|PERFORM)\s+(?:\*\s+FROM\s+)?(` + strings.Join(runtimeFunctions, "|") + `)\(`)
	var faults []string
	for _, path := range files {
		data, _ := tree.File(path)
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, m := range table.FindAllStringSubmatch(line, -1) {
				faults = append(faults, fmt.Sprintf("%s:%d: the table %s with no schema", path, i+1, m[2]))
			}
			for _, m := range function.FindAllStringSubmatch(line, -1) {
				faults = append(faults, fmt.Sprintf("%s:%d: the function %s with no schema", path, i+1, m[2]))
			}
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
		// The predicate finds each form that it refuses.
		probe := Tree{files: map[string][]byte{
			"registry/runtime/x/x.go": []byte("const a = `SELECT * FROM leases WHERE name = $1`\nconst b = `SELECT idempotency_keys_purge($1)`\n" +
				"const c = `UPDATE public.leases SET x = 1`\n// FROM leases in a comment\nconst d = `INSERT INTO \"orders\" (id) VALUES ($1)`\n"),
			"examples/e/out/internal/storage/schema_manifest.go": []byte(`{Schema: "public", Name: "orders",`),
		}}
		got := unqualifiedSQL(probe)
		want := []string{"registry/runtime/x/x.go:1: the table leases with no schema", "registry/runtime/x/x.go:2: the function idempotency_keys_purge with no schema",
			"registry/runtime/x/x.go:5: the table orders with no schema"}
		if !slices.Equal(got, want) {
			t.Fatalf("the predicate gave %q, want %q", got, want)
		}
	})
}
