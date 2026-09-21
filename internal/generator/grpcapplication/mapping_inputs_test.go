package grpcapplication_test

import (
	"bytes"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"strings"
	"testing"
)

func preparedShipmentContext(t *testing.T) gen.Context {
	c := shipmentContext(t)
	declaration := mappingDeclaration(c)
	inputs := []any{}
	for _, item := range []struct {
		target, name, kind string
		optional           bool
	}{
		{"metadata.href", "Reference", "string", false}, {"metadata.created_at", "Created", "timestamp", false},
		{"name", "Display", "string", true}, {"count", "Count", "int64", false}, {"enabled", "Enabled", "bool", false},
		{"recorded_at", "Recorded", "timestamp", true}, {"reset", "Reset", "string", false},
		{"score", "Score", "float", false}, {"total", "Total", "double", false},
		{"limit", "Limit", "int64", true}, {"tags", "Tags", "jsonb", false},
	} {
		inputs = append(inputs, map[string]any{"name": item.name, "type": item.kind, "optional": item.optional})
		rule := mappingRule(c, item.target)
		delete(rule, "source")
		rule["input"] = item.name
	}
	c.Inputs["api.proto"] = []byte(strings.Replace(shipmentProto, "int32 count=3;", "int32 count=3; int32 prepared_count=13;", 1))
	inputs = append(inputs, map[string]any{"name": "PreparedCount", "type": "int32"})
	declaration["inputs"] = inputs
	declaration["fields"] = append(declaration["fields"].([]any), map[string]any{"target": "prepared_count", "input": "PreparedCount"})
	return c
}

func TestResponseMappingPreparedInputValidation(t *testing.T) {
	for name, edit := range map[string]func(gen.Context){
		"empty":              func(c gen.Context) { mappingDeclaration(c)["inputs"] = []any{} },
		"wrong shape":        func(c gen.Context) { mappingDeclaration(c)["inputs"] = "input" },
		"unknown input":      func(c gen.Context) { mappingRule(c, "name")["input"] = "Unknown" },
		"wrong name type":    func(c gen.Context) { mappingRule(c, "name")["input"] = 7 },
		"model and input":    func(c gen.Context) { mappingRule(c, "name")["source"] = "name" },
		"constant and input": func(c gen.Context) { mappingRule(c, "name")["constant"] = "name" },
		"unused": func(c gen.Context) {
			d := mappingDeclaration(c)
			d["inputs"] = append(d["inputs"].([]any), map[string]any{"name": "Unused", "type": "string"})
		},
		"duplicate": func(c gen.Context) {
			d := mappingDeclaration(c)
			d["inputs"] = append(d["inputs"].([]any), d["inputs"].([]any)[0])
		},
		"private": func(c gen.Context) { mappingDeclaration(c)["inputs"].([]any)[0].(map[string]any)["name"] = "private" },
		"expression": func(c gen.Context) {
			mappingDeclaration(c)["inputs"].([]any)[0].(map[string]any)["name"] = "Value.Call()"
		},
		"unknown type":   func(c gen.Context) { mappingDeclaration(c)["inputs"].([]any)[0].(map[string]any)["type"] = "Custom" },
		"unknown option": func(c gen.Context) { mappingDeclaration(c)["inputs"].([]any)[0].(map[string]any)["import"] = "custom" },
		"wrong presence": func(c gen.Context) { mappingDeclaration(c)["inputs"].([]any)[0].(map[string]any)["optional"] = "true" },
		"list pointer": func(c gen.Context) {
			for _, v := range mappingDeclaration(c)["inputs"].([]any) {
				i := v.(map[string]any)
				if i["name"] == "Tags" {
					i["optional"] = true
				}
			}
		},
		"optional to required": func(c gen.Context) {
			for _, v := range mappingDeclaration(c)["inputs"].([]any) {
				i := v.(map[string]any)
				if i["name"] == "Enabled" {
					i["optional"] = true
				}
			}
		},
		"type mismatch": func(c gen.Context) { mappingRule(c, "enabled")["input"] = "Reference" },
		"nested object": func(c gen.Context) {
			d := mappingDeclaration(c)
			d["fields"] = append(d["fields"].([]any), map[string]any{"target": "metadata", "input": "Reference"})
		},
		"too many": func(c gen.Context) {
			inputs := make([]any, 129)
			for i := range inputs {
				inputs[i] = map[string]any{"name": "Value", "type": "string"}
			}
			mappingDeclaration(c)["inputs"] = inputs
		},
		"name too long": func(c gen.Context) {
			mappingDeclaration(c)["inputs"].([]any)[0].(map[string]any)["name"] = strings.Repeat("A", 129)
		},
		"type name collision": func(c gen.Context) {
			d := mappingDeclaration(c)
			other := map[string]any{}
			for k, v := range d {
				other[k] = v
			}
			delete(other, "inputs")
			other["name"] = "ShipmentInput"
			c.ComponentConfig["response_mappings"] = append(c.ComponentConfig["response_mappings"].([]any), other)
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := preparedShipmentContext(t)
			edit(c)
			g := new(grpcapplication.Generator)
			if err := g.ValidateContext(c); err == nil {
				t.Fatal("invalid input passed validation")
			}
			if files, _, err := g.Generate(c); err == nil || len(files) != 0 {
				t.Fatal("invalid input produced files")
			}
		})
	}
}

func TestResponseMappingPreparedOrder(t *testing.T) {
	c := preparedShipmentContext(t)
	g := new(grpcapplication.Generator)
	first, _, err := g.Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	inputs := mappingDeclaration(c)["inputs"].([]any)
	for left, right := 0, len(inputs)-1; left < right; left, right = left+1, right-1 {
		inputs[left], inputs[right] = inputs[right], inputs[left]
	}
	second, _, err := g.Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatal("input order changed output files")
	}
	for i := range first {
		if first[i].Path != second[i].Path || !bytes.Equal(first[i].Bytes(), second[i].Bytes()) {
			t.Fatal("input order changed output")
		}
	}
}
func TestResponseMappingPreparedRuntime(t *testing.T) {
	checkGeneratedResponseMappings(t, preparedShipmentContext, "testdata/mapping_inputs_test.go")
}
