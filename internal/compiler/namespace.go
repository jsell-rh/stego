package compiler

import (
	"fmt"
	"path"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func validateComponentNamespaces(components map[string]*types.Component) []ValidationError {
	var errors []ValidationError
	names := sortedKeys(components)
	for i, name := range names {
		namespace := components[name].OutputNamespace
		if namespace == "" {
			continue
		}
		if err := gen.ValidatePath(namespace); err != nil {
			errors = append(errors, ValidationError{Category: "namespace", Message: fmt.Sprintf("component %q: %v", name, err)})
			continue
		}
		lower := strings.ToLower(namespace)
		if lower == "contracts" || strings.HasPrefix(lower, "contracts/") {
			errors = append(errors, ValidationError{Category: "namespace", Message: fmt.Sprintf("component %q uses compiler-owned namespace %q", name, namespace)})
			continue
		}
		for _, other := range names[:i] {
			previous := components[other].OutputNamespace
			a, b := strings.ToLower(previous), strings.ToLower(namespace)
			if previous != "" && (a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")) {
				errors = append(errors, ValidationError{
					Category: "namespace", Message: fmt.Sprintf("components %q and %q have overlapping output namespaces %q and %q", other, name, previous, namespace),
				})
			}
		}
	}
	return errors
}

// validateFileLayout rejects case aliases and file/directory collisions. These
// layouts cannot be applied consistently across supported filesystems.
func validateFileLayout(names []string) error {
	paths := make(map[string]string)
	for _, name := range names {
		key := strings.ToLower(name)
		if previous, exists := paths[key]; exists {
			return fmt.Errorf("output paths %q and %q refer to the same portable path", previous, name)
		}
		paths[key] = name
	}
	for _, key := range sortedKeys(paths) {
		for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
			if previous, exists := paths[parent]; exists {
				return fmt.Errorf("output file %q conflicts with directory for %q", previous, paths[key])
			}
		}
	}
	return nil
}

// Application layout can change output paths without replacing common metadata.
// Copy each selected component before applying the local path declaration.
func applyComponentNamespaces(namespaces map[string]string, components map[string]*types.Component) []ValidationError {
	var failures []ValidationError
	for _, name := range sortedKeys(namespaces) {
		component := components[name]
		namespace := namespaces[name]
		if component == nil {
			failures = append(failures, ValidationError{Category: "namespace", Message: fmt.Sprintf("component_namespaces refers to inactive component %q", name)})
			continue
		}
		if namespace == "" {
			failures = append(failures, ValidationError{Category: "namespace", Message: fmt.Sprintf("component %q requires a nonempty output namespace", name)})
			continue
		}
		if err := gen.ValidateGoImportNamespace(namespace); err != nil {
			failures = append(failures, ValidationError{Category: "namespace", Message: fmt.Sprintf("component %q: %v", name, err)})
			continue
		}
		copy := *component
		copy.OutputNamespace = namespace
		components[name] = &copy
	}
	return failures
}
