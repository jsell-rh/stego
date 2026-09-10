// Package postgresclient generates verified PostgreSQL reads for external state.
package postgresclient

import (
	"bytes"
	_ "embed"
	"github.com/jsell-rh/stego/internal/gen"
	"go/format"
	"path"
	"text/template"
)

//go:embed client.go.tmpl
var source string

type Generator struct{}

// MinimumGoVersion covers the pinned pgx dependency.
func (*Generator) MinimumGoVersion() string { return "1.25.0" }

func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}

	return nil
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	tmpl, err := template.New("postgres-client").Parse(source)
	if err != nil {
		return nil, nil, err
	}
	var output bytes.Buffer
	if err = tmpl.Execute(&output, struct{ Package string }{path.Base(ctx.OutputNamespace)}); err != nil {
		return nil, nil, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return nil, nil, err
	}
	files := []gen.File{{Path: path.Join(ctx.OutputNamespace, "client.go"), Content: code}}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{GoModRequires: map[string]string{"github.com/jackc/pgx/v5": "v5.11.0"}}, nil
}
