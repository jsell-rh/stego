package fill

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
)

const (
	closureModule   = "example.com/service"
	closureContract = closureModule + "/out/commands"
	closureFill     = closureModule + "/fills/reserve-and-charge"
	closureHelper   = "example.com/dbhelper"
)

// closureTree makes the module of a service with the command reserve-and-charge: its command slot contract, which
// imports database/sql itself, the fill reserve-and-charge with the imports given and a test file that imports
// os/exec, the listed fill module example.com/dbhelper, whose package imports database/sql, and a local copy of
// github.com/jackc/pgx/v5, all bound by replace, so go list needs no network.
func closureTree(t *testing.T, fillImports ...string) string {
	t.Helper()
	return closureTreeWith(t, nil, fillImports...)
}

// closureTreeWith is closureTree with extra files of the fill, by name in its folder.
func closureTreeWith(t *testing.T, extra map[string]string, fillImports ...string) string {
	t.Helper()
	root := t.TempDir()
	var body strings.Builder
	body.WriteString("package reserveandcharge\n\nimport (\n")
	for _, path := range fillImports {
		body.WriteString("\t_ \"" + path + "\"\n")
	}
	body.WriteString("\t_ \"" + closureContract + "\"\n)\n")
	files := map[string]string{
		"go.mod": "module " + closureModule + "\n\ngo 1.26\n\nrequire (\n\t" + closureHelper + " v0.0.0\n" +
			"\tgithub.com/jackc/pgx/v5 v5.0.0\n)\n\nreplace " + closureHelper + " => ./mods/dbhelper\n\n" +
			"replace github.com/jackc/pgx/v5 => ./mods/pgx\n",
		"out/commands/commands.go":              "package commands\n\nimport _ \"database/sql\"\n",
		"fills/reserve-and-charge/fill.go":      body.String(),
		"fills/reserve-and-charge/fill_test.go": "package reserveandcharge\n\nimport _ \"os/exec\"\n",
		"mods/dbhelper/go.mod":                  "module " + closureHelper + "\n\ngo 1.26\n",
		"mods/dbhelper/helper.go":               "package dbhelper\n\nimport _ \"database/sql\"\n",
		"mods/pgx/go.mod":                       "module github.com/jackc/pgx/v5\n\ngo 1.26\n",
		"mods/pgx/pgx.go":                       "package pgx\n",
	}
	for name, text := range extra {
		files["fills/reserve-and-charge/"+name] = text
	}
	for name, text := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// treeFiles lists every file under root.
func treeFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// qualifyCommandFill runs the compiler's fill qualifier on the command fill reserve-and-charge of root, with
// example.com/dbhelper listed for fills, and fails when the check writes a file.
func qualifyCommandFill(t *testing.T, root string) []compiler.FillFault {
	t.Helper()
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	if pinned := os.Getenv("STEGO_TEST_PINNED_GO"); pinned != "" {
		before := GoCommand
		GoCommand = pinned
		t.Cleanup(func() { GoCommand = before })
	}
	before := treeFiles(t, root)
	var qualifier compiler.FillQualifier = QualificationFaults
	faults, err := qualifier(compiler.FillCheck{ProjectDir: root, Module: closureModule, FillModules: []string{closureHelper},
		CommandFills: []compiler.CommandFill{{Fill: "reserve-and-charge", Command: "reserve-and-charge", Path: "commands[0].fill", Contract: closureContract}}})
	if err != nil {
		t.Fatal(err)
	}
	if after := treeFiles(t, root); !slices.Equal(before, after) {
		t.Fatalf("the check wrote files: before %v, after %v", before, after)
	}
	return faults
}

func TestCommandFillClosure(t *testing.T) {
	t.Run("ST-SUP-RC32-OC03-T01 A command fill that reaches a database package through a module is refused", func(t *testing.T) {
		cases := []struct {
			name, imports, refused, chain string
		}{
			{"a listed module that imports database/sql", closureHelper, "database/sql",
				closureFill + " (fill.go) -> " + closureHelper + " (helper.go) -> database/sql"},
			{"a driver module", "github.com/jackc/pgx/v5", "github.com/jackc/pgx/v5",
				closureFill + " (fill.go) -> github.com/jackc/pgx/v5"},
			{"os/exec", "os/exec", "os/exec", closureFill + " (fill.go) -> os/exec"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				faults := qualifyCommandFill(t, closureTree(t, c.imports))
				if len(faults) != 1 {
					t.Fatalf("the check gave %d faults %v, want one", len(faults), faults)
				}
				f := faults[0]
				if f.Found != "reserve-and-charge" || f.Path != "commands[0].fill" || f.Rule != "fill-imports" {
					t.Errorf("the fault %+v does not name the fill and its place", f)
				}
				for _, want := range []string{`fill "reserve-and-charge"`, "import " + c.refused + ",", c.chain} {
					if !strings.Contains(f.Message, want) {
						t.Errorf("the fault %q does not name %q", f.Message, want)
					}
				}
			})
		}
	})
	t.Run("ST-SUP-RC32-OC03-T01 A file that go list does not build for this platform or these tags is checked too", func(t *testing.T) {
		faults := qualifyCommandFill(t, closureTreeWith(t, map[string]string{
			"evil_windows.go": "package reserveandcharge\n\nimport _ \"os/exec\"\n",
			"tagged.go":       "//go:build stego_secret\n\npackage reserveandcharge\n\nimport _ \"github.com/jackc/pgx/v5\"\n",
		}, "fmt"))
		var text []string
		for _, f := range faults {
			text = append(text, f.Message)
		}
		joined := strings.Join(text, "\n")
		for _, want := range []string{"the file evil_windows.go imports os/exec, which a fill may not import", "the file tagged.go imports github.com/jackc/pgx/v5, which a fill may not import"} {
			if !strings.Contains(joined, want) {
				t.Errorf("the faults %q do not hold %q", joined, want)
			}
		}
		if len(faults) != 2 {
			t.Errorf("the check gave %d faults, want 2", len(faults))
		}
	})
	t.Run("ST-SUP-RC32-OC03-T02 Files that the go tool never builds are not read", func(t *testing.T) {
		faults := qualifyCommandFill(t, closureTreeWith(t, map[string]string{
			"testdata/broken.go": "this is not Go\n",
			"testdata/exec.go":   "package x\n\nimport _ \"os/exec\"\n",
			"_scratch/exec.go":   "package x\n\nimport _ \"os/exec\"\n",
			".hidden/broken.go":  "not Go either\n",
			"_ignored.go":        "package reserveandcharge\n\nimport _ \"os/exec\"\n",
		}, "fmt", "time"))
		if len(faults) != 0 {
			t.Fatalf("the check read files that the go tool ignores: %v", faults)
		}
	})
	t.Run("ST-SUP-RC32-OC03-T02 A command fill that imports only allow-listed packages is accepted", func(t *testing.T) {
		if faults := qualifyCommandFill(t, closureTree(t, "fmt", "encoding/json", "time")); len(faults) != 0 {
			t.Fatalf("the fill was refused: %v", faults)
		}
	})
}

// qualifyQueryFill runs the compiler's fill qualifier on root as the service of the query open-reservations, whose
// fill reserve-and-charge implements the query slot with the contract out/queries, with example.com/dbhelper listed
// for fills, and fails when the check writes a file.
func qualifyQueryFill(t *testing.T, root string) []compiler.FillFault {
	t.Helper()
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	if pinned := os.Getenv("STEGO_TEST_PINNED_GO"); pinned != "" {
		before := GoCommand
		GoCommand = pinned
		t.Cleanup(func() { GoCommand = before })
	}
	before := treeFiles(t, root)
	var qualifier compiler.FillQualifier = QualificationFaults
	faults, err := qualifier(compiler.FillCheck{ProjectDir: root, Module: closureModule, FillModules: []string{closureHelper},
		QueryContract: closureModule + "/out/queries",
		QueryFills:    []compiler.QueryFill{{Fill: "reserve-and-charge", Query: "open-reservations", Path: "queries[0].fill"}}})
	if err != nil {
		t.Fatal(err)
	}
	if after := treeFiles(t, root); !slices.Equal(before, after) {
		t.Fatalf("the check wrote files: before %v, after %v", before, after)
	}
	return faults
}

func TestQueryFillClosure(t *testing.T) {
	t.Run("ST-SUP-RC33-OC03-T01 A query fill that reaches a database package through a module is refused", func(t *testing.T) {
		cases := []struct {
			name, imports, refused, chain string
		}{
			{"a listed module that imports database/sql", closureHelper, "database/sql",
				closureFill + " (fill.go) -> " + closureHelper + " (helper.go) -> database/sql"},
			{"a driver module", "github.com/jackc/pgx/v5", "github.com/jackc/pgx/v5", closureFill + " (fill.go) -> github.com/jackc/pgx/v5"},
			{"net", "net", "net", closureFill + " (fill.go) -> net"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				root := closureTree(t, c.imports)
				// The fill imports the query slot contract, which imports database/sql itself, in place of the
				// command slot contract.
				writeFile(t, root, "out/queries/queries.go", "package queries\n\nimport _ \"database/sql\"\n")
				fillFile := filepath.Join(root, "fills", "reserve-and-charge", "fill.go")
				data, err := os.ReadFile(fillFile)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, root, "fills/reserve-and-charge/fill.go", strings.Replace(string(data), closureContract, closureModule+"/out/queries", 1))
				faults := qualifyQueryFill(t, root)
				if len(faults) != 1 {
					t.Fatalf("the check gave %d faults %v, want one", len(faults), faults)
				}
				f := faults[0]
				if f.Found != "reserve-and-charge" || f.Path != "queries[0].fill" || f.Rule != "fill-imports" {
					t.Errorf("the fault %+v does not name the fill and its place", f)
				}
				for _, want := range []string{`fill "reserve-and-charge"`, "import " + c.refused + ",", c.chain} {
					if !strings.Contains(f.Message, want) {
						t.Errorf("the fault %q does not name %q", f.Message, want)
					}
				}
			})
		}
	})
}

func TestFillClosureBuildSettings(t *testing.T) {
	t.Run("ST-SUP-RC32-OC03-T03 The closure is read with the service build's settings, whatever the host's", func(t *testing.T) {
		t.Setenv("CGO_ENABLED", "1")
		cases := []struct {
			name, refused string
			files         map[string]string
		}{
			{"a fill file for builds with no cgo reaches os/exec through a listed module", "may not import os/exec,", map[string]string{
				"fills/reserve-and-charge/nocgo.go": "//go:build !cgo\n\npackage reserveandcharge\n\nimport _ \"" + closureHelper + "/exe\"\n",
				"mods/dbhelper/exe/exe.go":          "package exe\n\nimport _ \"os/exec\"\n",
			}},
			{"a listed module file for builds with no cgo imports os/exec", "may not import os/exec,", map[string]string{
				"mods/dbhelper/run/run.go":        "package run\n",
				"mods/dbhelper/run/run_nocgo.go":  "//go:build !cgo\n\npackage run\n\nimport _ \"os/exec\"\n",
				"fills/reserve-and-charge/run.go": "package reserveandcharge\n\nimport _ \"" + closureHelper + "/run\"\n",
			}},
			{"a listed module file that the build leaves out imports os/exec", "which the service build leaves out, imports os/exec", map[string]string{
				"mods/dbhelper/win/win.go":         "package win\n",
				"mods/dbhelper/win/win_windows.go": "package win\n\nimport _ \"os/exec\"\n",
				"fills/reserve-and-charge/win.go":  "package reserveandcharge\n\nimport _ \"" + closureHelper + "/win\"\n",
			}},
			{"a fill file that the build leaves out imports a listed package that go list does not report", "does not report", map[string]string{
				"mods/dbhelper/exe/exe.go":                  "package exe\n\nimport _ \"os/exec\"\n",
				"fills/reserve-and-charge/leave_windows.go": "package reserveandcharge\n\nimport _ \"" + closureHelper + "/exe\"\n",
			}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				root := closureTree(t, "fmt")
				for name, text := range c.files {
					writeFile(t, root, name, text)
				}
				var texts []string
				for _, f := range qualifyCommandFill(t, root) {
					texts = append(texts, f.Message)
				}
				if joined := strings.Join(texts, "\n"); !strings.Contains(joined, c.refused) {
					t.Errorf("the faults %q do not hold %q", joined, c.refused)
				}
			})
		}
	})
}

func TestWorkerFillClosure(t *testing.T) {
	t.Run("ST-SUP-RC35-OC03-T02 A worker fill is held to the fill rules with its worker contract", func(t *testing.T) {
		t.Setenv("GOPROXY", "off")
		t.Setenv("GOTOOLCHAIN", "local")
		if pinned := os.Getenv("STEGO_TEST_PINNED_GO"); pinned != "" {
			before := GoCommand
			GoCommand = pinned
			t.Cleanup(func() { GoCommand = before })
		}
		root := closureTree(t, "net")
		writeFile(t, root, "out/workers/workers.go", "package workers\n")
		data, err := os.ReadFile(filepath.Join(root, "fills", "reserve-and-charge", "fill.go"))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, root, "fills/reserve-and-charge/fill.go", strings.Replace(string(data), closureContract, closureModule+"/out/workers", 1))
		faults, err := QualificationFaults(compiler.FillCheck{ProjectDir: root, Module: closureModule, FillModules: []string{closureHelper},
			WorkerContract: closureModule + "/out/workers",
			WorkerFills:    []compiler.QueryFill{{Fill: "reserve-and-charge", Query: "sync-orders", Path: "workers[0].fill"}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(faults) != 1 || faults[0].Path != "workers[0].fill" || !strings.Contains(faults[0].Message, "may not import net,") {
			t.Fatalf("got %+v, want one fault for the import of net at workers[0].fill", faults)
		}
	})
}

func TestPendingFillCheck(t *testing.T) {
	t.Run("ST-SUP-RC32-OC03-T04 Before the first generation an import that cannot be checked is a fault", func(t *testing.T) {
		cases := []struct{ name, imported, want string }{
			{"a listed module", closureHelper, "imports " + closureHelper + ", which cannot be checked before the project's first generation"},
			{"the contract of another module", "example.com/other/out/commands", "imports example.com/other/out/commands, which cannot be checked"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				root := closureTree(t, c.imported)
				if err := os.Remove(filepath.Join(root, "go.mod")); err != nil {
					t.Fatal(err)
				}
				var texts []string
				for _, f := range qualifyCommandFill(t, root) {
					texts = append(texts, f.Message)
				}
				if joined := strings.Join(texts, "\n"); !strings.Contains(joined, c.want) {
					t.Errorf("the faults %q do not hold %q", joined, c.want)
				}
			})
		}
		root := closureTree(t, "fmt")
		if err := os.Remove(filepath.Join(root, "go.mod")); err != nil {
			t.Fatal(err)
		}
		if faults := qualifyCommandFill(t, root); len(faults) != 0 {
			t.Errorf("a fill of its declared contract and fmt got faults before the first generation: %v", faults)
		}
	})
}
