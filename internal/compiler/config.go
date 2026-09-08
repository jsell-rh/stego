package compiler

import (
	"fmt"
	"math"
	"reflect"
	"sort"

	"github.com/jsell-rh/stego/internal/types"
)

// validateComponentConfig checks supplied values and registry defaults before
// generators consume them. Missing runtime settings are checked at startup.
func validateComponentConfig(service *types.ServiceDeclaration, components map[string]*types.Component) []ValidationError {
	var errors []ValidationError
	names := sortedKeys(service.Overrides)
	for _, name := range names {
		if isConventionOverrideKey(name) {
			continue
		}
		value := service.Overrides[name]
		if _, binding := value.(string); binding {
			continue // Port resolution checks string-valued bindings.
		}
		config, ok := value.(map[string]any)
		if !ok {
			errors = append(errors, configError("overrides."+name, "must be a component configuration map or a port binding"))
			continue
		}
		component := components[name]
		if component == nil {
			errors = append(errors, configError("overrides."+name, "component is not active"))
			continue
		}
		errors = append(errors, validateConfigObject("overrides."+name, config, component.Config, false)...)
	}
	for _, name := range sortedKeys(components) {
		for _, key := range sortedKeys(components[name].Config) {
			field := components[name].Config[key]
			if field.Default != nil {
				errors = append(errors, validateConfigValue("components."+name+".config."+key+".default", field.Default, field)...)
			}
		}
	}
	return errors
}

func configError(path, message string) ValidationError {
	return ValidationError{Category: "config", Message: path + ": " + message}
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateConfigObject(path string, values map[string]any, schema map[string]types.ConfigField, required bool) []ValidationError {
	var errors []ValidationError
	for _, key := range sortedKeys(values) {
		field, ok := schema[key]
		if !ok {
			errors = append(errors, configError(path+"."+key, "unknown configuration field"))
			continue
		}
		errors = append(errors, validateConfigValue(path+"."+key, values[key], field)...)
	}
	if required {
		for _, key := range sortedKeys(schema) {
			field := schema[key]
			if _, exists := values[key]; !exists && !field.Optional && field.Default == nil {
				errors = append(errors, configError(path+"."+key, "required configuration field is missing"))
			}
		}
	}
	return errors
}

func validateConfigValue(path string, value any, field types.ConfigField) []ValidationError {
	if value == nil {
		return []ValidationError{configError(path, "configuration values must not be null")}
	}
	typ := field.Type
	if typ == "" && len(field.Enum) > 0 {
		typ = "string"
	}
	valid := false
	switch typ {
	case "string", "entity-ref", "field-ref":
		_, valid = value.(string)
	case "bool":
		_, valid = value.(bool)
	case "int":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			valid = true
		}
	case "float", "number":
		switch number := value.(type) {
		case float32:
			valid = !math.IsNaN(float64(number)) && !math.IsInf(float64(number), 0)
		case float64:
			valid = !math.IsNaN(number) && !math.IsInf(number, 0)
		case int, int64, uint64:
			valid = true
		}
	case "list":
		valid = reflect.TypeOf(value).Kind() == reflect.Slice
	default:
		return []ValidationError{configError(path, fmt.Sprintf("unsupported configuration type %q", field.Type))}
	}
	if !valid {
		return []ValidationError{configError(path, fmt.Sprintf("must have type %s; got %T", typ, value))}
	}
	if len(field.Enum) > 0 {
		text, ok := value.(string)
		found := false
		for _, allowed := range field.Enum {
			found = found || (ok && text == allowed)
		}
		if !found {
			return []ValidationError{configError(path, fmt.Sprintf("must be one of %v", field.Enum))}
		}
	}
	if typ != "list" || field.Items == nil {
		return nil
	}
	var errors []ValidationError
	list := reflect.ValueOf(value)
	for i := 0; i < list.Len(); i++ {
		itemPath := fmt.Sprintf("%s[%d]", path, i)
		item := list.Index(i).Interface()
		if field.Items.Fields != nil {
			object, ok := item.(map[string]any)
			if !ok {
				errors = append(errors, configError(itemPath, "must be a configuration map"))
				continue
			}
			errors = append(errors, validateConfigObject(itemPath, object, field.Items.Fields, true)...)
		} else if field.Items.Inline != nil {
			errors = append(errors, validateConfigValue(itemPath, item, *field.Items.Inline)...)
		}
	}
	return errors
}
