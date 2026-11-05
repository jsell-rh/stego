package parser

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
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
	undeclared, err := decodeCollecting(data, path, target, limit)
	if err != nil {
		return err
	}
	if len(undeclared) > 0 {
		first := undeclared[0]
		return &ParseError{Path: path, Line: first.Line, Column: first.Column, Context: lineAt(data, first.Line), Err: fmt.Errorf("unknown field %q", first.Path)}
	}
	return nil
}

// UndeclaredField is a key that the target type does not declare: its path in the document, such as
// slots[0].merge, the key, where it is, and the keys that are declared at that place.
type UndeclaredField struct {
	Path     string
	Key      string
	Line     int
	Column   int
	Declared []string
}

// DecodeCollecting reads one YAML document as DecodeStrict does, but it does not stop at a key that the target
// type does not declare: it leaves the key out, decodes the rest, and returns every such key in document order.
// Any other fault of reading is returned as the error.
func DecodeCollecting(data []byte, path string, target any) ([]UndeclaredField, error) {
	typ := reflect.TypeOf(target)
	if typ == nil || typ.Kind() != reflect.Pointer || reflect.ValueOf(target).IsNil() {
		return nil, errorf(path, "decode target must be a non-nil pointer")
	}
	return decodeCollecting(data, path, target, MaxDocumentBytes)
}

func decodeCollecting(data []byte, path string, target any, limit int) ([]UndeclaredField, error) {
	root, err := readNodeWithLimit(data, path, limit)
	if err != nil {
		return nil, err
	}
	var undeclared []UndeclaredField
	if err := checkSchema(data, path, root, reflect.TypeOf(target).Elem(), "", &undeclared); err != nil {
		return nil, err
	}
	if err := root.Decode(target); err != nil {
		return nil, parseErrorWithLineInfo(data, path, err)
	}
	return undeclared, nil
}

func readNode(data []byte, path string) (*yaml.Node, error) {
	return readNodeWithLimit(data, path, MaxDocumentBytes)
}

func readNodeWithLimit(data []byte, path string, limit int) (*yaml.Node, error) {
	if len(data) > limit {
		bounded := errorf(path, "document exceeds the %d byte limit", limit)
		bounded.Bounded = true
		return nil, bounded
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, syntaxError(data, path, err)
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

func nodeError(data []byte, path string, node *yaml.Node, format string, args ...any) *ParseError {
	return &ParseError{
		Path: path, Line: node.Line, Column: node.Column, Context: lineAt(data, node.Line),
		Err: fmt.Errorf(format, args...),
	}
}

// checkNodes rejects syntax that can hide or replace declaration content.
// Check all nodes, including values held in interface{} fields.
func checkNodes(data []byte, path string, node *yaml.Node, depth int) error {
	if depth > 128 {
		bounded := nodeError(data, path, node, "YAML nesting exceeds the 128 level limit")
		bounded.Bounded = true
		return bounded
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

// ScalarForm is a type whose YAML form may also be one scalar, such as a
// component's port given by its bare name. The strict decoder accepts a scalar
// node for such a type, and the type's own UnmarshalYAML reads it. The
// registry's Port implements it, so that this package need not import the
// registry.
type ScalarForm interface {
	YAMLScalarForm()
}

var scalarFormType = reflect.TypeFor[ScalarForm]()

// RenamedKeys is a type whose UnmarshalYAML refuses keys that were renamed,
// with a message that names the new key, such as the entity key of a slot
// binding or a fill, now collection. The strict decoder accepts those keys, so
// that the type's own message is the one reported. A fill implements it, so
// that this package need not import Extending.
type RenamedKeys interface {
	YAMLRenamedKeys() []string
}

func checkSchema(data []byte, path string, node *yaml.Node, typ reflect.Type, location string, undeclared *[]UndeclaredField) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[time.Time]() || typ.Kind() == reflect.Interface || node.Tag == "!!null" {
		return nil
	}
	// Ports have an explicit scalar-or-mapping syntax.
	if node.Kind == yaml.ScalarNode && reflect.PointerTo(typ).Implements(scalarFormType) {
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
		if renamed, ok := reflect.New(typ).Interface().(RenamedKeys); ok {
			for _, key := range renamed.YAMLRenamedKeys() {
				fields[key] = reflect.TypeFor[string]()
			}
		}
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if typ == reflect.TypeFor[types.Entity]() && key.Value == "versioned" && (value.Tag != "!!bool" || (value.Value != "true" && value.Value != "false")) {
				return nodeError(data, path, value, "versioned must be true or false")
			}
			if typ == reflect.TypeFor[types.Entity]() && key.Value == "cleanup_targets" {
				if value.Kind != yaml.MappingNode {
					return nodeError(data, path, value, "cleanup_targets must map owners to field names")
				}
				for _, item := range value.Content {
					if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
						return nodeError(data, path, item, "cleanup target owners and fields must be strings")
					}
				}
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
				declared := make([]string, 0, len(fields))
				for name := range fields {
					declared = append(declared, name)
				}
				sort.Strings(declared)
				*undeclared = append(*undeclared, UndeclaredField{Path: childLocation, Key: key.Value, Line: key.Line, Column: key.Column, Declared: declared})
				continue
			}
			if err := checkSchema(data, path, value, childType, childLocation, undeclared); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if node.Kind == yaml.SequenceNode {
			for i, value := range node.Content {
				if err := checkSchema(data, path, value, typ.Elem(), fmt.Sprintf("%s[%d]", location, i), undeclared); err != nil {
					return err
				}
			}
		}
	case reflect.Map:
		if node.Kind == yaml.MappingNode {
			for i := 0; i < len(node.Content); i += 2 {
				if err := checkSchema(data, path, node.Content[i+1], typ.Elem(), location+"."+node.Content[i].Value, undeclared); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// syntaxLine reads the line that a YAML syntax error names.
var syntaxLine = regexp.MustCompile(`line (\d+):`)

// syntaxError gives the place where reading of a document that is not valid YAML stopped: the line and the
// position in the line. The YAML reader names a line but no position, and for a tab in the indentation it names
// the line before. So a tab in the indentation of the named line or the next is the place; else the first
// character of the named line. When the reader names no line, the place is the first line at which the document
// read so far stops being valid YAML.
func syntaxError(data []byte, path string, err error) *ParseError {
	lines := strings.Split(string(data), "\n")
	line := 0
	if match := syntaxLine.FindStringSubmatch(err.Error()); match != nil {
		line, _ = strconv.Atoi(match[1])
	} else {
		for n := 1; n <= len(lines); n++ {
			var node yaml.Node
			if yaml.Unmarshal([]byte(strings.Join(lines[:n], "\n")), &node) != nil {
				line = n
				break
			}
		}
	}
	column := 0
	if line > 0 {
		for _, candidate := range []int{line, line + 1} {
			if candidate > len(lines) {
				continue
			}
			text := lines[candidate-1]
			indentation := text[:len(text)-len(strings.TrimLeft(text, " \t"))]
			if tab := strings.IndexByte(indentation, '\t'); tab >= 0 {
				line, column = candidate, tab+1
				break
			}
		}
		if column == 0 && line <= len(lines) {
			text := lines[line-1]
			column = len(text) - len(strings.TrimLeft(text, " ")) + 1
		}
	}
	message := strings.TrimPrefix(err.Error(), "yaml: ")
	message = syntaxLine.ReplaceAllString(message, "")
	return &ParseError{Path: path, Line: line, Column: column, Context: lineAt(data, line), Err: fmt.Errorf("invalid YAML: %s", strings.TrimSpace(message))}
}
