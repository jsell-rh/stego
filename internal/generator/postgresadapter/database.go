package postgresadapter

import (
	"bytes"
	_ "embed"
	"github.com/jsell-rh/stego/internal/gen"
	"go/format"
	"path"
	"text/template"
)

//go:embed database.go.tmpl
var databaseSource string

func generateDatabaseOpener(ctx gen.Context) (gen.File, error) {
	t, err := template.New("database").Parse(databaseSource)
	if err != nil {
		return gen.File{}, err
	}
	var output bytes.Buffer
	tracing := ""
	if peer := ctx.PeerNamespaces["otel-tracing"]; peer != "" {
		tracing = path.Join(ctx.ModuleName, ctx.OutDirName, peer)
	}
	err = t.Execute(&output, struct{ Package, Tracing string }{path.Base(ctx.OutputNamespace), tracing})
	if err != nil {
		return gen.File{}, err
	}
	code, err := format.Source(output.Bytes())
	return gen.File{Path: path.Join(ctx.OutputNamespace, "database.go"), Content: code}, err
}
