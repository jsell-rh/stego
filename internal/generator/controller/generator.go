// Package controller generates a bounded reconciliation runtime.
package controller

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed runtime.go.tmpl
var source string

//go:embed keyed.go.tmpl
var keyedSource string

type Generator struct{}

func (*Generator) MinimumGoVersion() string { return "1.25.0" }

func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if ctx.OutputNamespace == "" || gen.ValidatePath(ctx.OutputNamespace) != nil {
		return nil, nil, fmt.Errorf("controller requires a valid output namespace")
	}
	if len(ctx.ComponentConfig) != 0 {
		return nil, nil, fmt.Errorf("controller has no component settings")
	}
	var files []gen.File
	for _, entry := range []struct{ name, source string }{{"runtime.go", source}, {"keyed.go", keyedSource}} {
		tmpl, err := template.New(entry.name).Parse(entry.source)
		if err != nil {
			return nil, nil, err
		}
		var output bytes.Buffer
		if err := tmpl.Execute(&output, struct{ Package string }{path.Base(ctx.OutputNamespace)}); err != nil {
			return nil, nil, err
		}
		code, err := format.Source(output.Bytes())
		if err != nil {
			return nil, nil, err
		}
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, entry.name), Content: code})
	}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{}, nil
}
