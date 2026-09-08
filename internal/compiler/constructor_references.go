package compiler

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
)

// renameConstructorReferences keeps package qualifiers separate from value
// references. Only arguments can refer to earlier constructor values.
func renameConstructorReferences(source string, values, packages map[string]string, importNames map[string]bool, dependencies []string) (string, error) {
	expression, err := parser.ParseExpr(source)
	if err != nil {
		return "", fmt.Errorf("invalid constructor expression: %w", err)
	}
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return "", fmt.Errorf("constructor must be a call expression")
	}
	declared := make(map[string]bool)
	for _, name := range dependencies {
		declared[name] = true
	}
	var ambiguity error
	// Local bindings require scope information that string wiring does not carry.
	// Reject affected closures instead of changing their capture semantics.
	ast.Inspect(expression, func(node ast.Node) bool {
		literal, ok := node.(*ast.FuncLit)
		if !ok {
			return true
		}
		ast.Inspect(literal, func(node ast.Node) bool {
			if name, ok := node.(*ast.Ident); ok && (values[name.Name] != "" || packages[name.Name] != "") {
				ambiguity = fmt.Errorf("constructor closure uses renamed identifier %q; use a named helper", name.Name)
			}
			return true
		})
		return false
	})
	for _, argument := range call.Args {
		excluded := make(map[*ast.Ident]bool)
		excludeType := func(node ast.Node) {
			if node == nil {
				return
			}
			ast.Inspect(node, func(node ast.Node) bool {
				if name, ok := node.(*ast.Ident); ok {
					excluded[name] = true
				}
				return true
			})
		}
		ast.Inspect(argument, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CompositeLit:
				excludeType(node.Type)
				_, explicitMap := node.Type.(*ast.MapType)
				_, explicitStruct := node.Type.(*ast.StructType)
				if !explicitMap {
					for _, element := range node.Elts {
						if pair, ok := element.(*ast.KeyValueExpr); ok {
							if key, ok := pair.Key.(*ast.Ident); ok {
								if explicitStruct {
									excluded[key] = true
								} else if values[key.Name] != "" {
									ambiguity = fmt.Errorf("constructor literal key %q requires type information; use an explicit type or named helper", key.Name)
								}
							}
						}
					}
				}
			case *ast.ArrayType, *ast.MapType, *ast.StructType, *ast.InterfaceType, *ast.FuncType, *ast.ChanType:
				excludeType(node)
			}
			return true
		})
		ast.Inspect(argument, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			excluded[selector.Sel] = true
			if name, ok := selector.X.(*ast.Ident); ok {
				if importNames[name.Name] || packages[name.Name] != "" {
					excluded[name] = true
					if values[name.Name] != "" && declared[name.Name] {
						ambiguity = fmt.Errorf("constructor reference %q can name a package or a dependency", name.Name)
					}
				}
			}
			return true
		})
		ast.Inspect(argument, func(node ast.Node) bool {
			if name, ok := node.(*ast.Ident); ok && !excluded[name] {
				if replacement := values[name.Name]; replacement != "" {
					name.Name = replacement
				}
			}
			return true
		})
	}
	if ambiguity != nil {
		return "", ambiguity
	}
	ast.Inspect(expression, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if name, ok := selector.X.(*ast.Ident); ok {
				if replacement := packages[name.Name]; replacement != "" {
					name.Name = replacement
				}
			}
		}
		return true
	})
	var buf bytes.Buffer
	if err := format.Node(&buf, token.NewFileSet(), expression); err != nil {
		return "", err
	}
	return buf.String(), nil
}
