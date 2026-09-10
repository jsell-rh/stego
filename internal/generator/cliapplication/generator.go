// Package cliapplication generates a CLI over the common HTTPS client.
package cliapplication

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"github.com/jsell-rh/stego/internal/buildidentity"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpclient"
	"go/format"
	"path"
	"strings"
	"text/template"
)

//go:embed runtime.go.tmpl
var runtimeSource string

//go:embed config.go.tmpl
var configSource string

//go:embed output.go.tmpl
var outputSource string

//go:embed oauth.go.tmpl
var oauthSource string

//go:embed browser.go.tmpl
var browserSource string

//go:embed session.go.tmpl
var sessionSource string

//go:embed identity.go.tmpl
var identitySource string

//go:embed apply.go.tmpl
var applySource string

//go:embed apply_input.go.tmpl
var applyInputSource string

//go:embed version.go.tmpl
var versionSource string

//go:embed apply_immutable.go.tmpl
var applyImmutableSource string

type Generator struct{}

// MinimumGoVersion covers file operations and the OIDC dependency.
func (*Generator) MinimumGoVersion() string { return "1.25.0" }

func (*Generator) ValidateContext(ctx gen.Context) error {
	if ctx.OutputNamespace == "" || gen.ValidateGoImportNamespace(ctx.OutputNamespace) != nil || ctx.ModuleName == "" || ctx.OutDirName == "" {
		return fmt.Errorf("CLI requires a module and output namespace")
	}
	factory, ok := ctx.ComponentConfig["factory_package"].(string)
	if !ok || factory == "" || gen.ValidateGoImportNamespace(factory) != nil || factory == ctx.OutDirName || strings.HasPrefix(factory, ctx.OutDirName+"/") {
		return fmt.Errorf("CLI requires a factory outside generated output")
	}

	if tracing := ctx.PeerNamespaces["otel-tracing"]; tracing != "" {
		if err := gen.ValidateGoPackageNamespace(tracing); err != nil {
			return err
		}
	}
	return nil
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	factory := ctx.ComponentConfig["factory_package"].(string)
	root := path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace)
	record, err := json.Marshal(buildidentity.Current())
	if err != nil {
		return nil, nil, err
	}
	data := struct{ Client, Runtime, Factory, BuildIdentity, CompilerBuild string }{root + "/client", root + "/command", path.Join(ctx.ModuleName, factory), root + "/buildidentity", string(record)}
	main := `package main
import (
 "context"
 "fmt"
 "os"
 "os/signal"
 app {{printf "%q" .Factory}}
 command {{printf "%q" .Runtime}}
)
func main(){
 ctx,stop:=signal.NotifyContext(context.Background(),os.Interrupt);defer stop()
 if err:=command.Run(ctx,app.Commands(),os.Args[1:],os.Stdout);err!=nil {fmt.Fprintln(os.Stderr,err);os.Exit(1)}
}
`
	var files []gen.File
	for _, item := range []struct{ name, source string }{{"command/runtime.go", runtimeSource + gen.UnicodeEscapeValidation}, {"command/apply.go", applySource}, {"command/apply_immutable.go", applyImmutableSource}, {"command/apply_input.go", applyInputSource}, {"command/config.go", configSource}, {"command/output.go", outputSource}, {"command/oauth.go", oauthSource}, {"command/browser.go", browserSource}, {"command/session.go", sessionSource}, {"command/identity.go", identitySource}, {"command/version.go", versionSource}, {"buildidentity/runtime.go", buildidentity.Source}, {"cmd/main.go", main}} {
		t, err := template.New(item.name).Parse(item.source)
		if err != nil {
			return nil, nil, err
		}
		var out bytes.Buffer
		if err = t.Execute(&out, data); err != nil {
			return nil, nil, err
		}
		code, err := format.Source(out.Bytes())
		if err != nil {
			return nil, nil, fmt.Errorf("format CLI %s: %w", item.name, err)
		}
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, item.name), Content: code})
	}
	tracing := ""
	if peer := ctx.PeerNamespaces["otel-tracing"]; peer != "" {
		tracing = path.Join(ctx.ModuleName, ctx.OutDirName, peer)
	}
	client, err := httpclient.Render(path.Join(ctx.OutputNamespace, "client"), tracing)
	if err != nil {
		return nil, nil, err
	}
	files = append(files, client)
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{GoModRequires: map[string]string{"github.com/coreos/go-oidc/v3": "v3.21.0", "gopkg.in/yaml.v3": "v3.0.1"}}, nil
}
