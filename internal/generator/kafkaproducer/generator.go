// Package kafkaproducer generates a Kafka publisher with verified TLS.
package kafkaproducer

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"strings"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed publisher.go.tmpl
var publisherSource string

//go:embed runtime.go.tmpl
var runtimeSource string

type Generator struct{}

// ValidateContext checks the publisher path and the optional outbox runtime.
func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	if outbox := ctx.PeerNamespaces["outbox"]; outbox != "" {
		if err := gen.ValidateGoPackageNamespace(outbox); err != nil {
			return err
		}
		if ctx.ModuleName == "" {
			return fmt.Errorf("Kafka runtime requires a module name")
		}
	} else if ctx.StorageContract != "" {
		return fmt.Errorf("Kafka service runtime requires the outbox component")
	}
	return nil
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	ns := ctx.OutputNamespace
	source, err := format.Source([]byte(strings.Replace(publisherSource, "package publisher", "package "+path.Base(ns), 1)))
	if err != nil {
		return nil, nil, fmt.Errorf("formatting Kafka publisher: %w", err)
	}
	files := []gen.File{{Path: path.Join(ns, "publisher.go"), Content: source}}
	wiring := &gen.Wiring{GoModRequires: map[string]string{
		"github.com/twmb/franz-go": "v1.21.6", "github.com/google/uuid": "v1.6.0",
	}}
	if outbox := ctx.PeerNamespaces["outbox"]; outbox != "" {
		data := struct{ Package, OutboxImport string }{path.Base(ns), path.Join(ctx.ModuleName, ctx.OutDirName, outbox)}
		tmpl, err := template.New("runtime").Parse(runtimeSource)
		if err != nil {
			return nil, nil, err
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return nil, nil, err
		}
		source, err := format.Source(buf.Bytes())
		if err != nil {
			return nil, nil, fmt.Errorf("formatting Kafka runtime: %w", err)
		}
		files = append(files, gen.File{Path: path.Join(ns, "runtime.go"), Content: source})
		wiring.Imports = []string{ns}
		wiring.Constructors = []string{path.Base(ns) + ".NewRuntime()"}
		wiring.ConstructorResources = map[int][]gen.Resource{0: {gen.ServiceContext, gen.SQLDatabase}}
		wiring.ConstructorReturnsError = map[int]bool{0: true}
		wiring.ConstructorDeferCalls = map[int]string{0: "Close()"}
		wiring.BackgroundTasks = []int{0}
	}
	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}
	return files, wiring, nil
}
