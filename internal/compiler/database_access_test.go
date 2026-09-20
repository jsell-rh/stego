package compiler

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestDatabaseAccessDeclaration(t *testing.T) {
	good := gen.DatabaseObject{Schema: "public", Name: "records", Kind: "table", Privileges: []string{"SELECT", "UPDATE"}}
	for _, mode := range []string{"schema", "system-schema", "name", "long-name", "kind", "empty", "ddl", "grant-option", "duplicate-privilege", "sequence-write", "duplicate-object"} {
		t.Run(mode, func(t *testing.T) {
			item := good
			item.Privileges = append([]string(nil), good.Privileges...)
			switch mode {
			case "schema":
				item.Schema = "public;SELECT"
			case "system-schema":
				item.Schema = "pg_catalog"
			case "name":
				item.Name = "records\""
			case "long-name":
				item.Name = strings.Repeat("a", 64)
			case "kind":
				item.Kind = "view"
			case "empty":
				item.Privileges = nil
			case "ddl":
				item.Privileges = []string{"TRUNCATE"}
			case "grant-option":
				item.Privileges = []string{"SELECT WITH GRANT OPTION"}
			case "duplicate-privilege":
				item.Privileges = []string{"SELECT", "SELECT"}
			case "sequence-write":
				item.Kind = "sequence"
				item.Privileges = []string{"UPDATE"}
			}
			wirings := []ComponentWiring{{Name: "records", Wiring: &gen.Wiring{DatabaseAccess: []gen.DatabaseObject{item}}}}
			if mode == "duplicate-object" {
				wirings = append(wirings, ComponentWiring{Name: "other", Wiring: &gen.Wiring{DatabaseAccess: []gen.DatabaseObject{good}}})
			}
			if _, err := generateDatabaseAccess(wirings); err == nil {
				t.Fatal("unsafe database declaration was accepted")
			}
		})
	}
	marker := gen.DatabaseObject{Schema: "stego_schema", Name: "generation", Kind: "table", Privileges: []string{"SELECT"}}
	first := []ComponentWiring{{Name: "records", Wiring: &gen.Wiring{DatabaseAccess: []gen.DatabaseObject{good, marker}}}}
	snapshot := append([]string(nil), good.Privileges...)
	a, err := generateDatabaseAccess(first)
	if err != nil {
		t.Fatal(err)
	}
	second := []ComponentWiring{{Name: "records", Wiring: &gen.Wiring{DatabaseAccess: []gen.DatabaseObject{marker, good}}}}
	b, err := generateDatabaseAccess(second)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("database access generation differs by declaration order", err)
	}
	if !reflect.DeepEqual(snapshot, good.Privileges) {
		t.Fatal("generation changed its input")
	}
	if len(a) != 2 {
		t.Fatal("missing database access artifacts")
	}
	var manifest struct {
		Format  int
		Objects []databaseAccessObject
	}
	if json.Unmarshal(a[1].Bytes(), &manifest) != nil || manifest.Format != 1 || len(manifest.Objects) != 2 {
		t.Fatal("invalid database access manifest")
	}
	if !bytes.Contains(a[0].Bytes(), []byte("func GrantRuntime(")) || !bytes.Contains(a[0].Bytes(), []byte("func VerifyRuntime(")) {
		t.Fatal("missing installation boundary")
	}
	empty, err := generateDatabaseAccess(nil)
	if err != nil || len(empty) != 0 {
		t.Fatal("service without database objects changed")
	}
}
