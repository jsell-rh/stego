package kubernetesservice

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

// InputFiles gives the compiler ownership of reading and hashing each callback
// declaration. Domain packages keep their source outside generated output.
func (*Generator) InputFiles(config map[string]any) ([]string, error) {
	entries, err := configList(gen.Context{ComponentConfig: config}, "workers")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		value, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("worker must be an object")
		}
		pkg, ok := value["package"].(string)
		if !ok || len(pkg) > 256 {
			return nil, fmt.Errorf("invalid worker package")
		}
		segments := strings.Split(pkg, "/")
		if len(segments) > 8 {
			return nil, fmt.Errorf("invalid worker package")
		}
		for _, segment := range segments {
			if !directory.MatchString(segment) {
				return nil, fmt.Errorf("invalid worker package")
			}
		}
		seen[path.Join(pkg, "worker.go")] = true
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func validateWorkerFunction(ctx gen.Context, w worker) error {
	name := path.Join(w.Package, "worker.go")
	source, ok := ctx.Inputs[name]
	if !ok {
		return fmt.Errorf("worker source %s was not supplied by the compiler", name)
	}
	file, err := parser.ParseFile(token.NewFileSet(), name, source, parser.ParseComments)
	if err != nil || file.Name.Name == "main" {
		return fmt.Errorf("worker source must be a valid importable Go package")
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.HasPrefix(comment.Text, "//go:build") || strings.HasPrefix(comment.Text, "// +build") {
				return fmt.Errorf("worker declaration cannot have a build constraint")
			}
		}
	}
	imports := map[string]string{}
	for _, spec := range file.Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return fmt.Errorf("invalid worker import")
		}
		alias := path.Base(value)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		imports[alias] = value
	}
	matches := func(expr ast.Expr, pkg, name string) bool {
		selector, ok := expr.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != name {
			return false
		}
		qualifier, ok := selector.X.(*ast.Ident)
		return ok && imports[qualifier.Name] == pkg
	}
	count := 0
	for _, decl := range file.Decls {
		f, ok := decl.(*ast.FuncDecl)
		if !ok || f.Recv != nil || f.Name.Name != w.Function {
			continue
		}
		count++
		valid := f.Body != nil && f.Type.TypeParams.NumFields() == 0 && f.Type.Params.NumFields() == 2 && len(f.Type.Params.List) == 2 && f.Type.Results.NumFields() == 1 && len(f.Type.Results.List) == 1
		if valid {
			metrics, ok := f.Type.Params.List[1].Type.(*ast.StarExpr)
			result, resultOK := f.Type.Results.List[0].Type.(*ast.Ident)
			valid = matches(f.Type.Params.List[0].Type, "context", "Context") && ok && matches(metrics.X, path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["controller"]), "Metrics") && resultOK && result.Name == "error"
		}
		if !valid {
			return fmt.Errorf("worker function must accept context.Context and *controller.Metrics and return error")
		}
	}
	if count != 1 {
		return fmt.Errorf("worker source must declare its function exactly once")
	}
	return nil
}
