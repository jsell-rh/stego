package compiler

import (
	"fmt"
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
		for _, other := range names[:i] {
			previous := components[other].OutputNamespace
			if previous != "" && (previous == namespace || strings.HasPrefix(previous, namespace+"/") || strings.HasPrefix(namespace, previous+"/")) {
				errors = append(errors, ValidationError{
					Category: "namespace", Message: fmt.Sprintf("components %q and %q have overlapping output namespaces %q and %q", other, name, previous, namespace),
				})
			}
		}
	}
	return errors
}
