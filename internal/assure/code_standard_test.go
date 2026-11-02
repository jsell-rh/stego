package assure

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
const fixtureKinds = `{"name":"cli_command","stage":"technical-specification","complexity":"accidental"}
{"name":"generator","stage":"technical-specification","complexity":"accidental"}
{"name":"note","stage":"use-cases","complexity":"essential"}
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
			"reader":      `{"id":"ST-FXD-RM01-RD","title":"folder.Read","file":"internal/folder/read.go"}`,
			"generator":   `{"id":"ST-FXS-RC01-G01","title":"templates","file":"internal/folder/"}`,
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
			"internal/folder/read.go":        "package folder\n",
			"internal/folder/sub/emit.go":    "package sub\n",
		})
		domains, _ := packageDomains(spec, tree)
		want := map[string][]string{
			"internal/mixed":      {"ST-FXD", "ST-FXS"},
			"internal/clean":      {"ST-FXD"},
			"cmd/fx":              {"ST-FXB", "ST-FXD"},
			"internal/folder":     {"ST-FXD", "ST-FXS"},
			"internal/folder/sub": {"ST-FXS"},
		}
		if !reflect.DeepEqual(domains, want) {
			t.Fatalf("package domains = %v, want %v", domains, want)
		}
		expectFaults(t, packageOfTwoDomains(spec, tree),
			CodeFault{Rule: "ST-STD-CODE-R01", Key: "internal/folder", Path: "internal/folder", Reason: "the package holds files that the records of more than one domain name: ST-FXD, ST-FXS"},
			CodeFault{Rule: "ST-STD-CODE-R01", Key: "internal/mixed", Path: "internal/mixed", Reason: "the package holds files that the records of more than one domain name: ST-FXD, ST-FXS"},
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

// Handler is declared in another file: this file declares only its method Handle.
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
			"service": `{"id":"ST-FXD-DS01-SV","title":"other.Decision","file":"internal/fx/fx.go"}
{"id":"ST-FXD-DS02-SV","title":"fx.Handler","file":"internal/fx/fx.go"}
`,
			"value_type": `{"id":"ST-VO01-VT","title":"fx.Store","file":"internal/fx/fx.go"}`,
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
			CodeFault{Rule: "ST-STD-CODE-R02", Key: "ST-FXD-CMD02-H", Path: file, Reason: "ST-FXD-CMD02-H names fx.Handler.Missing, which the file does not declare"},
			CodeFault{Rule: "ST-STD-CODE-R02", Key: "ST-FXD-DS01-SV", Path: file, Reason: `ST-FXD-DS01-SV is titled "other.Decision", which is not an identifier of package fx`},
			CodeFault{Rule: "ST-STD-CODE-R02", Key: "ST-FXD-DS02-SV", Path: file, Reason: "ST-FXD-DS02-SV names fx.Handler, which the file does not declare"},
			CodeFault{Rule: "ST-STD-CODE-R02", Key: "ST-FXD-EN02-TB", Path: file, Reason: `ST-FXD-EN02-TB is titled "proof-run.json", which is not an identifier of package fx`},
			CodeFault{Rule: "ST-STD-CODE-R02", Key: "ST-FXD-EN03-DM", Path: file, Reason: "ST-FXD-EN03-DM names fx.Decision.Branch, which the file does not declare"},
			CodeFault{Rule: "ST-STD-CODE-R02", Key: "ST-FXD-EV01-ET", Path: file, Reason: `ST-FXD-EV01-ET is titled "Decided", which is not an identifier of package fx`},
			CodeFault{Rule: "ST-STD-CODE-R02", Key: "ST-FXD-SP02-PR", Path: file, Reason: "ST-FXD-SP02-PR names fx.isMissing, which the file does not declare"},
			CodeFault{Rule: "ST-STD-CODE-R02", Key: "ST-FXD-SP03-PR", Path: file, Reason: "ST-FXD-SP03-PR names fx.isElsewhere, which the file does not declare"},
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
			"value_type": `{"id":"ST-VO01-VT","title":"shared.Value","file":"internal/shared/shared.go"}
{"id":"ST-VO02-VT","title":"values.Value","file":"internal/values/values.go"}
`,
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
			"internal/supply/ext_test.go":       imports("supply_test", "internal/supply", "internal/generate"),
			"internal/assure/bridged.go":        imports("assure", "internal/bridge"),
			"internal/bridge/bridge.go":         imports("bridge", "internal/shared", "internal/declare"),
			"internal/bridge/bridge_test.go":    imports("bridge", "internal/build"),
			"internal/declare/shared.go":        imports("declare", "internal/values"),
			"internal/values/values.go":         imports("values", "internal/supply"),
			"internal/supply/supply.go":         imports("supply", "internal/declare", "internal/shared"),
			"internal/generate/generate.go":     imports("generate", "internal/declare", "internal/supply"),
			"internal/build/build.go":           imports("build", "internal/generate", "internal/declare"),
			"internal/distribute/distribute.go": imports("distribute", "internal/build"),
			"cmd/fx/main.go": imports("main", "internal/assure", "internal/declare", "internal/supply",
				"internal/generate", "internal/build", "internal/distribute"),
		})
		faults, err := importAgainstOrder(spec, tree)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := importAgainstOrder(spec, codeTree(map[string]string{"internal/declare/declare.go": "package declare\n"})); err == nil {
			t.Fatal("a tree with no go.mod gave no error, so its imports went unchecked")
		}
		expectFaults(t, faults,
			CodeFault{Rule: "ST-STD-CODE-R03", Key: "internal/assure imports internal/declare", Path: "internal/assure", Reason: "the package reaches internal/declare through internal/bridge, of no domain, against the domain order: Assuring (ST-FXA) may not import Declaring (ST-FXD)"},
			CodeFault{Rule: "ST-STD-CODE-R03", Key: "internal/build imports internal/declare", Path: "internal/build", Reason: "the package imports internal/declare against the domain order: Building (ST-FXB) may not import Declaring (ST-FXD)"},
			CodeFault{Rule: "ST-STD-CODE-R03", Key: "internal/declare imports internal/generate", Path: "internal/declare", Reason: "the package imports internal/generate against the domain order: Declaring (ST-FXD) may not import Generating (ST-FXG)"},
			CodeFault{Rule: "ST-STD-CODE-R03", Key: "internal/declare imports internal/supply", Path: "internal/declare", Reason: "the package imports internal/supply, and reaches it through internal/values, of no domain, against the domain order: Declaring (ST-FXD) may not import Supplying (ST-FXS)"},
			CodeFault{Rule: "ST-STD-CODE-R03", Key: "internal/supply imports internal/generate", Path: "internal/supply", Reason: "the package imports internal/generate against the domain order: Supplying (ST-FXS) may not import Generating (ST-FXG)"},
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
{"id":"ST-FXD-SP06-T","state":"automated","file":"internal/build/build_test.go"}
{"id":"ST-FXD-SP07-T","state":"automated","file":"internal/fx/const_test.go"}
{"id":"ST-FXD-SP08-T","state":"automated","file":"internal/fx/runner_test.go"}
`,
			"use_case_test": `{"id":"ST-FXD-UC01-R01-T01","state":"automated","file":"scripts/test_fx.py"}
{"id":"ST-FXD-UC01-R02-T01","state":"automated","file":"scripts/test_named.py"}
`,
		})
		tree := codeTree(map[string]string{
			"lathe.config.json":                    `{"schema": 5, "data": "data", "tests": ["**/*_test.go", "scripts/test_*.py"]}`,
			"internal/fx/fx_test.go":               fixtureTest("ST-FXD-SP01-T") + "\n// ST-FXD-SP02-T is only in this comment.\n",
			"internal/fx/helpers_test.go":          "package fx\n\n// Helpers of the fx tests, once ST-FXD-SP01-T.\nconst fixture = \"ST-FXD-SP01-T\"\n\nfunc helper() string { return fixture }\n",
			"internal/fx/table_test.go":            "package fx\n\nimport \"testing\"\n\nfunc TestTable(t *testing.T) {\n\tfor _, name := range []string{\"ST-FXD-SP01-T holds\"} {\n\t\tt.Run(name, func(t *testing.T) {})\n\t}\n}\n",
			"internal/fx/joined_test.go":           fixtureTest("ST-FXD-SP05-T-holds", "ST-FXD-SP05-Tx"),
			"internal/fx/const_test.go":            "package fx\n\nimport \"testing\"\n\nconst proof = \"ST-FXD-SP07-T holds\"\n\nfunc TestConst(t *testing.T) {\n\tt.Run(proof, func(t *testing.T) {})\n}\n",
			"internal/fx/runner_test.go":           "package fx\n\nimport \"testing\"\n\ntype runner struct{}\n\nfunc (runner) Run(name string, times int) {}\n\nfunc TestRunner(t *testing.T) {\n\trunner{}.Run(\"ST-FXD-SP08-T holds\", 1)\n}\n",
			"internal/fx/unknown_test.go":          fixtureTest("ST-FXD-SP09-T"),
			"internal/fx/none_test.go":             fixtureTest("a case with no ID"),
			"internal/fx/fx.go":                    "package fx\n",
			"internal/build/build_test.go":         "package build\n\nimport \"testing\"\n\nfunc TestBuild(t *testing.T) {\n\tname := \"holds\"\n\tt.Run(\"ST-FXD-SP06-T \"+name, func(t *testing.T) {})\n}\n",
			"internal/.cache/x_test.go":            fixtureTest(),
			"scripts/test_fx.py":                   "import unittest\n\n\nclass Fx(unittest.TestCase):\n    def test_cases(self):\n        with self.subTest('ST-FXD-UC01-R01-T01 holds'):\n            pass\n",
			"scripts/test_named.py":                "# ST-FXD-UC01-R02-T01 is only in a comment.\nNAME = \"ST-FXD-UC01-R02-T01\"\n\n\ndef test_st_fxd_uc01_r02_t01():\n    pass\n",
			"scripts/fx.py":                        "print('no test here')\n",
			".github/fx_test.go":                   fixtureTest(),
			"node_modules/x/x_test.go":             fixtureTest(),
			"examples/other/lathe.config.json":     "{}",
			"examples/other/internal/x/x_test.go":  fixtureTest(),
			"examples/service/fills/check_test.go": fixtureTest(),
		})
		none := "the test file names no test record's ID in a test or subtest name"
		carries := func(id string) string {
			return "no test or subtest name in the file carries the ID of automated test record " + id
		}
		fault := func(key, path, reason string) CodeFault {
			return CodeFault{Rule: "ST-STD-CODE-R04", Key: key, Path: path, Reason: reason}
		}
		expectFaults(t, testWithoutID(spec, tree),
			fault("examples/service/fills/check_test.go", "examples/service/fills/check_test.go", none),
			fault("internal/.cache/x_test.go", "internal/.cache/x_test.go", none),
			fault("internal/fx/fx_test.go ST-FXD-SP02-T", "internal/fx/fx_test.go", carries("ST-FXD-SP02-T")),
			fault("internal/fx/gone_test.go ST-FXD-SP04-T", "internal/fx/gone_test.go", "automated test record ST-FXD-SP04-T names this file, which the tree does not hold"),
			fault("internal/fx/helpers_test.go", "internal/fx/helpers_test.go", none),
			fault("internal/fx/joined_test.go", "internal/fx/joined_test.go", none),
			fault("internal/fx/joined_test.go ST-FXD-SP05-T", "internal/fx/joined_test.go", carries("ST-FXD-SP05-T")),
			fault("internal/fx/none_test.go", "internal/fx/none_test.go", none),
			fault("internal/fx/runner_test.go", "internal/fx/runner_test.go", none),
			fault("internal/fx/runner_test.go ST-FXD-SP08-T", "internal/fx/runner_test.go", carries("ST-FXD-SP08-T")),
			fault("internal/fx/table_test.go", "internal/fx/table_test.go", none),
			fault("internal/fx/unknown_test.go", "internal/fx/unknown_test.go", none),
			fault("scripts/test_named.py", "scripts/test_named.py", none),
			fault("scripts/test_named.py ST-FXD-UC01-R02-T01", "scripts/test_named.py", carries("ST-FXD-UC01-R02-T01")),
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
			"lathe.config.json":                      `{"schema": 5, "data": "data", "tests": ["**/*_test.go", "scripts/test_*.py"]}`,
			"internal/fx/named.go":                   "package fx\n",
			"internal/fx/lost.go":                    "package fx\n",
			"internal/fx/contract.go":                "package fx\n",
			"internal/fx/lost_test.go":               "package fx\n",
			"internal/fx/zz_generated.go":            "// Code generated by fx. DO NOT EDIT.\n\npackage fx\n",
			"internal/fx/README.md":                  "# fx\n",
			"internal/fx/fixtures/helper.go":         "package fixtures\n",
			"internal/fx/test/helper.go":             "package test\n",
			"internal/fx/web.config.js":              "export default {}\n",
			"internal/build/build.go":                "package build\n",
			"internal/fx/out/out.go":                 "package out\n",
			"internal/.hidden/hidden.go":             "package hidden\n",
			"internal/sub/lathe.config.json":         "{}",
			"internal/sub/sub.go":                    "package sub\n",
			"cmd/dist/main.go":                       "package main\n",
			"registry/components/web/generate.go":    "package web\n",
			"registry/components/web/sub/emit.ts":    "export {}\n",
			"registry/components/api/component.yaml": "name: api\n",
			"registry/components/api/generate.go":    "package api\n",
			"scripts/tools/build.py":                 "print('build')\n",
			"scripts/tools/run.sh":                   "#!/bin/sh\n",
			"scripts/lost.py":                        "print('lost')\n",
			"scripts/lost.sh":                        "#!/bin/sh\n",
			"scripts/test_fx.py":                     "print('a test')\n",
			"examples/fx/main.go":                    "package main\n",
			"node_modules/x/index.js":                "export {}\n",
		})
		fault := func(path string) CodeFault {
			return CodeFault{Rule: "ST-STD-CODE-R05", Key: path, Path: path, Reason: "no technical record names the file, directly or through a folder"}
		}
		expectFaults(t, fileWithoutRecord(spec, tree),
			fault("cmd/dist/main.go"),
			fault("internal/.hidden/hidden.go"),
			fault("internal/build/build.go"),
			fault("internal/fx/contract.go"),
			fault("internal/fx/fixtures/helper.go"),
			fault("internal/fx/lost.go"),
			fault("internal/fx/out/out.go"),
			fault("internal/fx/test/helper.go"),
			fault("internal/fx/web.config.js"),
			fault("internal/fx/zz_generated.go"),
			fault("internal/sub/sub.go"),
			fault("registry/components/api/generate.go"),
			fault("scripts/lost.py"),
			fault("scripts/lost.sh"),
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

func viaLocal(d Decision) bool { ok := false; return ok }

func viaHelper(d Decision) error { return notImplemented() }

func notImplemented() error { return errors.New("not implemented") }

func viaSprintf(d Decision) error { return errors.New(fmt.Sprintf("not %s", "yet")) }

func ifConstant(d Decision) bool { if true { return false }; return true }

func unnamed(Decision) bool { return true }

func blank(_ Decision) bool { return true }

func fieldNamedLikeInput(d Decision) string { var other Decision; return other.d }

func blankRead(d Decision) bool { _ = d; return true }

func noInputs() bool { return true }

func noInputsPanics() error { panic("not written yet") }

func noInputsComputes() Decision { return newDecision() }

func newDecision() Decision { return Decision{ID: fmt.Sprint(1)} }

func fieldKey(ID string) Decision { return Decision{ID: "fixed"} }

func mapKey(d string) map[string]int { return map[string]int{d: 1} }
`

func TestStubBuilt(t *testing.T) {
	t.Run("ST-ASR-SP10-T Stub counted as built is found in a tree built for it, and not in one that follows the rule", func(t *testing.T) {
		builtFunctions := []string{"fx.isGood", "fx.Handler.Count", "fx.reads", "fx.returnsInput", "fx.panicsWithInput", "fx.delegates", "fx.blankRead", "fx.noInputsComputes", "fx.mapKey", "fx.missing"}
		stubs := []string{"fx.alwaysTrue", "fx.Handler.Handle", "fx.notYet", "fx.notYetEither", "fx.empty", "fx.viaLocal", "fx.viaHelper", "fx.viaSprintf", "fx.ifConstant", "fx.unnamed", "fx.blank", "fx.fieldNamedLikeInput", "fx.noInputs", "fx.noInputsPanics", "fx.fieldKey"}
		kinds := []string{"predicate", "command_handler", "query_handler", "service"}
		tables := make(map[string]string)
		var want []CodeFault
		for i, title := range append(append([]string(nil), builtFunctions...), stubs...) {
			id := fmt.Sprintf("ST-FXD-SP%02d-PR", i+1)
			kind := kinds[i%len(kinds)]
			tables[kind] += fmt.Sprintf(`{"id":%q,"title":%q,"file":"internal/fx/fx.go"}`+"\n", id, title)
			if i >= len(builtFunctions) {
				why := ", whose body reads none of its parameters or its receiver"
				if strings.HasPrefix(title, "fx.noInputs") {
					why = ", which has no input and whose body only returns constants or panics"
				}
				want = append(want, CodeFault{Rule: "ST-STD-CODE-R06", Key: id, Path: "internal/fx/fx.go", Reason: id + " names " + title + why})
			}
		}
		tables["value_type"] = `{"id":"ST-VO01-VT","title":"fx.alwaysTrue","file":"internal/fx/fx.go"}`
		spec := codeSpec(t, tables)
		tree := codeTree(map[string]string{"internal/fx/fx.go": stubFile})
		sortFaults(want)
		expectFaults(t, stubBuilt(spec, tree), want...)
	})
}

// codeRepository makes a git repository that holds the files, by path and text, with no commit yet, and returns
// its folder.
func codeRepository(t *testing.T, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repository")
	for name, text := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, root, "init", "-q")
	return root
}

// commitAll commits every file of the working tree and returns the commit.
func commitAll(t *testing.T, root, message string) string {
	t.Helper()
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", message)
	return strings.TrimSpace(gitIn(t, root, "rev-parse", "HEAD"))
}

// readFaults reads the spec and the tree at the commit from git objects and gives their code faults.
func readFaults(t *testing.T, root, commit string) []CodeFault {
	t.Helper()
	spec, err := ReadCommittedSpec(root, commit)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := ReadCommittedTree(root, commit)
	if err != nil {
		t.Fatal(err)
	}
	faults, err := CodeFaults(spec, tree)
	if err != nil {
		t.Fatal(err)
	}
	return faults
}

func TestFindTheCodeFaults(t *testing.T) {
	t.Run("ST-ASR-DS04-T01 The code faults of a commit come from its tree and spec alone, keyed and ordered, and two readings agree", func(t *testing.T) {
		root := codeRepository(t, map[string]string{
			"go.mod":            "module example.com/fx\n",
			"lathe.config.json": `{"schema": 5, "data": "data", "tests": ["**/*_test.go"]}` + "\n",
			"data/domain.jsonl": fixtureDomains,
			"data/predicate.jsonl": `{"id":"ST-FXD-SP01-PR","title":"mixed.isDeclared","file":"internal/mixed/declared.go"}
{"id":"ST-FXG-SP01-PR","title":"mixed.isGenerated","file":"internal/mixed/generated.go"}
{"id":"ST-FXS-SP01-PR","title":"mixed.isSupplied","file":"internal/mixed/supplied.go"}
{"id":"ST-FXS-SP02-PR","title":"mixed.isMissing","file":"internal/mixed/supplied.go"}
`,
			"data/specification_test.jsonl": `{"id":"ST-FXD-SP01-T","state":"automated","file":"internal/mixed/mixed_test.go"}` + "\n",
			"internal/mixed/declared.go":    "package mixed\n\nfunc isDeclared(name string) bool { return name != \"\" }\n",
			"internal/mixed/generated.go":   "package mixed\n\nfunc isGenerated(name string) bool { return len(name) > 1 }\n",
			"internal/mixed/supplied.go":    "package mixed\n\nfunc isSupplied(name string) bool { return true }\n",
			"internal/mixed/mixed_test.go":  fixtureTest("ST-FXD-SP01-T"),
			"internal/lost/lost.go":         "package lost\n",
			"data/data_model.jsonl": `{"id":"ST-FXD-EN01-DM","title":"plan.Plan","file":"internal/plan/plan.go"}
{"id":"ST-FXS-EN01-DM","title":"supply.Supply","file":"internal/supply/supply.go"}
`,
			"data/value_type.jsonl": `{"id":"ST-VO01-VT","title":"bridgea.Value","file":"internal/bridgea/bridgea.go"}
{"id":"ST-VO02-VT","title":"bridgeb.Value","file":"internal/bridgeb/bridgeb.go"}
`,
			"internal/plan/plan.go":       "package plan\n\nimport _ \"example.com/fx/internal/bridgea\"\n\ntype Plan struct{}\n",
			"internal/bridgea/bridgea.go": "package bridgea\n\nimport _ \"example.com/fx/internal/supply\"\n\ntype Value int\n",
			"internal/bridgeb/bridgeb.go": "package bridgeb\n\nimport _ \"example.com/fx/internal/supply\"\n\ntype Value int\n",
			"internal/supply/supply.go":   "package supply\n\ntype Supply struct{}\n",
		})
		first := commitAll(t, root, "first")
		faults := readFaults(t, root, first)
		keys := make([]string, 0, len(faults))
		for _, fault := range faults {
			keys = append(keys, fault.Rule+" "+fault.Key)
		}
		want := []string{
			"ST-STD-CODE-R01 internal/mixed",
			"ST-STD-CODE-R02 ST-FXS-SP02-PR",
			"ST-STD-CODE-R03 internal/plan imports internal/supply",
			"ST-STD-CODE-R05 internal/lost/lost.go",
			"ST-STD-CODE-R06 ST-FXS-SP01-PR",
		}
		if !reflect.DeepEqual(keys, want) {
			t.Fatalf("faults at the first commit:\n%s\nwant one per rule and key, in rule then key order: %v", faultLines(faults), want)
		}

		// The working tree changes, with a new file and a stub made real, but the commit does not: a second
		// reading of the commit gives the same faults.
		for name, text := range map[string]string{
			"internal/extra/extra.go":    "package extra\n",
			"internal/mixed/supplied.go": "package mixed\n\nfunc isSupplied(name string) bool { return name == \"s\" }\n",
			"data/predicate.jsonl":       "",
		} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if again := readFaults(t, root, first); !reflect.DeepEqual(again, faults) {
			t.Fatalf("a second reading of the same commit gave other faults:\n%s\nfirst reading:\n%s", faultLines(again), faultLines(faults))
		}

		// A later commit takes one of the package's three domains away, renames the identifier a record names
		// to another that the file does not declare either, and reaches the same package through another package of
		// no domain. Each fault stands, with the same key and another reason.
		gitIn(t, root, "checkout", "-q", "--", ".")
		gitIn(t, root, "clean", "-q", "-f", "-d")
		if err := os.Remove(filepath.Join(root, "internal/mixed/generated.go")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "data/predicate.jsonl"), []byte(`{"id":"ST-FXD-SP01-PR","title":"mixed.isDeclared","file":"internal/mixed/declared.go"}
{"id":"ST-FXS-SP01-PR","title":"mixed.isSupplied","file":"internal/mixed/supplied.go"}
{"id":"ST-FXS-SP02-PR","title":"mixed.isAbsent","file":"internal/mixed/supplied.go"}
`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "internal/plan/plan.go"), []byte("package plan\n\nimport _ \"example.com/fx/internal/bridgeb\"\n\ntype Plan struct{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		second := commitAll(t, root, "second")
		later := readFaults(t, root, second)
		if len(later) != len(faults) {
			t.Fatalf("faults at the second commit:\n%s\nwant the same five faults", faultLines(later))
		}
		for i := range later {
			if later[i].Rule != faults[i].Rule || later[i].Key != faults[i].Key {
				t.Fatalf("fault %d changed its identity: %s, then %s", i, faults[i], later[i])
			}
		}
		for i := 0; i < 3; i++ {
			if later[i].Reason == faults[i].Reason {
				t.Fatalf("the %s fault kept its reason %q; the fixture must change it", later[i].Rule, later[i].Reason)
			}
		}
	})
}

// knownCodeFaultsRecord is the tooling record that names the committed list of the code-standard faults STEGO's
// own tree has, which the rule tests hold the tree to.
const knownCodeFaultsRecord = "ST-ASR-TOOL03"

// readKnownCodeFaults reads the list of known faults: one fault per line, as rule, key, path and reason with a tab
// between them, in the order CodeFaults gives them, each fault once. A line that starts with # is a comment. It
// refuses a line of another form, a line out of order and a line that is there twice.
func readKnownCodeFaults(data []byte) ([]CodeFault, error) {
	var faults []CodeFault
	for number, text := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("line %d has %d fields, not rule, key, path and reason: %q", number+1, len(fields), text)
		}
		fault := CodeFault{Rule: fields[0], Key: fields[1], Path: fields[2], Reason: fields[3]}
		if last := len(faults) - 1; last >= 0 && !faultBefore(faults[last], fault) {
			if faults[last] == fault {
				return nil, fmt.Errorf("line %d is there twice: %q", number+1, text)
			}
			return nil, fmt.Errorf("line %d is out of order: the list is sorted by rule, then key, path and reason: %q", number+1, text)
		}
		faults = append(faults, fault)
	}
	return faults, nil
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

// ratchetBaseVariable names the environment variable that gives the base commit of the ratchet, such as a pull
// request's base, which the proof run sets after it fetches that commit.
const ratchetBaseVariable = "ASSURE_RATCHET_BASE"

// ratchetBase is the commit whose list of known faults the ratchet compares with: the commit that
// ASSURE_RATCHET_BASE names when it is set, else HEAD's first parent. It gives "" and why when there is no base to
// compare with, and skip is true when the rule tests must be skipped rather than pass with the ratchet unchecked:
// in a shallow clone that leaves the parent out, such as CI's depth-1 checkout, with no base given. A first commit
// has no parent, and there the ratchet is not checked.
func ratchetBase(t *testing.T, root string) (commit, why string, skip bool) {
	t.Helper()
	if base := os.Getenv(ratchetBaseVariable); base != "" {
		out, err := git(root, "rev-parse", "--verify", "--quiet", base+"^{commit}")
		if err != nil {
			t.Fatalf("%s=%s names no commit the repository holds: %v", ratchetBaseVariable, base, err)
		}
		return strings.TrimSpace(string(out)), "", false
	}
	if out, err := git(root, "rev-parse", "--verify", "--quiet", "HEAD^1^{commit}"); err == nil {
		return strings.TrimSpace(string(out)), "", false
	}
	shallow, err := git(root, "rev-parse", "--is-shallow-repository")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(shallow)) == "true" {
		return "", "the clone is shallow and does not hold HEAD's first parent, and " + ratchetBaseVariable +
			" gives no base, so the ratchet cannot be checked: fetch the parent (fetch-depth: 2) or set " + ratchetBaseVariable, true
	}
	return "", "HEAD has no parent, so the ratchet is not checked", false
}

// baseKnownCodeFaults reads the list of known faults at the base commit: the rule and key of each fault, and
// whether there is a list. There is none when the base's spec has no record of the list or the base has no file
// for it, as before the list was made.
func baseKnownCodeFaults(t *testing.T, root, base string) (map[[2]string]bool, bool) {
	t.Helper()
	spec, err := ReadCommittedSpec(root, base)
	if err != nil {
		t.Fatal(err)
	}
	file := ""
	for _, record := range spec.Technical {
		if record.ID == knownCodeFaultsRecord {
			file = record.File
		}
	}
	if file == "" {
		return nil, false
	}
	if listed, err := git(root, "ls-tree", "--name-only", base, "--", file); err != nil || len(listed) == 0 {
		return nil, false
	}
	data, err := git(root, "cat-file", "blob", base+":"+file)
	if err != nil {
		t.Fatal(err)
	}
	keys := make(map[[2]string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) >= 2 {
			keys[[2]string{fields[0], fields[1]}] = true
		}
	}
	return keys, true
}

func TestLatheTechnicalKinds(t *testing.T) {
	t.Run("ST-ASR-SP09-T The code standard reads every kind of Lathe's own technical stage that has a file column, as the pinned Lathe's meta model lists them", func(t *testing.T) {
		lathe := latheCLI(t)
		root := codeRepository(t, map[string]string{
			"lathe.config.json":  `{"schema": 5, "data": "data"}` + "\n",
			"data/product.jsonl": `{"id":"FX","title":"Fixture","description":"What Fixture is, who it is for, and what it must do."}` + "\n",
		})
		command := exec.Command(lathe, "meta-model", "get-meta-model", "--json", "-")
		command.Dir = root
		command.Stdin = strings.NewReader("{}")
		output, err := command.Output()
		if err != nil {
			t.Fatalf("lathe meta-model get-meta-model: %v", err)
		}
		var model struct {
			Kinds []struct {
				Kind       string  `json:"kind"`
				Stage      *string `json:"stage"`
				DeclaredBy *string `json:"declared_by"`
			} `json:"kinds"`
			Columns map[string][]struct {
				Name string `json:"name"`
			} `json:"columns"`
		}
		if err := json.Unmarshal(output, &model); err != nil {
			t.Fatalf("the meta model is not JSON: %v", err)
		}
		var withFile []string
		for _, kind := range model.Kinds {
			if kind.DeclaredBy != nil || kind.Stage == nil || *kind.Stage != "technical-specification" {
				continue
			}
			for _, column := range model.Columns[kind.Kind] {
				if column.Name == "file" {
					withFile = append(withFile, kind.Kind)
					break
				}
			}
		}
		sort.Strings(withFile)
		if len(withFile) == 0 {
			t.Fatal("the meta model lists no technical kind with a file column")
		}
		if got := latheTechnicalKinds(); !reflect.DeepEqual(got, withFile) {
			t.Fatalf("the code standard reads the kinds %v, but Lathe's technical kinds with a file column are %v", got, withFile)
		}
	})
}

// TestOwnTree runs each predicate over STEGO's own tree and spec at the commit under test, HEAD, read from git
// objects, never from the working tree. The Product owner decided (ST-ASR-Q02) that the committed list of known
// faults (ST-ASR-TOOL03) stays until it is empty. Each rule's test proves three things:
//   - the tree has no fault of the rule outside the list at the same commit;
//   - the list holds no fault of the rule that the tree no longer has;
//   - the ratchet: the list holds no fault of the rule, by rule and key, that the list at the base commit does not
//     hold. The base is the commit ASSURE_RATCHET_BASE names, or else HEAD's first parent. A fault whose reason
//     changes while its key stands, such as a package that goes from three domains to two, passes the ratchet.
//
// Where the base is HEAD's first parent, the ratchet holds for the commit under test only: for the last commit of
// a push, or, on a pull request whose merge ref is checked out, for the whole change against the base branch's
// tip. In a shallow clone that does not hold the parent, with no base given, each rule test is skipped, as it
// cannot check the ratchet. A first commit has no parent; the test logs that the ratchet is not checked. The change
// gate compares a change with its base (ST-ASR-UC02-R05) in any case.
//
// The list must be sorted and hold each fault once. CodeFaults must give the same faults as the predicate.
//
// The test reads git through a subprocess, which go test cannot see, so go test can give a cached result for a
// new commit. Run it with -count=1.
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
	known, listError := readKnownCodeFaults(data)
	base, why, skip := ratchetBase(t, root)
	var baseKeys map[[2]string]bool
	ratchet := false
	if base != "" {
		baseKeys, ratchet = baseKnownCodeFaults(t, root, base)
		if !ratchet {
			why = "the base commit " + base + " has no list of known faults, so the ratchet is not checked"
		}
	}
	if why != "" && !skip {
		t.Log(why)
	}
	all, err := CodeFaults(spec, tree)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, rule string, got []CodeFault) {
		t.Helper()
		if skip {
			t.Skip(why)
		}
		if listError != nil {
			t.Fatalf("the list of known faults (%s): %v", list.File, listError)
		}
		var listed, fromAll []CodeFault
		for _, fault := range known {
			if fault.Rule == rule {
				listed = append(listed, fault)
			}
		}
		for _, fault := range all {
			if fault.Rule == rule {
				fromAll = append(fromAll, fault)
			}
		}
		if !reflect.DeepEqual(got, fromAll) {
			t.Fatalf("CodeFaults gives other faults than the predicate:\n%s\nthe predicate:\n%s", faultLines(fromAll), faultLines(got))
		}
		for i := 1; i < len(got); i++ {
			if !faultBefore(got[i-1], got[i]) {
				t.Fatalf("the faults are not in order, or a fault is there twice:\n%s", faultLines(got))
			}
		}
		var problems []string
		if added := difference(got, listed); len(added) > 0 {
			problems = append(problems, fmt.Sprintf("%d faults that the list of known faults (%s) does not hold:\n%s", len(added), list.File, faultLines(added)))
		}
		if gone := difference(listed, got); len(gone) > 0 {
			problems = append(problems, fmt.Sprintf("%d listed faults are gone; remove them from %s:\n%s", len(gone), list.File, faultLines(gone)))
		}
		var grown []CodeFault
		for _, fault := range listed {
			if ratchet && !baseKeys[[2]string{fault.Rule, fault.Key}] {
				grown = append(grown, fault)
			}
		}
		if len(grown) > 0 {
			problems = append(problems, fmt.Sprintf("%d faults of %s, by rule and key, are not in the list at the base commit %s; the list may only get shorter, so fix these faults:\n%s", len(grown), list.File, base, faultLines(grown)))
		}
		if len(problems) > 0 {
			t.Fatal(strings.Join(problems, "\n"))
		}
	}
	t.Run("ST-STD-CODE-R01-T01 STEGO's own tree holds the rule: package of two domains is not found in it", func(t *testing.T) {
		check(t, RuleOneDomainPerPackage, packageOfTwoDomains(spec, tree))
	})
	t.Run("ST-STD-CODE-R02-T01 STEGO's own tree holds the rule: identifier missing from its file is not found in it", func(t *testing.T) {
		check(t, RuleNamedAsRecordNames, identifierMissing(spec, tree))
	})
	t.Run("ST-STD-CODE-R03-T01 STEGO's own tree holds the rule: import against the order is not found in it", func(t *testing.T) {
		faults, err := importAgainstOrder(spec, tree)
		if err != nil {
			t.Fatal(err)
		}
		check(t, RuleImportsFollowOrder, faults)
	})
	t.Run("ST-STD-CODE-R04-T01 STEGO's own tree holds the rule: test without its id is not found in it", func(t *testing.T) {
		check(t, RuleTestNamesWhatItDoes, testWithoutID(spec, tree))
	})
	t.Run("ST-STD-CODE-R05-T01 STEGO's own tree holds the rule: source file without a record is not found in it", func(t *testing.T) {
		check(t, RuleNoFileWithoutRecord, fileWithoutRecord(spec, tree))
	})
	t.Run("ST-STD-CODE-R06-T01 STEGO's own tree holds the rule: stub counted as built is not found in it", func(t *testing.T) {
		check(t, RuleStubIsNotBuilt, stubBuilt(spec, tree))
	})
}
