// Package grpcapplication connects protobuf contracts and domain services to a gRPC runtime.
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

//go:embed client.go.tmpl
var clientSource string

//go:embed stream_headers.go.tmpl
var streamHeadersSource string

type Generator struct{}

// MinimumGoVersion covers the compiler minimum for golang.org/x/sys.
func (*Generator) MinimumGoVersion() string { return "1.26.0" }

func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	_, err := prepareProto(ctx)
	return err
}
func validateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	factory, ok := ctx.ComponentConfig["factory_package"].(string)
	if !ok || factory == "" || gen.ValidateGoImportNamespace(factory) != nil || factory == ctx.OutDirName || strings.HasPrefix(factory, ctx.OutDirName+"/") {
		return fmt.Errorf("grpc-application requires a factory_package outside generated output")
	}
	if ctx.ModuleName == "" || ctx.StorageContract == "" || ctx.AuthPackage == "" || ctx.PeerNamespaces["jwt-auth"] == "" {
		return fmt.Errorf("grpc-application requires public storage and JWT verifier contracts")
	}
	watch := false
	if value, present := ctx.ComponentConfig["watch_events"]; present {
		var ok bool
		watch, ok = value.(bool)
		if !ok {
			return fmt.Errorf("watch_events must be a boolean")
		}
	}
	if watch && (ctx.EventsContract == "" || ctx.PeerNamespaces["outbox"] == "") {
		return fmt.Errorf("watch_events requires the outbox and public event contract")
	}

	if watch {
		if err := gen.ValidateGoPackageNamespace(ctx.PeerNamespaces["outbox"]); err != nil {
			return err
		}
	}

	if tracing := ctx.PeerNamespaces["otel-tracing"]; tracing != "" {
		if err := gen.ValidateGoPackageNamespace(tracing); err != nil {
			return err
		}
	}
	return validateProcesses(ctx)
}

func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := validateContext(ctx); err != nil {
		return nil, nil, err
	}
	factory := ctx.ComponentConfig["factory_package"].(string)
	watch, _ := ctx.ComponentConfig["watch_events"].(bool)
	files, methods, err := generateProto(ctx)
	if err != nil {
		return nil, nil, err
	}
	data := struct {
		Package, Factory, Storage, Auth, Transport, Events, Tracing string
		Methods                                                     []string
		Watch                                                       bool
	}{path.Base(ctx.OutputNamespace), path.Join(ctx.ModuleName, factory), ctx.StorageContract, ctx.AuthPackage, path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, "transport"), ctx.EventsContract, "", methods, watch}
	if tracing := ctx.PeerNamespaces["otel-tracing"]; tracing != "" {
		data.Tracing = path.Join(ctx.ModuleName, ctx.OutDirName, tracing)
	}
	bridge := `package {{.Package}}
import (
 application {{printf "%q" .Factory}}
 storage {{printf "%q" .Storage}}
 auth {{printf "%q" .Auth}}
 transport {{printf "%q" .Transport}}
 "google.golang.org/grpc"
 "context"
 "time"
 {{if .Tracing}}tracing {{printf "%q" .Tracing}}{{end}}
 {{if .Watch}}events {{printf "%q" .Events}}{{end}}
)
type Repository = storage.Repository
func NewGRPCRuntime(repository Repository, verifier *auth.Verifier{{if .Watch}},source events.Source{{end}}{{if .Tracing}},tracingRuntime *tracing.Runtime{{end}})(*transport.Runtime,error){
 return transport.New(verifier.Authenticate,func(registrar grpc.ServiceRegistrar)error{return application.Register(registrar,repository{{if .Watch}},source{{end}})},transport.Options{ {{if .Tracing}}TraceRPC:tracingRuntime.TraceRPC,{{end}}IdentityInfo:func(ctx context.Context)(string,time.Time){identity:=auth.IdentityFromContext(ctx);return identity.UserID,identity.ExpiresAt}})
}
`
	runtimeFiles, err := renderFiles(ctx, data, []templateSource{{"bridge.go", bridge}, {"transport/runtime.go", runtimeSource}, {"client/client.go", clientSource}, {"client/stream_headers.go", streamHeadersSource}})
	if err != nil {
		return nil, nil, err
	}
	files = append(files, runtimeFiles...)
	wiring := &gen.Wiring{Contracts: []gen.Contract{gen.StorageV1}, Imports: []string{ctx.OutputNamespace}, Constructors: []string{path.Base(ctx.OutputNamespace) + ".NewGRPCRuntime(store, verifierFromEnvironment)"}, ConstructorDeps: map[int][]string{0: {"store", "verifierFromEnvironment"}}, ConstructorReturnsError: map[int]bool{0: true}, ConstructorDeferCalls: map[int]string{0: "Close()"}, BackgroundTasks: []int{0}, GoModRequires: rpcDependencies(ctx)}
	if watch {
		outbox := ctx.PeerNamespaces["outbox"]
		if err := gen.ValidateGoPackageNamespace(outbox); err != nil {
			return nil, nil, err
		}
		wiring.Imports = append(wiring.Imports, outbox)
		wiring.Contracts = append(wiring.Contracts, gen.EventsV1)
		wiring.Constructors = []string{path.Base(outbox) + ".NewSource()", path.Base(ctx.OutputNamespace) + ".NewGRPCRuntime(store, verifierFromEnvironment, source)"}
		wiring.ConstructorResources = map[int][]gen.Resource{0: {gen.ServiceContext, gen.SQLDatabase}}
		wiring.ConstructorDeps = map[int][]string{1: {"store", "verifierFromEnvironment", "source"}}
		wiring.ConstructorReturnsError = map[int]bool{0: true, 1: true}
		wiring.ConstructorDeferCalls = map[int]string{0: "Close()", 1: "Close()"}
		wiring.BackgroundTasks = []int{0, 1}
	}
	if data.Tracing != "" {
		index := len(wiring.Constructors) - 1
		wiring.Constructors[index] = strings.TrimSuffix(wiring.Constructors[index], ")") + ", tracingRuntime)"
		wiring.ConstructorDeps[index] = append(wiring.ConstructorDeps[index], "tracingRuntime")
	}
	processes, err := processFiles(ctx)
	if err != nil {
		return nil, nil, err
	}
	files = append(files, processes...)
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, wiring, nil
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
	dependencies := map[string]string{"google.golang.org/grpc": "v1.82.1", "google.golang.org/protobuf": "v1.36.11"}
	if declared, _ := processes(ctx.ComponentConfig); len(declared) != 0 {
		dependencies["golang.org/x/net"] = "v0.58.0"
	}
	return dependencies
}
