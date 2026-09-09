package types

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// ValidateVersioned reserves the database-managed revision column and Go field.
func ValidateVersioned(entities []Entity, collectionSets ...[]Collection) []error {
	var result []error
	for _, entity := range entities {
		for _, field := range entity.Fields {
			if field.Unobserved == nil {
				continue
			}
			value := *field.Unobserved
			valid := entity.IsObservationField(field.Name) && field.Type == FieldTypeString && len(value) <= 8192 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
			length := utf8.RuneCountInString(value)
			if field.MinLength != nil && length < *field.MinLength {
				valid = false
			}
			if field.MaxLength != nil && length > *field.MaxLength {
				valid = false
			}
			if field.Pattern != "" {
				pattern, err := regexp.Compile(field.Pattern)
				if err != nil || !pattern.MatchString(value) {
					valid = false
				}
			}
			if !valid {
				result = append(result, fmt.Errorf("entity %s: unobserved value requires a valid string observation field %s", entity.Name, field.Name))
			}
		}
		if (len(entity.GenerationFields) > 0 || len(entity.Observations) > 0 || len(entity.CleanupOwners) > 0) && !entity.Versioned {
			result = append(result, fmt.Errorf("entity %s: generations, observations, and cleanup owners require versioned", entity.Name))
		}
		cleanupOwners := map[string]bool{}
		if len(entity.CleanupOwners) > 32 {
			result = append(result, fmt.Errorf("entity %s: at most 32 cleanup owners are supported", entity.Name))
		}
		for _, owner := range entity.CleanupOwners {
			if !observationName.MatchString(owner) || cleanupOwners[owner] {
				result = append(result, fmt.Errorf("entity %s: invalid or repeated cleanup owner %s", entity.Name, owner))
			}
			cleanupOwners[owner] = true
		}
		if !entity.Versioned {
			continue
		}
		for _, field := range entity.Fields {
			name := strings.ToLower(strings.ReplaceAll(field.Name, "_", ""))
			if len(entity.CleanupOwners) > 0 && (name == "stegocleanup" || name == "cleanupstate" || name == "cleanupobservations" || name == "pendingcleanup" || name == "cleanupcomplete") {
				result = append(result, fmt.Errorf("entity %s: field %s conflicts with cleanup metadata", entity.Name, field.Name))
			}
			if name == "stegorevision" || name == "resourceversion" || (len(entity.GenerationFields) > 0 && (name == "stegogeneration" || name == "resourcegeneration" || name == "stegoobservations" || name == "observedgenerations" || name == "observedgeneration" || name == "currentobservations")) {
				result = append(result, fmt.Errorf("entity %s: field %s conflicts with resource version metadata", entity.Name, field.Name))
			}
		}
		fields := map[string]Field{}
		for _, field := range entity.Fields {
			fields[field.Name] = field
		}
		used := map[string]bool{}
		for _, name := range entity.GenerationFields {
			field, exists := fields[name]
			if !exists || field.Computed || used[name] {
				result = append(result, fmt.Errorf("entity %s: invalid or repeated generation field %s", entity.Name, name))
			}
			used[name] = true
		}
		if len(entity.Observations) > 0 && len(entity.GenerationFields) == 0 {
			result = append(result, fmt.Errorf("entity %s: observations require generation_fields", entity.Name))
		}
		owners := make([]string, 0, len(entity.Observations))
		for owner := range entity.Observations {
			owners = append(owners, owner)
		}
		sort.Strings(owners)
		for _, owner := range owners {
			names := entity.Observations[owner]
			if !observationName.MatchString(owner) || len(names) == 0 {
				result = append(result, fmt.Errorf("entity %s: invalid observation group %s", entity.Name, owner))
			}
			for _, name := range names {
				field, exists := fields[name]
				if !exists || used[name] || field.Computed || !field.Optional || field.Default != nil || field.FilledBy != "" {
					result = append(result, fmt.Errorf("entity %s: observation field %s must be unique, optional, writable, and have no default", entity.Name, name))
				}
				used[name] = true
			}
		}
	}
	byName := map[string]Entity{}
	for _, entity := range entities {
		byName[entity.Name] = entity
	}
	for _, collections := range collectionSets {
		for _, collection := range collections {
			name := collection.Name
			entity := byName[collection.Entity]
			for _, field := range append(append([]string{}, collection.UpsertKey...), collection.Patchable...) {
				if entity.IsObservationField(field) {
					result = append(result, fmt.Errorf("collection %s: observation field %s cannot be an upsert key or patchable field", name, field))
				}
			}
		}
	}
	return result
}

var observationName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func (entity Entity) IsObservationField(name string) bool {
	for _, fields := range entity.Observations {
		for _, field := range fields {
			if field == name {
				return true
			}
		}
	}
	return false
}
