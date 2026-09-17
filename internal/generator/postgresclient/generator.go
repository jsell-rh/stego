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

//go:embed provision.go.tmpl
var provisionSource string

//go:embed schema.go.tmpl
var schemaSource string

//go:embed credentials.go.tmpl
var credentialsSource string

type Generator struct{}

// MinimumGoVersion covers the pinned pgx dependency.
func (*Generator) MinimumGoVersion() string { return "1.25.0" }

func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}

	if peer := ctx.PeerNamespaces["otel-tracing"]; peer != "" {
		return gen.ValidateGoPackageNamespace(peer)
	}
	return nil
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	tracing := ""
	if peer := ctx.PeerNamespaces["otel-tracing"]; peer != "" {
		tracing = path.Join(ctx.ModuleName, ctx.OutDirName, peer)
	}
	var files []gen.File
	for _, entry := range []struct{ name, source string }{{"client.go", source}, {"provision.go", provisionSource}, {"schema.go", schemaSource}, {"credentials.go", credentialsSource}} {
		tmpl, err := template.New(entry.name).Parse(entry.source)
		if err != nil {
			return nil, nil, err
		}
		var output bytes.Buffer
		if err = tmpl.Execute(&output, struct{ Package, Tracing string }{path.Base(ctx.OutputNamespace), tracing}); err != nil {
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
	return files, &gen.Wiring{GoModRequires: map[string]string{"github.com/jackc/pgx/v5": "v5.11.0"}}, nil
}
