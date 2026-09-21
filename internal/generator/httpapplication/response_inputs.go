package httpapplication

import (
	"fmt"
	"go/token"
	"sort"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/openapicontract"
	"github.com/jsell-rh/stego/internal/types"
)

// Input types have fixed Go representations. Declarations cannot name Go code.
var responseInputTypes = map[types.FieldType]string{
	types.FieldTypeString: "string", types.FieldTypeBool: "bool",
	types.FieldTypeInt32: "int32", types.FieldTypeInt64: "int64",
	types.FieldTypeFloat: "float32", types.FieldTypeDouble: "float64",
	types.FieldTypeTimestamp: "time.Time", types.FieldTypeJsonb: "[]byte",
}

func responseInputs(raw any) ([]gen.GoModelField, error) {
	entries, ok := raw.([]any)
	if !ok || len(entries) == 0 || len(entries) > 128 {
		return nil, fmt.Errorf("HTTP response inputs require 1 through 128 fields")
	}
	fields := make([]gen.GoModelField, 0, len(entries))
	seen := map[string]bool{}
	for _, raw := range entries {
		item, ok := raw.(map[string]any)
		if !ok || (len(item) != 2 && len(item) != 3) {
			return nil, fmt.Errorf("HTTP response input requires name and type")
		}
		for key := range item {
			if key != "name" && key != "type" && key != "optional" {
				return nil, fmt.Errorf("unknown HTTP response input option")
			}
		}
		name, n := item["name"].(string)
		kind, k := item["type"].(string)
		if !n || !k || len(name) > 128 || !token.IsIdentifier(name) || !token.IsExported(name) || seen[name] || responseInputTypes[types.FieldType(kind)] == "" {
			return nil, fmt.Errorf("invalid or duplicate HTTP response input")
		}
		pointer := false
		if raw, present := item["optional"]; present {
			var ok bool
			pointer, ok = raw.(bool)
			if !ok || (pointer && types.FieldType(kind) == types.FieldTypeJsonb) {
				return nil, fmt.Errorf("invalid HTTP response input presence")
			}
		}
		seen[name] = true
		fields = append(fields, gen.GoModelField{Name: name, Selection: name, Type: types.FieldType(kind), Pointer: pointer})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	return fields, nil
}

func bindPreparedResponseField(target openapicontract.GoProperty, model, inputs []gen.GoModelField, rule map[string]any) (responseField, error) {
	raw, prepared := rule["input"]
	if !prepared {
		return bindResponseField(target, model, rule)
	}
	name, ok := raw.(string)
	if !ok {
		return responseField{}, fmt.Errorf("HTTP response input must name a declared field")
	}
	if _, exists := rule["source"]; exists {
		return responseField{}, fmt.Errorf("HTTP response field cannot use both a model source and an input")
	}
	if _, exists := rule["constant"]; exists {
		return responseField{}, fmt.Errorf("HTTP response field cannot use both a constant and an input")
	}
	copy := make(map[string]any, len(rule))
	for k, v := range rule {
		if k != "input" {
			copy[k] = v
		}
	}
	copy["source"] = name
	field, err := bindResponseField(target, inputs, copy)
	if err != nil {
		return responseField{}, err
	}
	field.input = true
	return field, nil
}
