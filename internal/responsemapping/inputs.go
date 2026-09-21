package responsemapping

import (
	"fmt"
	"go/token"
	"sort"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// Input types have fixed Go representations. Declarations cannot name Go code.
var inputTypes = map[types.FieldType]string{
	types.FieldTypeString: "string", types.FieldTypeBool: "bool",
	types.FieldTypeInt32: "int32", types.FieldTypeInt64: "int64",
	types.FieldTypeFloat: "float32", types.FieldTypeDouble: "float64",
	types.FieldTypeTimestamp: "time.Time", types.FieldTypeJsonb: "[]byte",
}

// PreparedInputs checks bounded, named values supplied by an application.
func PreparedInputs(raw any, scope string) ([]gen.GoModelField, error) {
	entries, ok := raw.([]any)
	if !ok || len(entries) == 0 || len(entries) > 128 {
		return nil, fmt.Errorf("%s inputs require 1 through 128 fields", scope)
	}
	fields := make([]gen.GoModelField, 0, len(entries))
	seen := map[string]bool{}
	for _, raw := range entries {
		item, ok := raw.(map[string]any)
		if !ok || (len(item) != 2 && len(item) != 3) {
			return nil, fmt.Errorf("%s input requires name and type", scope)
		}
		for key := range item {
			if key != "name" && key != "type" && key != "optional" {
				return nil, fmt.Errorf("unknown %s input option", scope)
			}
		}
		name, n := item["name"].(string)
		kind, k := item["type"].(string)
		if !n || !k || len(name) > 128 || !token.IsIdentifier(name) || !token.IsExported(name) || seen[name] || inputTypes[types.FieldType(kind)] == "" {
			return nil, fmt.Errorf("invalid or duplicate %s input", scope)
		}
		pointer := false
		if raw, present := item["optional"]; present {
			var ok bool
			pointer, ok = raw.(bool)
			if !ok || (pointer && types.FieldType(kind) == types.FieldTypeJsonb) {
				return nil, fmt.Errorf("invalid %s input presence", scope)
			}
		}
		seen[name] = true
		fields = append(fields, gen.GoModelField{Name: name, Selection: name, Type: types.FieldType(kind), Pointer: pointer})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	return fields, nil
}

// InputGoType returns a fixed Go representation for a declared input type.
func InputGoType(kind types.FieldType) string { return inputTypes[kind] }
