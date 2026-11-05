package compiler

import (
	"sort"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// validateComponentConfig checks supplied values and registry defaults before
// generators consume them. Missing runtime settings are checked at startup.
func validateComponentConfig(service *types.ServiceDeclaration, components map[string]*registry.Component) []ValidationError {
	var errors []ValidationError
	names := sortedKeys(service.Overrides)
	for _, name := range names {
		if declaration.IsConventionOverrideKey(name) {
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
		errors = append(errors, declaration.ValidateSettings("overrides."+name, config, component.Config, false)...)
	}
	for _, name := range sortedKeys(components) {
		for _, key := range sortedKeys(components[name].Config) {
			field := components[name].Config[key]
			if field.Default != nil {
				errors = append(errors, declaration.ValidateSettingValue("components."+name+".config."+key+".default", field.Default, field)...)
			}
		}
	}
	return errors
}

func configError(path, message string) ValidationError {
	return ValidationError{Category: "config", Rule: "config", Path: path, Message: path + ": " + message}
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
