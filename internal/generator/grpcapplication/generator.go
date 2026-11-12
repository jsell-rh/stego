// Package grpcapplication serves the gRPC methods of the declared commands and queries on the runtime library
// (Generator), and emits RPC contracts and process executables with no data access (ProcessGenerator).
package grpcapplication

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

//go:embed runtime.go.tmpl
var runtimeSource string

//go:embed conversion.go.tmpl
var conversionSource string

//go:embed client.go.tmpl
var clientSource string

//go:embed stream_headers.go.tmpl
var streamHeadersSource string

type Generator struct{}

// MinimumGoVersion covers the compiler minimum for golang.org/x/sys.
func (*Generator) MinimumGoVersion() string { return "1.26.0" }

// ValidateContext refuses a service that grpc-application cannot serve, then checks the settings of the component.
// grpc-application serves only the gRPC methods of the declared commands and queries, through the command and query
// components (ST-SUP-RC07-OC19). Its former bridge bound a hand-written factory to the GORM store, which STEGO no
// longer generates (ST-SUP-RC20-OC39).
func (*Generator) ValidateContext(ctx gen.Context) error {
	if len(ctx.Entities) == 0 && len(ctx.Commands) == 0 && len(ctx.Queries) == 0 {
		return fmt.Errorf("grpc-application serves the gRPC methods of the declared commands and queries (ST-SUP-RC07-OC19), and a service with no entity, no command and no query has none (ST-SUP-Q101); its former bridge to a hand-written factory on the GORM store is gone (ST-SUP-RC20-OC39): declare grpc-processes for an RPC process")
	}
	var missing []string
	for _, component := range []string{commandComponent, queryComponent} {
		if ctx.PeerNamespaces[component] == "" {
			missing = append(missing, component)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("grpc-application serves the declared commands and queries through the command and the query components (ST-SUP-RC07-OC19), and the service has no %s component; the former bridge to the GORM store is gone (ST-SUP-RC20-OC39): add the command and the query components", strings.Join(missing, " and no "))
	}
	return validateRuntimeData(ctx)
}

// Generate serves the declared methods through the command and query runtimes (ST-SUP-RC07-OC19).
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	return generateRuntimeData(ctx)
}

type templateSource struct{ Name, Source string }

func renderFiles(ctx gen.Context, data any, sources []templateSource) ([]gen.File, error) {
	var files []gen.File
	for _, item := range sources {
		tmpl, err := template.New(item.Name).Parse(item.Source)
		if err != nil {
			return nil, err
		}
		var buffer bytes.Buffer
		if err := tmpl.Execute(&buffer, data); err != nil {
			return nil, err
		}
		code, err := format.Source(buffer.Bytes())
		if err != nil {
			return nil, err
		}
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, item.Name), Content: code})
	}
	return files, nil
}
func rpcDependencies(ctx gen.Context) map[string]string {
	dependencies := map[string]string{"google.golang.org/grpc": "v1.83.2", "google.golang.org/protobuf": "v1.36.11"}
	if declared, _ := processes(ctx.ComponentConfig); len(declared) != 0 {
		dependencies["golang.org/x/net"] = "v0.60.0"
	}
	return dependencies
}
