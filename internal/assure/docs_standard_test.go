package assure

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// updateDocs makes the own-tree test of ST-STD-DOCS-R03 write each generated section of each user doc again, in
// the working tree, from the working tree's spec, before it checks them.
var updateDocs = flag.Bool("update-docs", false, "write the generated sections of the user docs again from the working tree's spec")

// docFixtureTables is a spec with the doc inventory record, two stego commands that a handler runs, of which the
// tree holds only the first handler's file, and a third command that no handler runs.
var docFixtureTables = map[string]string{
	"tooling":     `{"id":"ST-ASR-TOOL05","title":"user-docs.txt","domain_id":"ST-ASR","file":"docs.txt"}` + "\n",
	"cli_command": `{"id":"ST-FXD-CLI01","title":"stego plan","domain_id":"ST-FXD","usage":"stego plan [--show] [--base <ref>]","description":"Plans."}` + "\n" + `{"id":"ST-FXD-CLI02","title":"stego fill sign","domain_id":"ST-FXD","usage":"stego fill sign <name>","description":"Signs."}` + "\n" + `{"id":"ST-FXD-CLI03","title":"stego fill","domain_id":"ST-FXD","usage":"stego fill a|b","description":"Fills."}` + "\n",
	"cli_handler": `{"id":"ST-FXD-CH01","title":"main.runPlan","domain_id":"ST-FXD","file":"cmd/stego/plan.go"}` + "\n" + `{"id":"ST-FXD-CH02","title":"main.runSign","domain_id":"ST-FXD","file":"cmd/stego/sign.go"}` + "\n",
	"runs":        `{"source_id":"ST-FXD-CH01","target_id":"ST-FXD-CLI01"}` + "\n" + `{"source_id":"ST-FXD-CH02","target_id":"ST-FXD-CLI02"}` + "\n",
}

func docTree(files map[string]string) Tree {
	base := map[string]string{"docs.txt": "# user docs\nREADME.md\n", "cmd/stego/plan.go": "package main\n"}
	for name, text := range files {
		if text == "" {
			delete(base, name)
			continue
		}
		base[name] = text
	}
	return codeTree(base)
}

func TestDocInventoryFaults(t *testing.T) {
	t.Run("ST-ASR-SP14-T Inventory entry not a held Markdown file is found in a tree built for it, and not in one that follows the rule", func(t *testing.T) {
		spec := codeSpec(t, docFixtureTables)
		expectFaults(t, docInventoryFaults(spec, docTree(map[string]string{"README.md": "# Fixture\n"})))
		expectFaults(t, docInventoryFaults(spec, docTree(nil)),
			CodeFault{Rule: RuleDocInventory, Key: "README.md", Path: "README.md", Reason: "the user doc inventory lists the file, and the tree does not hold it"})
		expectFaults(t, docInventoryFaults(spec, docTree(map[string]string{"docs.txt": "notes.txt\n", "notes.txt": "x\n"})),
			CodeFault{Rule: RuleDocInventory, Key: "notes.txt", Path: "notes.txt", Reason: "the user doc inventory lists a file that is not Markdown"})
		if docs := UserDocs(codeSpec(t, map[string]string{}), docTree(map[string]string{"README.md": "x\n"})); docs != nil {
			t.Fatalf("a spec with no inventory record has the user docs %v, want none", docs)
		}
	})
}

func TestDocNamesUnknownCommand(t *testing.T) {
	t.Run("ST-ASR-SP11-T Doc names an unknown command is found in a tree built for it, and not in one that follows the rule", func(t *testing.T) {
		spec := codeSpec(t, docFixtureTables)
		good := "Run `stego plan --show` first.\n\n```bash\nstego plan -base main   # compare\n$ stego plan --help\ngo build -o stego ./cmd/stego\n```\n" +
			"<!-- stego:generated cli-commands -->\n`stego nothing`\n<!-- stego:end -->\n"
		expectFaults(t, docNamesUnknownCommand(spec, docTree(map[string]string{"README.md": good})))
		bad := "```sh\nstego plan --dry-run\nstego fill sign x\nstego deploy now\n```\n"
		expectFaults(t, docNamesUnknownCommand(spec, docTree(map[string]string{"README.md": bad})),
			CodeFault{Rule: RuleDocNamesResolve, Key: "README.md stego deploy now", Path: "README.md", Reason: "the doc names stego deploy now, and no CLI command record has that title"},
			CodeFault{Rule: RuleDocNamesResolve, Key: "README.md stego fill sign x", Path: "README.md", Reason: "the doc names stego fill sign (ST-FXD-CLI02), which is not built: no handler file in the tree runs it"},
			CodeFault{Rule: RuleDocNamesResolve, Key: "README.md stego plan --dry-run --dry-run", Path: "README.md", Reason: "the doc gives stego plan the flag --dry-run, which its usage (ST-FXD-CLI01) does not list"},
		)
	})
}

func TestDocNamesMissingPath(t *testing.T) {
	t.Run("ST-ASR-SP12-T Doc links to a missing path is found in a tree built for it, and not in one that follows the rule", func(t *testing.T) {
		spec := codeSpec(t, docFixtureTables)
		good := "See [the plan](cmd/stego/plan.go), [the folder](cmd/stego/), [Go](https://go.dev/dl/) and [below](#usage).\n"
		expectFaults(t, docNamesMissingPath(spec, docTree(map[string]string{"README.md": good})))
		bad := "See [the license](LICENSE#top).\n"
		expectFaults(t, docNamesMissingPath(spec, docTree(map[string]string{"README.md": bad})),
			CodeFault{Rule: RuleDocNamesResolve, Key: "README.md LICENSE#top", Path: "README.md", Reason: "the doc links to LICENSE#top, which the tree does not hold"})
	})
}

func TestDocSectionStale(t *testing.T) {
	t.Run("ST-ASR-SP13-T Generated doc section out of date is found in a tree built for it, and not in one that follows the rule", func(t *testing.T) {
		spec := codeSpec(t, docFixtureTables)
		table := "| Command | Usage |\n|---|---|\n| `stego plan` | `stego plan [--show] [--base <ref>]` |\n"
		section := func(body string) string {
			return "# Fixture\n\n<!-- stego:generated cli-commands -->\n" + body + "<!-- stego:end -->\n"
		}
		tree := docTree(map[string]string{"README.md": section(table)})
		if got := cliCommandTable(spec, tree); got != table {
			t.Fatalf("the cli-commands section is\n%s\nwant only the built stego command:\n%s", got, table)
		}
		expectFaults(t, docSectionStale(spec, tree))
		stale := docTree(map[string]string{"README.md": section("| old |\n")})
		expectFaults(t, docSectionStale(spec, stale),
			CodeFault{Rule: RuleDocSectionsCurrent, Key: "README.md cli-commands", Path: "README.md",
				Reason: "the generated section cli-commands is not what the spec gives; write it again with go test ./internal/assure -run TestOwnTree -update-docs"})
		written, unknown := RegenerateDocSections(spec, stale, section("| old |\n"))
		if written != section(table) || unknown != nil {
			t.Fatalf("RegenerateDocSections gives %q and %v, want %q and no unknown section", written, unknown, section(table))
		}
		odd := docTree(map[string]string{"README.md": "<!-- stego:generated other -->\nx\n<!-- stego:end -->\n<!-- stego:generated cli-commands -->\n"})
		expectFaults(t, docSectionStale(spec, odd),
			CodeFault{Rule: RuleDocSectionsCurrent, Key: "README.md", Path: "README.md", Reason: "a generated section has a start marker with no end marker"},
			CodeFault{Rule: RuleDocSectionsCurrent, Key: "README.md other", Path: "README.md", Reason: "the generated section other names no generator"},
		)
	})
}

func TestDocsLagChange(t *testing.T) {
	t.Run("ST-STD-DOCS-R04-T01 A change that alters or removes a CLI command a user doc names, and leaves the doc as it is, gives a reason; one that changes the doc gives none", func(t *testing.T) {
		base := codeSpec(t, docFixtureTables)
		doc := "Run `stego plan`.\n<!-- stego:generated cli-commands -->\n`stego fill sign`\n<!-- stego:end -->\n"
		baseTree := docTree(map[string]string{"README.md": doc})
		altered := make(map[string]string, len(docFixtureTables))
		for name, rows := range docFixtureTables {
			altered[name] = strings.ReplaceAll(strings.ReplaceAll(rows, `"Plans."`, `"Plans, now with a base."`), `"Signs."`, `"Signs, now twice."`)
		}
		change := codeSpec(t, altered)
		want := []string{"README.md (ST-STD-DOCS-R04, ST-FXD-CLI01): the doc names stego plan, whose record ST-FXD-CLI01 the change alters, and the change does not change the doc"}
		if got := docsLagChange(base, change, baseTree, baseTree); !reflect.DeepEqual(got, want) {
			t.Fatalf("the reasons are %q, want %q (a command named only in a generated section gives none)", got, want)
		}
		if rule, key, ok := codeFaultKey(want[0]); !ok || rule != RuleDocFollowsChange || key != "ST-FXD-CLI01" {
			t.Fatalf("the reason does not read back as a fault of %s with its record as key: %q %q %t", RuleDocFollowsChange, rule, key, ok)
		}
		removed := map[string]string{}
		for name, rows := range docFixtureTables {
			removed[name] = rows
		}
		removed["cli_command"] = strings.SplitAfter(docFixtureTables["cli_command"], "\n")[1]
		gone := []string{"README.md (ST-STD-DOCS-R04, ST-FXD-CLI01): the doc names stego plan, whose record ST-FXD-CLI01 the change removes, and the change does not change the doc"}
		if got := docsLagChange(base, codeSpec(t, removed), baseTree, baseTree); !reflect.DeepEqual(got, gone) {
			t.Fatalf("the reasons are %q, want %q", got, gone)
		}
		touched := docTree(map[string]string{"README.md": doc + "Run `stego plan --base main` to compare.\n"})
		if got := docsLagChange(base, change, baseTree, touched); got != nil {
			t.Fatalf("a change that changes the doc gives the reasons %q, want none", got)
		}
		if got := docsLagChange(base, base, baseTree, baseTree); got != nil {
			t.Fatalf("a change that alters no command gives the reasons %q, want none", got)
		}
	})
}

// workingTree reads the tracked and new files of the working tree at root, for the -update-docs mode.
func workingTree(t *testing.T, root string) (Spec, Tree) {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-co", "--exclude-standard", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte)
	var data []SpecFile
	for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		bytes, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		files[name] = bytes
		if filepath.Dir(name) == "data" && strings.HasSuffix(name, ".jsonl") {
			data = append(data, SpecFile{Path: name, Data: bytes})
		}
	}
	spec, err := ReadSpec(data)
	if err != nil {
		t.Fatal(err)
	}
	return spec, NewTree(files)
}

// writeDocSections writes each generated section of each user doc of the working tree again.
func writeDocSections(t *testing.T, root string) {
	t.Helper()
	spec, tree := workingTree(t, root)
	for _, doc := range UserDocs(spec, tree) {
		data, ok := tree.File(doc)
		if !ok {
			continue
		}
		written, unknown := RegenerateDocSections(spec, tree, string(data))
		if len(unknown) > 0 {
			t.Fatalf("%s has generated sections with no generator: %v", doc, unknown)
		}
		if written != string(data) {
			if err := os.WriteFile(filepath.Join(root, doc), []byte(written), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote the generated sections of %s again; commit it and run the test again", doc)
		}
	}
}
