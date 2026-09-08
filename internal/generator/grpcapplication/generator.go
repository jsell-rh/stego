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

type Generator struct{}

func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := gen.ValidatePath(ctx.OutputNamespace); err != nil {
		return nil, nil, err
	}
	factory, ok := ctx.ComponentConfig["factory_package"].(string)
	if !ok || factory == "" || gen.ValidatePath(factory) != nil || factory == ctx.OutDirName || strings.HasPrefix(factory, ctx.OutDirName+"/") {
		return nil, nil, fmt.Errorf("grpc-application requires a factory_package outside generated output")
	}
	if ctx.ModuleName == "" || ctx.StorageContract == "" || ctx.AuthPackage == "" || ctx.PeerNamespaces["jwt-auth"] == "" {
		return nil, nil, fmt.Errorf("grpc-application requires public storage and JWT verifier contracts")
	}
	files, err := generateProto(ctx)
	if err != nil {
		return nil, nil, err
	}
	data := struct{ Package, Factory, Storage, Auth, Transport string }{path.Base(ctx.OutputNamespace), path.Join(ctx.ModuleName, factory), ctx.StorageContract, ctx.AuthPackage, path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, "transport")}
	bridge := `package {{.Package}}
import (
 application {{printf "%q" .Factory}}
 storage {{printf "%q" .Storage}}
 auth {{printf "%q" .Auth}}
 transport {{printf "%q" .Transport}}
 "google.golang.org/grpc"
)
type Repository interface { storage.Storage; storage.Transactor }
func NewGRPCRuntime(repository Repository, verifier *auth.Verifier)(*transport.Runtime,error){
 return transport.New(verifier.Authenticate,func(registrar grpc.ServiceRegistrar)error{return application.Register(registrar,repository)})
}
`
	for _, item := range []struct{ Name, Source string }{{"bridge.go", bridge}, {"transport/runtime.go", runtimeSource}} {
		tmpl, err := template.New(item.Name).Parse(item.Source)
		if err != nil {
			return nil, nil, err
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return nil, nil, err
		}
		code, err := format.Source(buf.Bytes())
		if err != nil {
			return nil, nil, err
		}
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, item.Name), Content: code})
	}
	wiring := &gen.Wiring{Contracts: []gen.Contract{gen.StorageV1}, Imports: []string{ctx.OutputNamespace}, Constructors: []string{path.Base(ctx.OutputNamespace) + ".NewGRPCRuntime(store, verifierFromEnvironment)"}, ConstructorDeps: map[int][]string{0: {"store", "verifierFromEnvironment"}}, ConstructorReturnsError: map[int]bool{0: true}, ConstructorDeferCalls: map[int]string{0: "Close()"}, BackgroundTasks: []int{0}, GoModRequires: map[string]string{"google.golang.org/grpc": "v1.82.1", "google.golang.org/protobuf": "v1.36.11"}}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, wiring, nil
}
