package assure

import (
	"bufio"
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The fixture domains, one per title of the domain order, so that their IDs cannot be taken for STEGO's own.
const fixtureDomains = `{"id":"ST-FXA","title":"Assuring"}
{"id":"ST-FXD","title":"Declaring"}
{"id":"ST-FXS","title":"Supplying"}
{"id":"ST-FXG","title":"Generating"}
{"id":"ST-FXB","title":"Building"}
{"id":"ST-FXR","title":"Distributing"}
`

// fixtureKinds declares the product's own kinds of the technical stage, as STEGO's spec declares cli_command and
// generator, and one kind of another stage, whose records are not technical records even when they name a file.
const fixtureKinds = `{"name":"cli_command","stage":"technical-specification"}
{"name":"generator","stage":"technical-specification"}
{"name":"note","stage":"use-cases"}
`

// codeSpec reads a spec built for a test from its tables, by name, through ReadSpec, as the gates read the spec
// at a commit.
func codeSpec(t *testing.T, tables map[string]string) Spec {
	t.Helper()
	files := []SpecFile{
		{Path: "data/domain.jsonl", Data: []byte(fixtureDomains)},
		{Path: "data/product_kind.jsonl", Data: []byte(fixtureKinds)},
	}
	for name, rows := range tables {
		files = append(files, SpecFile{Path: "data/" + name + ".jsonl", Data: []byte(rows)})
	}
	spec, err := ReadSpec(files)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// codeTree is a tree built for a test, from each file's path and text.
func codeTree(files map[string]string) Tree {
	data := make(map[string][]byte, len(files))
	for name, text := range files {
		data[name] = []byte(text)
	}
	return NewTree(data)
}

// expectFaults fails the test unless the faults are exactly those given, in order.
func expectFaults(t *testing.T, got []CodeFault, want ...CodeFault) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("faults:\n%s\nwant:\n%s", faultLines(got), faultLines(want))
	}
}

func faultLines(faults []CodeFault) string {
	var lines []string
	for _, fault := range faults {
		lines = append(lines, "  "+fault.String())
	}
	return strings.Join(lines, "\n")
}

func TestPackageOfTwoDomains(t *testing.T) {
	t.Run("ST-ASR-SP05-T Package of two domains is found in a tree built for it, and not in one that follows the rule", func(t *testing.T) {
		spec := codeSpec(t, map[string]string{
			"predicate": `{"id":"ST-FXD-SP01-PR","title":"mixed.isDeclared","file":"internal/mixed/declared.go"}
{"id":"ST-FXD-SP02-PR","title":"clean.isDeclared","file":"internal/clean/declared.go"}
{"id":"ST-FXG-SP01-PR","title":"later.isPlanned","file":"internal/clean/planned.go"}
`,
			"service":     `{"id":"ST-FXS-DS01-SV","title":"mixed.supply","file":"internal/mixed/supplied.go"}`,
			"value_type":  `{"id":"ST-VO01-VT","title":"clean.Shared","file":"internal/clean/shared.go"}`,
			"adapter":     `{"id":"ST-INT01-AD","title":"clean.Outside","file":"internal/clean/outside.go"}`,
			"entry_point": `{"id":"ST-FXD-ENTRY01","title":"fx","file":"cmd/fx/main.go"}`,
			"cli_command": `{"id":"ST-FXB-CLI01","title":"fx build","file":"cmd/fx/build.go"}`,
			"tooling":     `{"id":"ST-FXG-TOOL01","title":"gen.go","file":"internal/clean/testdata/gen.go"}`,
		})
		tree := codeTree(map[string]string{
			"internal/mixed/declared.go":     "package mixed\n",
			"internal/mixed/supplied.go":     "package mixed\n",
			"internal/clean/declared.go":     "package clean\n",
			"internal/clean/shared.go":       "package clean\n",
			"internal/clean/outside.go":      "package clean\n",
			"internal/clean/testdata/gen.go": "package gen\n",
			"cmd/fx/main.go":                 "package main\n",
			"cmd/fx/build.go":                "package main\n",
		})
		domains := packageDomains(spec, tree)
		want := map[string][]string{
			"internal/mixed": {"ST-FXD", "ST-FXS"},
			"internal/clean": {"ST-FXD"},
			"cmd/fx":         {"ST-FXB", "ST-FXD"},
		}
		if !reflect.DeepEqual(domains, want) {
			t.Fatalf("package domains = %v, want %v", domains, want)
		}
		expectFaults(t, packageOfTwoDomains(spec, tree),
			CodeFault{Rule: "ST-STD-CODE-R01", Path: "cmd/fx", Reason: "the package holds files that the records of more than one domain name: ST-FXB, ST-FXD"},
			CodeFault{Rule: "ST-STD-CODE-R01", Path: "internal/mixed", Reason: "the package holds files that the records of more than one domain name: ST-FXD, ST-FXS"},
		)
	})
}

// declaringFile declares one of each kind of identifier a record's title can name.
const declaringFile = `package fx

import "errors"

// Decision is a type with a field and a method.
type Decision struct {
	Commit string
}

// Store is an interface with a method.
type Store interface {
	Keep(d Decision) error
}

const ProofRunFile = "proof-run.json"

var errRefused = errors.New("refused")

func isGood(d Decision) bool { return d.Commit != "" }

func (d Decision) Check() error { return errRefused }

// Handler is only named by its methods in this file.
func (h *Handler) Handle(d Decision) error { return h.store.Keep(d) }
`

func TestIdentifierMissing(t *testing.T) {
	t.Run("ST-ASR-SP06-T Identifier missing from its file is found in a tree built for it, and not in one that follows the rule", func(t *testing.T) {
		spec := codeSpec(t, map[string]string{
			"predicate": `{"id":"ST-FXD-SP01-PR","title":"fx.isGood","file":"internal/fx/fx.go"}
{"id":"ST-FXD-SP02-PR","title":"fx.isMissing","file":"internal/fx/fx.go"}
{"id":"ST-FXD-SP03-PR","title":"fx.isElsewhere","file":"internal/fx/fx.go"}
{"id":"ST-FXD-SP04-PR","title":"fx.notWrittenYet","file":"internal/fx/later.go"}
`,
			"command_handler": `{"id":"ST-FXD-CMD01-H","title":"fx.Handler.Handle","file":"internal/fx/fx.go"}
{"id":"ST-FXD-CMD02-H","title":"fx.Handler.Missing","file":"internal/fx/fx.go"}
`,
			"query_handler": `{"id":"ST-FXD-QRY01-H","title":"fx.Decision.Check","file":"internal/fx/fx.go"}`,
			"service":       `{"id":"ST-FXD-DS01-SV","title":"other.Decision","file":"internal/fx/fx.go"}`,
			"value_type":    `{"id":"ST-VO01-VT","title":"fx.Store","file":"internal/fx/fx.go"}`,
			"data_model": `{"id":"ST-FXD-EN01-DM","title":"fx.Decision","file":"internal/fx/fx.go"}
{"id":"ST-FXD-EN02-DM","title":"fx.Decision.Commit","file":"internal/fx/fx.go"}
{"id":"ST-FXD-EN03-DM","title":"fx.Decision.Branch","file":"internal/fx/fx.go"}
`,
			"event_type": `{"id":"ST-FXD-EV01-ET","title":"Decided","file":"internal/fx/fx.go"}`,
			"projection": `{"id":"ST-FXD-RM01-PJ","title":"fx.errRefused","file":"internal/fx/fx.go"}`,
			"repository": `{"id":"ST-FXD-REPO01","title":"fx.Store.Keep","file":"internal/fx/fx.go"}`,
			"table": `{"id":"ST-FXD-EN01-TB","title":"fx.ProofRunFile","file":"internal/fx/fx.go"}
{"id":"ST-FXD-EN02-TB","title":"proof-run.json","file":"internal/fx/fx.go"}
`,
			"tooling":     `{"id":"ST-FXD-TOOL01","title":"run.py","file":"scripts/run.py"}`,
			"cli_command": `{"id":"ST-FXD-CLI01","title":"fx decide","file":"cmd/fx/main.go"}`,
		})
		tree := codeTree(map[string]string{
			"internal/fx/fx.go":    declaringFile,
			"internal/fx/other.go": "package fx\n\nfunc isElsewhere() bool { return true }\n",
			"scripts/run.py":       "print('run')\n",
			"cmd/fx/main.go":       "package main\n",
		})
		file := "internal/fx/fx.go"
		expectFaults(t, identifierMissing(spec, tree),
			CodeFault{Rule: "ST-STD-CODE-R02", Path: file, Reason: "ST-FXD-CMD02-H names fx.Handler.Missing, which the file does not declare"},
			CodeFault{Rule: "ST-STD-CODE-R02", Path: file, Reason: `ST-FXD-DS01-SV is titled "other.Decision", which is not an identifier of package fx`},
			CodeFault{Rule: "ST-STD-CODE-R02", Path: file, Reason: `ST-FXD-EN02-TB is titled "proof-run.json", which is not an identifier of package fx`},
			CodeFault{Rule: "ST-STD-CODE-R02", Path: file, Reason: "ST-FXD-EN03-DM names fx.Decision.Branch, which the file does not declare"},
			CodeFault{Rule: "ST-STD-CODE-R02", Path: file, Reason: `ST-FXD-EV01-ET is titled "Decided", which is not an identifier of package fx`},
			CodeFault{Rule: "ST-STD-CODE-R02", Path: file, Reason: "ST-FXD-SP02-PR names fx.isMissing, which the file does not declare"},
			CodeFault{Rule: "ST-STD-CODE-R02", Path: file, Reason: "ST-FXD-SP03-PR names fx.isElsewhere, which the file does not declare"},
		)
	})
}

func TestImportAgainstOrder(t *testing.T) {
	t.Run("ST-ASR-SP07-T Import against the order is found in a tree built for it, and not in one that follows the rule", func(t *testing.T) {
		spec := codeSpec(t, map[string]string{
			"predicate": `{"id":"ST-FXA-SP01-PR","title":"assure.holds","file":"internal/assure/assure.go"}
{"id":"ST-FXD-SP01-PR","title":"declare.holds","file":"internal/declare/declare.go"}
{"id":"ST-FXS-SP01-PR","title":"supply.holds","file":"internal/supply/supply.go"}
{"id":"ST-FXG-SP01-PR","title":"generate.holds","file":"internal/generate/generate.go"}
{"id":"ST-FXB-SP01-PR","title":"build.holds","file":"internal/build/build.go"}
{"id":"ST-FXR-SP01-PR","title":"distribute.holds","file":"internal/distribute/distribute.go"}
`,
			"value_type":  `{"id":"ST-VO01-VT","title":"shared.Value","file":"internal/shared/shared.go"}`,
			"entry_point": `{"id":"ST-FXD-ENTRY01","title":"fx","file":"cmd/fx/main.go"}`,
		})
		module := "example.com/fx"
		imports := func(pkg string, paths ...string) string {
			text := "package " + pkg + "\n\nimport (\n\t\"fmt\"\n"
			for _, p := range paths {
				text += "\t\"" + module + "/" + p + "\"\n"
			}
			return text + ")\n"
		}
		tree := codeTree(map[string]string{
			"go.mod":                            "module " + module + " // the fixture\n\ngo 1.26\n",
			"internal/shared/shared.go":         imports("shared"),
			"internal/assure/assure.go":         imports("assure", "internal/shared"),
			"internal/declare/declare.go":       imports("declare", "internal/shared"),
			"internal/declare/reverse.go":       imports("declare", "internal/supply"),
			"internal/declare/declare_test.go":  imports("declare", "internal/generate"),
			"internal/supply/supply.go":         imports("supply", "internal/declare", "internal/shared"),
			"internal/generate/generate.go":     imports("generate", "internal/declare", "internal/supply"),
			"internal/build/build.go":           imports("build", "internal/generate", "internal/declare"),
			"internal/distribute/distribute.go": imports("distribute", "internal/build"),
			"cmd/fx/main.go": imports("main", "internal/assure", "internal/declare", "internal/supply",
				"internal/generate", "internal/build", "internal/distribute"),
		})
		expectFaults(t, importAgainstOrder(spec, tree),
			CodeFault{Rule: "ST-STD-CODE-R03", Path: "internal/build", Reason: "the package imports internal/declare against the domain order: Building (ST-FXB) may not import Declaring (ST-FXD)"},
			CodeFault{Rule: "ST-STD-CODE-R03", Path: "internal/declare", Reason: "the package imports internal/supply against the domain order: Declaring (ST-FXD) may not import Supplying (ST-FXS)"},
		)
	})
}

// fixtureTest is a Go test file whose subtests carry the IDs given.
func fixtureTest(ids ...string) string {
	text := "package fx\n\nimport \"testing\"\n\nfunc TestFx(t *testing.T) {\n"
	for _, id := range ids {
		text += "\tt.Run(\"" + id + " holds\", func(t *testing.T) {})\n"
	}
	return text + "}\n"
}

func TestTestWithoutID(t *testing.T) {
	t.Run("ST-ASR-SP08-T Test without its ID is found in a tree built for it, and not in one that follows the rule", func(t *testing.T) {
		spec := codeSpec(t, map[string]string{
			"specification_test": `{"id":"ST-FXD-SP01-T","state":"automated","file":"internal/fx/fx_test.go"}
{"id":"ST-FXD-SP02-T","state":"automated","file":"internal/fx/fx_test.go"}
{"id":"ST-FXD-SP03-T","state":"not automated yet","file":"internal/fx/fx_test.go"}
{"id":"ST-FXD-SP04-T","state":"automated","file":"internal/fx/gone_test.go"}
{"id":"ST-FXD-SP05-T","state":"automated","file":"internal/fx/joined_test.go"}
`,
			"use_case_test": `{"id":"ST-FXD-UC01-R01-T01","state":"automated","file":"scripts/test_fx.py"}`,
		})
		tree := codeTree(map[string]string{
			"lathe.config.json":                    `{"schema": 2, "data": "data", "tests": ["**/*_test.go", "scripts/test_*.py"]}`,
			"internal/fx/fx_test.go":               fixtureTest("ST-FXD-SP01-T"),
			"internal/fx/helpers_test.go":          "package fx\n\n// Helpers of the fx tests. ST-FXD-SP09-T has the form of an ID, but no test record has it.\n",
			"internal/fx/joined_test.go":           fixtureTest("ST-FXD-SP05-T-holds", "ST-FXD-SP05-Tx"),
			"internal/fx/unknown_test.go":          fixtureTest("ST-FXD-SP09-T"),
			"internal/fx/none_test.go":             fixtureTest("a case with no ID"),
			"internal/fx/fx.go":                    "package fx\n",
			"scripts/test_fx.py":                   "class Fx:\n    def test_cases(self):\n        self.subTest('ST-FXD-UC01-R01-T01 holds')\n",
			"scripts/fx.py":                        "print('no test here')\n",
			".github/fx_test.go":                   fixtureTest(),
			"node_modules/x/x_test.go":             fixtureTest(),
			"examples/other/lathe.config.json":     "{}",
			"examples/other/internal/x/x_test.go":  fixtureTest(),
			"examples/service/fills/check_test.go": fixtureTest(),
		})
		expectFaults(t, testWithoutID(spec, tree),
			CodeFault{Rule: "ST-STD-CODE-R04", Path: "examples/service/fills/check_test.go", Reason: "the test file names no test record's ID"},
			CodeFault{Rule: "ST-STD-CODE-R04", Path: "internal/fx/fx_test.go", Reason: "the file does not carry the ID of automated test record ST-FXD-SP02-T"},
			CodeFault{Rule: "ST-STD-CODE-R04", Path: "internal/fx/gone_test.go", Reason: "automated test record ST-FXD-SP04-T names this file, which the tree does not hold"},
			CodeFault{Rule: "ST-STD-CODE-R04", Path: "internal/fx/helpers_test.go", Reason: "the test file names no test record's ID"},
			CodeFault{Rule: "ST-STD-CODE-R04", Path: "internal/fx/joined_test.go", Reason: "the file does not carry the ID of automated test record ST-FXD-SP05-T"},
			CodeFault{Rule: "ST-STD-CODE-R04", Path: "internal/fx/joined_test.go", Reason: "the test file names no test record's ID"},
			CodeFault{Rule: "ST-STD-CODE-R04", Path: "internal/fx/none_test.go", Reason: "the test file names no test record's ID"},
			CodeFault{Rule: "ST-STD-CODE-R04", Path: "internal/fx/unknown_test.go", Reason: "the test file names no test record's ID"},
		)
	})
}

func TestFileWithoutRecord(t *testing.T) {
	t.Run("ST-ASR-SP09-T Source file without a record is found in a tree built for it, and not in one that follows the rule", func(t *testing.T) {
		spec := codeSpec(t, map[string]string{
			"predicate": `{"id":"ST-FXD-SP01-PR","title":"fx.holds","file":"./internal/fx/named.go"}`,
			"generator": `{"id":"ST-FXS-RC01-G01","title":"templates","file":"registry/components/web/"}`,
			"tooling": `{"id":"ST-FXA-TOOL01","title":"scripts","file":"scripts/tools"}
{"id":"ST-FXA-TOOL02","title":"a file that is not a string","file":["scripts/lost.py"]}
this line is not JSON, and Lathe passes over it
`,
			"note":               `{"id":"ST-FXS-UC01-N01","title":"fx.contract","file":"internal/fx/contract.go"}`,
			"specification_test": `{"id":"ST-FXD-SP01-T","state":"automated","file":"internal/fx/lost.go"}`,
		})
		tree := codeTree(map[string]string{
			"lathe.config.json":                      `{"schema": 2, "data": "data", "tests": ["**/*_test.go", "scripts/test_*.py"]}`,
			"internal/fx/named.go":                   "package fx\n",
			"internal/fx/lost.go":                    "package fx\n",
			"internal/fx/contract.go":                "package fx\n",
			"internal/fx/lost_test.go":               "package fx\n",
			"internal/fx/zz_generated.go":            "// Code generated by fx. DO NOT EDIT.\n\npackage fx\n",
			"internal/fx/README.md":                  "# fx\n",
			"internal/fx/fixtures/helper.go":         "package fixtures\n",
			"internal/fx/web.config.js":              "export default {}\n",
			"registry/components/web/generate.go":    "package web\n",
			"registry/components/web/sub/emit.ts":    "export {}\n",
			"registry/components/api/component.yaml": "name: api\n",
			"registry/components/api/generate.go":    "package api\n",
			"scripts/tools/build.py":                 "print('build')\n",
			"scripts/lost.py":                        "print('lost')\n",
			"scripts/test_fx.py":                     "print('a test')\n",
			"cmd/fx/main.go":                         "package main\n",
			"examples/fx/main.go":                    "package main\n",
		})
		lost := "no technical record names the file, directly or through a folder"
		expectFaults(t, fileWithoutRecord(spec, tree),
			CodeFault{Rule: "ST-STD-CODE-R05", Path: "cmd/fx/main.go", Reason: lost},
			CodeFault{Rule: "ST-STD-CODE-R05", Path: "internal/fx/contract.go", Reason: lost},
			CodeFault{Rule: "ST-STD-CODE-R05", Path: "internal/fx/lost.go", Reason: lost},
			CodeFault{Rule: "ST-STD-CODE-R05", Path: "registry/components/api/generate.go", Reason: lost},
			CodeFault{Rule: "ST-STD-CODE-R05", Path: "scripts/lost.py", Reason: lost},
		)
	})
}

// stubFile holds functions that read their inputs and functions that do not.
const stubFile = `package fx

import (
	"errors"
	"fmt"
)

type Decision struct{ ID string }

type Handler struct{ count int }

func isGood(d Decision) bool { return d.ID != "" }

func alwaysTrue(d Decision) bool { return true }

func (h *Handler) Handle(d Decision) error { panic("not written yet") }

func (h *Handler) Count() int { return h.count }

func notYet(d Decision) (Decision, error) {
	return Decision{ID: "none"}, errors.New("not implemented")
}

func notYetEither(d Decision) (*Decision, error) {
	return &Decision{}, fmt.Errorf("decision %s: %w", "none", errors.New("not implemented"))
}

func empty(d Decision) {}

func reads(d Decision) (Decision, error) {
	if d.ID == "" {
		return Decision{}, errors.New("no ID")
	}
	return Decision{ID: d.ID}, nil
}

func returnsInput(d Decision) Decision { return d }

func panicsWithInput(d Decision) error { panic(fmt.Sprint(d.ID)) }

func delegates(d Decision) error { return check(d) }

func check(d Decision) error { return nil }

func constantValue() Decision { return Decision{ID: "fixed"} }
`

func TestStubBuilt(t *testing.T) {
	t.Run("ST-ASR-SP10-T Stub counted as built is found in a tree built for it, and not in one that follows the rule", func(t *testing.T) {
		spec := codeSpec(t, map[string]string{
			"predicate": `{"id":"ST-FXD-SP01-PR","title":"fx.isGood","file":"internal/fx/fx.go"}
{"id":"ST-FXD-SP02-PR","title":"fx.alwaysTrue","file":"internal/fx/fx.go"}
{"id":"ST-FXD-SP03-PR","title":"fx.missing","file":"internal/fx/fx.go"}
`,
			"command_handler": `{"id":"ST-FXD-CMD01-H","title":"fx.Handler.Handle","file":"internal/fx/fx.go"}
{"id":"ST-FXD-CMD02-H","title":"fx.empty","file":"internal/fx/fx.go"}
{"id":"ST-FXD-CMD03-H","title":"fx.panicsWithInput","file":"internal/fx/fx.go"}
`,
			"query_handler": `{"id":"ST-FXD-QRY01-H","title":"fx.Handler.Count","file":"internal/fx/fx.go"}
{"id":"ST-FXD-QRY02-H","title":"fx.reads","file":"internal/fx/fx.go"}
`,
			"service": `{"id":"ST-FXD-DS01-SV","title":"fx.notYet","file":"internal/fx/fx.go"}
{"id":"ST-FXD-DS02-SV","title":"fx.notYetEither","file":"internal/fx/fx.go"}
{"id":"ST-FXD-DS03-SV","title":"fx.returnsInput","file":"internal/fx/fx.go"}
{"id":"ST-FXD-DS04-SV","title":"fx.delegates","file":"internal/fx/fx.go"}
`,
			"value_type": `{"id":"ST-VO01-VT","title":"fx.constantValue","file":"internal/fx/fx.go"}`,
		})
		tree := codeTree(map[string]string{"internal/fx/fx.go": stubFile})
		file := "internal/fx/fx.go"
		reason := func(id, title string) string {
			return id + " names " + title + ", whose body only returns constants or panics, so it reads no input"
		}
		expectFaults(t, stubBuilt(spec, tree),
			CodeFault{Rule: "ST-STD-CODE-R06", Path: file, Reason: reason("ST-FXD-CMD01-H", "fx.Handler.Handle")},
			CodeFault{Rule: "ST-STD-CODE-R06", Path: file, Reason: reason("ST-FXD-CMD02-H", "fx.empty")},
			CodeFault{Rule: "ST-STD-CODE-R06", Path: file, Reason: reason("ST-FXD-DS01-SV", "fx.notYet")},
			CodeFault{Rule: "ST-STD-CODE-R06", Path: file, Reason: reason("ST-FXD-DS02-SV", "fx.notYetEither")},
			CodeFault{Rule: "ST-STD-CODE-R06", Path: file, Reason: reason("ST-FXD-SP02-PR", "fx.alwaysTrue")},
		)
	})
}

// knownCodeFaultsRecord is the tooling record that names the committed list of the code-standard faults STEGO's
// own tree has: the list the rule tests hold the tree to, which may only get shorter.
const knownCodeFaultsRecord = "ST-ASR-TOOL03"

// knownCodeFaults reads the list of known faults: one fault per line, as rule, path and reason separated by tabs.
// A line that starts with # is a comment.
func knownCodeFaults(t *testing.T, data []byte) []CodeFault {
	t.Helper()
	var faults []CodeFault
	scanner := bufio.NewScanner(bytes.NewReader(data))
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, "\t")
		if len(fields) != 3 {
			t.Fatalf("known faults line %d has %d fields, not rule, path and reason: %q", line, len(fields), text)
		}
		faults = append(faults, CodeFault{Rule: fields[0], Path: fields[1], Reason: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return faults
}

// difference lists the faults of a that b does not hold.
func difference(a, b []CodeFault) []CodeFault {
	held := make(map[CodeFault]bool, len(b))
	for _, fault := range b {
		held[fault] = true
	}
	var only []CodeFault
	for _, fault := range a {
		if !held[fault] {
			only = append(only, fault)
		}
	}
	return only
}

// TestOwnTree runs each predicate over STEGO's own tree and spec at the commit under test, read from git objects,
// never from the working tree. A fault the committed list of known faults does not hold fails the rule's test, and
// so does a listed fault that is gone, so that the list only gets shorter. CodeFaults must give the same faults as
// the predicate, in order.
func TestOwnTree(t *testing.T) {
	root, err := RepositoryRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ReadCommittedSpec(root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := ReadCommittedTree(root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var list TechnicalRecord
	for _, record := range spec.Technical {
		if record.ID == knownCodeFaultsRecord {
			list = record
		}
	}
	data, held := tree.File(list.File)
	if list.ID == "" || !held {
		t.Fatalf("the spec at HEAD has no record %s that names a file of the tree", knownCodeFaultsRecord)
	}
	known := knownCodeFaults(t, data)
	all := CodeFaults(spec, tree)
	for _, rule := range []struct {
		test      string
		rule      string
		predicate func(Spec, Tree) []CodeFault
	}{
		{"ST-STD-CODE-R01-T01 STEGO's own tree holds the rule: package of two domains is not found in it", RuleOneDomainPerPackage, packageOfTwoDomains},
		{"ST-STD-CODE-R02-T01 STEGO's own tree holds the rule: identifier missing from its file is not found in it", RuleNamedAsRecordNames, identifierMissing},
		{"ST-STD-CODE-R03-T01 STEGO's own tree holds the rule: import against the order is not found in it", RuleImportsFollowOrder, importAgainstOrder},
		{"ST-STD-CODE-R04-T01 STEGO's own tree holds the rule: test without its id is not found in it", RuleTestNamesWhatItDoes, testWithoutID},
		{"ST-STD-CODE-R05-T01 STEGO's own tree holds the rule: source file without a record is not found in it", RuleNoFileWithoutRecord, fileWithoutRecord},
		{"ST-STD-CODE-R06-T01 STEGO's own tree holds the rule: stub counted as built is not found in it", RuleStubIsNotBuilt, stubBuilt},
	} {
		t.Run(rule.test, func(t *testing.T) {
			got := rule.predicate(spec, tree)
			var listed, fromAll []CodeFault
			for _, fault := range known {
				if fault.Rule == rule.rule {
					listed = append(listed, fault)
				}
			}
			for _, fault := range all {
				if fault.Rule == rule.rule {
					fromAll = append(fromAll, fault)
				}
			}
			if !reflect.DeepEqual(got, fromAll) {
				t.Fatalf("CodeFaults gives other faults than the predicate:\n%s\nthe predicate:\n%s", faultLines(fromAll), faultLines(got))
			}
			if !sort.SliceIsSorted(got, func(i, j int) bool {
				return got[i].Path < got[j].Path || got[i].Path == got[j].Path && got[i].Reason < got[j].Reason
			}) {
				t.Fatalf("the faults are not in order:\n%s", faultLines(got))
			}
			var problems []string
			if added := difference(got, listed); len(added) > 0 {
				problems = append(problems, fmt.Sprintf("%d new faults, which the list of known faults (%s) does not hold:\n%s", len(added), list.File, faultLines(added)))
			}
			if gone := difference(listed, got); len(gone) > 0 {
				problems = append(problems, fmt.Sprintf("%d listed faults are gone; remove them from %s, so that the list gets shorter:\n%s", len(gone), list.File, faultLines(gone)))
			}
			if len(problems) > 0 {
				t.Fatal(strings.Join(problems, "\n"))
			}
		})
	}
}
