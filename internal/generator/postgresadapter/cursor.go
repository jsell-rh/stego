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

//go:embed cursor.go.tmpl
var cursorSource string

func generateCursor(ctx gen.Context) (gen.File, error) {
	type entity struct {
		Name    string
		Columns []string
	}
	data := struct {
		Package, StorageImport string
		Entities               []entity
	}{Package: path.Base(ctx.OutputNamespace), StorageImport: ctx.StorageContract}
	for _, e := range ctx.Entities {
		data.Entities = append(data.Entities, entity{e.Name, allColumns(e)})
	}
	tmpl, err := template.New("cursor").Parse(cursorSource)
	if err != nil {
		return gen.File{}, err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return gen.File{}, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("format storage cursor: %w", err)
	}
	return gen.File{Path: path.Join(ctx.OutputNamespace, "cursor.go"), Content: code}, nil
}
