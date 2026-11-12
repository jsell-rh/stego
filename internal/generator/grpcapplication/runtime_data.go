package grpcapplication

import (
	"bytes"
	"fmt"
	"go/format"
	"path"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/capability"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication/grpcgen"
	gengo "google.golang.org/protobuf/cmd/protoc-gen-go/internal_gengo"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// The runtime data path of the grpc-application component, its only path (ST-SUP-RC07-OC19, ST-SUP-RC20-OC39): the runtime
// that the component emits registers only the methods that a declared command or query names, each served by a
// generated handler that calls the fill of its command or query through the command and query runtimes of the
// runtime library, with the same verification, transaction, permission, audit and idempotency as its HTTP handler.
// It imports no package of the service's author and runs no hand-written registrar (ST-SUP-RC07-OC18).

// The components whose generated packages the runtime of the gRPC application calls.
const (
	commandComponent = "command"
	queryComponent   = "query"
)

// RuntimeService is the variable of main.go that holds the runtime wiring of the service on the runtime data path,
// which the compiler builds; the gRPC application takes it.
const RuntimeService = "service"

// rpcKind is how a declared method is served.
type rpcKind uint8

const (
	rpcCommand rpcKind = iota + 1
	rpcGet
	rpcList
	rpcStream
)

// rpcMethod is one method of a service of the proto files that a declared command or query names.
type rpcMethod struct {
	method *protogen.Method
	kind   rpcKind
	name   string // the declared command or query
	goName string // its Go name in the command handlers or the query handlers
	// member is the request field of the resource member of a get or stream query; cursor and limit are the
	// request fields of a list query, limit nil when the message has none.
	member, cursor, limit *protogen.Field
}

// rpcService is one service of the proto files with the methods that the declaration names.
type rpcService struct {
	service *protogen.Service
	// ident is the Go name of the service and the import path of its generated package.
	ident   protogen.GoIdent
	methods []*rpcMethod
}

// rpcPlan is what the runtime of the gRPC application is generated from.
type rpcPlan struct {
	plugin   *protogen.Plugin
	services []*rpcService
	// commands and queries are the import paths of the command handlers, the command contract and the query
	// handlers, empty when the declaration has none.
	commandHandlers, commandContract, queryServe string
	subjectType                                  string
}

// validateRuntimeData checks the settings of the component on the runtime data path: proto_files alone. The former
// settings factory_package, watch_events and processes ran hand-written code outside a fill, which the runtime data
// path does not have (ST-SUP-RC07-OC18, ST-DECL-INV72).
func validateRuntimeData(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	if ctx.ModuleName == "" {
		return fmt.Errorf("grpc-application requires the module name")
	}
	keys := make([]string, 0, len(ctx.ComponentConfig))
	for key := range ctx.ComponentConfig {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch key {
		case "proto_files":
		case "factory_package":
			return fmt.Errorf("grpc-application on the runtime data path takes no setting factory_package (%v): hand-written code runs only as a fill, so a write is a command (ST-DECL-EN18), a read a query (ST-DECL-EN19) and a background task or an event source a worker (ST-DECL-EN25) (ST-DECL-INV72, ST-SUP-RC07-OC18)", ctx.ComponentConfig[key])
		default:
			return fmt.Errorf("grpc-application on the runtime data path takes no setting %s: it serves the gRPC methods of the declared commands and queries only (ST-SUP-RC07-OC19)", key)
		}
	}
	if ctx.Authorization == nil || ctx.Authorization.SubjectFromIdentity == "" {
		return fmt.Errorf("grpc-application on the runtime data path needs authorization with subject_from_identity, which maps a verified identity to its subject")
	}
	_, err := planRuntimeData(ctx)
	return err
}

// ownedFiles gives the import paths of the proto files of the command component, by their project path: their Go
// code is the command contract's (ST-SUP-RC32-OC11), so a command's request and response message is one Go type over
// HTTP and gRPC.
func ownedFiles(ctx gen.Context) map[string]string {
	owned := map[string]string{}
	config := ctx.PeerConfigs[commandComponent]
	values, _ := config["proto_files"].([]any)
	for _, value := range values {
		item, _ := value.(map[string]any)
		file, _ := item["path"].(string)
		name, _ := item["import_path"].(string)
		if file != "" && name != "" {
			owned[name] = file
		}
	}
	return owned
}

// planRuntimeData compiles the proto files and binds each method that a declared command or query names to its
// handler. A proto file of the command component keeps the Go package of the command contract and gets no Go code
// here. It refuses a method whose messages do not suit its command or query, naming the method.
func planRuntimeData(ctx gen.Context) (*rpcPlan, error) {
	items, err := sources(ctx.ComponentConfig)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.Reference != "" {
			return nil, fmt.Errorf("grpc-application on the runtime data path takes no reference overlay for %s", item.File)
		}
	}
	text, _, linked, err := compileSources(items, ctx.Inputs)
	if err != nil {
		return nil, err
	}
	p := &rpcPlan{}
	if ctx.Authorization != nil {
		p.subjectType = ctx.Authorization.SubjectFromIdentity
	}
	generated := func(ns string) string { return path.Join(ctx.ModuleName, ctx.OutDirName, ns) }
	owned := ownedFiles(ctx)
	if ns := ctx.PeerNamespaces[commandComponent]; ns != "" && len(ctx.Commands) > 0 {
		p.commandContract, p.commandHandlers = generated(ns), generated(path.Join(ns, "handlers"))
	}
	if ns := ctx.PeerNamespaces[queryComponent]; ns != "" && len(ctx.Queries) > 0 {
		p.queryServe = generated(path.Join(ns, "serve"))
	}
	var toGenerate []string
	for _, item := range items {
		if file, ok := owned[item.Import]; ok {
			if file != item.File {
				return nil, fmt.Errorf("grpc-application names the proto file %s at %s, and the command component names it at %s; name one file", item.Import, item.File, file)
			}
			if p.commandContract == "" {
				return nil, fmt.Errorf("grpc-application names the proto file %s of the command component, which declares no command", item.Import)
			}
			continue
		}
		toGenerate = append(toGenerate, item.Import)
	}
	sort.Strings(toGenerate)
	request := &pluginpb.CodeGeneratorRequest{FileToGenerate: toGenerate, Parameter: proto.String("paths=source_relative")}
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
			if _, ok := owned[file.Path()]; ok {
				// The command generator gives these files the Go package of the command contract.
				descriptor.Options.GoPackage = proto.String(p.commandContract + ";" + path.Base(p.commandContract))
			} else {
				descriptor.Options.GoPackage = proto.String(generated(path.Join(ctx.OutputNamespace, "pb", path.Dir(file.Path()))))
			}
		}
		request.ProtoFile = append(request.ProtoFile, descriptor)
		return nil
	}
	for _, file := range linked {
		if err := appendFile(file); err != nil {
			return nil, err
		}
	}
	if p.plugin, err = (protogen.Options{}).New(request); err != nil {
		return nil, err
	}
	methods := map[string]*protogen.Method{}
	services := map[string]*protogen.Service{}
	idents := map[*protogen.Service]protogen.GoIdent{}
	for _, file := range p.plugin.Files {
		if _, local := text[file.Desc.Path()]; !local {
			continue
		}
		if file.Generate {
			if err := gen.ValidateGoImportNamespace(string(file.GoImportPath)); err != nil {
				return nil, fmt.Errorf("protobuf %s: %w", file.Desc.Path(), err)
			}
			if err := gen.ValidateGoLibraryName(string(file.GoPackageName)); err != nil {
				return nil, fmt.Errorf("protobuf %s: %w", file.Desc.Path(), err)
			}
		} else if len(file.Services) > 0 {
			return nil, fmt.Errorf("the proto file %s of the command component declares a service; declare the services in another proto file of grpc-application", file.Desc.Path())
		}
		for _, s := range file.Services {
			idents[s] = file.GoImportPath.Ident(s.GoName)
			for _, m := range s.Methods {
				full := string(s.Desc.FullName()) + "/" + string(m.Desc.Name())
				methods[full], services[full] = m, s
			}
		}
	}
	byService := map[*protogen.Service]*rpcService{}
	bind := func(full string, r *rpcMethod) error {
		m := methods[full]
		if m == nil {
			return fmt.Errorf("no service of the proto files of grpc-application declares the method %s of %s (ST-DECL-INV73)", full, r.name)
		}
		r.method = m
		s := byService[services[full]]
		if s == nil {
			s = &rpcService{service: services[full], ident: idents[services[full]]}
			byService[services[full]] = s
			p.services = append(p.services, s)
		}
		s.methods = append(s.methods, r)
		return nil
	}
	for _, c := range ctx.Commands {
		if c.Method == "" {
			continue
		}
		r := &rpcMethod{kind: rpcCommand, name: c.Name, goName: capability.MixedCase(c.Name)}
		if err := bind(c.Method, r); err != nil {
			return nil, err
		}
		if err := checkCommandMethod(p, c.Method, r); err != nil {
			return nil, err
		}
	}
	for _, q := range ctx.Queries {
		if q.Method == "" {
			continue
		}
		r := &rpcMethod{name: q.Name, goName: capability.MixedCase(q.Name)}
		switch {
		case ctx.QueryStreams[q.Name]:
			r.kind = rpcStream
		case q.ResourceMember != "":
			r.kind = rpcGet
		default:
			r.kind = rpcList
		}
		if p.queryServe == "" {
			return nil, fmt.Errorf("the query %s has the gRPC method %s, and the service has no query component", q.Name, q.Method)
		}
		if err := bind(q.Method, r); err != nil {
			return nil, err
		}
		if err := checkQueryMethod(q.Method, q.ResourceMember, r); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// checkCommandMethod refuses a unary method of a command whose request and response are not the command's request
// and response messages of the command contract (ST-SUP-RC32-OC11).
func checkCommandMethod(p *rpcPlan, full string, r *rpcMethod) error {
	m := r.method
	if m.Desc.IsStreamingClient() || m.Desc.IsStreamingServer() {
		return fmt.Errorf("the command %s has the gRPC method %s, which is a stream; a command is served by a unary method (ST-DECL-INV73)", r.name, full)
	}
	if p.commandContract == "" {
		return fmt.Errorf("the command %s has the gRPC method %s, and the service has no command component", r.name, full)
	}
	for _, pair := range []struct {
		message *protogen.Message
		want    string
	}{{m.Input, r.goName + "Request"}, {m.Output, r.goName + "Response"}} {
		if string(pair.message.GoIdent.GoImportPath) != p.commandContract || pair.message.GoIdent.GoName != pair.want {
			return fmt.Errorf("the gRPC method %s of the command %s takes %s, which is not the message %s of the proto files of the command component; HTTP and gRPC take one request and one response message (ST-SUP-RC32-OC11)", full, r.name, pair.message.Desc.FullName(), pair.want)
		}
	}
	return nil
}

// checkQueryMethod binds the request fields of a query's method: the resource member of a get or stream query, a
// plain string field whose JSON name or proto name is the member; the cursor of a list query, a plain string field
// cursor, with an optional whole number field limit; and refuses a list response with no repeated field items and
// no string field next, which take the page of the HTTP body.
func checkQueryMethod(full, member string, r *rpcMethod) error {
	m := r.method
	switch {
	case m.Desc.IsStreamingClient():
		return fmt.Errorf("the query %s has the gRPC method %s, which takes a client stream; a query is served by a unary method or a server stream (ST-DECL-INV73)", r.name, full)
	case m.Desc.IsStreamingServer() != (r.kind == rpcStream):
		return fmt.Errorf("the query %s has the gRPC method %s, which the compiler did not mark as the stream it is (ST-SUP-RC33-OC08)", r.name, full)
	}
	field := func(msg *protogen.Message, name string, kinds ...protoreflect.Kind) *protogen.Field {
		for _, f := range msg.Fields {
			if (f.Desc.JSONName() == name || string(f.Desc.Name()) == name) && f.Desc.Cardinality() != protoreflect.Repeated &&
				f.Oneof == nil && !f.Desc.HasPresence() {
				for _, k := range kinds {
					if f.Desc.Kind() == k {
						return f
					}
				}
			}
		}
		return nil
	}
	if member != "" && r.kind != rpcList {
		if r.member = field(m.Input, member, protoreflect.StringKind); r.member == nil {
			return fmt.Errorf("the gRPC method %s of the query %s takes %s, which has no plain string field for the resource member %s", full, r.name, m.Input.Desc.FullName(), member)
		}
	}
	if r.kind != rpcList {
		return nil
	}
	if r.cursor = field(m.Input, "cursor", protoreflect.StringKind); r.cursor == nil {
		return fmt.Errorf("the gRPC method %s of the list query %s takes %s, which has no plain string field cursor for the cursor of the page", full, r.name, m.Input.Desc.FullName())
	}
	r.limit = field(m.Input, "limit", protoreflect.Int32Kind, protoreflect.Int64Kind, protoreflect.Uint32Kind, protoreflect.Sint32Kind, protoreflect.Sint64Kind)
	var items bool
	for _, f := range m.Output.Fields {
		items = items || (string(f.Desc.Name()) == "items" && f.Desc.IsList())
	}
	if !items || field(m.Output, "next", protoreflect.StringKind) == nil {
		return fmt.Errorf("the gRPC method %s of the list query %s gives %s, which needs the repeated field items and the string field next for the page", full, r.name, m.Output.Desc.FullName())
	}
	return nil
}

// generateRuntimeData emits the Go code of the proto files that the command component does not own, with their
// service stubs, the transport runtime and the server that registers the declared methods, and the wiring of
// main.go: one constructor that takes the runtime wiring of the service and runs as a background task.
func generateRuntimeData(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := validateRuntimeData(ctx); err != nil {
		return nil, nil, err
	}
	p, err := planRuntimeData(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, file := range p.plugin.Files {
		if file.Generate {
			gengo.GenerateFile(p.plugin, file)
			grpcgen.GenerateFile(p.plugin, file)
		}
	}
	response := p.plugin.Response()
	if response.GetError() != "" {
		return nil, nil, fmt.Errorf("protobuf generation: %s", response.GetError())
	}
	var files []gen.File
	for _, file := range response.File {
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "pb", file.GetName()), Content: []byte(file.GetContent())})
	}
	runtimeFiles, err := renderFiles(ctx, struct{}{}, []templateSource{{"transport/runtime.go", runtimeSource}})
	if err != nil {
		return nil, nil, err
	}
	files = append(files, runtimeFiles...)
	server, err := renderServer(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "server.go"), Content: server})
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	pkg := path.Base(ctx.OutputNamespace)
	wiring := &gen.Wiring{
		Imports:                 []string{ctx.OutputNamespace},
		Constructors:            []string{pkg + ".NewRuntime(" + RuntimeService + ")"},
		ConstructorDeps:         map[int][]string{0: {RuntimeService}},
		ConstructorReturnsError: map[int]bool{0: true},
		ConstructorDeferCalls:   map[int]string{0: "Close()"},
		BackgroundTasks:         []int{0},
		GoModRequires:           map[string]string{"google.golang.org/grpc": "v1.83.2", "google.golang.org/protobuf": "v1.36.12"},
	}
	return files, wiring, nil
}

// renderServer writes the server of the gRPC application: for each service with a declared method a type that
// embeds the service's Unimplemented server, so a method that no declaration names answers Unimplemented, and
// serves each declared method through the generated handler of its command or query; and NewRuntime, which makes the
// handlers on the runtimes of the running service and the transport runtime that verifies each call.
func renderServer(ctx gen.Context, p *rpcPlan) ([]byte, error) {
	var w bytes.Buffer
	f := func(format string, args ...any) { fmt.Fprintf(&w, format, args...) }
	aliases := map[string]string{}
	var paths []string
	alias := func(ident protogen.GoIdent) string {
		imp := string(ident.GoImportPath)
		if imp == p.commandContract {
			return "commands." + ident.GoName
		}
		if aliases[imp] == "" {
			aliases[imp] = fmt.Sprintf("pb%d", len(aliases))
			paths = append(paths, imp)
		}
		return aliases[imp] + "." + ident.GoName
	}
	// Resolve the aliases before the imports are written.
	for _, s := range p.services {
		alias(s.ident)
		for _, m := range s.methods {
			alias(m.method.Input.GoIdent)
			alias(m.method.Output.GoIdent)
		}
	}
	hasCommand, hasQuery := false, false
	for _, s := range p.services {
		for _, m := range s.methods {
			hasCommand = hasCommand || m.kind == rpcCommand
			hasQuery = hasQuery || m.kind != rpcCommand
		}
	}
	pkg := path.Base(ctx.OutputNamespace)
	f("// Package %s is the gRPC application of the service on the runtime data path (ST-SUP-RC07-OC19): it registers\n", pkg)
	f("// only the methods that a declared command or query names, each served by the generated handler of its command or\n")
	f("// query with the same verification, transaction, permission, audit and idempotency as its HTTP handler. A method\n")
	f("// that no declaration names answers Unimplemented. It imports no package of the service's author.\n")
	f("package %s\n\nimport (\n\t\"context\"\n\t\"errors\"\n\t\"time\"\n\n\t\"google.golang.org/grpc\"\n\n", pkg)
	f("\t\"github.com/jsell-rh/stego/registry/runtime/authn\"\n\t\"github.com/jsell-rh/stego/registry/runtime/service\"\n\n")
	f("\ttransport %q\n", path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, "transport"))
	if hasCommand {
		f("\tcommands %q\n\tcommandhandlers %q\n", p.commandContract, p.commandHandlers)
	}
	if hasQuery {
		f("\tqueryserve %q\n", p.queryServe)
	}
	sort.Strings(paths)
	for _, imp := range paths {
		f("\t%s %q\n", aliases[imp], imp)
	}
	f(")\n\n")
	f("// subjectType is the subject type of the schema that a verified identity maps to.\nconst subjectType = %q\n\n", p.subjectType)
	f("// Service is the runtime wiring of the service, which main.go gives NewRuntime: the runtimes of the running service,\n")
	f("// the retention of the idempotency keys and whether the process runs the api mode.\n")
	f("type Service interface {\n\tService() *service.Service\n\tRetention() time.Duration\n\tServesAPI() bool\n}\n\n")
	f("// Runtime is the gRPC application: the transport runtime, or none in a process that does not run the api mode.\n")
	f("type Runtime struct{ transport *transport.Runtime }\n\n")
	f("// NewRuntime makes the handler of each declared method on the runtimes of svc and the transport runtime that\n")
	f("// verifies the bearer token of each call with the verifier of the service. A process that does not run the api\n")
	f("// mode serves no method.\n")
	f("func NewRuntime(svc Service) (*Runtime, error) {\n")
	f("\tif svc == nil || svc.Service() == nil {\n\t\treturn nil, errors.New(\"grpc: the gRPC application needs the runtime of the service\")\n\t}\n")
	f("\tif !svc.ServesAPI() {\n\t\treturn &Runtime{}, nil\n\t}\n")
	f("\trt := svc.Service()\n")
	if hasCommand {
		f("\tfills := commandhandlers.BoundFills()\n\toptions := commandhandlers.Options{Retention: svc.Retention()}\n")
	}
	if hasQuery {
		f("\tqueries, err := queryserve.New(queryserve.Config{Runtime: rt.Queries(), Verifier: rt.Verifier(), Probes: rt.Probes(), SubjectType: subjectType})\n\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	} else {
		f("\tvar err error\n")
	}
	for i, s := range p.services {
		f("\tsrv%d := &%s{}\n", i, serverType(s))
		for _, m := range s.methods {
			field := capability.LowerFirst(m.method.GoName)
			in, out := alias(m.method.Input.GoIdent), alias(m.method.Output.GoIdent)
			switch m.kind {
			case rpcCommand:
				f("\tif srv%d.%s, err = commandhandlers.%sRPC(rt.Commands(), fills.%s, options); err != nil {\n\t\treturn nil, err\n\t}\n", i, field, m.goName, m.goName)
			case rpcGet:
				f("\tif srv%d.%s, err = queryserve.Serve%sRPC(queries, func(in *%s) string { return in.Get%s() }, func() *%s { return &%s{} }); err != nil {\n\t\treturn nil, err\n\t}\n",
					i, field, m.goName, in, m.member.GoName, out, out)
			case rpcList:
				limit := "nil"
				if m.limit != nil {
					limit = fmt.Sprintf("func(in *%s) int64 { return int64(in.Get%s()) }", in, m.limit.GoName)
				}
				f("\tif srv%d.%s, err = queryserve.Serve%sRPC(queries, func(in *%s) string { return in.Get%s() }, %s, func() *%s { return &%s{} }); err != nil {\n\t\treturn nil, err\n\t}\n",
					i, field, m.goName, in, m.cursor.GoName, limit, out, out)
			case rpcStream:
				member := ""
				if m.member != nil {
					member = fmt.Sprintf("func(in *%s) string { return in.Get%s() }, ", in, m.member.GoName)
				}
				f("\tif srv%d.%s, err = queryserve.Serve%sStream[*%s, *%s](queries, %sfunc() *%s { return &%s{} }); err != nil {\n\t\treturn nil, err\n\t}\n",
					i, field, m.goName, in, out, member, out, out)
			}
		}
	}
	f("\tverifier := rt.Verifier()\n")
	f("\tauthenticate := func(ctx context.Context, token string) (context.Context, error) { return verifier.Authenticate(ctx, \"Bearer \"+token) }\n")
	f("\tserver, err := transport.New(authenticate, func(registrar grpc.ServiceRegistrar) error {\n")
	for i, s := range p.services {
		f("\t\t%s.Register%sServer(registrar, srv%d)\n", strings.SplitN(alias(s.ident), ".", 2)[0], s.service.GoName, i)
	}
	f("\t\treturn nil\n\t}, transport.Options{IdentityInfo: identityInfo})\n\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	f("\treturn &Runtime{transport: server}, nil\n}\n\n")
	f("// identityInfo gives the subject and the expiry of the verified identity of a stream, which the transport ends the\n// stream at.\n")
	f("func identityInfo(ctx context.Context) (string, time.Time) {\n\tid, ok := authn.FromContext(ctx)\n\tif !ok {\n\t\treturn \"\", time.Time{}\n\t}\n\treturn id.UserID(), id.ExpiresAt()\n}\n\n")
	f("// Run serves the declared methods until ctx ends; a process that does not run the api mode waits for the end.\n")
	f("func (r *Runtime) Run(ctx context.Context) error {\n\tif r.transport == nil {\n\t\t<-ctx.Done()\n\t\treturn nil\n\t}\n\treturn r.transport.Run(ctx)\n}\n\n")
	f("// Close stops the transport runtime.\nfunc (r *Runtime) Close() {\n\tif r.transport != nil {\n\t\tr.transport.Close()\n\t}\n}\n\n")
	for _, s := range p.services {
		pb := strings.SplitN(alias(s.ident), ".", 2)[0]
		f("// %s serves the declared methods of %s; every other method answers Unimplemented.\n", serverType(s), s.service.Desc.FullName())
		f("type %s struct {\n\t%s.Unimplemented%sServer\n", serverType(s), pb, s.service.GoName)
		for _, m := range s.methods {
			in, out := alias(m.method.Input.GoIdent), alias(m.method.Output.GoIdent)
			if m.kind == rpcStream {
				f("\t%s func(context.Context, *%s, func(*%s) error) error\n", capability.LowerFirst(m.method.GoName), in, out)
			} else {
				f("\t%s func(context.Context, *%s) (*%s, error)\n", capability.LowerFirst(m.method.GoName), in, out)
			}
		}
		f("}\n\n")
		for _, m := range s.methods {
			in, out := alias(m.method.Input.GoIdent), alias(m.method.Output.GoIdent)
			field := capability.LowerFirst(m.method.GoName)
			if m.kind == rpcStream {
				f("// %s serves the stream query %s.\nfunc (s *%s) %s(in *%s, stream grpc.ServerStreamingServer[%s]) error {\n\treturn s.%s(stream.Context(), in, stream.Send)\n}\n\n",
					m.method.GoName, m.name, serverType(s), m.method.GoName, in, out, field)
				continue
			}
			what := "command"
			if m.kind != rpcCommand {
				what = "query"
			}
			f("// %s serves the %s %s.\nfunc (s *%s) %s(ctx context.Context, in *%s) (*%s, error) {\n\treturn s.%s(ctx, in)\n}\n\n",
				m.method.GoName, what, m.name, serverType(s), m.method.GoName, in, out, field)
		}
	}
	source, err := format.Source(w.Bytes())
	if err != nil {
		return nil, fmt.Errorf("grpc-application: formatting the server: %w\n%s", err, w.Bytes())
	}
	return source, nil
}

// serverType is the Go type of the server of a service.
func serverType(s *rpcService) string { return capability.LowerFirst(s.service.GoName) + "Server" }
