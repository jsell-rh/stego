// Package parser reads the declaration (service.yaml) through the strict decoder,
// and gives the strict decoder and the kind check to the packages that read
// their own documents: the registry (archetypes, components and mixins) and
// fills.
package parser

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// ParseError wraps a parse failure with the originating file path and optional
// line context extracted from the YAML parser.
type ParseError struct {
	Path    string
	Line    int    // 0 means unknown
	Context string // snippet of the offending line, if available
	Err     error
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		if e.Context != "" {
			return fmt.Sprintf("%s:%d: %s (near %q)", e.Path, e.Line, e.Err, e.Context)
		}
		return fmt.Sprintf("%s:%d: %s", e.Path, e.Line, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error {
	return e.Err
}

// errorf creates a ParseError for the given path.
func errorf(path, format string, args ...any) *ParseError {
	return &ParseError{Path: path, Err: fmt.Errorf(format, args...)}
}

// parseErrorWithLineInfo attempts to extract line number and context from a
// yaml.TypeError or by decoding into a yaml.Node, and returns a ParseError
// enriched with that information.
func parseErrorWithLineInfo(data []byte, path string, underlying error) *ParseError {
	pe := &ParseError{Path: path, Err: underlying}

	// yaml.TypeError contains per-line error details we can extract line numbers from.
	// Use errors.As to unwrap through fmt.Errorf wrapping layers.
	var te *yaml.TypeError
	if errors.As(underlying, &te) && len(te.Errors) > 0 {
		// Try to extract line info from the first error string.
		// yaml.v3 TypeError errors look like: "line N: ..."
		for _, msg := range te.Errors {
			var line int
			if _, err := fmt.Sscanf(msg, "line %d:", &line); err == nil && line > 0 {
				pe.Line = line
				pe.Context = lineAt(data, line)
				return pe
			}
		}
	}

	// For other error types, try to decode into a yaml.Node to get line info.
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err == nil && node.Content != nil && len(node.Content) > 0 {
		// The node tree parsed fine but typed unmarshal failed — report the
		// document root line as a starting point (better than nothing).
		pe.Line = node.Content[0].Line
		pe.Context = lineAt(data, pe.Line)
	}

	return pe
}

// lineAt returns the content of line number n (1-based) from data, trimmed.
func lineAt(data []byte, n int) string {
	lines := strings.Split(string(data), "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	return strings.TrimSpace(lines[n-1])
}

// ParseServiceDeclaration reads and parses a service declaration YAML file.
func ParseServiceDeclaration(path string) (*types.ServiceDeclaration, error) {
	return ReadKind(path, "service", serviceKind)
}

// ParseServiceDeclarationFromBytes parses a service declaration from raw YAML
// bytes. The path is used only for error messages.
func ParseServiceDeclarationFromBytes(data []byte, path string) (*types.ServiceDeclaration, error) {
	return ParseKind(data, path, "service", serviceKind)
}

func serviceKind(s *types.ServiceDeclaration) string { return s.Kind }

// ReadKind reads a bounded YAML file and decodes it as ParseKind does.
func ReadKind[T any](path, expectedKind string, kind func(*T) string) (*T, error) {
	data, err := ReadDocument(path)
	if err != nil {
		return nil, err
	}
	return ParseKind(data, path, expectedKind, kind)
}

// ParseKind unmarshals data into T through the strict decoder, checks that
// the kind field, which kind reads, is the expected kind, and returns a
// pointer to the result. The path is used only for error messages.
func ParseKind[T any](data []byte, path, expectedKind string, kind func(*T) string) (*T, error) {
	var v T
	if err := DecodeStrict(data, path, &v); err != nil {
		return nil, err
	}

	// Validate the kind field.
	actual := kind(&v)
	if actual == "" {
		return nil, errorf(path, "missing required field: kind")
	}
	if actual != expectedKind {
		return nil, errorf(path, "expected kind %q, got %q", expectedKind, actual)
	}

	return &v, nil
}
