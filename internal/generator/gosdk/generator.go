// Package gosdk generates typed JSON clients from captured OpenAPI contracts.
package gosdk

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpclient"
	"github.com/oapi-codegen/oapi-codegen/v2/pkg/codegen"
)

type Generator struct{}

func (*Generator) MinimumGoVersion() string { return "1.25.0" }
func (*Generator) ValidateContext(ctx gen.Context) error {
	if gen.ValidateGoPackageNamespace(ctx.OutputNamespace) != nil || ctx.ModuleName == "" {
		return fmt.Errorf("go-sdk requires a Go namespace and module")
	}
	if peer := ctx.PeerNamespaces["otel-tracing"]; peer != "" {
		if err := gen.ValidateGoPackageNamespace(peer); err != nil {
			return err
		}
	}
	_, err := loadDocument(ctx)
	return err
}

// The upstream backend has package-level generation state.
var backendMu sync.Mutex

//go:embed client.go.tmpl
var clientSource string

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	doc, err := loadDocument(ctx)
	if err != nil {
		return nil, nil, err
	}
	version := "stego/openapi-backend-v2.8.0"
	backendMu.Lock()
	source, err := func() (source string, err error) {
		defer func() {
			if recover() != nil {
				err = fmt.Errorf("OpenAPI backend failed")
			}
		}()
		return codegen.Generate(doc, codegen.Configuration{PackageName: "wire", Generate: codegen.GenerateOptions{Models: true, Client: true}, OutputOptions: codegen.OutputOptions{SkipPrune: true, NullableType: true}, NoVCSVersionOverride: &version})
	}()
	backendMu.Unlock()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot generate SDK: %w", err)
	}
	if len(source) > 8<<20 {
		return nil, nil, fmt.Errorf("SDK output exceeds limit")
	}
	expected := 0
	for _, item := range doc.Paths.Map() {
		expected += len(item.Operations())
	}
	root, err := publicAPI(source, expected, path.Base(ctx.OutputNamespace), path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, "internal/wire"))
	if err != nil {
		return nil, nil, err
	}
	tracing := ""
	if peer := ctx.PeerNamespaces["otel-tracing"]; peer != "" {
		tracing = path.Join(ctx.ModuleName, ctx.OutDirName, peer)
	}
	data := struct{ Package, Wire, Transport, Tracing string }{path.Base(ctx.OutputNamespace), path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, "internal/wire"), path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, "internal/transport"), tracing}
	tmpl, err := template.New("client").Parse(clientSource)
	if err != nil {
		return nil, nil, err
	}
	var output bytes.Buffer
	if err = tmpl.Execute(&output, data); err != nil {
		return nil, nil, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return nil, nil, fmt.Errorf("format SDK: %w", err)
	}
	transport, err := httpclient.Render(path.Join(ctx.OutputNamespace, "internal/transport"), tracing)
	if err != nil {
		return nil, nil, err
	}
	apiCode, err := format.Source([]byte(root))
	if err != nil {
		return nil, nil, fmt.Errorf("format SDK API: %w", err)
	}
	files := []gen.File{{Path: path.Join(ctx.OutputNamespace, "api.go"), Content: apiCode}, {Path: path.Join(ctx.OutputNamespace, "client.go"), Content: code}, {Path: path.Join(ctx.OutputNamespace, "internal/wire/api.go"), Content: []byte(source)}, transport}
	if err = gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{GoModRequires: map[string]string{"github.com/oapi-codegen/runtime": "v1.7.0", "github.com/oapi-codegen/nullable": "v1.1.0", "github.com/google/uuid": "v1.6.0"}}, nil
}

func publicAPI(source string, expected int, pkg, wireImport string) (string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "wire.go", source, 0)
	if err != nil {
		return "", err
	}
	var structureError error
	ast.Inspect(file, func(node ast.Node) bool {
		if fields, ok := node.(*ast.StructType); ok {
			names := map[string]bool{}
			for _, field := range fields.Fields.List {
				for _, name := range field.Names {
					if names[name.Name] {
						structureError = fmt.Errorf("duplicate SDK structure field %q", name.Name)
					}
					names[name.Name] = true
				}
			}
		}
		return true
	})
	if structureError != nil {
		return "", structureError
	}
	reserved := map[string]bool{"Client": true, "ClientOption": true, "ClientInterface": true, "ClientWithResponses": true, "ClientWithResponsesInterface": true, "HttpRequestDoer": true, "RequestEditorFn": true}
	own := map[string]bool{"Client": true, "Options": true, "NewClient": true}
	seen := map[string]bool{}
	imports := map[string]string{"context": "context", "errors": "errors", "wire": wireImport}
	backendImports := map[string]string{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return "", err
		}
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		backendImports[name] = importPath
	}
	var out bytes.Buffer
	writeType := func(node ast.Node) string {
		ast.Inspect(node, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					if p, ok := backendImports[id.Name]; ok {
						imports[id.Name] = p
					}
				}
			}
			return true
		})
		var b bytes.Buffer
		format.Node(&b, token.NewFileSet(), node)
		return b.String()
	}
	methods := 0
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					name := s.Name.Name
					if seen[name] {
						return "", fmt.Errorf("duplicate generated SDK identifier %q", name)
					}
					seen[name] = true
					if !ast.IsExported(name) || reserved[name] {
						continue
					}
					if own[name] {
						return "", fmt.Errorf("SDK schema conflicts with public identifier %q", name)
					}
					fmt.Fprintf(&out, "type %s = wire.%s\n", name, name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if seen[n.Name] {
							return "", fmt.Errorf("duplicate SDK identifier")
						}
						seen[n.Name] = true
						if d.Tok == token.CONST && ast.IsExported(n.Name) {
							if own[n.Name] {
								return "", fmt.Errorf("SDK constant conflicts with public identifier")
							}
							fmt.Fprintf(&out, "const %s = wire.%s\n", n.Name, n.Name)
						}
					}
				}
			}
		case *ast.FuncDecl:
			if d.Recv == nil {
				if seen[d.Name.Name] {
					return "", fmt.Errorf("duplicate SDK function identifier")
				}
				seen[d.Name.Name] = true
				continue
			}
			recv, ok := d.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			id, ok := recv.X.(*ast.Ident)
			if !ok || id.Name != "ClientWithResponses" {
				continue
			}
			rawBody := false
			for _, field := range d.Type.Params.List {
				if sel, ok := field.Type.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "io" && sel.Sel.Name == "Reader" {
						rawBody = true
					}
				}
			}
			if rawBody {
				if !strings.HasSuffix(d.Name.Name, "WithBodyWithResponse") {
					return "", fmt.Errorf("unsupported raw SDK method")
				}
				continue
			}
			if !strings.HasSuffix(d.Name.Name, "WithResponse") {
				return "", fmt.Errorf("unsupported generated SDK method")
			}
			var params, args []string
			for _, field := range d.Type.Params.List {
				if rest, ok := field.Type.(*ast.Ellipsis); ok {
					if ident, ok := rest.Elt.(*ast.Ident); !ok || ident.Name != "RequestEditorFn" {
						return "", fmt.Errorf("unsupported SDK variadic parameter")
					}
					continue
				}
				for range field.Names {
					name := fmt.Sprintf("arg%d", len(args))
					if len(args) == 0 {
						name = "ctx"
					}
					params = append(params, name+" "+writeType(field.Type))
					args = append(args, name)
				}
			}
			if len(args) == 0 || args[0] != "ctx" || len(d.Type.Results.List) != 2 {
				return "", fmt.Errorf("unexpected SDK method contract")
			}
			result := writeType(d.Type.Results.List[0].Type)
			methods++
			fmt.Fprintf(&out, `func(c *Client)%s(%s)(%s,error){
 if err:=c.acquire(ctx);err!=nil{return nil,err};defer c.release()
 ctx,cancel:=context.WithTimeout(ctx,c.timeout);defer cancel()
 c.bind(&ctx)
 response,err:=c.wire.%s(%s)
 if ctx.Err()!=nil{return nil,ctx.Err()}
 if err!=nil{return nil,errors.New("SDK request failed")}
 return response,nil
 }
`, d.Name.Name, strings.Join(params, ","), result, d.Name.Name, strings.Join(args, ","))
		}
	}
	if methods == 0 || methods != expected {
		return "", fmt.Errorf("OpenAPI contract did not produce one typed SDK method per operation")
	}
	var header bytes.Buffer
	fmt.Fprintf(&header, "// Code generated by stego. DO NOT EDIT.\npackage %s\nimport(\n", pkg)
	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&header, "%s %q\n", name, imports[name])
	}
	header.WriteString(")\n")
	header.Write(out.Bytes())
	return header.String(), nil
}
