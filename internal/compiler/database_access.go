package compiler

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/format"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed database_access.go.tmpl
var databaseAccessSource string

type databaseAccessObject struct {
	Component string `json:"component"`
	gen.DatabaseObject
}

var databaseIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func generateDatabaseAccess(wirings []ComponentWiring) ([]gen.File, error) {
	var objects []databaseAccessObject
	seen := map[string]bool{}
	schemas := map[string]bool{}
	for _, component := range wirings {
		if component.Wiring == nil {
			continue
		}
		if component.Wiring.VerifyDatabaseAccess && (!component.Wiring.NeedsDB || len(component.Wiring.DatabaseAccess) == 0) {
			return nil, fmt.Errorf("component %q requires database objects and a database resource for runtime access checks", component.Name)
		}
		for _, input := range component.Wiring.DatabaseAccess {
			object := input
			object.Privileges = append([]string(nil), input.Privileges...)
			key := object.Schema + "." + object.Name
			if component.Name == "" || !databaseIdentifier.MatchString(object.Schema) || !databaseIdentifier.MatchString(object.Name) || strings.HasPrefix(object.Schema, "pg_") || object.Schema == "information_schema" || seen[key] || len(objects) >= 4096 {
				return nil, fmt.Errorf("component %q has an invalid or duplicate database object", component.Name)
			}
			if object.Kind != "table" && object.Kind != "sequence" {
				return nil, fmt.Errorf("component %q has an unsupported database object kind", component.Name)
			}
			if len(object.Privileges) == 0 || len(object.Privileges) > 4 {
				return nil, fmt.Errorf("component %q has an invalid database privilege set", component.Name)
			}
			sort.Strings(object.Privileges)
			if object.Kind == "table" && !containsDatabasePrivilege(object.Privileges, "SELECT") {
				return nil, fmt.Errorf("component %q requires SELECT for table inspection", component.Name)
			}
			for i, privilege := range object.Privileges {
				valid := privilege == "USAGE" && object.Kind == "sequence"
				if object.Kind == "table" {
					valid = privilege == "SELECT" || privilege == "INSERT" || privilege == "UPDATE" || privilege == "DELETE"
				}
				if !valid || i > 0 && privilege == object.Privileges[i-1] {
					return nil, fmt.Errorf("component %q has an unsafe or duplicate database privilege", component.Name)
				}
			}
			seen[key], schemas[object.Schema] = true, true
			objects = append(objects, databaseAccessObject{Component: component.Name, DatabaseObject: object})
		}
	}
	if len(objects) == 0 {
		return nil, nil
	}
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Schema+"."+objects[i].Name < objects[j].Schema+"."+objects[j].Name
	})
	var names []string
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	data := struct {
		Objects []databaseAccessObject
		Schemas []string
	}{objects, names}
	tmpl, err := template.New("database-access").Parse(databaseAccessSource)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return nil, err
	}
	source, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format database access: %w", err)
	}
	manifest, err := json.MarshalIndent(struct {
		Format  int                    `json:"format"`
		Objects []databaseAccessObject `json:"objects"`
	}{1, objects}, "", "  ")
	if err != nil {
		return nil, err
	}
	return []gen.File{
		{Path: gen.DatabaseAccessNamespace + "/access.go", Content: source},
		{Path: gen.DatabaseAccessNamespace + "/access.json", Content: append(manifest, '\n')},
	}, nil
}

func needsDatabaseAccessCheck(input AssemblerInput, consumed map[int]bool) bool {
	for i, component := range input.Wirings {
		if consumed[i] && component.Wiring != nil && component.Wiring.VerifyDatabaseAccess {
			return true
		}
	}
	return false
}

func containsDatabasePrivilege(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
