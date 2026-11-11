package compiler

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// grpcComponent is the component whose proto files declare the gRPC methods of the commands and queries.
const grpcComponent = "grpc-application"

// grpcMethodFacts reads the kind of each method of the services of the proto files of the grpc-application
// component, for the check of each command's and query's gRPC method (ST-DECL-INV73) and the stream queries
// (ST-SUP-RC33-OC08). It gives nil when the component is not active. When the proto files cannot be read or do not
// compile, it gives the fault only for a declaration that names a gRPC method, and no method otherwise, so the
// component's own check names the fault.
func grpcMethodFacts(projectDir string, svcDecl *types.ServiceDeclaration, components map[string]*registry.Component) (map[string]declaration.MethodKind, error) {
	component := components[grpcComponent]
	if component == nil {
		return nil, nil
	}
	methods, err := readGRPCMethods(projectDir, resolveComponentConfig(component, svcDecl))
	if err == nil {
		return methods, nil
	}
	for _, c := range svcDecl.Commands {
		if c.Method != "" {
			return nil, err
		}
	}
	for _, q := range svcDecl.Queries {
		if q.Method != "" {
			return nil, err
		}
	}
	return map[string]declaration.MethodKind{}, nil
}

// readGRPCMethods reads the proto files that config names from the project, each at most its input limit, and gives
// the kind of each method of their services.
func readGRPCMethods(projectDir string, config map[string]any) (map[string]declaration.MethodKind, error) {
	files, err := new(grpcapplication.Generator).InputFiles(config)
	if err != nil {
		return nil, fmt.Errorf("reading the gRPC methods of %s: %w", grpcComponent, err)
	}
	inputs := map[string][]byte{}
	for _, file := range files {
		if !strings.HasSuffix(file.Path, ".proto") {
			continue
		}
		if err := gen.ValidatePath(file.Path); err != nil {
			return nil, fmt.Errorf("reading the gRPC methods of %s: %w", grpcComponent, err)
		}
		data, err := readLimited(filepath.Join(projectDir, filepath.FromSlash(file.Path)), file.MaxBytes)
		if err != nil {
			return nil, fmt.Errorf("reading the gRPC methods of %s: %s: %w", grpcComponent, file.Path, err)
		}
		inputs[file.Path] = data
	}
	methods, err := grpcapplication.Methods(config, inputs)
	if err != nil {
		return nil, fmt.Errorf("reading the gRPC methods of %s: %w", grpcComponent, err)
	}
	return methods, nil
}

// readLimited reads a regular file of at most limit bytes.
func readLimited(name string, limit int64) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("more than %d bytes", limit)
	}
	return data, nil
}
