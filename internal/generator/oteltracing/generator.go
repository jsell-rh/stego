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

//go:embed signals.go.tmpl
var signalsSource string

//go:embed service.go.tmpl
var serviceSource string

//go:embed controller.go.tmpl
var controllerSource string

//go:embed identity.go.tmpl
var identitySource string

//go:embed client.go.tmpl
var clientSource string

//go:embed http_client.go.tmpl
var httpClientSource string

//go:embed command.go.tmpl
var commandSource string

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
	var files []gen.File
	for _, item := range []struct{ name, source string }{{"runtime.go", source}, {"signals.go", signalsSource}, {"service.go", serviceSource}, {"controller.go", controllerSource}, {"identity.go", identitySource}, {"client.go", clientSource}, {"http_client.go", httpClientSource}, {"command.go", commandSource}} {
		tmpl, err := template.New(item.name).Parse(item.source)
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
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, item.name), Content: code})
	}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	loggerIndex := 0
	return files, &gen.Wiring{HTTPErrorLogger: &loggerIndex, Imports: []string{ctx.OutputNamespace}, Constructors: []string{path.Base(ctx.OutputNamespace) + ".NewTracingRuntime()"}, ConstructorReturnsError: map[int]bool{0: true}, ConstructorDeferCalls: map[int]string{0: "Close()"}, Middlewares: []gen.MiddlewareSpec{{ConstructorIndex: 0, WrapExpr: "%s.Route(%s)"}}, OuterMiddlewares: []gen.MiddlewareSpec{{ConstructorIndex: 0, WrapExpr: "%s.Handler(%s)"}}, GoModRequires: map[string]string{
		"go.opentelemetry.io/otel/metric": "v1.46.0", "go.opentelemetry.io/otel/sdk/metric": "v1.46.0", "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc": "v1.46.0",
		"go.opentelemetry.io/otel/log": "v0.22.0", "go.opentelemetry.io/otel/sdk/log": "v0.22.0", "go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc": "v0.22.0",
		"go.opentelemetry.io/otel": "v1.46.0", "go.opentelemetry.io/otel/trace": "v1.46.0", "go.opentelemetry.io/otel/sdk": "v1.46.0", "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc": "v1.46.0", "google.golang.org/grpc": "v1.83.1", "github.com/felixge/httpsnoop": "v1.0.4",
	}}, nil
}
