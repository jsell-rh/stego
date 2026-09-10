package grpcapplication

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication/grpcgen"
	gengo "google.golang.org/protobuf/cmd/protoc-gen-go/internal_gengo"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

type source struct{ File, Import string }

func sources(config map[string]any) ([]source, error) {
	values, ok := config["proto_files"].([]any)
	if !ok || len(values) == 0 || len(values) > 128 {
		return nil, fmt.Errorf("grpc-application requires 1 to 128 proto_files")
	}
	result := make([]source, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok || len(item) != 2 {
			return nil, fmt.Errorf("proto_files requires path and import_path")
		}
		file, ok1 := item["path"].(string)
		name, ok2 := item["import_path"].(string)
		if !ok1 || !ok2 || gen.ValidatePath(file) != nil || gen.ValidatePath(name) != nil || !strings.HasSuffix(file, ".proto") || !strings.HasSuffix(name, ".proto") || strings.HasPrefix(name, "google/protobuf/") || seen[name] {
			return nil, fmt.Errorf("invalid or duplicate protobuf source")
		}
		seen[name] = true
		result = append(result, source{file, name})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Import < result[j].Import })
	return result, nil
}

func (*Generator) InputFiles(config map[string]any) ([]string, error) {
	items, err := sources(config)
	if err != nil {
		return nil, err
	}
	result := make([]string, len(items))
	for i, item := range items {
		result[i] = item.File
	}
	return result, nil
}

func prepareProto(ctx gen.Context) (*protogen.Plugin, error) {
	items, err := sources(ctx.ComponentConfig)
	if err != nil {
		return nil, err
	}
	text := map[string]string{}
	names := make([]string, len(items))
	for i, item := range items {
		data, ok := ctx.Inputs[item.File]
		if !ok {
			return nil, fmt.Errorf("missing compiler input %q", item.File)
		}
		text[item.Import] = string(data)
		names[i] = item.Import
	}
	compiler := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{Accessor: protocompile.SourceAccessorFromMap(text)}), MaxParallelism: 1, SourceInfoMode: protocompile.SourceInfoStandard}
	compileContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	linked, err := compiler.Compile(compileContext, names...)
	if err != nil {
		return nil, err
	}
	request := &pluginpb.CodeGeneratorRequest{FileToGenerate: names, Parameter: proto.String("paths=source_relative")}
	visited := map[string]bool{}
	var appendFile func(protoreflect.FileDescriptor) error
	appendFile = func(file protoreflect.FileDescriptor) error {
		if visited[file.Path()] {
			return nil
		}
		visited[file.Path()] = true
		for i := 0; i < file.Imports().Len(); i++ {
			if err := appendFile(file.Imports().Get(i).FileDescriptor); err != nil {
				return err
			}
		}
		descriptor := protodesc.ToFileDescriptorProto(file)
		if _, local := text[file.Path()]; local {
			if descriptor.GetSyntax() != "proto3" {
				return fmt.Errorf("only proto3 API contracts are supported: %s", file.Path())
			}
			if descriptor.Options == nil {
				descriptor.Options = &descriptorpb.FileOptions{}
			}
			descriptor.Options.GoPackage = proto.String(path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, "pb", path.Dir(file.Path())))
		}
		request.ProtoFile = append(request.ProtoFile, descriptor)
		return nil
	}
	for _, file := range linked {
		if err := appendFile(file); err != nil {
			return nil, err
		}
	}
	plugin, err := (protogen.Options{}).New(request)
	if err != nil {
		return nil, err
	}
	for _, file := range plugin.Files {
		if !file.Generate {
			continue
		}
		if err := gen.ValidateGoImportNamespace(string(file.GoImportPath)); err != nil {
			return nil, fmt.Errorf("protobuf %s: %w", file.Desc.Path(), err)
		}
		if err := gen.ValidateGoLibraryName(string(file.GoPackageName)); err != nil {
			return nil, fmt.Errorf("protobuf %s: %w", file.Desc.Path(), err)
		}
	}
	return plugin, nil
}

func generateProto(ctx gen.Context) ([]gen.File, error) {
	plugin, err := prepareProto(ctx)
	if err != nil {
		return nil, err
	}
	for _, file := range plugin.Files {
		if file.Generate {
			gengo.GenerateFile(plugin, file)
			grpcgen.GenerateFile(plugin, file)
		}
	}
	response := plugin.Response()
	if response.GetError() != "" {
		return nil, fmt.Errorf("protobuf generation: %s", response.GetError())
	}
	result := make([]gen.File, 0, len(response.File))
	for _, file := range response.File {
		result = append(result, gen.File{Path: path.Join(ctx.OutputNamespace, "pb", file.GetName()), Content: []byte(file.GetContent())})
	}
	return result, nil
}
