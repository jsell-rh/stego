// Package controller generates a bounded reconciliation runtime.
package controller

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed runtime.go.tmpl
var source string

//go:embed keyed.go.tmpl
var keyedSource string

//go:embed watch_keyed.go.tmpl
var watchKeyedSource string

//go:embed sweep.go.tmpl
var sweepSource string

//go:embed scan.go.tmpl
var scanSource string

//go:embed stream.go.tmpl
var streamSource string

//go:embed observation.go.tmpl
var observationSource string

//go:embed metrics.go.tmpl
var metricsSource string

//go:embed monitor.go.tmpl
var monitorSource string

//go:embed checkpoint.go.tmpl
var checkpointSource string

//go:embed cycle.go.tmpl
var cycleSource string

//go:embed telemetry.go.tmpl
var telemetrySource string

//go:embed process.go.tmpl
var processSource string

type Generator struct{}

func (*Generator) MinimumGoVersion() string { return "1.25.0" }

func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	if len(ctx.ComponentConfig) != 0 {
		return fmt.Errorf("controller has no component settings")
	}

	if tracing := ctx.PeerNamespaces["otel-tracing"]; tracing != "" {
		if err := gen.ValidateGoPackageNamespace(tracing); err != nil {
			return err
		}
		if ctx.ModuleName == "" {
			return fmt.Errorf("controller telemetry requires a module name")
		}
	}
	return nil
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	var files []gen.File
	tracing := ""
	if peer := ctx.PeerNamespaces["otel-tracing"]; peer != "" {
		tracing = path.Join(ctx.ModuleName, ctx.OutDirName, peer)
	}
	for _, entry := range []struct{ name, source string }{{"runtime.go", source}, {"keyed.go", keyedSource}, {"watch_keyed.go", watchKeyedSource}, {"sweep.go", sweepSource}, {"scan.go", scanSource}, {"stream.go", streamSource}, {"observation.go", observationSource}, {"metrics.go", metricsSource}, {"monitor.go", monitorSource}, {"process.go", processSource}, {"checkpoint.go", checkpointSource}, {"cycle.go", cycleSource}, {"telemetry.go", telemetrySource}} {
		tmpl, err := template.New(entry.name).Parse(entry.source)
		if err != nil {
			return nil, nil, err
		}
		var output bytes.Buffer
		if err := tmpl.Execute(&output, struct{ Package, Tracing string }{path.Base(ctx.OutputNamespace), tracing}); err != nil {
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
