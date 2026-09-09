// Package cliapplication generates a CLI over the common HTTPS client.
package cliapplication

import (
	"bytes"
	_ "embed"
	"fmt"
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

type Generator struct{}

// MinimumGoVersion covers the os.Root file operations in generated code.
func (*Generator) MinimumGoVersion() string { return "1.25" }

func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if ctx.OutputNamespace == "" || gen.ValidatePath(ctx.OutputNamespace) != nil || ctx.ModuleName == "" || ctx.OutDirName == "" {
		return nil, nil, fmt.Errorf("CLI requires a module and output namespace")
	}
	factory, ok := ctx.ComponentConfig["factory_package"].(string)
	if !ok || factory == "" || gen.ValidatePath(factory) != nil || factory == ctx.OutDirName || strings.HasPrefix(factory, ctx.OutDirName+"/") {
		return nil, nil, fmt.Errorf("CLI requires a factory outside generated output")
	}
	root := path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace)
	data := struct{ Client, Runtime, Factory string }{root + "/client", root + "/command", path.Join(ctx.ModuleName, factory)}
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
	for _, item := range []struct{ name, source string }{{"command/runtime.go", runtimeSource + gen.UnicodeEscapeValidation}, {"command/config.go", configSource}, {"cmd/main.go", main}} {
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
	client, err := httpclient.Render(path.Join(ctx.OutputNamespace, "client"))
	if err != nil {
		return nil, nil, err
	}
	files = append(files, client)
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{}, nil
}
