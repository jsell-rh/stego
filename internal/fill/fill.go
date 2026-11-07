// Package fill holds Extending's fill as its manifest (fill.yaml) states it,
// and the reading of the manifest through the declaration's strict decoder.
package fill

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jsell-rh/stego/internal/parser"
	"gopkg.in/yaml.v3"
)

// ManifestFile is the name of the fill manifest in a fill's folder: the file
// that states the fill and holds its evidence and its sign-off.
const ManifestFile = "fill.yaml"

// Fill represents a human-authored implementation of a slot contract, as its
// fill manifest states it, with the evidence that its tests passed and a
// person's sign-off of that evidence.
type Fill struct {
	Kind       string        `yaml:"kind"`
	Name       string        `yaml:"name"`
	Implements string        `yaml:"implements"`
	Collection string        `yaml:"collection"`
	Evidence   *FillEvidence `yaml:"evidence,omitempty"`
	SignOff    *FillSignOff  `yaml:"sign_off,omitempty"`
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

// ParseFill reads and parses a fill YAML file. It refuses evidence or a
// sign-off with a field missing, and names the field.
func ParseFill(path string) (*Fill, error) {
	f, err := parser.ReadKind(path, "fill", func(f *Fill) string { return f.Kind })
	if err != nil {
		return nil, err
	}
	if f.Evidence != nil {
		if err := f.Evidence.check(); err != nil {
			return nil, fmt.Errorf("%s: evidence: %w", path, err)
		}
	}
	if f.SignOff != nil {
		if err := f.SignOff.check(); err != nil {
			return nil, fmt.Errorf("%s: sign_off: %w", path, err)
		}
	}
	return f, nil
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

// writeSection sets one top-level key of the fill manifest at path to value,
// or removes the key when value is nil, and keeps each other key and its
// comments as they are. It writes a new file and renames it over the old one,
// so a reader sees the old manifest or the new one, never a part of either.
func writeSection(path, key string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: the fill manifest is not a mapping", path)
	}
	root := document.Content[0]
	removeKey(root, key)
	if value != nil {
		var encoded yaml.Node
		if err := encoded.Encode(value); err != nil {
			return err
		}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &encoded)
	}
	out, err := encodeNode(&document)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".fill-*.yaml")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(out); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// removeKey removes a key and its value from a mapping node.
func removeKey(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// encodeNode writes a YAML node with an indent of two spaces.
func encodeNode(node *yaml.Node) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(node); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
