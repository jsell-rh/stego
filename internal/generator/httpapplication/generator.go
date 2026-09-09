// Package httpapplication connects domain HTTP handlers to the generated runtime.
package httpapplication

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"strings"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpclient"
)

//go:embed endpoint.go.tmpl
var endpointSource string

type Generator struct{}

func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := gen.ValidatePath(ctx.OutputNamespace); err != nil {
		return nil, nil, err
	}
	factory, ok := ctx.ComponentConfig["factory_package"].(string)
	if !ok || gen.ValidatePath(factory) != nil || factory == "" {
		return nil, nil, fmt.Errorf("http-application requires a module-relative factory_package")
	}
	if ctx.ModuleName == "" || ctx.StorageContract == "" || ctx.AuthPackage == "" || ctx.PeerNamespaces["jwt-auth"] == "" {
		return nil, nil, fmt.Errorf("http-application requires public storage and JWT verifier contracts")
	}
	// A factory must remain outside generated output to prevent an import cycle.
	if ctx.OutDirName != "" && (factory == ctx.OutDirName || strings.HasPrefix(factory, ctx.OutDirName+"/")) {
		return nil, nil, fmt.Errorf("application factory must be outside generated output")
	}
	data := struct{ Package, Factory, Storage, Auth string }{path.Base(ctx.OutputNamespace), path.Join(ctx.ModuleName, factory), ctx.StorageContract, ctx.AuthPackage}
	bridge := `package {{.Package}}
import (
 "database/sql"
 "context"
 "sync"
 "errors"
 "net/http"
 application {{printf "%q" .Factory}}
 storage {{printf "%q" .Storage}}
 auth {{printf "%q" .Auth}}
)
// Repository supplies operations and one atomic commit boundary.
type Repository = storage.Repository
// ManagedHandler adds application tasks and cleanup to the generated supervisor.
type ManagedHandler interface {http.Handler;Run(context.Context)error;Close()}
type Runtime struct {http.Handler;managed ManagedHandler;closeOnce sync.Once}
func(r *Runtime)Run(ctx context.Context)error{if r.managed!=nil{return r.managed.Run(ctx)};<-ctx.Done();return nil}
func(r *Runtime)Close(){r.closeOnce.Do(func(){if r.managed!=nil{r.managed.Close()}})}
// NewHandler connects application code to compiler-owned resources.
func NewHandler(repository Repository, verifier *auth.Verifier, database *sql.DB) (*Runtime,error) {
 handler,err:=application.New(repository,verifier,database)
 if err!=nil{return nil,err};if handler==nil{return nil,errors.New("application returned no handler")}
 managed,_:=any(handler).(ManagedHandler)
 _,hasRun:=any(handler).(interface{Run(context.Context)error})
 closer,hasClose:=any(handler).(interface{Close()})
 if hasRun!=hasClose{if hasClose{closer.Close()};return nil,errors.New("application lifecycle requires both Run and Close")}
 return &Runtime{Handler:handler,managed:managed},nil
}

`
	var files []gen.File
	for _, item := range []struct{ name, source string }{{"bridge.go", bridge}, {"transport/endpoint.go", endpointSource + gen.UnicodeEscapeValidation}} {
		itemData := data
		if strings.HasPrefix(item.name, "transport/") {
			itemData.Package = "transport"
		}
		if strings.HasPrefix(item.name, "client/") {
			itemData.Package = "client"
		}
		tmpl, err := template.New(item.name).Parse(item.source)
		if err != nil {
			return nil, nil, err
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, itemData); err != nil {
			return nil, nil, err
		}
		code, err := format.Source(out.Bytes())
		if err != nil {
			return nil, nil, fmt.Errorf("formatting HTTP application: %w", err)
		}
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, item.name), Content: code})
	}
	client, err := httpclient.Render(path.Join(ctx.OutputNamespace, "client"))
	if err != nil {
		return nil, nil, err
	}
	files = append(files, client)
	ns := path.Base(ctx.OutputNamespace)
	wiring := &gen.Wiring{Contracts: []gen.Contract{gen.StorageV1}, Imports: []string{ctx.OutputNamespace}, Constructors: []string{ns + ".NewHandler(store, verifierFromEnvironment)"}, ConstructorDeps: map[int][]string{0: {"store", "verifierFromEnvironment"}}, ConstructorResources: map[int][]gen.Resource{0: {gen.SQLDatabase}}, ConstructorReturnsError: map[int]bool{0: true}, ConstructorDeferCalls: map[int]string{0: "Close()"}, BackgroundTasks: []int{0}, Routes: []string{`mux.Handle("/", handler)`}}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, wiring, nil
}
