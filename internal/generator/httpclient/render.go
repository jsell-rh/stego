// Package httpclient renders the common HTTPS client for service and CLI code.
package httpclient

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

func Render(namespace string) (gen.File, error) {
	if err := gen.ValidatePath(namespace); err != nil {
		return gen.File{}, err
	}
	t, err := template.New("client").Parse(source)
	if err != nil {
		return gen.File{}, err
	}
	var output bytes.Buffer
	if err = t.Execute(&output, struct{ Package string }{path.Base(namespace)}); err != nil {
		return gen.File{}, err
	}
	code, err := format.Source(output.Bytes())
	return gen.File{Path: path.Join(namespace, "client.go"), Content: code}, err
}
