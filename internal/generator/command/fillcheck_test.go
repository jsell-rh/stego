package command

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	serviceModule = "example.com/service"
	slotContract  = serviceModule + "/out/slots"
	fillPackage   = serviceModule + "/fills/reserve-and-charge"
	helperModule  = "example.com/dbhelper"
)

// writeTree writes files, by path from root, into a new folder.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// serviceTree makes the module of a service with the command reserve-and-charge: its slot contract, which imports
// database/sql itself, the fill reserve-and-charge with the imports given, the listed fill module example.com/dbhelper,
// whose package imports database/sql, and a local copy of github.com/jackc/pgx/v5, all bound by replace, so go list
// needs no network.
func serviceTree(t *testing.T, fillImports ...string) string {
	t.Helper()
	root := t.TempDir()
	var body strings.Builder
	body.WriteString("package reserveandcharge\n\nimport (\n")
	for _, path := range fillImports {
		body.WriteString("\t_ \"" + path + "\"\n")
	}
	body.WriteString("\t_ \"" + slotContract + "\"\n)\n")
	writeTree(t, root, map[string]string{
		"go.mod": "module " + serviceModule + "\n\ngo 1.26\n\nrequire (\n\t" + helperModule + " v0.0.0\n" +
			"\tgithub.com/jackc/pgx/v5 v5.0.0\n)\n\nreplace " + helperModule + " => ./mods/dbhelper\n\n" +
			"replace github.com/jackc/pgx/v5 => ./mods/pgx\n",
		"out/slots/slot.go":                     "package slots\n\nimport _ \"database/sql\"\n",
		"fills/reserve-and-charge/fill.go":      body.String(),
		"fills/reserve-and-charge/fill_test.go": "package reserveandcharge\n\nimport _ \"os/exec\"\n",
		"mods/dbhelper/go.mod":                  "module " + helperModule + "\n\ngo 1.26\n",
		"mods/dbhelper/helper.go":               "package dbhelper\n\nimport _ \"database/sql\"\n",
		"mods/pgx/go.mod":                       "module github.com/jackc/pgx/v5\n\ngo 1.26\n",
		"mods/pgx/pgx.go":                       "package pgx\n",
	})
	return root
}

// listFiles lists every file under root.
func listFiles(t *testing.T, root string) []string {
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

// check runs CheckFill on the fill reserve-and-charge of root, with example.com/dbhelper listed for fills, and
// reports whether it wrote a file.
func check(t *testing.T, root string) error {
	t.Helper()
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	before := listFiles(t, root)
	err := CheckFill(context.Background(), FillCheck{
		Fill: "reserve-and-charge", Dir: root, Package: fillPackage, SlotContract: slotContract,
		FillModules: []string{helperModule}, GoTool: os.Getenv("STEGO_TEST_PINNED_GO"),
	})
	if after := listFiles(t, root); !slices.Equal(before, after) {
		t.Fatalf("the check wrote files: before %v, after %v", before, after)
	}
	return err
}

func TestCheckFill(t *testing.T) {
	t.Run("ST-SUP-RC32-OC03-T01 A command fill that reaches a database package through a module is refused", func(t *testing.T) {
		cases := []struct {
			name, imports, refused, chain string
		}{
			{"a listed module that imports database/sql", helperModule, "database/sql",
				fillPackage + " -> " + helperModule + " -> database/sql"},
			{"a driver module", "github.com/jackc/pgx/v5", "github.com/jackc/pgx/v5",
				fillPackage + " -> github.com/jackc/pgx/v5"},
			{"os/exec", "os/exec", "os/exec", fillPackage + " -> os/exec"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				err := check(t, serviceTree(t, c.imports))
				if err == nil {
					t.Fatal("the fill was accepted")
				}
				for _, want := range []string{"reserve-and-charge", "import " + c.refused + ",", c.chain} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the fault %q does not name %q", err, want)
					}
				}
			})
		}
	})
	t.Run("ST-SUP-RC32-OC03-T02 A command fill that imports only allow-listed packages is accepted", func(t *testing.T) {
		if err := check(t, serviceTree(t, "fmt", "encoding/json", "time")); err != nil {
			t.Fatalf("the fill was refused: %v", err)
		}
	})
}
