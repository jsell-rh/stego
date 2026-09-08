package postgresadapter

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed transaction.go.tmpl
var transactionSource string

func generateTransaction(ctx gen.Context) (gen.File, error) {
	type lookup struct {
		Name   string
		Fields []string
	}
	data := struct {
		Package, OutboxImport, StorageImport, LookupImport string
		Entities                                           []lookup
	}{Package: path.Base(ctx.OutputNamespace), StorageImport: ctx.StorageContract}
	if ctx.StorageContract == "" && ctx.ModuleName != "" && ctx.PeerNamespaces["rest-api"] != "" {
		data.LookupImport = path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["rest-api"])
	}
	for _, entity := range ctx.Entities {
		item := lookup{Name: entity.Name, Fields: []string{"id"}}
		for _, field := range entity.Fields {
			if field.Unique && (field.Type == types.FieldTypeString || field.Type == types.FieldTypeRef || field.Type == types.FieldTypeEnum) {
				item.Fields = append(item.Fields, field.Name)
			}
		}
		data.Entities = append(data.Entities, item)
	}
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
