package fill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
)

// writeFile writes a file of the test project.
func writeFile(t *testing.T, dir, name, text string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSlotContracts(t *testing.T) {
	t.Run("ST-EXT-INV05-T A query fill and a command fill import their own slot's contract and the runtime packages it reaches", func(t *testing.T) {
		dir := t.TempDir()
		// The generated query and command contracts, which import packages of the runtime library.
		writeFile(t, dir, "out/queries/queries.go", "package queries\n\nimport (\n\t\"github.com/jsell-rh/stego/registry/runtime/query\"\n\t\"github.com/jsell-rh/stego/registry/runtime/audit\"\n\tprobes \"example.com/shop/out/probes\"\n)\n\nvar _ = query.ErrNotFound\nvar _ audit.Fields\nvar _ probes.Probe\n")
		writeFile(t, dir, "out/commands/commands.go", "package commands\n\nimport \"github.com/jsell-rh/stego/registry/runtime/command\"\n\nvar _ = command.NewID\n")
		check := compiler.FillCheck{ProjectDir: dir, Module: "example.com/shop", QueryContract: "example.com/shop/out/queries",
			CommandFills: []compiler.CommandFill{{Fill: "add-note", Contract: "example.com/shop/out/commands"}}}
		fill := func(name, imports string) string {
			fillDir := filepath.Join(dir, "fills", name)
			writeFile(t, dir, "fills/"+name+"/fill.go", "package fill\n\nimport (\n"+imports+")\n")
			return fillDir
		}
		cases := []struct {
			name, fill, implements, imports, refused string
		}{
			{"a query fill of out/queries and the runtime query package", "get-order", querySlot,
				"\t\"example.com/shop/out/queries\"\n\t\"github.com/jsell-rh/stego/registry/runtime/query\"\n\t\"context\"\n", ""},
			{"a query fill of out/slots", "get-order-slots", querySlot, "\t\"example.com/shop/out/slots\"\n", "example.com/shop/out/slots"},
			{"a query fill of a runtime package that the contract does not reach", "get-order-authz", querySlot,
				"\t\"example.com/shop/out/queries\"\n\t\"github.com/jsell-rh/stego/registry/runtime/authz\"\n", "github.com/jsell-rh/stego/registry/runtime/authz"},
			{"a query fill of a generated package that the contract imports", "get-order-probes", querySlot,
				"\t\"example.com/shop/out/queries\"\n\t\"example.com/shop/out/probes\"\n", "example.com/shop/out/probes"},
			{"a command fill of its command contract and the runtime command package", "add-note", commandSlot,
				"\t\"example.com/shop/out/commands\"\n\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n", ""},
			{"a command fill of the query contract", "add-note", commandSlot, "\t\"example.com/shop/out/queries\"\n", "example.com/shop/out/queries"},
			{"a protobuf slot fill of out/slots", "admin-policy", "rest-api.before_create", "\t\"example.com/shop/out/slots\"\n", ""},
			{"a protobuf slot fill of the query contract", "admin-policy-q", "rest-api.before_create", "\t\"example.com/shop/out/queries\"\n", "example.com/shop/out/queries"},
		}
		for _, c := range cases {
			err := checkImports(fill(c.fill, c.imports), check, c.fill, c.implements)
			switch {
			case c.refused == "" && err != nil:
				t.Errorf("%s was refused: %v", c.name, err)
			case c.refused != "" && (err == nil || !strings.Contains(err.Error(), "imports "+c.refused)):
				t.Errorf("%s gave %v, want a refusal that names %s", c.name, err, c.refused)
			}
		}
	})
}
