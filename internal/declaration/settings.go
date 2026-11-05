package declaration

import (
	"fmt"
	"math"
	"reflect"
	"sort"

	"github.com/jsell-rh/stego/internal/types"
)

// ValidateSettings checks the settings of one component against the component's schema (ST-DECL-INV36): each
// setting must be one the schema declares, and its value must suit the declared type. With required, each
// setting the schema requires and gives no default for must be present.
func ValidateSettings(path string, values map[string]any, schema map[string]types.ConfigField, required bool) []ValidationError {
	var errors []ValidationError
	for _, key := range sortedKeys(values) {
		field, ok := schema[key]
		if !ok {
			errors = append(errors, configError(path+"."+key, "unknown configuration field"))
			continue
		}
		errors = append(errors, ValidateSettingValue(path+"."+key, values[key], field)...)
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

// ValidateSettingValue checks that one value suits the schema field it is given for.
func ValidateSettingValue(path string, value any, field types.ConfigField) []ValidationError {
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
			errors = append(errors, ValidateSettings(itemPath, object, field.Items.Fields, true)...)
		} else if field.Items.Inline != nil {
			errors = append(errors, ValidateSettingValue(itemPath, item, *field.Items.Inline)...)
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
