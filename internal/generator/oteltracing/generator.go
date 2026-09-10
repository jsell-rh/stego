// Package oteltracing generates bounded HTTP and gRPC tracing and OTLP export.
package oteltracing

import (
	"bytes"
	_ "embed"
	"fmt"
	"github.com/jsell-rh/stego/internal/gen"
	"go/format"
	"path"
	"strings"
	"text/template"
)

//go:embed runtime.go.tmpl
var source string

type Generator struct{}

func (*Generator) MinimumGoVersion() string { return "1.26.0" }
func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	if len(ctx.ComponentConfig) != 0 {
		return fmt.Errorf("otel-tracing has no component settings")
	}
	if len(ctx.ServiceName) == 0 || len(ctx.ServiceName) > 128 || strings.Trim(ctx.ServiceName, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") != "" {
		return fmt.Errorf("otel-tracing requires a service name")
	}
	return nil
}
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	tmpl, err := template.New("tracing").Parse(source)
	if err != nil {
		return nil, nil, err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, struct{ Package, Service string }{path.Base(ctx.OutputNamespace), ctx.ServiceName}); err != nil {
		return nil, nil, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return nil, nil, err
	}
	files := []gen.File{{Path: path.Join(ctx.OutputNamespace, "runtime.go"), Content: code}}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{Imports: []string{ctx.OutputNamespace}, Constructors: []string{path.Base(ctx.OutputNamespace) + ".NewTracingRuntime()"}, ConstructorReturnsError: map[int]bool{0: true}, ConstructorDeferCalls: map[int]string{0: "Close()"}, Middlewares: []gen.MiddlewareSpec{{ConstructorIndex: 0, WrapExpr: "%s.Route(%s)"}}, OuterMiddlewares: []gen.MiddlewareSpec{{ConstructorIndex: 0, WrapExpr: "%s.Handler(%s)"}}, GoModRequires: map[string]string{
		"go.opentelemetry.io/otel": "v1.46.0", "go.opentelemetry.io/otel/trace": "v1.46.0", "go.opentelemetry.io/otel/sdk": "v1.46.0", "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc": "v1.46.0", "google.golang.org/grpc": "v1.83.1", "github.com/felixge/httpsnoop": "v1.0.4",
	}}, nil
}
