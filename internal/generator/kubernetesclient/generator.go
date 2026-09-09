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

//go:embed observe.go.tmpl
var observeSource string

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
	files := []gen.File{}
	for _, entry := range []struct{ name, source string }{{"client.go", source}, {"observe.go", observeSource}} {
		tmpl, err := template.New(entry.name).Parse(entry.source)
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
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, entry.name), Content: code})
	}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{}, nil
}
