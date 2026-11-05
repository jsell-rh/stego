package compiler

import (
	"fmt"
	"path/filepath"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// compilationSource holds one read of the declaration and registry. Validation,
// generation, and the state hash must use the same declaration bytes.
type compilationSource struct {
	ServiceData []byte
	Service     *types.ServiceDeclaration
	// Undeclared holds the fields that the declaration format does not declare; validation reports each.
	Undeclared []parser.UndeclaredField
	Registry   *registry.Registry
	Resolved   *resolvedCompilation
}

func loadCompilationSource(input ReconcilerInput) (*compilationSource, error) {
	path := filepath.Join(input.ProjectDir, "service.yaml")
	data, err := parser.ReadDocument(path)
	if err != nil {
		return nil, fmt.Errorf("reading service.yaml: %w", err)
	}
	service, undeclared, err := parser.ReadServiceDeclaration(data, path)
	if err != nil {
		return nil, &declarationReadError{fmt.Errorf("parsing service.yaml: %w", err)}
	}
	dirs := input.RegistryDirs
	if len(dirs) == 0 {
		dirs = []string{input.RegistryDir}
	}
	reg, err := registry.LoadDirectories(dirs...)
	if err != nil {
		return nil, fmt.Errorf("loading registry: %w", err)
	}
	return &compilationSource{ServiceData: data, Service: service, Undeclared: undeclared, Registry: reg}, nil
}

// declarationReadError is a declaration that cannot be read as one document. Validation reports it as the one
// fault of the declaration (ST-DECL-UC04-R06); plan and apply stop with it.
type declarationReadError struct{ err error }

func (e *declarationReadError) Error() string { return e.err.Error() }
func (e *declarationReadError) Unwrap() error { return e.err }
