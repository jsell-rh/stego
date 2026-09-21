package httpapplication

import (
	"fmt"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/openapicontract"
	"github.com/jsell-rh/stego/internal/responsemapping"
)

func responseInputs(raw any) ([]gen.GoModelField, error) {
	return responsemapping.PreparedInputs(raw, "HTTP response")
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
