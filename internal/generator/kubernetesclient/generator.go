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

//go:embed discovery.go.tmpl
var discoverySource string

type Generator struct{}

func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidatePath(ctx.OutputNamespace); err != nil {
		return err
	}
	peer := ctx.PeerNamespaces["http-application"]
	if ctx.ModuleName == "" || peer == "" || gen.ValidatePath(peer) != nil {
		return fmt.Errorf("kubernetes-client requires the generated HTTP application client")
	}

	return nil
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	peer := ctx.PeerNamespaces["http-application"]
	data := struct{ Package, Transport string }{path.Base(ctx.OutputNamespace), path.Join(ctx.ModuleName, ctx.OutDirName, peer, "client")}
	files := []gen.File{}
	for _, entry := range []struct{ name, source string }{{"client.go", source}, {"observe.go", observeSource}, {"discovery.go", discoverySource}} {
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
