package gosdk

import (
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/openapicontract"
)

func (*Generator) InputFiles(config map[string]any) ([]gen.InputFile, error) {
	names, err := openapicontract.InputFiles(config)
	return gen.SourceInputs(names), err
}
func loadDocument(ctx gen.Context) (*openapi3.T, error) { return openapicontract.Load(ctx) }
