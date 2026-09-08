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
	Registry    *registry.Registry
}

func loadCompilationSource(input ReconcilerInput) (*compilationSource, error) {
	path := filepath.Join(input.ProjectDir, "service.yaml")
	data, err := parser.ReadDocument(path)
	if err != nil {
		return nil, fmt.Errorf("reading service.yaml: %w", err)
	}
	service, err := parser.ParseServiceDeclarationFromBytes(data, path)
	if err != nil {
		return nil, fmt.Errorf("parsing service.yaml: %w", err)
	}
	reg, err := registry.Load(input.RegistryDir)
	if err != nil {
		return nil, fmt.Errorf("loading registry: %w", err)
	}
	return &compilationSource{ServiceData: data, Service: service, Registry: reg}, nil
}
