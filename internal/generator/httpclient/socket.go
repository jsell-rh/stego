package httpclient

import (
	"bytes"
	_ "embed"
	"github.com/jsell-rh/stego/internal/gen"
	"go/format"
	"path"
	"text/template"
)

const WebSocketVersion = "v1.8.15"

//go:embed socket.go.tmpl
var socketSource string

// RenderWebSocket adds bounded WebSocket transport to the common HTTP client.
func RenderWebSocket(namespace, tracing string) (gen.File, error) {
	if err := gen.ValidatePath(namespace); err != nil {
		return gen.File{}, err
	}
	if tracing != "" {
		if err := gen.ValidateGoImportNamespace(tracing); err != nil {
			return gen.File{}, err
		}
	}
	tmpl, err := template.New("socket").Parse(socketSource)
	if err != nil {
		return gen.File{}, err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, struct{ Package, Tracing string }{path.Base(namespace), tracing}); err != nil {
		return gen.File{}, err
	}
	code, err := format.Source(out.Bytes())
	return gen.File{Path: path.Join(namespace, "socket.go"), Content: code}, err
}
