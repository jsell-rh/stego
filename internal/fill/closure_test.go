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
				closureFill + " -> " + closureHelper + " -> database/sql"},
			{"a driver module", "github.com/jackc/pgx/v5", "github.com/jackc/pgx/v5",
				closureFill + " -> github.com/jackc/pgx/v5"},
			{"os/exec", "os/exec", "os/exec", closureFill + " -> os/exec"},
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
	t.Run("ST-SUP-RC32-OC03-T02 A command fill that imports only allow-listed packages is accepted", func(t *testing.T) {
		if faults := qualifyCommandFill(t, closureTree(t, "fmt", "encoding/json", "time")); len(faults) != 0 {
			t.Fatalf("the fill was refused: %v", faults)
		}
	})
}
