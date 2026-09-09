// Package kubernetesclient generates bounded Kubernetes resource clients.
package kubernetesclient

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed client.go.tmpl
var source string

type Generator struct{}

func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := gen.ValidatePath(ctx.OutputNamespace); err != nil {
		return nil, nil, err
	}
	peer := ctx.PeerNamespaces["http-application"]
	if ctx.ModuleName == "" || peer == "" || gen.ValidatePath(peer) != nil {
		return nil, nil, fmt.Errorf("kubernetes-client requires the generated HTTP application client")
	}
	data := struct{ Package, Transport string }{path.Base(ctx.OutputNamespace), path.Join(ctx.ModuleName, ctx.OutDirName, peer, "client")}
	tmpl, err := template.New("client").Parse(source)
	if err != nil {
		return nil, nil, err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
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
	return files, &gen.Wiring{}, nil
}
