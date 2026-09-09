package types

import (
	"fmt"
	"strings"
)

// ValidateVersioned reserves the database-managed revision column and Go field.
func ValidateVersioned(entities []Entity) []error {
	var result []error
	for _, entity := range entities {
		if !entity.Versioned {
			continue
		}
		for _, field := range entity.Fields {
			name := strings.ToLower(strings.ReplaceAll(field.Name, "_", ""))
			if name == "stegorevision" || name == "resourceversion" {
				result = append(result, fmt.Errorf("entity %s: field %s conflicts with resource version metadata", entity.Name, field.Name))
			}
		}
	}
	return result
}
