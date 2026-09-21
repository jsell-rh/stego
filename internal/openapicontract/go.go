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
	if doc == nil || doc.Paths == nil {
		return "", fmt.Errorf("Go OpenAPI generation requires a loaded document")
	}
	if len(packageName) > 128 || !token.IsIdentifier(packageName) || packageName == "_" {
		return "", fmt.Errorf("Go OpenAPI generation requires a package name")
	}
	if mode != GoModels && mode != GoClient {
		return "", fmt.Errorf("unsupported Go OpenAPI generation mode")
	}
	goBackendMu.Lock()
	defer goBackendMu.Unlock()
	defer func() {
		if recover() != nil {
			source = ""
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
		return "", fmt.Errorf("cannot generate Go OpenAPI code: %w", err)
	}
	if len(source) > 8<<20 {
		return "", fmt.Errorf("Go OpenAPI output exceeds limit")
	}
	return source, nil
}
