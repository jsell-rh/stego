package assure

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// latheSpec is a small Lathe spec in two scopes: the domain FX-LEND with three invariants, and a constraint of
// the product's own value object. FX-LEND-INV01 and FX-LEND-INV02 have a rule in internal/lend/rules.go and an
// automated test, FX-LEND-INV03 has neither, and the constraint's value type names internal/lend/due.go.
var latheSpec = map[string]string{
	"lathe.config.json":    `{"schema": 5, "data": "data", "tests": ["**/*_test.go"]}` + "\n",
	"data/product.jsonl":   `{"id":"FX","title":"Fixture","description":"What Fixture is, who it is for, and what it must do."}` + "\n",
	"data/domain.jsonl":    `{"id":"FX-LEND","title":"Lending","product_id":"FX","subdomain_type":"core","description":"Lending tools."}` + "\n",
	"data/aggregate.jsonl": `{"id":"FX-LEND-A1","title":"Loan","domain_id":"FX-LEND","description":"A loan.","identity":"Loan ID"}` + "\n",
	"data/invariant.jsonl": `{"id":"FX-LEND-INV01","title":"One loan per tool","aggregate_id":"FX-LEND-A1","statement":"A tool MUST have at most one open loan."}
{"id":"FX-LEND-INV02","title":"Due after start","aggregate_id":"FX-LEND-A1","statement":"A loan MUST end after it starts."}
{"id":"FX-LEND-INV03","title":"Borrower known","aggregate_id":"FX-LEND-A1","statement":"A loan MUST name a known borrower."}
`,
	"data/rule.jsonl": `{"id":"FX-LEND-INV01-RL","title":"lend.oneLoanPerTool","invariant_id":"FX-LEND-INV01","file":"internal/lend/rules.go"}
{"id":"FX-LEND-INV02-RL","title":"lend.dueAfterStart","invariant_id":"FX-LEND-INV02","file":"internal/lend/rules.go"}
`,
	"data/invariant_test.jsonl": `{"id":"FX-LEND-INV01-T","title":"One loan","invariant_id":"FX-LEND-INV01","level":"unit","state":"automated","file":"internal/lend/rules_test.go","when":"Lends twice.","then":"Refused."}
{"id":"FX-LEND-INV02-T","title":"Due after start","invariant_id":"FX-LEND-INV02","level":"unit","state":"automated","file":"internal/lend/rules_test.go","when":"Ends before it starts.","then":"Refused."}
`,
	"data/entity.jsonl":             `{"id":"FX-LEND-EN01","title":"Loan","aggregate_id":"FX-LEND-A1","role":"root","description":"A loan of a tool."}` + "\n",
	"data/entity_field.jsonl":       `{"entity_id":"FX-LEND-EN01","position":1,"name":"Due","meaning":"When it ends.","type_value_object_id":"FX-VO01","required":true}` + "\n",
	"data/value_object.jsonl":       `{"id":"FX-VO01","title":"Due date","description":"The day a loan ends."}` + "\n",
	"data/value_object_field.jsonl": `{"value_object_id":"FX-VO01","position":1,"name":"Day","meaning":"The day.","type":"date","required":true}` + "\n",
	"data/constraint.jsonl":         `{"id":"FX-VO01-C01","title":"In the future","value_object_id":"FX-VO01","statement":"A due date MUST be after today."}` + "\n",
	"data/value_type.jsonl":         `{"id":"FX-VO01-VT","title":"lend.DueDate","value_object_id":"FX-VO01","file":"internal/lend/due.go"}` + "\n",
	"internal/lend/rules.go":        "package lend\n\nfunc oneLoanPerTool(open int) bool { return open < 2 }\n",
	"internal/lend/rules_test.go": "package lend\n\nimport \"testing\"\n\nfunc TestRules(t *testing.T) {\n" +
		"\tt.Run(\"FX-LEND-INV01-T one loan\", func(t *testing.T) {})\n\tt.Run(\"FX-LEND-INV02-T due after start\", func(t *testing.T) {})\n}\n",
}

// latheCLI is the Lathe CLI the tests run: STEGO_LATHE, or node_modules/.bin/lathe of the repository the tests
// run in. Without it the test is skipped, unless STEGO_REQUIRE_LATHE is 1, as it is in the proof run.
func latheCLI(t *testing.T) string {
	t.Helper()
	lathe := os.Getenv("STEGO_LATHE")
	if lathe == "" {
		lathe = filepath.Join("..", "..", "node_modules", ".bin", "lathe")
	}
	if _, err := os.Stat(lathe); err != nil {
		if os.Getenv("STEGO_REQUIRE_LATHE") == "1" {
			t.Fatalf("the Lathe CLI %s is required: %v", lathe, err)
		}
		t.Skipf("no Lathe CLI at %s", lathe)
	}
	absolute, err := filepath.Abs(lathe)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

// commitFiles writes the files into the repository, removes each file whose content is empty, and commits all.
// It returns the commit.
func commitFiles(t *testing.T, root string, files map[string]string, message string) string {
	t.Helper()
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if content == "" {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", message)
	return strings.TrimSpace(gitIn(t, root, "rev-parse", "HEAD"))
}

// newRepository makes an empty git repository.
func newRepository(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "init", "-q")
	return root
}

func TestCoverage(t *testing.T) {
	t.Run("ST-VO55-C01-T Each count is between zero and the total, a share is the count over the total, and an empty spec is 100%", func(t *testing.T) {
		for _, counts := range [][4]int{{4, 4, 2, 0}, {4, 0, 0, 0}, {4, 4, 4, 4}, {0, 0, 0, 0}, {412, 412, 301, 96}} {
			coverage, err := NewCoverage(counts[0], counts[1], counts[2], counts[3])
			if err != nil {
				t.Fatalf("%v is refused: %v", counts, err)
			}
			for i, share := range allShares() {
				if coverage.Count(share) != counts[i+1] {
					t.Fatalf("%v: %s count is %d", counts, share, coverage.Count(share))
				}
				want := 1.0
				if counts[0] > 0 {
					want = float64(counts[i+1]) / float64(counts[0])
				}
				if coverage.Ratio(share) != want {
					t.Fatalf("%v: the %s share is %v, want %v", counts, share, coverage.Ratio(share), want)
				}
			}
		}
		empty, _ := NewCoverage(0, 0, 0, 0)
		if got := empty.String(); got != "0 obligations: 0 designed (100.0%), 0 built (100.0%), 0 proved (100.0%)" {
			t.Fatalf("an empty spec reads %q", got)
		}
		some, _ := NewCoverage(412, 412, 301, 96)
		if got := some.String(); got != "412 obligations: 412 designed (100.0%), 301 built (73.1%), 96 proved (23.3%)" {
			t.Fatalf("the coverage reads %q", got)
		}
		for _, counts := range [][4]int{{4, 5, 0, 0}, {4, 4, 5, 0}, {4, 4, 4, 5}, {4, -1, 0, 0}, {4, 0, -1, 0}, {4, 0, 0, -1}, {-1, 0, 0, 0}, {0, 1, 0, 0}} {
			if coverage, err := NewCoverage(counts[0], counts[1], counts[2], counts[3]); err == nil {
				t.Fatalf("%v is accepted as %+v", counts, coverage)
			}
		}
	})

	t.Run("ST-ASR-DS02-T01 The coverage at a commit is read from lathe check on that commit's tree alone, summed over every domain and kind, and two readings agree", func(t *testing.T) {
		lathe := latheCLI(t)
		root := newRepository(t)
		first := commitFiles(t, root, latheSpec, "spec")
		second := commitFiles(t, root, map[string]string{
			"internal/lend/rules.go": "",
			"internal/lend/due.go":   "package lend\n\ntype DueDate struct{ Day string }\n",
		}, "the rules go, the due date comes")
		// The working tree differs from both commits: a test record is added and the due date is gone again.
		writeFileIn(t, root, "data/invariant_test.jsonl", latheSpec["data/invariant_test.jsonl"]+
			`{"id":"FX-LEND-INV03-T","title":"Borrower","invariant_id":"FX-LEND-INV03","level":"unit","state":"automated","file":"internal/lend/rules_test.go","when":"Lends to a stranger.","then":"Refused."}`+"\n")
		if err := os.Remove(filepath.Join(root, "internal", "lend", "due.go")); err != nil {
			t.Fatal(err)
		}

		measured, err := MeasureCoverage(root, first, lathe)
		if err != nil {
			t.Fatal(err)
		}
		want := Measurement{
			Commit:   first,
			Coverage: Coverage{Total: 4, Designed: 4, Built: 2, Proved: 2},
			Obligations: map[string]Standing{
				"FX-LEND-INV01": {Designed: true, Built: true, Proved: true},
				"FX-LEND-INV02": {Designed: true, Built: true, Proved: true},
				"FX-LEND-INV03": {Designed: true},
				"FX-VO01-C01":   {Designed: true},
			},
		}
		if !reflect.DeepEqual(measured, want) {
			t.Fatalf("the coverage at the first commit is %+v, want %+v", measured, want)
		}
		again, err := MeasureCoverage(root, first, lathe)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(measured)
		b, _ := json.Marshal(again)
		if string(a) != string(b) {
			t.Fatalf("two readings of one commit differ:\n%s\n%s", a, b)
		}

		later, err := MeasureCoverage(root, second, lathe)
		if err != nil {
			t.Fatal(err)
		}
		want = Measurement{
			Commit:   second,
			Coverage: Coverage{Total: 4, Designed: 4, Built: 1, Proved: 2},
			Obligations: map[string]Standing{
				"FX-LEND-INV01": {Designed: true, Proved: true},
				"FX-LEND-INV02": {Designed: true, Proved: true},
				"FX-LEND-INV03": {Designed: true},
				"FX-VO01-C01":   {Designed: true, Built: true},
			},
		}
		if !reflect.DeepEqual(later, want) {
			t.Fatalf("the coverage at the second commit is %+v, want %+v", later, want)
		}
		if _, err := MeasureCoverage(root, first[:12], lathe); err == nil {
			t.Fatal("a short commit ID is measured")
		}

		// At a commit whose spec has an error, Lathe gives no coverage, and nothing is measured.
		broken := commitFiles(t, root, map[string]string{"internal/lend/rules_test.go": ""}, "the tests' file goes")
		measured, err = MeasureCoverage(root, broken, lathe)
		var specErr *SpecError
		if !errors.As(err, &specErr) || specErr.Commit != broken || !reflect.DeepEqual(measured, Measurement{}) {
			t.Fatalf("a spec with an error gives %+v, %v; want no measurement and a SpecError", measured, err)
		}
		for _, id := range []string{"FX-LEND-INV01-T", "FX-LEND-INV02-T"} {
			if !strings.Contains(err.Error(), "[test-file-missing] "+id+": ") {
				t.Fatalf("the error does not name the error of %s:\n%v", id, err)
			}
		}
	})

	t.Run("ST-ASR-DS02-T03 The tree of a commit is written only inside its folder, and a link that leads out of it is refused", func(t *testing.T) {
		root := newRepository(t)
		outside := t.TempDir()
		blob := func(content string) string {
			command := exec.Command("git", "-C", root, "hash-object", "-w", "--stdin")
			command.Stdin = strings.NewReader(content)
			out, err := command.Output()
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(string(out))
		}
		tree := func(entries string) string {
			command := exec.Command("git", "-C", root, "mktree")
			command.Stdin = strings.NewReader(entries)
			out, err := command.Output()
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(string(out))
		}
		commitOf := func(tree string) string {
			return strings.TrimSpace(gitIn(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "commit-tree", tree, "-m", "tree"))
		}
		readme := blob("Fixture.\n")
		pwned := tree("100644 blob " + blob("pwned\n") + "\tpwned\n")
		for name, entries := range map[string]string{
			"a link and a folder of one name":    "120000 blob " + blob(outside) + "\ta\n040000 tree " + pwned + "\ta\n",
			"a link to an absolute path":         "120000 blob " + blob("/etc") + "\tlink\n",
			"a link out of the tree by its text": "040000 tree " + tree("120000 blob "+blob("../../outside")+"\tlink\n") + "\tsub\n",
			"a link out of the tree through a link": "040000 tree " + tree("120000 blob "+blob("..")+"\ty\n") + "\td\n" +
				"120000 blob " + blob("d/y/..") + "\tx\n",
		} {
			folder := t.TempDir()
			if err := writeTree(root, commitOf(tree(entries)), folder); err == nil {
				t.Fatalf("%s is written", name)
			}
			if leftovers, _ := os.ReadDir(outside); len(leftovers) != 0 {
				t.Fatalf("%s writes outside the folder: %v", name, leftovers)
			}
		}
		folder := t.TempDir()
		good := tree("100644 blob " + readme + "\tREADME.md\n040000 tree " + tree("120000 blob "+blob("../README.md")+"\treadme\n") + "\tdocs\n")
		if err := writeTree(root, commitOf(good), folder); err != nil {
			t.Fatalf("a link inside the tree is refused: %v", err)
		}
		if target, err := os.Readlink(filepath.Join(folder, "docs", "readme")); err != nil || target != "../README.md" {
			t.Fatalf("the link inside the tree is %q, %v", target, err)
		}
	})

	t.Run("ST-ASR-DS02-T02 The coverage sums Lathe's counts over every domain and kind, and output whose gaps disagree with its counts is refused", func(t *testing.T) {
		check := func(coverage, chains string) []byte {
			return []byte(`{"products": [{"root": ".", "workspace": {"coverage": [` + coverage + `], "chains": [` + chains + `]}}]}`)
		}
		lend := `{"domain": "FX-LEND", "obligations": [
			{"kind": "invariant", "total": 2, "designed": 2, "built": 1, "proved": 0},
			{"kind": "policy", "total": 1, "designed": 0, "built": 0, "proved": 1}],
			"gaps": [{"id": "FX-LEND-INV02", "missing": "built"}, {"id": "FX-LEND-INV01", "missing": "proved"},
				{"id": "FX-LEND-INV02", "missing": "proved"}, {"id": "FX-LEND-PO01", "missing": "designed"},
				{"id": "FX-LEND-A1", "missing": "aggregate-without-root"}]}`
		shop := `{"domain": "FX-SHOP", "obligations": [{"kind": "invariant", "total": 1, "designed": 1, "built": 1, "proved": 1}], "gaps": []}`
		shared := `{"domain": null, "obligations": [{"kind": "standard_rule", "total": 1, "designed": 1, "built": 0, "proved": 0}],
			"gaps": [{"id": "FX-STD-R01", "missing": "built"}, {"id": "FX-STD-R01", "missing": "proved"}, {"id": "internal/x.go", "missing": "code-file-untraced"}]}`
		chains := `{"obligation": "FX-LEND-INV01"}, {"obligation": "FX-LEND-INV02"}, {"obligation": "FX-LEND-PO01"}, {"obligation": "FX-SHOP-INV01"}, {"obligation": "FX-STD-R01"}`
		measured, err := readLatheCheck(check(lend+","+shop+","+shared, chains))
		if err != nil {
			t.Fatal(err)
		}
		want := Measurement{
			Coverage: Coverage{Total: 5, Designed: 4, Built: 2, Proved: 2},
			Obligations: map[string]Standing{
				"FX-LEND-INV01": {Designed: true, Built: true},
				"FX-LEND-INV02": {Designed: true},
				"FX-LEND-PO01":  {Proved: true},
				"FX-SHOP-INV01": {Designed: true, Built: true, Proved: true},
				"FX-STD-R01":    {Designed: true},
			},
		}
		if !reflect.DeepEqual(measured, want) {
			t.Fatalf("got %+v, want %+v", measured, want)
		}
		for name, output := range map[string][]byte{
			"a gap that Lathe's counts do not hold": check(strings.Replace(lend, `"built": 1`, `"built": 2`, 1)+","+shop+","+shared, chains),
			"a gap in another word":                 check(strings.ReplaceAll(lend, `"missing": "proved"`, `"missing": "unproved"`)+","+shop+","+shared, chains),
			"an obligation with no chain":           check(lend+","+shop+","+shared, `{"obligation": "FX-LEND-INV01"}, {"obligation": "FX-LEND-PO01"}, {"obligation": "FX-SHOP-INV01"}, {"obligation": "FX-STD-R01"}`),
			"an obligation with two chains":         check(lend+","+shop+","+shared, chains+`, {"obligation": "FX-STD-R01"}`),
			"a count above the total":               check(strings.Replace(shop, `"proved": 1`, `"proved": 2`, 1), `{"obligation": "FX-SHOP-INV01"}`),
			"no product":                            []byte(`{"products": []}`),
			"text that is not JSON":                 []byte("Checked 14 records\n"),
		} {
			if got, err := readLatheCheck(output); err == nil {
				t.Fatalf("%s is read as %+v", name, got)
			}
		}
	})
}

// writeFileIn writes the file in the repository's working tree, and commits nothing.
func writeFileIn(t *testing.T, root, name, content string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
