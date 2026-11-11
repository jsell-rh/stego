package declaration

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// ValidationError is one fault of a declaration as structured data (ST-DECL-RM01): the rule it breaks, the path in
// the declaration where it is, what was found there and the values the rule allows there. Message says the same
// fault in a sentence. The prose for a person is rendered from all of these fields (Prose), so the two cannot
// disagree.
type ValidationError struct {
	// Category groups the faults for the fixed order of the compile check. It is not part of the structured fault.
	Category string `json:"-"`
	// Rule is the name of the rule the fault breaks.
	Rule string `json:"rule"`
	// Path is where in the declaration the fault is, such as entities[1].name or collections.todos.entity.
	Path string `json:"path"`
	// Found is what the declaration holds at the path.
	Found string `json:"found,omitempty"`
	// Allowed holds the values the rule allows at the path, when the rule states a set.
	Allowed []string `json:"allowed,omitempty"`
	// Message is the fault in a sentence for a person.
	Message string `json:"message"`
	// Line and Position name where reading stopped, for a declaration that cannot be read as one document.
	Line     int `json:"line,omitempty"`
	Position int `json:"position,omitempty"`
}

// Prose renders the fault for a person from its structured fields: the rule, the path, the message, what was
// found and the allowed values.
func (e ValidationError) Prose() string {
	var sb strings.Builder
	rule := e.Rule
	if rule == "" {
		rule = e.Category
	}
	if e.Path == "" {
		fmt.Fprintf(&sb, "[%s] %s", rule, e.Message)
	} else {
		fmt.Fprintf(&sb, "[%s] %s: %s", rule, e.Path, e.Message)
	}
	var details []string
	if e.Found != "" {
		details = append(details, fmt.Sprintf("found %q", e.Found))
	}
	if len(e.Allowed) > 0 {
		details = append(details, "allowed: "+strings.Join(e.Allowed, ", "))
	}
	if len(details) > 0 {
		fmt.Fprintf(&sb, " (%s)", strings.Join(details, "; "))
	}
	return sb.String()
}

// ValidationResult collects all validation errors that a check finds.
type ValidationResult struct {
	Errors []ValidationError
	// Settings holds the settings of the declaration as checking resolved them (ResolvedSettings).
	Settings []Setting
}

// HasErrors returns true if the result contains any validation errors.
func (r *ValidationResult) HasErrors() bool {
	return len(r.Errors) > 0
}

// Faults holds the faults that a declaration's own checks find, in the groups in which the compile check of
// Generating reports them between its registry and generator checks.
type Faults struct {
	// Undeclared holds the faults of the fields that the declaration format does not declare.
	Undeclared []ValidationError
	// Service holds the faults of the base path, the convention overrides, the archetype and the idempotency
	// setting.
	Service []ValidationError
	// Entities holds the faults of the field types and of the versioned entities.
	Entities []ValidationError
	// Collections holds the faults of the unique-when-live keys, the collections and the mixin names.
	Collections []ValidationError
	// Slots holds the faults of the slot bindings.
	Slots []ValidationError
}

// All returns every fault, group by group.
func (f Faults) All() []ValidationError {
	var all []ValidationError
	for _, group := range [][]ValidationError{f.Undeclared, f.Service, f.Entities, f.Collections, f.Slots} {
		all = append(all, group...)
	}
	return all
}

// Validate runs the checks that read only the declaration (ST-DECL-QRY01): the fields that the declaration format
// does not declare, which the strict reading stage found, the base path, the convention overrides, the
// idempotency setting, the field types, the versioned and unique-when-live entities, the collections and their
// operations, the mixin names and the slot bindings. It reads no registry, generator or file, and it returns every fault it finds.
func Validate(service *types.ServiceDeclaration, undeclared ...parser.UndeclaredField) Faults {
	var faults Faults
	faults.Undeclared = undeclaredFaults(undeclared)
	if err := gen.ValidateHTTPBasePath(service.BasePath); err != nil {
		faults.Service = append(faults.Service, ValidationError{Category: "service", Rule: "base-path", Path: "base_path", Found: service.BasePath, Message: err.Error()})
	}
	faults.Service = append(faults.Service, validateConventionOverrides(service.Overrides)...)

	faults.Service = append(faults.Service, validateArchetype(service.Archetype)...)
	faults.Service = append(faults.Service, validateNameForms(service)...)
	faults.Service = append(faults.Service, validateIdempotency(service)...)
	faults.Service = append(faults.Service, validateIdempotencyValues(service)...)
	faults.Service = append(faults.Service, validateDefaultsLeft(service)...)
	faults.Service = append(faults.Service, validateTenancyClaim(service)...)
	faults.Entities = append(faults.Entities, validateEntityTenants(service.Entities)...)
	faults.Service = append(faults.Service, validateTenantlessPaths(service)...)
	faults.Service = append(faults.Service, validateAuditValues(service)...)
	faults.Entities = append(faults.Entities, validateFieldTypes(service.Entities)...)
	faults.Entities = append(faults.Entities, validateFieldDefaults(service.Entities)...)
	for _, err := range types.ValidateVersioned(service.Entities, service.Collections) {
		faults.Entities = append(faults.Entities, ValidationError{Category: "field-type", Rule: "versioned-metadata", Path: locate(service, err.Error()), Message: err.Error()})
	}

	for _, err := range types.ValidateLiveUnique(service.Entities, service.Collections) {
		faults.Collections = append(faults.Collections, ValidationError{Category: "field-type", Rule: "unique-when-live", Path: locate(service, err.Error()), Message: err.Error()})
	}
	faults.Collections = append(faults.Collections, validateCollectionReferences(service.Collections, service.Entities)...)
	faults.Collections = append(faults.Collections, validateCollectionOps(service.Collections)...)
	faults.Collections = append(faults.Collections, validateMixinUniqueness(service.Mixins)...)

	faults.Slots = append(faults.Slots, validateSlotBindingCollections(service.Slots, service.Collections)...)
	faults.Slots = append(faults.Slots, validateSlotBindingShortCircuit(service.Slots)...)
	faults.Slots = append(faults.Slots, validateSlotBindingOperatorPresence(service.Slots, undeclared)...)
	faults.Slots = append(faults.Slots, validateSlotBindingOperatorExclusivity(service.Slots)...)
	faults.Slots = append(faults.Slots, validateSlotBindingFillUniqueness(service.Slots)...)
	faults.Slots = append(faults.Slots, validateSlotBindingUniquenessCollect(service.Slots)...)
	return faults
}

// validateArchetype checks that the declaration selects an archetype (ST-DECL-INV01). Reading refuses a list of
// archetypes, since the archetype is one name; the compile check refuses a name the registry does not hold.
func validateArchetype(archetype string) []ValidationError {
	if archetype != "" {
		return nil
	}
	return []ValidationError{{Category: "archetype", Rule: "archetype", Path: "archetype", Message: "the declaration selects no archetype; it must select exactly one"}}
}

// validateFieldDefaults checks that each field's default suits the type the field declares (ST-DECL-INV13): a
// string for a string, true or false for a bool, a whole number in range for int32 and int64, a number for float
// and double, and one of the values for an enum.
func validateFieldDefaults(entities []types.Entity) []ValidationError {
	var errs []ValidationError
	for ei, e := range entities {
		for fi, f := range e.Fields {
			if f.Default == nil || defaultSuits(f) {
				continue
			}
			fault := ValidationError{
				Category: "field-type", Rule: "default-type", Path: fieldPath(ei, fi) + ".default", Found: fmt.Sprint(f.Default),
				Allowed: []string{string(f.Type)},
				Message: fmt.Sprintf("entity %q field %q has the default %v, which does not suit its type %q", e.Name, f.Name, f.Default, f.Type),
			}
			if f.Type == types.FieldTypeEnum {
				fault.Allowed = append([]string(nil), f.Values...)
			}
			errs = append(errs, fault)
		}
	}
	return errs
}

// defaultSuits tells whether a field's default suits its type. A type whose default form the format does not fix
// (bytes, timestamp, ref, jsonb) accepts any default here, except on the server-managed generation field, whose
// default the generated handlers write as a Go literal, so it must be a string, an integer or a boolean of its type.
func defaultSuits(f types.Field) bool {
	if f.Name == "generation" {
		switch f.Type {
		case types.FieldTypeString, types.FieldTypeBool, types.FieldTypeInt32, types.FieldTypeInt64:
		default:
			return false
		}
	}
	switch f.Type {
	case types.FieldTypeString:
		_, ok := f.Default.(string)
		return ok
	case types.FieldTypeBool:
		_, ok := f.Default.(bool)
		return ok
	case types.FieldTypeInt32, types.FieldTypeInt64:
		var value int64
		switch number := f.Default.(type) {
		case int:
			value = int64(number)
		case int64:
			value = number
		case uint64:
			return f.Type == types.FieldTypeInt64 && number <= math.MaxInt64
		default:
			return false
		}
		return f.Type == types.FieldTypeInt64 || (value >= math.MinInt32 && value <= math.MaxInt32)
	case types.FieldTypeFloat, types.FieldTypeDouble:
		switch number := f.Default.(type) {
		case int, int64, uint64:
			return true
		case float64:
			return !math.IsNaN(number) && !math.IsInf(number, 0)
		}
		return false
	case types.FieldTypeEnum:
		text, ok := f.Default.(string)
		if !ok {
			return false
		}
		for _, value := range f.Values {
			if value == text {
				return true
			}
		}
		return false
	}
	return true
}

// The forms of the declared names (ST-DECL-INV86): a generator writes these names into Go, SQL, proto, YAML and
// JSON, so the declaration holds them to a form that no output can read as code, whatever components are active.
var (
	serviceNameForm    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	collectionNameForm = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	entityNameForm     = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)
	fieldNameForm      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	fillNameForm       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// validateNameForms gives one fault for each service, collection, entity, field and slot-binding fill name outside
// its form
// (ST-DECL-INV86), with its path and the form.
func validateNameForms(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	check := func(kind, path, name string, form *regexp.Regexp) {
		if !form.MatchString(name) {
			faults = append(faults, ValidationError{Category: kind, Rule: "name-form", Path: path, Found: name, Allowed: []string{form.String()},
				Message: fmt.Sprintf("the %s name %q does not match %s", kind, name, form)})
		}
	}
	check("service", "name", service.Name, serviceNameForm)
	for ei, e := range service.Entities {
		check("entity", entityPath(ei)+".name", e.Name, entityNameForm)
		for fi, f := range e.Fields {
			check("field", fieldPath(ei, fi)+".name", f.Name, fieldNameForm)
		}
	}
	for _, c := range service.Collections {
		check("collection", collectionPath(c.Name), c.Name, collectionNameForm)
	}
	// A fill that a slot binding names becomes a Go import alias and a folder under fills (ST-DECL-INV86).
	for i, sb := range service.Slots {
		for _, operator := range []struct {
			name  string
			fills []string
		}{{"gate", sb.Gate}, {"chain", sb.Chain}, {"fan-out", sb.FanOut}} {
			for j, name := range operator.fills {
				check("fill", fmt.Sprintf("%s.%s[%d]", slotPath(i), operator.name, j), name, fillNameForm)
			}
		}
	}
	return faults
}

// slotBindingKey matches the path of a key of one slot binding, such as slots[0].merge.
var slotBindingKey = regexp.MustCompile(`^slots\[(\d+)\]\.([^.\[]+)$`)

// undeclaredFaults makes one fault of each field that the declaration format does not declare (ST-DECL-INV02). A
// key of a slot binding that is not declared is an operator the format does not know, so its fault names the
// operators.
func undeclaredFaults(undeclared []parser.UndeclaredField) []ValidationError {
	var faults []ValidationError
	for _, field := range undeclared {
		if slotBindingKey.MatchString(field.Path) {
			faults = append(faults, ValidationError{
				Category: "slot", Rule: "slot-operator", Path: field.Path, Found: field.Key, Allowed: slotOperators(),
				Message: fmt.Sprintf("slot binding has the operator %q, which the declaration format does not know", field.Key),
			})
			continue
		}
		faults = append(faults, ValidationError{
			Category: "field", Rule: "undeclared-field", Path: field.Path, Found: field.Key, Allowed: field.Declared,
			Message: fmt.Sprintf("field %q is not declared by the declaration format", field.Path),
		})
	}
	return faults
}

// ReadingFault makes the one fault of a declaration that cannot be read as one document (ST-DECL-INV03,
// ST-DECL-INV04), with the line and the position in the line where reading stopped when the reader knows them.
func ReadingFault(err error) ValidationError {
	fault := ValidationError{Category: "document", Rule: "one-document", Path: "service.yaml", Message: err.Error()}
	var parseError *parser.ParseError
	if errors.As(err, &parseError) {
		fault.Found = parseError.Context
		fault.Line, fault.Position = parseError.Line, parseError.Column
		if parseError.Bounded {
			fault.Rule = "bounded-input"
		}
	}
	if fault.Line > 0 {
		fault.Path = fmt.Sprintf("line %d, position %d", fault.Line, fault.Position)
	}
	return fault
}

// locate gives the path of the entity or the collection that a message of the types package names first.
func locate(service *types.ServiceDeclaration, message string) string {
	for i, entity := range service.Entities {
		if strings.HasPrefix(message, fmt.Sprintf("entity %s:", entity.Name)) || strings.HasPrefix(message, fmt.Sprintf("entity %q", entity.Name)) {
			return entityPath(i)
		}
	}
	for _, collection := range service.Collections {
		if strings.HasPrefix(message, fmt.Sprintf("collection %s:", collection.Name)) || strings.HasPrefix(message, fmt.Sprintf("collection %q", collection.Name)) {
			return collectionPath(collection.Name)
		}
	}
	return "entities"
}

func entityPath(i int) string { return fmt.Sprintf("entities[%d]", i) }
func fieldPath(entity, field int) string {
	return fmt.Sprintf("entities[%d].fields[%d]", entity, field)
}
func collectionPath(name string) string { return "collections." + name }
func slotPath(i int) string             { return fmt.Sprintf("slots[%d]", i) }
func slotOperators() []string           { return []string{"gate", "chain", "fan-out"} }
func numericTypeNames() []string        { return []string{"int32", "int64", "float", "double"} }
func entityNameList(entities []types.Entity) []string {
	names := make([]string, 0, len(entities))
	for _, entity := range entities {
		names = append(names, entity.Name)
	}
	return names
}

func fieldNameList(entity types.Entity) []string {
	names := make([]string, 0, len(entity.Fields))
	for _, field := range entity.Fields {
		names = append(names, field.Name)
	}
	return names
}

func fieldTypeNames() []string {
	names := make([]string, 0, len(types.ValidFieldTypes))
	for name := range types.ValidFieldTypes {
		names = append(names, string(name))
	}
	sort.Strings(names)
	return names
}

func operationNames() []string {
	names := make([]string, 0, len(types.ValidOperations))
	for name := range types.ValidOperations {
		names = append(names, string(name))
	}
	sort.Strings(names)
	return names
}

// ValidateFieldTypes checks the field types and their constraints of the given entities (ST-DECL-INV05,
// INV06, INV07).
func ValidateFieldTypes(entities []types.Entity) []ValidationError {
	return validateFieldTypes(entities)
}

// FormatValidation renders the faults for a person, each from its structured fields. Each line it gives ends with
// a line break, so the next text a caller prints starts on a line of its own.
func FormatValidation(r *ValidationResult) string {
	if !r.HasErrors() {
		return "Validation passed. No issues found.\n"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Validation failed with %d error(s):\n\n", len(r.Errors))
	for _, e := range r.Errors {
		fmt.Fprintf(&sb, "  %s\n", e.Prose())
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
	for ei, e := range entities {
		if entityNames[e.Name] {
			errs = append(errs, ValidationError{
				Category: "entity",
				Rule:     "unique-entity-name",
				Path:     entityPath(ei) + ".name",
				Found:    e.Name,
				Message:  fmt.Sprintf("entity %q is defined more than once", e.Name),
			})
		}
		entityNames[e.Name] = true
	}

	for ei, e := range entities {
		// Check for duplicate field names within this entity.
		fieldSeen := make(map[string]bool, len(e.Fields))
		for fi, f := range e.Fields {
			if fieldSeen[f.Name] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "unique-field-name",
					Path:     fieldPath(ei, fi) + ".name",
					Found:    f.Name,
					Message:  fmt.Sprintf("entity %q has duplicate field name %q", e.Name, f.Name),
				})
			}
			fieldSeen[f.Name] = true
		}

		for fi, f := range e.Fields {
			if !types.ValidFieldTypes[f.Type] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "field-type",
					Path:     fieldPath(ei, fi) + ".type",
					Found:    string(f.Type),
					Allowed:  fieldTypeNames(),
					Message:  fmt.Sprintf("entity %q field %q has invalid type %q", e.Name, f.Name, f.Type),
				})
				continue
			}

			// --- Type-specific required attributes ---

			if f.Type == types.FieldTypeRef {
				if f.To == "" {
					errs = append(errs, ValidationError{
						Category: "field-type",
						Rule:     "ref-target",
						Path:     fieldPath(ei, fi) + ".to",
						Found:    "",
						Message:  fmt.Sprintf("entity %q field %q has type ref but no 'to' attribute", e.Name, f.Name),
					})
				} else if !entityNames[f.To] {
					errs = append(errs, ValidationError{
						Category: "field-type",
						Rule:     "ref-target",
						Path:     fieldPath(ei, fi) + ".to",
						Found:    f.To,
						Allowed:  entityNameList(entities),
						Message:  fmt.Sprintf("entity %q field %q references entity %q in 'to' but no such entity is defined", e.Name, f.Name, f.To),
					})
				}
			}
			if f.Type == types.FieldTypeEnum && len(f.Values) == 0 {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "enum-values",
					Path:     fieldPath(ei, fi) + ".values",
					Found:    "",
					Message:  fmt.Sprintf("entity %q field %q has type enum but no values", e.Name, f.Name),
				})
			}

			// Check for duplicate enum values within the values list.
			if len(f.Values) > 0 {
				valueSeen := make(map[string]bool, len(f.Values))
				for vi, v := range f.Values {
					if valueSeen[v] {
						errs = append(errs, ValidationError{
							Category: "field-type",
							Rule:     "enum-values",
							Path:     fmt.Sprintf("%s.values[%d]", fieldPath(ei, fi), vi),
							Found:    v,
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
					Rule:     "constraint-type",
					Path:     fieldPath(ei, fi) + ".min_length",
					Found:    string(f.Type),
					Allowed:  []string{"string"},
					Message:  fmt.Sprintf("entity %q field %q: constraint 'min_length' is only valid for string fields, not %q", e.Name, f.Name, f.Type),
				})
			}
			if f.MaxLength != nil && !stringTypes[f.Type] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "constraint-type",
					Path:     fieldPath(ei, fi) + ".max_length",
					Found:    string(f.Type),
					Allowed:  []string{"string"},
					Message:  fmt.Sprintf("entity %q field %q: constraint 'max_length' is only valid for string fields, not %q", e.Name, f.Name, f.Type),
				})
			}
			if f.Pattern != "" && !stringTypes[f.Type] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "constraint-type",
					Path:     fieldPath(ei, fi) + ".pattern",
					Found:    string(f.Type),
					Allowed:  []string{"string"},
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
						Rule:     "pattern",
						Path:     fieldPath(ei, fi) + ".pattern",
						Found:    f.Pattern,
						Message:  fmt.Sprintf("entity %q field %q has invalid pattern constraint %q — regexp parse error: %v", e.Name, f.Name, f.Pattern, err),
					})
				}
			}

			// Numeric bounds must be finite before any target formats them.
			for _, bound := range []*float64{f.Min, f.Max} {
				if bound != nil && (math.IsNaN(*bound) || math.IsInf(*bound, 0)) {
					errs = append(errs, ValidationError{Category: "field-type", Rule: "numeric-bound", Path: fieldPath(ei, fi), Found: "", Message: fmt.Sprintf("entity %q field %q: numeric bounds must be finite", e.Name, f.Name)})
				}
			}
			// Numeric-only constraints: min, max.
			if f.Min != nil && !numericTypes[f.Type] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "constraint-type",
					Path:     fieldPath(ei, fi) + ".min",
					Found:    string(f.Type),
					Allowed:  numericTypeNames(),
					Message:  fmt.Sprintf("entity %q field %q: constraint 'min' is only valid for numeric fields (int32, int64, float, double), not %q", e.Name, f.Name, f.Type),
				})
			}
			if f.Max != nil && !numericTypes[f.Type] {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "constraint-type",
					Path:     fieldPath(ei, fi) + ".max",
					Found:    string(f.Type),
					Allowed:  numericTypeNames(),
					Message:  fmt.Sprintf("entity %q field %q: constraint 'max' is only valid for numeric fields (int32, int64, float, double), not %q", e.Name, f.Name, f.Type),
				})
			}

			// Enum-only constraint: values.
			if len(f.Values) > 0 && f.Type != types.FieldTypeEnum {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "constraint-type",
					Path:     fieldPath(ei, fi) + ".values",
					Found:    string(f.Type),
					Allowed:  []string{"enum"},
					Message:  fmt.Sprintf("entity %q field %q: constraint 'values' is only valid for enum fields, not %q", e.Name, f.Name, f.Type),
				})
			}

			// Ref-only constraint: to.
			if f.To != "" && f.Type != types.FieldTypeRef {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "constraint-type",
					Path:     fieldPath(ei, fi) + ".to",
					Found:    string(f.Type),
					Allowed:  []string{"ref"},
					Message:  fmt.Sprintf("entity %q field %q: attribute 'to' is only valid for ref fields, not %q", e.Name, f.Name, f.Type),
				})
			}

			// --- Range consistency: min_length <= max_length, min <= max ---

			if f.MinLength != nil && f.MaxLength != nil && *f.MinLength > *f.MaxLength {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "length-range",
					Path:     fieldPath(ei, fi) + ".min_length",
					Found:    fmt.Sprint(*f.MinLength),
					Message:  fmt.Sprintf("entity %q field %q has min_length (%d) > max_length (%d) — the constraint range is empty and no value can satisfy both bounds", e.Name, f.Name, *f.MinLength, *f.MaxLength),
				})
			}
			if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "numeric-range",
					Path:     fieldPath(ei, fi) + ".min",
					Found:    fmt.Sprint(*f.Min),
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
				for ui, ucField := range f.UniqueComposite {
					if ucSeen[ucField] {
						errs = append(errs, ValidationError{
							Category: "field-type",
							Rule:     "unique-composite",
							Path:     fmt.Sprintf("%s.unique_composite[%d]", fieldPath(ei, fi), ui),
							Found:    ucField,
							Message:  fmt.Sprintf("entity %q field %q has duplicate entry %q in unique_composite", e.Name, f.Name, ucField),
						})
					}
					ucSeen[ucField] = true
					if !fieldNames[ucField] {
						errs = append(errs, ValidationError{
							Category: "field-type",
							Rule:     "unique-composite",
							Path:     fmt.Sprintf("%s.unique_composite[%d]", fieldPath(ei, fi), ui),
							Found:    ucField,
							Allowed:  fieldNameList(e),
							Message:  fmt.Sprintf("entity %q field %q: unique_composite references field %q which does not exist in entity %q", e.Name, f.Name, ucField, e.Name),
						})
					}
				}
			}

			// --- Computed/filled_by co-occurrence ---

			if f.Computed && f.FilledBy == "" {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "computed-fill",
					Path:     fieldPath(ei, fi) + ".filled_by",
					Found:    "",
					Message:  fmt.Sprintf("entity %q field %q has 'computed: true' but no 'filled_by' attribute — computed fields must specify the fill that populates them", e.Name, f.Name),
				})
			}
			if f.FilledBy != "" && !f.Computed {
				errs = append(errs, ValidationError{
					Category: "field-type",
					Rule:     "computed-fill",
					Path:     fieldPath(ei, fi) + ".computed",
					Found:    "false",
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
			Rule:     "unique-collection-name",
			Path:     collectionPath(name),
			Found:    name,
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
				Rule:     "collection-entity",
				Path:     collectionPath(c.Name) + ".entity",
				Found:    c.Entity,
				Allowed:  entityNameList(entities),
				Message:  fmt.Sprintf("collection %q references entity %q which is not defined in entities", c.Name, c.Entity),
			})
			continue
		}

		// Validate scope: each key must be a field on the entity, each value
		// must be a defined entity name.
		for _, fieldName := range sortedKeys(c.Scope) {
			targetEntity := c.Scope[fieldName]
			if !entityFields[c.Entity][fieldName] {
				errs = append(errs, ValidationError{
					Category: "collection",
					Rule:     "collection-scope",
					Path:     collectionPath(c.Name) + ".scope." + fieldName,
					Found:    fieldName,
					Message:  fmt.Sprintf("collection %q has scope field %q which is not a field on entity %q", c.Name, fieldName, c.Entity),
				})
			}
			if !entityNames[targetEntity] {
				errs = append(errs, ValidationError{
					Category: "collection",
					Rule:     "collection-scope",
					Path:     collectionPath(c.Name) + ".scope." + fieldName,
					Found:    targetEntity,
					Allowed:  entityNameList(entities),
					Message:  fmt.Sprintf("collection %q has scope value %q which is not a defined entity", c.Name, targetEntity),
				})
			}
		}

		// Validate implicit field references: each key must be a field on
		// the entity, each value must be non-empty, keys must not overlap
		// with computed fields, and keys must not overlap with scope keys.
		if len(c.Implicit) > 0 {
			// Build field map for computed checks.
			entityFieldMap := make(map[string]types.Field)
			for _, e := range entities {
				if e.Name == c.Entity {
					for _, f := range e.Fields {
						entityFieldMap[f.Name] = f
					}
					break
				}
			}

			// Iterate implicit keys in sorted order for deterministic errors.
			implicitKeys := make([]string, 0, len(c.Implicit))
			for k := range c.Implicit {
				implicitKeys = append(implicitKeys, k)
			}
			sort.Strings(implicitKeys)

			for _, fieldName := range implicitKeys {
				val := c.Implicit[fieldName]

				// Key must be a field on the entity.
				if !entityFields[c.Entity][fieldName] {
					errs = append(errs, ValidationError{
						Category: "collection",
						Rule:     "collection-implicit",
						Path:     collectionPath(c.Name) + ".implicit." + fieldName,
						Found:    fieldName,
						Message:  fmt.Sprintf("collection %q has implicit key %q which is not a field on entity %q", c.Name, fieldName, c.Entity),
					})
					continue
				}

				// Value must be non-empty.
				if val == "" {
					errs = append(errs, ValidationError{
						Category: "collection",
						Rule:     "collection-implicit",
						Path:     collectionPath(c.Name) + ".implicit." + fieldName,
						Found:    "",
						Message:  fmt.Sprintf("collection %q has implicit key %q with empty value — implicit values must be non-empty string literals", c.Name, fieldName),
					})
				}

				// Key must not overlap with computed fields.
				if f, ok := entityFieldMap[fieldName]; ok && f.Computed {
					errs = append(errs, ValidationError{
						Category: "collection",
						Rule:     "collection-implicit",
						Path:     collectionPath(c.Name) + ".implicit." + fieldName,
						Found:    fieldName,
						Message:  fmt.Sprintf("collection %q: implicit key %q is a computed field — computed fields are populated by fills and cannot have implicit values", c.Name, fieldName),
					})
				}

				// Key must not overlap with scope keys.
				if _, inScope := c.Scope[fieldName]; inScope {
					errs = append(errs, ValidationError{
						Category: "collection",
						Rule:     "collection-implicit",
						Path:     collectionPath(c.Name) + ".implicit." + fieldName,
						Found:    fieldName,
						Message:  fmt.Sprintf("collection %q: implicit key %q overlaps with scope key — a field cannot be both scope-derived and implicitly set", c.Name, fieldName),
					})
				}
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
			for ki, keyField := range c.UpsertKey {
				if ukSeen[keyField] {
					errs = append(errs, ValidationError{
						Category: "collection",
						Rule:     "upsert-key",
						Path:     fmt.Sprintf("%s.upsert_key[%d]", collectionPath(c.Name), ki),
						Found:    keyField,
						Message:  fmt.Sprintf("collection %q: upsert_key contains duplicate field reference %q", c.Name, keyField),
					})
				}
				ukSeen[keyField] = true
				if !entityFields[c.Entity][keyField] {
					errs = append(errs, ValidationError{
						Category: "collection",
						Rule:     "upsert-key",
						Path:     fmt.Sprintf("%s.upsert_key[%d]", collectionPath(c.Name), ki),
						Found:    keyField,
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
						Rule:     "upsert-key",
						Path:     fmt.Sprintf("%s.upsert_key[%d]", collectionPath(c.Name), ki),
						Found:    keyField,
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
				Rule:     "upsert-key",
				Path:     collectionPath(c.Name) + ".operations",
				Found:    "",
				Allowed:  []string{"upsert"},
				Message:  fmt.Sprintf("collection %q specifies upsert_key but does not include 'upsert' in its operations list — upsert_key requires the upsert operation", c.Name),
			})
		}
		if hasUpsertOp && len(c.UpsertKey) == 0 {
			errs = append(errs, ValidationError{
				Category: "collection",
				Rule:     "upsert-key",
				Path:     collectionPath(c.Name) + ".upsert_key",
				Found:    "",
				Message:  fmt.Sprintf("collection %q includes 'upsert' operation but does not specify upsert_key — upsert requires a natural key for conflict resolution", c.Name),
			})
		}

		// Validate concurrency requires upsert operation.
		if c.Concurrency != "" && !hasUpsertOp {
			errs = append(errs, ValidationError{
				Category: "collection",
				Rule:     "concurrency",
				Path:     collectionPath(c.Name) + ".concurrency",
				Found:    string(c.Concurrency),
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
				Rule:     "concurrency",
				Path:     collectionPath(c.Name) + ".concurrency",
				Found:    string(c.Concurrency),
				Allowed:  validModes,
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
				Rule:     "patchable",
				Path:     collectionPath(c.Name) + ".operations",
				Found:    "",
				Allowed:  []string{"patch"},
				Message:  fmt.Sprintf("collection %q specifies patchable but does not include 'patch' in its operations list — patchable requires the patch operation", c.Name),
			})
		}
		if hasPatchOp && len(c.Patchable) == 0 {
			errs = append(errs, ValidationError{
				Category: "collection",
				Rule:     "patchable",
				Path:     collectionPath(c.Name) + ".patchable",
				Found:    "",
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
			for pi, pf := range c.Patchable {
				// Duplicate check.
				if patchSeen[pf] {
					errs = append(errs, ValidationError{
						Category: "collection",
						Rule:     "patchable",
						Path:     fmt.Sprintf("%s.patchable[%d]", collectionPath(c.Name), pi),
						Found:    pf,
						Message:  fmt.Sprintf("collection %q: patchable contains duplicate field reference %q", c.Name, pf),
					})
				}
				patchSeen[pf] = true

				// Field existence check.
				if !entityFields[c.Entity][pf] {
					errs = append(errs, ValidationError{
						Category: "collection",
						Rule:     "patchable",
						Path:     fmt.Sprintf("%s.patchable[%d]", collectionPath(c.Name), pi),
						Found:    pf,
						Message:  fmt.Sprintf("collection %q has patchable field %q which is not a field on entity %q", c.Name, pf, c.Entity),
					})
					continue
				}

				// Patchable fields must not be computed.
				f := entityFieldMap[pf]
				if f.Computed {
					errs = append(errs, ValidationError{
						Category: "collection",
						Rule:     "patchable",
						Path:     fmt.Sprintf("%s.patchable[%d]", collectionPath(c.Name), pi),
						Found:    pf,
						Message:  fmt.Sprintf("collection %q: patchable field %q is computed — computed fields cannot be patched", c.Name, pf),
					})
				}
				// Patchable fields must not be ref type.
				if f.Type == types.FieldTypeRef {
					errs = append(errs, ValidationError{
						Category: "collection",
						Rule:     "patchable",
						Path:     fmt.Sprintf("%s.patchable[%d]", collectionPath(c.Name), pi),
						Found:    pf,
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
				Rule:     "operation",
				Path:     collectionPath(c.Name) + ".operations",
				Found:    "",
				Allowed:  operationNames(),
				Message:  fmt.Sprintf("collection %q has no operations — each collection must have at least one operation", c.Name),
			})
		}

		opSeen := make(map[types.Operation]bool, len(c.Operations))
		for oi, op := range c.Operations {
			if opSeen[op] {
				errs = append(errs, ValidationError{
					Category: "operation",
					Rule:     "operation",
					Path:     fmt.Sprintf("%s.operations[%d]", collectionPath(c.Name), oi),
					Found:    string(op),
					Message:  fmt.Sprintf("collection %q has duplicate operation %q", c.Name, op),
				})
			}
			opSeen[op] = true

			if !types.ValidOperations[op] {
				errs = append(errs, ValidationError{
					Category: "operation",
					Rule:     "operation",
					Path:     fmt.Sprintf("%s.operations[%d]", collectionPath(c.Name), oi),
					Found:    string(op),
					Allowed:  operationNames(),
					Message:  fmt.Sprintf("collection %q has invalid operation %q", c.Name, op),
				})
			}
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
	for si, sb := range slots {
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
				Rule:     "slot-collection",
				Path:     slotPath(si) + ".collection",
				Found:    sb.Collection,
				Allowed:  names,
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
	for si, sb := range slots {
		if sb.ShortCircuit && len(sb.Chain) == 0 {
			errs = append(errs, ValidationError{
				Category: "slot",
				Rule:     "short-circuit",
				Path:     slotPath(si) + ".short_circuit",
				Found:    "true",
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
	for si, sb := range slots {
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
					Rule:     "unique-slot-binding",
					Path:     slotPath(si) + "." + op.name,
					Found:    op.name,
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
func validateSlotBindingOperatorPresence(slots []types.SlotDeclaration, undeclared []parser.UndeclaredField) []ValidationError {
	// A binding whose operator the format does not know already has its fault (undeclaredFaults).
	unknownOperator := make(map[string]bool)
	for _, field := range undeclared {
		if match := slotBindingKey.FindStringSubmatch(field.Path); match != nil {
			unknownOperator[match[1]] = true
		}
	}
	var errs []ValidationError
	for si, sb := range slots {
		if unknownOperator[fmt.Sprint(si)] {
			continue
		}
		if len(sb.Gate) == 0 && len(sb.Chain) == 0 && len(sb.FanOut) == 0 {
			entityDesc := ""
			if sb.Collection != "" {
				entityDesc = fmt.Sprintf(" on collection %q", sb.Collection)
			}
			errs = append(errs, ValidationError{
				Category: "slot",
				Rule:     "slot-operator",
				Path:     slotPath(si),
				Found:    "",
				Allowed:  slotOperators(),
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
	for si, sb := range slots {
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
				Rule:     "slot-operator",
				Path:     slotPath(si),
				Found:    strings.Join(present, ", "),
				Allowed:  slotOperators(),
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
	for si, sb := range slots {
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
						Rule:     "unique-fill",
						Path:     slotPath(si) + "." + opInfo.name,
						Found:    fill,
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
	for mi, name := range mixins {
		if seen[name] {
			errs = append(errs, ValidationError{
				Category: "mixin",
				Rule:     "unique-mixin",
				Path:     fmt.Sprintf("mixins[%d]", mi),
				Found:    name,
				Message:  fmt.Sprintf("mixin %q appears more than once in the mixins list", name),
			})
		}
		seen[name] = true
	}
	return errs
}

// validCORSOverrideValues is the set of recognized values for the cors
// convention override.
var validCORSOverrideValues = map[string]bool{
	"enabled":  true,
	"disabled": true,
}

// validateConventionOverrides checks that convention override keys in the
// service declaration's overrides map have recognized values.
func validateConventionOverrides(overrides map[string]any) []ValidationError {
	var errs []ValidationError
	if corsVal, ok := overrides["cors"]; ok {
		strVal, isStr := corsVal.(string)
		if !isStr {
			errs = append(errs, ValidationError{
				Category: "override",
				Rule:     "override-value",
				Path:     "overrides.cors",
				Found:    fmt.Sprint(corsVal),
				Allowed:  []string{"disabled", "enabled"},
				Message:  fmt.Sprintf("override 'cors' must be a string value (\"enabled\" or \"disabled\"), got %T", corsVal),
			})
		} else if !validCORSOverrideValues[strVal] {
			errs = append(errs, ValidationError{
				Category: "override",
				Rule:     "override-value",
				Path:     "overrides.cors",
				Found:    strVal,
				Allowed:  []string{"disabled", "enabled"},
				Message:  fmt.Sprintf("override 'cors' has invalid value %q — valid values: [enabled, disabled]", strVal),
			})
		}
	}
	return errs
}

// conventionOverrideKeys lists YAML override keys that correspond to archetype
// convention fields rather than port binding overrides. These keys must be
// excluded from port resolution and instead applied to the conventions struct.
var conventionOverrideKeys = map[string]bool{
	"cors": true,
}

// isConventionOverrideKey returns true if the given override key is a convention
// override rather than a port binding override.
func IsConventionOverrideKey(key string) bool {
	return conventionOverrideKeys[key]
}
