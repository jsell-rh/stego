package command

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/bufbuild/protocompile"
	"github.com/jsell-rh/stego/internal/gen"
	gengo "google.golang.org/protobuf/cmd/protoc-gen-go/internal_gengo"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// protoSource is one proto file of the command messages: its path in the project and its import path.
type protoSource struct {
	File, Import string
}

// protoSources reads the setting proto_files: one to 32 proto3 files, each with its project path and its import
// path, which hold the request and the response message of each command (ST-SUP-RC32-OC11).
func protoSources(config map[string]any) ([]protoSource, error) {
	values, ok := config["proto_files"].([]any)
	if !ok || len(values) == 0 || len(values) > 32 {
		return nil, fmt.Errorf("command: the setting proto_files needs 1 to 32 proto files that hold the request and the response message of each command")
	}
	seen := map[string]bool{}
	var result []protoSource
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok || len(item) != 2 {
			return nil, fmt.Errorf("command: each item of proto_files has exactly a path and an import_path")
		}
		file, ok1 := item["path"].(string)
		name, ok2 := item["import_path"].(string)
		if !ok1 || !ok2 || gen.ValidatePath(file) != nil || gen.ValidatePath(name) != nil || !strings.HasSuffix(file, ".proto") ||
			!strings.HasSuffix(name, ".proto") || strings.HasPrefix(name, "google/") || seen[name] {
			return nil, fmt.Errorf("command: the proto file %v is not a project .proto path with a unique import path", value)
		}
		seen[name] = true
		result = append(result, protoSource{File: file, Import: name})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Import < result[j].Import })
	return result, nil
}

// messages are the compiled command messages, ready for protoc-gen-go.
type messages struct {
	plugin *protogen.Plugin
	byName map[string]*message
}

// message is one top-level message of the command proto files.
type message struct {
	name   string // the Go name
	full   string // the protobuf full name
	fields []*messageField
}

// messageField is one field of a message.
type messageField struct {
	goName, protoName, jsonName string
	kind                        protoreflect.Kind
	list                        bool
}

func (f *messageField) isString() bool { return f.kind == protoreflect.StringKind && !f.list }

// field finds a field of the message by its JSON name or its proto name.
func (m *message) field(name string) *messageField {
	for _, f := range m.fields {
		if f.jsonName == name || f.protoName == name {
			return f
		}
	}
	return nil
}

// find gives the top-level message of the Go name.
func (ms *messages) find(name string) (*message, error) {
	m := ms.byName[name]
	if m == nil {
		return nil, fmt.Errorf("the proto files of the setting proto_files hold no message %s", name)
	}
	return m, nil
}

// goNames gives the Go names of every top-level message.
func (ms *messages) goNames() []string {
	names := make([]string, 0, len(ms.byName))
	for name := range ms.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// compileMessages compiles the proto files of the setting proto_files from the compiler's inputs and prepares the
// protoc-gen-go plugin that writes their Go code into the contract package.
func compileMessages(ctx gen.Context) (*messages, error) {
	items, err := protoSources(ctx.ComponentConfig)
	if err != nil {
		return nil, err
	}
	text := map[string]string{}
	names := make([]string, len(items))
	for i, item := range items {
		data, ok := ctx.Inputs[item.File]
		if !ok {
			return nil, fmt.Errorf("command: the compiler gave no input %s", item.File)
		}
		text[item.Import] = string(data)
		names[i] = item.Import
	}
	compiler := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{Accessor: protocompile.SourceAccessorFromMap(text)}), MaxParallelism: 1, SourceInfoMode: protocompile.SourceInfoStandard}
	linked, err := compiler.Compile(context.Background(), names...)
	if err != nil {
		return nil, fmt.Errorf("command: the proto files of the commands do not compile: %w", err)
	}
	contract := path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace)
	pkg := path.Base(ctx.OutputNamespace)
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
				return fmt.Errorf("command: the proto file %s is not proto3", file.Path())
			}
			if file.Services().Len() > 0 {
				return fmt.Errorf("command: the proto file %s declares a service; the command messages hold messages only", file.Path())
			}
			if descriptor.Options == nil {
				descriptor.Options = &descriptorpb.FileOptions{}
			}
			descriptor.Options.GoPackage = proto.String(contract + ";" + pkg)
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
		return nil, fmt.Errorf("command: the command messages cannot be prepared: %w", err)
	}
	ms := &messages{plugin: plugin, byName: map[string]*message{}}
	for _, file := range plugin.Files {
		if !file.Generate {
			continue
		}
		for _, msg := range file.Messages {
			m := &message{name: msg.GoIdent.GoName, full: string(msg.Desc.FullName())}
			if ms.byName[m.name] != nil {
				return nil, fmt.Errorf("command: two proto files declare the message %s", m.name)
			}
			for _, f := range msg.Fields {
				m.fields = append(m.fields, &messageField{goName: f.GoName, protoName: string(f.Desc.Name()),
					jsonName: f.Desc.JSONName(), kind: f.Desc.Kind(), list: f.Desc.IsList() || f.Desc.IsMap()})
			}
			ms.byName[m.name] = m
		}
	}
	return ms, nil
}

// messageFiles writes the Go code of the command messages with protoc-gen-go into the contract package.
func (m *model) messageFiles() ([]gen.File, error) {
	plugin := m.messages.plugin
	for _, file := range plugin.Files {
		if file.Generate {
			gengo.GenerateFile(plugin, file)
		}
	}
	response := plugin.Response()
	if response.GetError() != "" {
		return nil, fmt.Errorf("command: protobuf generation: %s", response.GetError())
	}
	files := make([]gen.File, 0, len(response.File))
	for _, file := range response.File {
		name := strings.ReplaceAll(strings.TrimSuffix(file.GetName(), ".pb.go"), "/", "_") + ".pb.go"
		files = append(files, gen.File{Path: path.Join(m.ctx.OutputNamespace, name), Content: []byte(file.GetContent())})
	}
	return files, nil
}
