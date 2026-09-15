// Package httpclient renders the common HTTPS client for service and CLI code.
package httpclient

import (
	"bytes"
	_ "embed"
	"fmt"
	"github.com/jsell-rh/stego/internal/gen"
	"go/format"
	"path"
	"text/template"
)

//go:embed client.go.tmpl
var source string

//go:embed local.go.tmpl
var localSource string

func Render(namespace, tracing string) (gen.File, error) {
	return render(namespace, tracing, 0)
}

// RenderLocalApplication adds a client for an application in the same network
// namespace. The compiler fixes its unprivileged port. The application must
// listen only on 127.0.0.1. New still requires verified HTTPS.
func RenderLocalApplication(namespace, tracing string, port int) (gen.File, error) {
	if port < 1024 || port > 65535 {
		return gen.File{}, fmt.Errorf("local application port must be 1024 through 65535")
	}
	return render(namespace, tracing, port)
}

func render(namespace, tracing string, port int) (gen.File, error) {
	if err := gen.ValidatePath(namespace); err != nil {
		return gen.File{}, err
	}
	if tracing != "" {
		if err := gen.ValidateGoImportNamespace(tracing); err != nil {
			return gen.File{}, err
		}
	}
	input := source
	if port != 0 {
		input += localSource
	}
	t, err := template.New("client").Parse(input)
	if err != nil {
		return gen.File{}, err
	}
	var output bytes.Buffer
	if err = t.Execute(&output, struct{ Package, Tracing, LocalAddress string }{path.Base(namespace), tracing, fmt.Sprintf("127.0.0.1:%d", port)}); err != nil {
		return gen.File{}, err
	}
	code, err := format.Source(output.Bytes())
	return gen.File{Path: path.Join(namespace, "client.go"), Content: code}, err
}
