package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/ports"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// ValidationError represents a single validation failure with a category for
// grouping and a human-readable message.
type ValidationError struct {
	Category string
	Message  string
}

// ValidationResult collects all validation errors found during Validate.
type ValidationResult struct {
	Errors []ValidationError
}

// HasErrors returns true if the result contains any validation errors.
func (r *ValidationResult) HasErrors() bool {
	return len(r.Errors) > 0
}

// Validate checks the service declaration against the registry and reports all
// semantic validation errors without running generators. Infrastructure
// failures (cannot read files, corrupt YAML) are returned as Go errors;
// semantic issues are collected in ValidationResult.Errors.
func Validate(input ReconcilerInput) (*ValidationResult, error) {
	result := &ValidationResult{}

	// Parse service.yaml.
	serviceYAMLPath := filepath.Join(input.ProjectDir, "service.yaml")
	serviceData, err := os.ReadFile(serviceYAMLPath)
	if err != nil {
		return nil, fmt.Errorf("reading service.yaml: %w", err)
	}
	svcDecl, err := parser.ParseServiceDeclarationFromBytes(serviceData, serviceYAMLPath)
	if err != nil {
		return nil, fmt.Errorf("parsing service.yaml: %w", err)
	}

	// Load registry.
	reg, err := registry.Load(input.RegistryDir)
	if err != nil {
		return nil, fmt.Errorf("loading registry: %w", err)
	}

	// Validate archetype exists.
	archetype := reg.Archetype(svcDecl.Archetype)
	if archetype == nil {
		result.Errors = append(result.Errors, ValidationError{
			Category: "archetype",
			Message:  fmt.Sprintf("archetype %q not found in registry", svcDecl.Archetype),
		})
	}

	// Component and port validation require a valid archetype.
	var components map[string]*types.Component
	if archetype != nil {
		componentNames, err := collectComponentNames(archetype, svcDecl, reg)
		if err != nil {
			result.Errors = append(result.Errors, ValidationError{
				Category: "component",
				Message:  err.Error(),
			})
		} else {
			// Look up all components.
			components = make(map[string]*types.Component)
			for _, name := range componentNames {
				comp := reg.Component(name)
				if comp == nil {
					result.Errors = append(result.Errors, ValidationError{
						Category: "component",
						Message:  fmt.Sprintf("component %q not found in registry (referenced by archetype %q)", name, archetype.Name),
					})
				} else {
					components[name] = comp
				}
			}

			// Validate port resolution (only if we resolved all components).
			if len(components) == len(componentNames) {
				servicePortOverrides := make(map[string]string)
				for key, val := range svcDecl.Overrides {
					if strVal, ok := val.(string); ok {
						servicePortOverrides[key] = strVal
					}
				}
				_, portErr := ports.Resolve(ports.ResolveInput{
					Components:        components,
					ArchetypeBindings: archetype.Bindings,
					ServiceOverrides:  servicePortOverrides,
				})
				if portErr != nil {
					result.Errors = append(result.Errors, ValidationError{
						Category: "port",
						Message:  portErr.Error(),
					})
				}
			}
		}
	}

	// Validate entity field types.
	result.Errors = append(result.Errors, validateFieldTypes(svcDecl.Entities)...)

	// Validate expose block references.
	result.Errors = append(result.Errors, validateExposeReferences(svcDecl.Expose, svcDecl.Entities)...)

	// Validate expose block operations.
	result.Errors = append(result.Errors, validateExposeOps(svcDecl.Expose)...)

	// Validate slot bindings reference available slots.
	if components != nil {
		result.Errors = append(result.Errors, validateSlotNames(svcDecl.Slots, components)...)
	}

	// Validate slot binding entities are in the expose list.
	result.Errors = append(result.Errors, validateSlotBindingEntitiesCollect(svcDecl.Slots, svcDecl.Expose)...)

	// Validate fills exist on disk.
	fillErrs, fillInfraErr := validateFillsExist(svcDecl.Slots, input.ProjectDir)
	if fillInfraErr != nil {
		return nil, fillInfraErr
	}
	result.Errors = append(result.Errors, fillErrs...)

	return result, nil
}

// FormatValidation produces a human-readable summary of validation results.
func FormatValidation(r *ValidationResult) string {
	if !r.HasErrors() {
		return "Validation passed. No issues found."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Validation failed with %d error(s):\n\n", len(r.Errors))
	for _, e := range r.Errors {
		fmt.Fprintf(&sb, "  [%s] %s\n", e.Category, e.Message)
	}
	return sb.String()
}

// stringTypes is the set of field types that accept string constraints
// (min_length, max_length, pattern).
var stringTypes = map[types.FieldType]bool{
	types.FieldTypeString: true,
}

// numericTypes is the set of field types that accept numeric constraints
// (min, max).
var numericTypes = map[types.FieldType]bool{
	types.FieldTypeInt32:  true,
	types.FieldTypeInt64:  true,
	types.FieldTypeFloat:  true,
	types.FieldTypeDouble: true,
}

// validateFieldTypes checks that all entity field types are valid, ref fields
// have a target entity, enum fields have values, constraint attributes are
// applied only to their designated field types, and computed/filled_by are
// declared together.
func validateFieldTypes(entities []types.Entity) []ValidationError {
	entityNames := make(map[string]bool, len(entities))
	for _, e := range entities {
		entityNames[e.Name] = true
	}

	var errs []ValidationError
	for _, e := range entities {
		for _, f := range e.Fields {
			if !types.ValidFieldTypes[f.Type] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q has invalid type %q", e.Name, f.Name, f.Type),
				})
				continue
			}

			// --- Type-specific required attributes ---

			if f.Type == types.FieldTypeRef {
				if f.To == "" {
					errs = append(errs, ValidationError{
						Category: "field-type",
						Message:  fmt.Sprintf("entity %q field %q has type ref but no 'to' attribute", e.Name, f.Name),
					})
				} else if !entityNames[f.To] {
					errs = append(errs, ValidationError{
						Category: "field-type",
						Message:  fmt.Sprintf("entity %q field %q references entity %q in 'to' but no such entity is defined", e.Name, f.Name, f.To),
					})
				}
			}
			if f.Type == types.FieldTypeEnum && len(f.Values) == 0 {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q has type enum but no values", e.Name, f.Name),
				})
			}

			// --- Constraint-type applicability ---

			// String-only constraints: min_length, max_length, pattern.
			if f.MinLength != nil && !stringTypes[f.Type] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q: constraint 'min_length' is only valid for string fields, not %q", e.Name, f.Name, f.Type),
				})
			}
			if f.MaxLength != nil && !stringTypes[f.Type] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q: constraint 'max_length' is only valid for string fields, not %q", e.Name, f.Name, f.Type),
				})
			}
			if f.Pattern != "" && !stringTypes[f.Type] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q: constraint 'pattern' is only valid for string fields, not %q", e.Name, f.Name, f.Type),
				})
			}

			// Numeric-only constraints: min, max.
			if f.Min != nil && !numericTypes[f.Type] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q: constraint 'min' is only valid for numeric fields (int32, int64, float, double), not %q", e.Name, f.Name, f.Type),
				})
			}
			if f.Max != nil && !numericTypes[f.Type] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q: constraint 'max' is only valid for numeric fields (int32, int64, float, double), not %q", e.Name, f.Name, f.Type),
				})
			}

			// Enum-only constraint: values.
			if len(f.Values) > 0 && f.Type != types.FieldTypeEnum {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q: constraint 'values' is only valid for enum fields, not %q", e.Name, f.Name, f.Type),
				})
			}

			// Ref-only constraint: to.
			if f.To != "" && f.Type != types.FieldTypeRef {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q: attribute 'to' is only valid for ref fields, not %q", e.Name, f.Name, f.Type),
				})
			}

			// --- Computed/filled_by co-occurrence ---

			if f.Computed && f.FilledBy == "" {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q has 'computed: true' but no 'filled_by' attribute — computed fields must specify the fill that populates them", e.Name, f.Name),
				})
			}
			if f.FilledBy != "" && !f.Computed {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q has 'filled_by: %s' but 'computed' is not set to true — fields with filled_by must also set computed: true", e.Name, f.Name, f.FilledBy),
				})
			}
		}
	}
	return errs
}

// validateExposeReferences checks that each expose block references a defined
// entity and a defined parent (if set).
func validateExposeReferences(expose []types.ExposeBlock, entities []types.Entity) []ValidationError {
	entityNames := make(map[string]bool, len(entities))
	for _, e := range entities {
		entityNames[e.Name] = true
	}

	var errs []ValidationError
	for _, eb := range expose {
		if !entityNames[eb.Entity] {
			errs = append(errs, ValidationError{
				Category: "entity-ref",
				Message:  fmt.Sprintf("expose block references entity %q which is not defined in entities", eb.Entity),
			})
		}
		if eb.Parent != "" && !entityNames[eb.Parent] {
			errs = append(errs, ValidationError{
				Category: "entity-ref",
				Message:  fmt.Sprintf("expose block for entity %q references parent %q which is not defined in entities", eb.Entity, eb.Parent),
			})
		}
	}
	return errs
}

// validateExposeOps checks that each operation in expose blocks is valid.
func validateExposeOps(expose []types.ExposeBlock) []ValidationError {
	var errs []ValidationError
	for _, eb := range expose {
		for _, op := range eb.Operations {
			if !types.ValidOperations[op] {
				errs = append(errs, ValidationError{
					Category: "operation",
					Message:  fmt.Sprintf("expose block for entity %q has invalid operation %q", eb.Entity, op),
				})
			}
		}
	}
	return errs
}

// validateSlotNames checks that each slot binding references a slot defined by
// one of the resolved components.
func validateSlotNames(slots []types.SlotDeclaration, components map[string]*types.Component) []ValidationError {
	// Build set of available slot names across all components.
	available := make(map[string]bool)
	for _, comp := range components {
		for _, sd := range comp.Slots {
			available[sd.Name] = true
		}
	}

	// Also check mixin-added slots — they are already included in the
	// component set via collectComponentNames, so their slots are covered.

	var errs []ValidationError
	for _, sb := range slots {
		if !available[sb.Slot] {
			// Collect available slot names for a helpful error message.
			var names []string
			for name := range available {
				names = append(names, name)
			}
			sort.Strings(names)
			errs = append(errs, ValidationError{
				Category: "slot",
				Message:  fmt.Sprintf("slot binding references slot %q which is not defined by any component (available slots: %v)", sb.Slot, names),
			})
		}
	}
	return errs
}

// validateSlotBindingEntitiesCollect is the error-collecting variant of
// validateSlotBindingEntities — it collects all violations rather than
// returning on the first.
func validateSlotBindingEntitiesCollect(slots []types.SlotDeclaration, expose []types.ExposeBlock) []ValidationError {
	if len(slots) == 0 {
		return nil
	}

	exposedEntities := make(map[string]bool, len(expose))
	for _, eb := range expose {
		exposedEntities[eb.Entity] = true
	}

	var errs []ValidationError
	for _, sb := range slots {
		if sb.Entity == "" {
			continue
		}
		if !exposedEntities[sb.Entity] {
			var exposed []string
			for _, eb := range expose {
				exposed = append(exposed, eb.Entity)
			}
			errs = append(errs, ValidationError{
				Category: "slot",
				Message: fmt.Sprintf("slot binding %q declares entity %q which is not in the expose list (exposed entities: %v)",
					sb.Slot, sb.Entity, exposed),
			})
		}
	}
	return errs
}

// validateFillsExist checks that each fill referenced in slot bindings has a
// fills/<name>/fill.yaml file in the project directory. It returns validation
// errors for missing fills, and returns an infrastructure Go error for
// unexpected filesystem failures (e.g. permission denied).
func validateFillsExist(slots []types.SlotDeclaration, projectDir string) ([]ValidationError, error) {
	fillNames := collectFillNames(slots)
	if len(fillNames) == 0 {
		return nil, nil
	}

	var errs []ValidationError
	for _, name := range fillNames {
		fillPath := filepath.Join(projectDir, "fills", name, "fill.yaml")
		if _, err := os.Stat(fillPath); err != nil {
			if os.IsNotExist(err) {
				errs = append(errs, ValidationError{
					Category: "fill",
					Message:  fmt.Sprintf("fill %q referenced in slot bindings but fills/%s/fill.yaml does not exist", name, name),
				})
			} else {
				return nil, fmt.Errorf("checking fill %q at %s: %w", name, fillPath, err)
			}
		}
	}
	return errs, nil
}
