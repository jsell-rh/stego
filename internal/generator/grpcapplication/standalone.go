package grpcapplication

import (
	"fmt"
	"path"

	"github.com/jsell-rh/stego/internal/gen"
)

// ProcessGenerator supplies RPC contracts and executables without API storage.
// Both generators use the same protobuf compiler and runtime templates.
type ProcessGenerator struct{}

func (*ProcessGenerator) MinimumGoVersion() string { return new(Generator).MinimumGoVersion() }
func (*ProcessGenerator) InputFiles(config map[string]any) ([]string, error) {
	return new(Generator).InputFiles(config)
}
func (*ProcessGenerator) ValidateContext(ctx gen.Context) error {
	if err := validateStandalone(ctx); err != nil {
		return err
	}
	_, err := prepareProto(ctx)
	return err
}
func validateStandalone(ctx gen.Context) error {
	for key := range ctx.ComponentConfig {
		if key != "processes" && key != "proto_files" {
			return fmt.Errorf("unsupported grpc-processes setting %q", key)
		}
	}
	if gen.ValidateGoPackageNamespace(ctx.OutputNamespace) != nil || ctx.ModuleName == "" || ctx.AuthPackage == "" || gen.ValidateGoImportNamespace(ctx.AuthPackage) != nil || ctx.PeerNamespaces["jwt-auth"] == "" {
		return fmt.Errorf("RPC processes require a valid namespace, module, and JWT verifier")
	}
	if tracing := ctx.PeerNamespaces["otel-tracing"]; tracing == "" || gen.ValidateGoPackageNamespace(tracing) != nil {
		return fmt.Errorf("RPC processes require otel-tracing")
	}
	entries, err := processes(ctx.ComponentConfig)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("grpc-processes requires process declarations")
	}
	return validateProcesses(ctx)
}
func (*ProcessGenerator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := validateStandalone(ctx); err != nil {
		return nil, nil, err
	}
	files, methods, err := generateProto(ctx)
	if err != nil {
		return nil, nil, err
	}
	data := struct {
		Tracing string
		Methods []string
	}{path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["otel-tracing"]), methods}
	runtimeFiles, err := renderFiles(ctx, data, []templateSource{{"transport/runtime.go", runtimeSource}, {"client/client.go", clientSource}, {"client/stream_headers.go", streamHeadersSource}})
	if err != nil {
		return nil, nil, err
	}
	files = append(files, runtimeFiles...)
	entries, err := processFiles(ctx)
	if err != nil {
		return nil, nil, err
	}
	files = append(files, entries...)
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{GoModRequires: rpcDependencies()}, nil
}
