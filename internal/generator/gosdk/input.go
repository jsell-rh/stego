package gosdk

import (
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/openapicontract"
)

func (*Generator) InputFiles(config map[string]any) ([]string, error) {
	return openapicontract.InputFiles(config)
}
func loadDocument(ctx gen.Context) (*openapi3.T, error) { return openapicontract.Load(ctx) }
