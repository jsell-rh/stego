package openapicontract

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"unicode/utf8"

	"github.com/getkin/kin-openapi/openapi3"
)

// Bind the declared values to the backend's actual string representation.
// Nullable and non-string enums remain unavailable for response conversion.
func stringEnumProperty(schema *openapi3.Schema, expression ast.Expr, declarations map[string]ast.Expr) ([]string, string, error) {
	if schema == nil || schema.Type == nil || !schema.Type.Is("string") || schema.Nullable || len(schema.Enum) == 0 {
		return nil, "", nil
	}
	if len(schema.Enum) > 256 || len(schema.AllOf)+len(schema.AnyOf)+len(schema.OneOf) != 0 || schema.Not != nil {
		return nil, "", fmt.Errorf("unsupported or excessive response enum")
	}
	values := make([]string, 0, len(schema.Enum))
	seen := map[string]bool{}
	bytes := 0
	for _, raw := range schema.Enum {
		value, ok := raw.(string)
		bytes += len(value)
		if !ok || len(value) > 4096 || bytes > 65536 || !utf8.ValidString(value) || seen[value] {
			return nil, "", fmt.Errorf("invalid or excessive response enum value")
		}
		seen[value] = true
		values = append(values, value)
	}
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	identifier, ok := expression.(*ast.Ident)
	if !ok {
		return nil, "", fmt.Errorf("response enum has no supported Go string type")
	}
	name := identifier.Name
	if name != "string" && !token.IsExported(name) {
		return nil, "", fmt.Errorf("response enum has no public Go string type")
	}
	current := name
	for range 49 {
		if current == "string" {
			sort.Strings(values)
			return values, name, nil
		}
		next, ok := declarations[current].(*ast.Ident)
		if !ok {
			break
		}
		current = next.Name
	}
	return nil, "", fmt.Errorf("response enum Go type does not resolve to string")
}
