// Package fill holds Extending's fill as its manifest (fill.yaml) states it,
// and the reading of the manifest through the declaration's strict decoder.
package fill

import (
	"fmt"
	"time"

	"github.com/jsell-rh/stego/internal/parser"
	"gopkg.in/yaml.v3"
)

// Fill represents a human-authored implementation of a slot contract.
type Fill struct {
	Kind        string    `yaml:"kind"`
	Name        string    `yaml:"name"`
	Implements  string    `yaml:"implements"`
	Collection  string    `yaml:"collection"`
	QualifiedBy string    `yaml:"qualified_by"`
	QualifiedAt time.Time `yaml:"qualified_at"`
}

// YAMLRenamedKeys marks Fill as a parser.RenamedKeys: the strict decoder lets
// the renamed entity key through, so that UnmarshalYAML reports its rename.
func (Fill) YAMLRenamedKeys() []string { return []string{"entity"} }

// UnmarshalYAML detects the deprecated `entity:` key (renamed to `collection:`
// in the Collection type system migration) and returns a migration error.
func (f *Fill) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		for i := 0; i < len(value.Content)-1; i += 2 {
			if value.Content[i].Value == "entity" {
				return fmt.Errorf("the 'entity' key in fill declarations has been renamed to 'collection' — please update your fill.yaml")
			}
		}
	}
	type rawFill Fill
	var raw rawFill
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*f = Fill(raw)
	return nil
}

// ParseFill reads and parses a fill YAML file.
func ParseFill(path string) (*Fill, error) {
	return parser.ReadKind(path, "fill", func(f *Fill) string { return f.Kind })
}

// ReadCollection reads the fill manifest at path and gives the collection
// the fill serves. It is the compiler's compiler.FillReader: cmd/stego gives
// it to the compiler, which may not import Extending (ST-STD-CODE-R03).
func ReadCollection(path string) (string, error) {
	f, err := ParseFill(path)
	if err != nil {
		return "", err
	}
	return f.Collection, nil
}
