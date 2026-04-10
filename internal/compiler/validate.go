package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

	// Validate language: must match archetype's language and only "go" is
	// supported in MVP.
	if archetype != nil {
		result.Errors = append(result.Errors, validateLanguage(svcDecl.Language, archetype.Language)...)
	}

	// Component and port validation require a valid archetype.
	var components map[string]*types.Component
	if archetype != nil {
		baselineNames, err := collectComponentNames(archetype, svcDecl, reg)
		if err != nil {
			result.Errors = append(result.Errors, ValidationError{
				Category: "component",
				Message:  err.Error(),
			})
		} else {
			// Look up baseline components.
			baselineComponents := make(map[string]*types.Component)
			allFound := true
			for _, name := range baselineNames {
				comp := reg.Component(name)
				if comp == nil {
					result.Errors = append(result.Errors, ValidationError{
						Category: "component",
						Message:  fmt.Sprintf("component %q not found in registry (referenced by archetype %q)", name, archetype.Name),
					})
					allFound = false
				} else {
					baselineComponents[name] = comp
				}
			}

			// Validate port resolution (only if we resolved all baseline components).
			if allFound {
				servicePortOverrides := make(map[string]string)
				for key, val := range svcDecl.Overrides {
					if strVal, ok := val.(string); ok {
						servicePortOverrides[key] = strVal
					}
				}
				resolution, portErr := ports.Resolve(ports.ResolveInput{
					Components:        baselineComponents,
					ArchetypeBindings: archetype.Bindings,
					ServiceOverrides:  servicePortOverrides,
					ComponentLoader:   reg.Component,
				})
				if portErr != nil {
					result.Errors = append(result.Errors, ValidationError{
						Category: "port",
						Message:  portErr.Error(),
					})
				} else {
					components = resolution.ActiveComponents
				}
			}
		}
	}

	// Validate base_path starts with "/" if set.
	if svcDecl.BasePath != "" && !strings.HasPrefix(svcDecl.BasePath, "/") {
		result.Errors = append(result.Errors, ValidationError{
			Category: "service",
			Message:  fmt.Sprintf("base_path must start with '/', got %q", svcDecl.BasePath),
		})
	}

	// Validate entity field types.
	result.Errors = append(result.Errors, validateFieldTypes(svcDecl.Entities)...)

	// Validate collection references.
	result.Errors = append(result.Errors, validateCollectionReferences(svcDecl.Collections, svcDecl.Entities)...)

	// Validate collection operations and operation-dependent attributes.
	result.Errors = append(result.Errors, validateCollectionOps(svcDecl.Collections)...)

	// Validate no duplicate mixin names.
	result.Errors = append(result.Errors, validateMixinUniqueness(svcDecl.Mixins)...)

	// Validate slot bindings reference available slots.
	if components != nil {
		// Collect mixin-added slots (adds_slots) — these are defined by the
		// mixin itself, not by any component, so they must be added to the
		// available set separately.
		var mixinSlots []types.SlotDefinition
		for _, mixinName := range svcDecl.Mixins {
			mixin := reg.Mixin(mixinName)
			if mixin != nil {
				mixinSlots = append(mixinSlots, mixin.AddsSlots...)
			}
		}
		result.Errors = append(result.Errors, validateSlotNames(svcDecl.Slots, components, mixinSlots)...)
	}

	// Validate slot binding collections reference existing collection names.
	result.Errors = append(result.Errors, validateSlotBindingCollections(svcDecl.Slots, svcDecl.Collections)...)

	// Validate short_circuit is only used with chain operator.
	result.Errors = append(result.Errors, validateSlotBindingShortCircuit(svcDecl.Slots)...)

	// Validate each slot binding has exactly one operator (gate, chain, or fan-out).
	result.Errors = append(result.Errors, validateSlotBindingOperatorPresence(svcDecl.Slots)...)
	result.Errors = append(result.Errors, validateSlotBindingOperatorExclusivity(svcDecl.Slots)...)

	// Validate no duplicate fill names within a single operator's fill list.
	result.Errors = append(result.Errors, validateSlotBindingFillUniqueness(svcDecl.Slots)...)

	// Validate slot binding uniqueness (no duplicate slot+entity+operator).
	result.Errors = append(result.Errors, validateSlotBindingUniquenessCollect(svcDecl.Slots)...)

	// Validate fills exist on disk and reference valid collections.
	fillErrs, fillInfraErr := validateFillsExist(svcDecl.Slots, svcDecl.Collections, input.ProjectDir)
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
	var errs []ValidationError

	// Check for duplicate entity names.
	for _, e := range entities {
		if entityNames[e.Name] {
			errs = append(errs, ValidationError{
				Category: "entity",
				Message:  fmt.Sprintf("entity %q is defined more than once", e.Name),
			})
		}
		entityNames[e.Name] = true
	}

	for _, e := range entities {
		// Check for duplicate field names within this entity.
		fieldSeen := make(map[string]bool, len(e.Fields))
		for _, f := range e.Fields {
			if fieldSeen[f.Name] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q has duplicate field name %q", e.Name, f.Name),
				})
			}
			fieldSeen[f.Name] = true
		}

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

			// Check for duplicate enum values within the values list.
			if len(f.Values) > 0 {
				valueSeen := make(map[string]bool, len(f.Values))
				for _, v := range f.Values {
					if valueSeen[v] {
						errs = append(errs, ValidationError{
							Category: "field-type",
							Message:  fmt.Sprintf("entity %q field %q has duplicate enum value %q", e.Name, f.Name, v),
						})
					}
					valueSeen[v] = true
				}
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

			// Pattern syntactic validity: the pattern value must be a valid
			// regular expression. Invalid patterns produce broken SQL CHECK
			// constraints and invalid OpenAPI schema pattern fields.
			if f.Pattern != "" && stringTypes[f.Type] {
				if _, err := regexp.Compile(f.Pattern); err != nil {
					errs = append(errs, ValidationError{
						Category: "field-type",
						Message:  fmt.Sprintf("entity %q field %q has invalid pattern constraint %q — regexp parse error: %v", e.Name, f.Name, f.Pattern, err),
					})
				}
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

			// --- Range consistency: min_length <= max_length, min <= max ---

			if f.MinLength != nil && f.MaxLength != nil && *f.MinLength > *f.MaxLength {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q has min_length (%d) > max_length (%d) — the constraint range is empty and no value can satisfy both bounds", e.Name, f.Name, *f.MinLength, *f.MaxLength),
				})
			}
			if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Message:  fmt.Sprintf("entity %q field %q has min (%v) > max (%v) — the constraint range is empty and no value can satisfy both bounds", e.Name, f.Name, *f.Min, *f.Max),
				})
			}

			// --- unique_composite field name references and intra-list uniqueness ---

			if len(f.UniqueComposite) > 0 {
				fieldNames := make(map[string]bool, len(e.Fields))
				for _, ef := range e.Fields {
					fieldNames[ef.Name] = true
				}
				ucSeen := make(map[string]bool, len(f.UniqueComposite))
				for _, ucField := range f.UniqueComposite {
					if ucSeen[ucField] {
						errs = append(errs, ValidationError{
							Category: "field-type",
							Message:  fmt.Sprintf("entity %q field %q has duplicate entry %q in unique_composite", e.Name, f.Name, ucField),
						})
					}
					ucSeen[ucField] = true
					if !fieldNames[ucField] {
						errs = append(errs, ValidationError{
							Category: "field-type",
							Message:  fmt.Sprintf("entity %q field %q: unique_composite references field %q which does not exist in entity %q", e.Name, f.Name, ucField, e.Name),
						})
					}
				}
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

// validateCollectionReferences checks that each collection references a defined
// entity, that scope field names exist on the referenced entity and scope
// values reference existing entities, that field-ref attributes (upsert_key)
// resolve to actual fields, and that operation-dependent attributes
// (upsert_key, concurrency) require the upsert operation.
func validateCollectionReferences(collections []types.Collection, entities []types.Entity) []ValidationError {
	entityNames := make(map[string]bool, len(entities))
	entityFields := make(map[string]map[string]bool, len(entities))
	for _, e := range entities {
		entityNames[e.Name] = true
		fields := make(map[string]bool, len(e.Fields))
		for _, f := range e.Fields {
			fields[f.Name] = true
		}
		entityFields[e.Name] = fields
	}

	// Check for duplicate collection names.
	var errs []ValidationError
	collCount := make(map[string]int, len(collections))
	for _, c := range collections {
		collCount[c.Name]++
	}
	var duplicateNames []string
	for name, count := range collCount {
		if count > 1 {
			duplicateNames = append(duplicateNames, name)
		}
	}
	sort.Strings(duplicateNames)
	for _, name := range duplicateNames {
		errs = append(errs, ValidationError{
			Category: "collection",
			Message:  fmt.Sprintf("duplicate collection name %q", name),
		})
	}

	// Build set of collection names for scope-entity-in-collections validation.
	collectionNames := make(map[string]bool, len(collections))
	for _, c := range collections {
		collectionNames[c.Name] = true
	}

	for _, c := range collections {
		if !entityNames[c.Entity] {
			errs = append(errs, ValidationError{
				Category: "collection",
				Message:  fmt.Sprintf("collection %q references entity %q which is not defined in entities", c.Name, c.Entity),
			})
			continue
		}

		// Validate scope cardinality: multi-field scopes are not yet supported.
		// ScopeField() and ParentEntity() iterate the map and return the first
		// element, which is non-deterministic for maps with more than one entry.
		if len(c.Scope) > 1 {
			errs = append(errs, ValidationError{
				Category: "collection",
				Message:  fmt.Sprintf("collection %q has %d scope entries but multi-field scopes are not yet supported — scope must contain exactly one entry", c.Name, len(c.Scope)),
			})
		}

		// Validate scope: each key must be a field on the entity, each value
		// must be a defined entity name.
		for fieldName, targetEntity := range c.Scope {
			if !entityFields[c.Entity][fieldName] {
				errs = append(errs, ValidationError{
					Category: "collection",
					Message:  fmt.Sprintf("collection %q has scope field %q which is not a field on entity %q", c.Name, fieldName, c.Entity),
				})
			}
			if !entityNames[targetEntity] {
				errs = append(errs, ValidationError{
					Category: "collection",
					Message:  fmt.Sprintf("collection %q has scope value %q which is not a defined entity", c.Name, targetEntity),
				})
			}
		}

		// Validate upsert_key field name references, intra-list uniqueness,
		// and constraint that upsert_key fields must not be computed.
		if len(c.UpsertKey) > 0 {
			// Build field map for constraint checks.
			entityFieldMap := make(map[string]types.Field)
			for _, e := range entities {
				if e.Name == c.Entity {
					for _, f := range e.Fields {
						entityFieldMap[f.Name] = f
					}
					break
				}
			}

			ukSeen := make(map[string]bool, len(c.UpsertKey))
			for _, keyField := range c.UpsertKey {
				if ukSeen[keyField] {
					errs = append(errs, ValidationError{
						Category: "collection",
						Message:  fmt.Sprintf("collection %q: upsert_key contains duplicate field reference %q", c.Name, keyField),
					})
				}
				ukSeen[keyField] = true
				if !entityFields[c.Entity][keyField] {
					errs = append(errs, ValidationError{
						Category: "collection",
						Message:  fmt.Sprintf("collection %q has upsert_key field %q which is not a field on entity %q", c.Name, keyField, c.Entity),
					})
					continue
				}
				// Upsert key fields must not be computed — computed fields are
				// read-only and populated by fills, so they cannot serve as
				// natural keys for conflict resolution.
				if f, ok := entityFieldMap[keyField]; ok && f.Computed {
					errs = append(errs, ValidationError{
						Category: "collection",
						Message:  fmt.Sprintf("collection %q: upsert_key field %q is computed — computed fields cannot be used as upsert keys", c.Name, keyField),
					})
				}
			}
		}

		// Determine whether upsert is in the operations list.
		hasUpsertOp := false
		for _, op := range c.Operations {
			if op == types.OpUpsert {
				hasUpsertOp = true
				break
			}
		}

		// Validate bidirectional upsert/upsert_key dependency.
		if len(c.UpsertKey) > 0 && !hasUpsertOp {
			errs = append(errs, ValidationError{
				Category: "collection",
				Message:  fmt.Sprintf("collection %q specifies upsert_key but does not include 'upsert' in its operations list — upsert_key requires the upsert operation", c.Name),
			})
		}
		if hasUpsertOp && len(c.UpsertKey) == 0 {
			errs = append(errs, ValidationError{
				Category: "collection",
				Message:  fmt.Sprintf("collection %q includes 'upsert' operation but does not specify upsert_key — upsert requires a natural key for conflict resolution", c.Name),
			})
		}

		// Validate concurrency requires upsert operation.
		if c.Concurrency != "" && !hasUpsertOp {
			errs = append(errs, ValidationError{
				Category: "collection",
				Message:  fmt.Sprintf("collection %q specifies concurrency %q but does not include 'upsert' in its operations list — concurrency is only meaningful with the upsert operation", c.Name, c.Concurrency),
			})
		}

		// Validate concurrency value is a recognized mode.
		if c.Concurrency != "" && !types.ValidConcurrencyModes[c.Concurrency] {
			var validModes []string
			for mode := range types.ValidConcurrencyModes {
				validModes = append(validModes, string(mode))
			}
			sort.Strings(validModes)
			errs = append(errs, ValidationError{
				Category: "collection",
				Message:  fmt.Sprintf("collection %q has invalid concurrency mode %q — valid modes: %v", c.Name, c.Concurrency, validModes),
			})
		}

		// Determine whether patch is in the operations list.
		hasPatchOp := false
		for _, op := range c.Operations {
			if op == types.OpPatch {
				hasPatchOp = true
				break
			}
		}

		// Validate bidirectional patch/patchable dependency.
		if len(c.Patchable) > 0 && !hasPatchOp {
			errs = append(errs, ValidationError{
				Category: "collection",
				Message:  fmt.Sprintf("collection %q specifies patchable but does not include 'patch' in its operations list — patchable requires the patch operation", c.Name),
			})
		}
		if hasPatchOp && len(c.Patchable) == 0 {
			errs = append(errs, ValidationError{
				Category: "collection",
				Message:  fmt.Sprintf("collection %q includes 'patch' operation but does not specify patchable — patch requires a list of patchable fields", c.Name),
			})
		}

		// Validate patchable field references and constraints.
		if len(c.Patchable) > 0 && entityNames[c.Entity] {
			// Build field map for constraint checks.
			entityFieldMap := make(map[string]types.Field, len(entities))
			for _, e := range entities {
				if e.Name == c.Entity {
					for _, f := range e.Fields {
						entityFieldMap[f.Name] = f
					}
					break
				}
			}

			patchSeen := make(map[string]bool, len(c.Patchable))
			for _, pf := range c.Patchable {
				// Duplicate check.
				if patchSeen[pf] {
					errs = append(errs, ValidationError{
						Category: "collection",
						Message:  fmt.Sprintf("collection %q: patchable contains duplicate field reference %q", c.Name, pf),
					})
				}
				patchSeen[pf] = true

				// Field existence check.
				if !entityFields[c.Entity][pf] {
					errs = append(errs, ValidationError{
						Category: "collection",
						Message:  fmt.Sprintf("collection %q has patchable field %q which is not a field on entity %q", c.Name, pf, c.Entity),
					})
					continue
				}

				// Patchable fields must not be computed.
				f := entityFieldMap[pf]
				if f.Computed {
					errs = append(errs, ValidationError{
						Category: "collection",
						Message:  fmt.Sprintf("collection %q: patchable field %q is computed — computed fields cannot be patched", c.Name, pf),
					})
				}
				// Patchable fields must not be ref type.
				if f.Type == types.FieldTypeRef {
					errs = append(errs, ValidationError{
						Category: "collection",
						Message:  fmt.Sprintf("collection %q: patchable field %q is a ref type — ref fields cannot be patched", c.Name, pf),
					})
				}
			}
		}
	}
	return errs
}

// validateCollectionOps checks that each operation in collections is valid and
// that no operation appears more than once within a single collection.
func validateCollectionOps(collections []types.Collection) []ValidationError {
	var errs []ValidationError
	for _, c := range collections {
		// Check for empty operations list — a collection with no operations
		// is semantically void.
		if len(c.Operations) == 0 {
			errs = append(errs, ValidationError{
				Category: "operation",
				Message:  fmt.Sprintf("collection %q has no operations — each collection must have at least one operation", c.Name),
			})
		}

		opSeen := make(map[types.Operation]bool, len(c.Operations))
		for _, op := range c.Operations {
			if opSeen[op] {
				errs = append(errs, ValidationError{
					Category: "operation",
					Message:  fmt.Sprintf("collection %q has duplicate operation %q", c.Name, op),
				})
			}
			opSeen[op] = true

			if !types.ValidOperations[op] {
				errs = append(errs, ValidationError{
					Category: "operation",
					Message:  fmt.Sprintf("collection %q has invalid operation %q", c.Name, op),
				})
			}
		}
	}
	return errs
}

// validateSlotNames checks that each slot binding references a slot defined by
// one of the resolved components or by a mixin's adds_slots.
func validateSlotNames(slots []types.SlotDeclaration, components map[string]*types.Component, mixinSlots []types.SlotDefinition) []ValidationError {
	// Build set of available slot names across all components.
	available := make(map[string]bool)
	for _, comp := range components {
		for _, sd := range comp.Slots {
			available[sd.Name] = true
		}
	}

	// Include mixin-added slots (adds_slots). These are defined directly
	// by mixins, not by any component — collectComponentNames only adds
	// the mixin's AddsComponents, not its AddsSlots.
	for _, sd := range mixinSlots {
		available[sd.Name] = true
	}

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

// validateSlotBindingCollections checks that each slot binding with a
// non-empty Collection field references a collection that is defined in
// the collections list.
func validateSlotBindingCollections(slots []types.SlotDeclaration, collections []types.Collection) []ValidationError {
	if len(slots) == 0 {
		return nil
	}

	collectionNames := make(map[string]bool, len(collections))
	for _, c := range collections {
		collectionNames[c.Name] = true
	}

	var errs []ValidationError
	for _, sb := range slots {
		if sb.Collection == "" {
			continue
		}
		if !collectionNames[sb.Collection] {
			var names []string
			for _, c := range collections {
				names = append(names, c.Name)
			}
			errs = append(errs, ValidationError{
				Category: "slot",
				Message: fmt.Sprintf("slot binding %q references collection %q which is not defined (available collections: %v)",
					sb.Slot, sb.Collection, names),
			})
		}
	}
	return errs
}

// validateSlotBindingShortCircuit checks that short_circuit: true is only set
// on slot bindings that use a chain operator. The spec defines short_circuit
// exclusively in the context of chain steps — it has no effect on gate or
// fan-out operators.
func validateSlotBindingShortCircuit(slots []types.SlotDeclaration) []ValidationError {
	var errs []ValidationError
	for _, sb := range slots {
		if sb.ShortCircuit && len(sb.Chain) == 0 {
			errs = append(errs, ValidationError{
				Category: "slot",
				Message:  fmt.Sprintf("slot binding %q has short_circuit: true but does not use a chain operator — short_circuit is only meaningful with chain", sb.Slot),
			})
		}
	}
	return errs
}

// validateSlotBindingUniquenessCollect checks that no two slot bindings share
// the same composite key (slot, collection, operator type). Duplicate bindings
// produce duplicate variable declarations in generated code. This is the
// error-collecting variant used by Validate.
func validateSlotBindingUniquenessCollect(slots []types.SlotDeclaration) []ValidationError {
	type compositeKey struct {
		slot, collection, operator string
	}
	seen := make(map[compositeKey]bool)

	var errs []ValidationError
	for _, sb := range slots {
		operators := []struct {
			name string
			has  bool
		}{
			{"gate", len(sb.Gate) > 0},
			{"chain", len(sb.Chain) > 0},
			{"fan-out", len(sb.FanOut) > 0},
		}
		for _, op := range operators {
			if !op.has {
				continue
			}
			key := compositeKey{slot: sb.Slot, collection: sb.Collection, operator: op.name}
			if seen[key] {
				collDesc := ""
				if sb.Collection != "" {
					collDesc = fmt.Sprintf(" for collection %q", sb.Collection)
				}
				errs = append(errs, ValidationError{
					Category: "slot",
					Message:  fmt.Sprintf("duplicate slot binding: slot %q%s with operator %q appears more than once", sb.Slot, collDesc, op.name),
				})
			}
			seen[key] = true
		}
	}
	return errs
}

// validateSlotBindingOperatorPresence checks that each slot binding has at
// least one operator (gate, chain, or fan-out). A slot binding without any
// operator is semantically void — it declares intent to bind a slot but wires
// no fills to it.
func validateSlotBindingOperatorPresence(slots []types.SlotDeclaration) []ValidationError {
	var errs []ValidationError
	for _, sb := range slots {
		if len(sb.Gate) == 0 && len(sb.Chain) == 0 && len(sb.FanOut) == 0 {
			entityDesc := ""
			if sb.Collection != "" {
				entityDesc = fmt.Sprintf(" on collection %q", sb.Collection)
			}
			errs = append(errs, ValidationError{
				Category: "slot",
				Message:  fmt.Sprintf("slot binding for slot %q%s has no operator — each slot binding must specify at least one of: gate, chain, fan-out", sb.Slot, entityDesc),
			})
		}
	}
	return errs
}

// validateSlotBindingOperatorExclusivity checks that each slot binding has at
// most one operator (gate, chain, or fan-out). The spec shows each slot binding
// with exactly one operator — gate, chain, and fan-out are semantically distinct
// composition strategies (AND-gate, sequential pipeline, concurrent broadcast)
// that cannot be meaningfully combined on a single slot invocation.
func validateSlotBindingOperatorExclusivity(slots []types.SlotDeclaration) []ValidationError {
	var errs []ValidationError
	for _, sb := range slots {
		var present []string
		if len(sb.Gate) > 0 {
			present = append(present, "gate")
		}
		if len(sb.Chain) > 0 {
			present = append(present, "chain")
		}
		if len(sb.FanOut) > 0 {
			present = append(present, "fan-out")
		}
		if len(present) > 1 {
			entityDesc := ""
			if sb.Collection != "" {
				entityDesc = fmt.Sprintf(" on collection %q", sb.Collection)
			}
			errs = append(errs, ValidationError{
				Category: "slot",
				Message:  fmt.Sprintf("slot binding for slot %q%s has multiple operators (%s) — each slot binding must specify exactly one operator: gate, chain, or fan-out", sb.Slot, entityDesc, strings.Join(present, ", ")),
			})
		}
	}
	return errs
}

// validateSlotBindingFillUniqueness checks that no fill name appears more than
// once within a single operator's fill list. Duplicate fills in a gate cause
// the same policy to be evaluated twice; in a chain, the same step runs twice;
// in a fan-out, the same handler fires twice producing duplicate side-effects.
func validateSlotBindingFillUniqueness(slots []types.SlotDeclaration) []ValidationError {
	var errs []ValidationError
	for _, sb := range slots {
		entityDesc := ""
		if sb.Collection != "" {
			entityDesc = fmt.Sprintf(" for collection %q", sb.Collection)
		}

		for _, opInfo := range []struct {
			name  string
			fills []string
		}{
			{"gate", sb.Gate},
			{"chain", sb.Chain},
			{"fan-out", sb.FanOut},
		} {
			seen := make(map[string]bool, len(opInfo.fills))
			for _, fill := range opInfo.fills {
				if seen[fill] {
					errs = append(errs, ValidationError{
						Category: "slot",
						Message:  fmt.Sprintf("slot binding %q%s has duplicate fill %q in %s operator list", sb.Slot, entityDesc, fill, opInfo.name),
					})
				}
				seen[fill] = true
			}
		}
	}
	return errs
}

// validateMixinUniqueness checks that no mixin name appears more than once in
// the service declaration's mixins list.
func validateMixinUniqueness(mixins []string) []ValidationError {
	if len(mixins) == 0 {
		return nil
	}
	var errs []ValidationError
	seen := make(map[string]bool, len(mixins))
	for _, name := range mixins {
		if seen[name] {
			errs = append(errs, ValidationError{
				Category: "mixin",
				Message:  fmt.Sprintf("mixin %q appears more than once in the mixins list", name),
			})
		}
		seen[name] = true
	}
	return errs
}

// validateFillsExist checks that each fill referenced in slot bindings has a
// fills/<name>/fill.yaml file in the project directory and that the fill's
// Collection field references a defined collection name. It returns validation
// errors for missing fills or invalid collection references, and returns an
// infrastructure Go error for unexpected filesystem or parsing failures.
func validateFillsExist(slots []types.SlotDeclaration, collections []types.Collection, projectDir string) ([]ValidationError, error) {
	fillNames := collectFillNames(slots)
	if len(fillNames) == 0 {
		return nil, nil
	}

	collectionNames := make(map[string]bool, len(collections))
	for _, c := range collections {
		collectionNames[c.Name] = true
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
			continue
		}

		// Parse the fill and validate its Collection field.
		fill, err := parser.ParseFill(fillPath)
		if err != nil {
			return nil, fmt.Errorf("parsing fill %q at %s: %w", name, fillPath, err)
		}
		if fill.Collection != "" && !collectionNames[fill.Collection] {
			var names []string
			for _, c := range collections {
				names = append(names, c.Name)
			}
			sort.Strings(names)
			errs = append(errs, ValidationError{
				Category: "fill",
				Message:  fmt.Sprintf("fill %q references collection %q which is not defined (available collections: %v)", name, fill.Collection, names),
			})
		}
	}
	return errs, nil
}

// validateLanguage checks that the service declaration's language matches the
// archetype's declared language and that only supported languages are used.
// Only "go" is supported in MVP.
func validateLanguage(serviceLanguage, archetypeLanguage string) []ValidationError {
	var errs []ValidationError

	if serviceLanguage == "" {
		errs = append(errs, ValidationError{
			Category: "language",
			Message:  "service declaration is missing the 'language' field",
		})
		return errs
	}

	if serviceLanguage != "go" {
		errs = append(errs, ValidationError{
			Category: "language",
			Message:  fmt.Sprintf("unsupported language %q — only 'go' is supported", serviceLanguage),
		})
	}

	if archetypeLanguage != "" && serviceLanguage != archetypeLanguage {
		errs = append(errs, ValidationError{
			Category: "language",
			Message:  fmt.Sprintf("service language %q does not match archetype language %q", serviceLanguage, archetypeLanguage),
		})
	}

	return errs
}
