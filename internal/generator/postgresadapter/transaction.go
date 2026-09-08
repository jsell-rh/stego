package postgresadapter

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed transaction.go.tmpl
var transactionSource string

func generateTransaction(ctx gen.Context) (gen.File, error) {
	data := struct{ Package, OutboxImport, StorageImport string }{Package: path.Base(ctx.OutputNamespace), StorageImport: ctx.StorageContract}
	if ns := ctx.PeerNamespaces["outbox"]; ns != "" {
		if err := gen.ValidatePath(ns); err != nil {
			return gen.File{}, err
		}
		if ctx.ModuleName == "" {
			return gen.File{}, fmt.Errorf("outbox integration requires a module name")
		}
		data.OutboxImport = path.Join(ctx.ModuleName, ctx.OutDirName, ns)
	}
	tmpl, err := template.New("transaction").Parse(transactionSource)
	if err != nil {
		return gen.File{}, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return gen.File{}, err
	}
	source, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting transaction: %w", err)
	}
	return gen.File{Path: path.Join(ctx.OutputNamespace, "transaction.go"), Content: source}, nil
}
