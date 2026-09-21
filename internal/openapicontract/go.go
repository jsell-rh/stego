package openapicontract

import (
	"fmt"
	"go/token"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/oapi-codegen/oapi-codegen/v2/pkg/codegen"
)

// GoMode selects one fixed backend profile. Callers cannot supply templates or
// change the backend's type, pruning, or version settings.
type GoMode uint8

const (
	GoModels GoMode = iota + 1
	GoClient
)

// The pinned backend uses package-level state. All Go consumers share this lock.
var goBackendMu sync.Mutex

// GenerateGo renders a document returned by Load. The caller must retain
// exclusive ownership of the document while this function runs.
func GenerateGo(doc *openapi3.T, packageName string, mode GoMode) (source string, err error) {
	source, _, err = generateGo(doc, packageName, mode, false)
	return source, err
}

// Schema names must be captured while the backend state is protected.
func generateGo(doc *openapi3.T, packageName string, mode GoMode, bind bool) (source string, names map[string]string, err error) {
	if doc == nil || doc.Paths == nil {
		return "", nil, fmt.Errorf("Go OpenAPI generation requires a loaded document")
	}
	if len(packageName) > 128 || !token.IsIdentifier(packageName) || packageName == "_" {
		return "", nil, fmt.Errorf("Go OpenAPI generation requires a package name")
	}
	if mode != GoModels && mode != GoClient {
		return "", nil, fmt.Errorf("unsupported Go OpenAPI generation mode")
	}
	goBackendMu.Lock()
	defer goBackendMu.Unlock()
	defer func() {
		if recover() != nil {
			source = ""
			names = nil
			err = fmt.Errorf("OpenAPI backend failed")
		}
	}()
	version := "stego/openapi-backend-v2.8.0"
	source, err = codegen.Generate(doc, codegen.Configuration{
		PackageName: packageName,
		Generate: codegen.GenerateOptions{
			Models: true,
			Client: mode == GoClient,
		},
		OutputOptions:        codegen.OutputOptions{SkipPrune: true, NullableType: true},
		NoVCSVersionOverride: &version,
	})
	if err != nil {
		return "", nil, fmt.Errorf("cannot generate Go OpenAPI code: %w", err)
	}
	if len(source) > 8<<20 {
		return "", nil, fmt.Errorf("Go OpenAPI output exceeds limit")
	}
	if bind && doc.Components != nil {
		// Use the backend's type declarations, including its schema renaming.
		definitions, err := codegen.GenerateTypesForSchemas(nil, doc.Components.Schemas, nil)
		if err != nil {
			return "", nil, fmt.Errorf("cannot bind Go OpenAPI schemas: %w", err)
		}
		names = make(map[string]string, len(doc.Components.Schemas))
		for _, definition := range definitions {
			if _, exists := doc.Components.Schemas[definition.JsonName]; !exists {
				continue
			}
			if _, exists := names[definition.JsonName]; exists {
				return "", nil, fmt.Errorf("duplicate Go OpenAPI schema binding")
			}
			names[definition.JsonName] = definition.TypeName
		}
		if len(names) != len(doc.Components.Schemas) {
			return "", nil, fmt.Errorf("incomplete Go OpenAPI schema bindings")
		}
	}
	return source, names, nil
}
