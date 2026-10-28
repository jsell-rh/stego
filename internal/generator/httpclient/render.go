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

// LinkPolicy says whether the emitted client may read a credential or CA file
// through a leaf symbolic link.
type LinkPolicy bool

// FollowLinks keeps the established behavior. A Kubernetes projected Secret
// mount is a symbolic link into "..data", so a service must follow the link.
const FollowLinks LinkPolicy = false

// RefuseLinks makes the emitted client refuse a leaf symbolic link. A command
// line program that reads a caller-supplied path asks for this.
const RefuseLinks LinkPolicy = true

func Render(namespace, tracing string, links LinkPolicy) (gen.File, error) {
	return render(namespace, tracing, 0, links)
}

// RenderLocalApplication adds a client for an application in the same network
// namespace. The compiler fixes its unprivileged port. The application must
// listen only on 127.0.0.1. New still requires verified HTTPS.
func RenderLocalApplication(namespace, tracing string, port int, links LinkPolicy) (gen.File, error) {
	if port < 1024 || port > 65535 {
		return gen.File{}, fmt.Errorf("local application port must be 1024 through 65535")
	}
	return render(namespace, tracing, port, links)
}

func render(namespace, tracing string, port int, links LinkPolicy) (gen.File, error) {
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
	data := struct {
		Package, Tracing, LocalAddress string
		RefuseLinks                    bool
	}{path.Base(namespace), tracing, fmt.Sprintf("127.0.0.1:%d", port), bool(links)}
	if err = t.Execute(&output, data); err != nil {
		return gen.File{}, err
	}
	code, err := format.Source(output.Bytes())
	return gen.File{Path: path.Join(namespace, "client.go"), Content: code}, err
}
