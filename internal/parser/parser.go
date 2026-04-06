// Package parser implements YAML file parsing for all stego file types.
// It reads files from disk, validates the kind field, and returns
// strongly-typed domain objects.
package parser

import (
	"fmt"
	"os"

	"github.com/stego-project/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// ParseError wraps a parse failure with the originating file path.
type ParseError struct {
	Path string
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error {
	return e.Err
}

// errorf creates a ParseError for the given path.
func errorf(path, format string, args ...any) *ParseError {
	return &ParseError{Path: path, Err: fmt.Errorf(format, args...)}
}

// kindHeader is used to peek at the kind field before full deserialization.
type kindHeader struct {
	Kind string `yaml:"kind"`
}

// Parse reads a YAML file and returns the appropriate typed object based on the
// kind field. The returned value is one of:
//   - *types.Archetype
//   - *types.Component
//   - *types.Mixin
//   - *types.ServiceDeclaration
//   - *types.Fill
func Parse(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &ParseError{Path: path, Err: err}
	}

	var header kindHeader
	if err := yaml.Unmarshal(data, &header); err != nil {
		return nil, errorf(path, "invalid YAML: %w", err)
	}

	switch header.Kind {
	case "archetype":
		return parseAs[types.Archetype](data, path, "archetype")
	case "component":
		return parseAs[types.Component](data, path, "component")
	case "mixin":
		return parseAs[types.Mixin](data, path, "mixin")
	case "service":
		return parseAs[types.ServiceDeclaration](data, path, "service")
	case "fill":
		return parseAs[types.Fill](data, path, "fill")
	case "":
		return nil, errorf(path, "missing required field: kind")
	default:
		return nil, errorf(path, "unknown kind: %q", header.Kind)
	}
}

// parseAs unmarshals data into a value of type T, validates the kind field,
// and returns a pointer to the result.
func parseAs[T any](data []byte, path, expectedKind string) (*T, error) {
	var v T
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, errorf(path, "unmarshal %s: %w", expectedKind, err)
	}
	return &v, nil
}

// ParseArchetype reads and parses an archetype YAML file.
func ParseArchetype(path string) (*types.Archetype, error) {
	return parseFile[types.Archetype](path, "archetype")
}

// ParseComponent reads and parses a component YAML file.
func ParseComponent(path string) (*types.Component, error) {
	return parseFile[types.Component](path, "component")
}

// ParseMixin reads and parses a mixin YAML file.
func ParseMixin(path string) (*types.Mixin, error) {
	return parseFile[types.Mixin](path, "mixin")
}

// ParseServiceDeclaration reads and parses a service declaration YAML file.
func ParseServiceDeclaration(path string) (*types.ServiceDeclaration, error) {
	return parseFile[types.ServiceDeclaration](path, "service")
}

// ParseFill reads and parses a fill YAML file.
func ParseFill(path string) (*types.Fill, error) {
	return parseFile[types.Fill](path, "fill")
}

// kindOf returns the kind field value from a typed struct.
// Each supported type has a Kind field populated during unmarshal.
func kindOf(v any) string {
	switch t := v.(type) {
	case *types.Archetype:
		return t.Kind
	case *types.Component:
		return t.Kind
	case *types.Mixin:
		return t.Kind
	case *types.ServiceDeclaration:
		return t.Kind
	case *types.Fill:
		return t.Kind
	default:
		return ""
	}
}

// parseFile reads a YAML file, unmarshals it into T, and validates the kind.
func parseFile[T any](path, expectedKind string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &ParseError{Path: path, Err: err}
	}

	var v T
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, errorf(path, "unmarshal %s: %w", expectedKind, err)
	}

	// Validate the kind field.
	actual := kindOf(&v)
	if actual == "" {
		return nil, errorf(path, "missing required field: kind")
	}
	if actual != expectedKind {
		return nil, errorf(path, "expected kind %q, got %q", expectedKind, actual)
	}

	return &v, nil
}
