package parser

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// MaxDocumentBytes limits memory used to decode a declaration.
const MaxDocumentBytes = 4 << 20

// ReadDocument reads a bounded declaration file and retains its source path.
func ReadDocument(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, &ParseError{Path: path, Err: err}
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxDocumentBytes+1))
	if err != nil {
		return nil, &ParseError{Path: path, Err: err}
	}
	if len(data) > MaxDocumentBytes {
		return nil, errorf(path, "document exceeds the %d byte limit", MaxDocumentBytes)
	}
	return data, nil
}

// DecodeStrict reads one YAML document. It checks keys before custom YAML
// methods run, because yaml.Decoder.KnownFields does not check Node.Decode.
// Dynamic maps retain their keys; their values follow the declared value type.
func DecodeStrict(data []byte, path string, target any) error {
	return DecodeStrictWithLimit(data, path, target, MaxDocumentBytes)
}

// DecodeStrictWithLimit permits a separate bound for compiler-owned artifacts.
// Service and registry declarations always use DecodeStrict's smaller limit.
func DecodeStrictWithLimit(data []byte, path string, target any, limit int) error {
	if limit <= 0 {
		return errorf(path, "document limit must be positive")
	}
	typ := reflect.TypeOf(target)
	if typ == nil || typ.Kind() != reflect.Pointer || reflect.ValueOf(target).IsNil() {
		return errorf(path, "decode target must be a non-nil pointer")
	}
	root, err := readNodeWithLimit(data, path, limit)
	if err != nil {
		return err
	}
	if err := checkSchema(data, path, root, typ.Elem(), ""); err != nil {
		return err
	}
	if err := root.Decode(target); err != nil {
		return parseErrorWithLineInfo(data, path, err)
	}
	return nil
}

func readNode(data []byte, path string) (*yaml.Node, error) {
	return readNodeWithLimit(data, path, MaxDocumentBytes)
}

func readNodeWithLimit(data []byte, path string, limit int) (*yaml.Node, error) {
	if len(data) > limit {
		return nil, errorf(path, "document exceeds the %d byte limit", limit)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, parseErrorWithLineInfo(data, path, fmt.Errorf("invalid YAML: %w", err))
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, parseErrorWithLineInfo(data, path, fmt.Errorf("invalid YAML: %w", err))
		}
		return nil, nodeError(data, path, &extra, "multiple YAML documents are not supported")
	}
	if len(document.Content) != 1 {
		return nil, errorf(path, "expected one YAML document")
	}
	root := document.Content[0]
	if err := checkNodes(data, path, root, 0); err != nil {
		return nil, err
	}
	return root, nil
}

func nodeError(data []byte, path string, node *yaml.Node, format string, args ...any) error {
	return &ParseError{
		Path: path, Line: node.Line, Context: lineAt(data, node.Line),
		Err: fmt.Errorf(format, args...),
	}
}

// checkNodes rejects syntax that can hide or replace declaration content.
// Check all nodes, including values held in interface{} fields.
func checkNodes(data []byte, path string, node *yaml.Node, depth int) error {
	if depth > 128 {
		return nodeError(data, path, node, "YAML nesting exceeds the 128 level limit")
	}
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return nodeError(data, path, node, "YAML anchors and aliases are not supported; write each value explicitly")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]int, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag == "!!merge" {
				return nodeError(data, path, key, "YAML merge keys are not supported")
			}
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return nodeError(data, path, key, "mapping keys must be strings")
			}
			if first, ok := seen[key.Value]; ok {
				return nodeError(data, path, key, "duplicate key %q (first declared at line %d)", key.Value, first)
			}
			seen[key.Value] = key.Line
		}
	}
	for _, child := range node.Content {
		if err := checkNodes(data, path, child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func checkSchema(data []byte, path string, node *yaml.Node, typ reflect.Type, location string) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[time.Time]() || typ.Kind() == reflect.Interface || node.Tag == "!!null" {
		return nil
	}
	// Ports have an explicit scalar-or-mapping syntax.
	if typ == reflect.TypeFor[types.Port]() && node.Kind == yaml.ScalarNode {
		return nil
	}
	if typ == reflect.TypeFor[types.ConfigFieldItems]() {
		// Named fields all have a type. Other objects use the inline schema.
		named := node.Kind == yaml.MappingNode && len(node.Content) > 0
		for i := 1; i < len(node.Content) && named; i += 2 {
			value := node.Content[i]
			hasType := false
			if value.Kind == yaml.MappingNode {
				for j := 0; j < len(value.Content); j += 2 {
					if value.Content[j].Value == "type" && value.Content[j+1].Value != "" {
						hasType = true
					}
				}
			}
			named = hasType
		}
		if named {
			typ = reflect.TypeFor[map[string]types.ConfigField]()
		} else {
			typ = reflect.TypeFor[types.ConfigField]()
		}
	}
	switch typ.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return nil // The typed decoder reports an invalid value shape.
		}
		fields := make(map[string]reflect.Type)
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("yaml"), ",")[0]
			if field.IsExported() && name != "-" && name != "" {
				fields[name] = field.Type
			}
		}
		if typ == reflect.TypeFor[types.ServiceDeclaration]() {
			fields["collections"] = reflect.TypeFor[map[string]types.Collection]()
			// Retain the existing migration diagnostic for the old syntax.
			fields["expose"] = reflect.TypeFor[any]()
		}
		if typ == reflect.TypeFor[types.SlotDeclaration]() || typ == reflect.TypeFor[types.Fill]() {
			fields["entity"] = reflect.TypeFor[string]()
		}
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if typ == reflect.TypeFor[types.Entity]() && key.Value == "versioned" && (value.Tag != "!!bool" || (value.Value != "true" && value.Value != "false")) {
				return nodeError(data, path, value, "versioned must be true or false")
			}
			if typ == reflect.TypeFor[types.Entity]() && key.Value == "cleanup_owners" {
				if value.Kind != yaml.SequenceNode {
					return nodeError(data, path, value, "cleanup_owners must be a list of strings")
				}
				for _, owner := range value.Content {
					if owner.Kind != yaml.ScalarNode || owner.Tag != "!!str" {
						return nodeError(data, path, owner, "cleanup owner must be a string")
					}
				}
			}
			childLocation := key.Value
			if location != "" {
				childLocation = location + "." + key.Value
			}
			childType, ok := fields[key.Value]
			if !ok {
				return nodeError(data, path, key, "unknown field %q", childLocation)
			}
			if err := checkSchema(data, path, value, childType, childLocation); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if node.Kind == yaml.SequenceNode {
			for i, value := range node.Content {
				if err := checkSchema(data, path, value, typ.Elem(), fmt.Sprintf("%s[%d]", location, i)); err != nil {
					return err
				}
			}
		}
	case reflect.Map:
		if node.Kind == yaml.MappingNode {
			for i := 0; i < len(node.Content); i += 2 {
				if err := checkSchema(data, path, node.Content[i+1], typ.Elem(), location+"."+node.Content[i].Value); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
