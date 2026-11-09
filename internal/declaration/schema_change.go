package declaration

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jsell-rh/stego/internal/types"
)

func schemaChangePath(i int) string { return fmt.Sprintf("schema_changes[%d]", i) }
func schemaStepPath(i int) string   { return fmt.Sprintf("schema_history[%d]", i) }

// schemaChangeKinds are the kinds of schema change, in the order the specification gives them.
var schemaChangeKinds = []string{"add", "rename", "retype", "require", "drop", "unique"}

// backfillKinds are the kinds of schema change whose expand step starts a backfill, which runs only in controller
// mode.
var backfillKinds = map[string]bool{"rename": true, "retype": true, "require": true, "unique": true}

// maxOpenSchemaChanges is the most schema changes that may be open at one time, because the backfill observations
// carry the change as an enum (ST-SUP-RP12-LC23).
const maxOpenSchemaChanges = 127

// schemaValue says whether a kind of schema change takes a value, and whether it needs it.
type schemaValue int

const (
	valueRefused schemaValue = iota
	valueOptional
	valueRequired
)

// schemaValues gives, for each value of a schema change, what each kind takes (ST-DECL-INV57). Field is optional
// for add and drop, which may change a whole entity.
var schemaValues = map[string]map[string]schemaValue{
	"field":              {"add": valueOptional, "rename": valueRequired, "retype": valueRequired, "require": valueRequired, "drop": valueOptional, "unique": valueRequired},
	"from":               {"rename": valueRequired, "retype": valueRequired},
	"conversion":         {"retype": valueRequired},
	"inverse_conversion": {"retype": valueRequired},
	"fill_value":         {"require": valueRequired, "retype": valueOptional, "drop": valueOptional},
	"definition":         {"drop": valueRequired},
}

func schemaValueOf(change types.SchemaChange, value string) bool {
	switch value {
	case "field":
		return change.Field != ""
	case "from":
		return change.From != ""
	case "conversion":
		return change.Conversion != ""
	case "inverse_conversion":
		return change.InverseConversion != ""
	case "fill_value":
		return change.FillValue != nil
	case "definition":
		return change.Definition != ""
	}
	return false
}

// validateSchemaChangeKind checks that each schema change has an ID in lower kebab case that is unique in the
// declaration, a known kind, and only the values its kind takes, with each value it needs (ST-DECL-INV57).
func validateSchemaChangeKind(changes []types.SchemaChange) []ValidationError {
	var faults []ValidationError
	seen := map[string]bool{}
	for i, change := range changes {
		path := schemaChangePath(i)
		switch {
		case !kebabName.MatchString(change.ID):
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-kind", Path: path + ".id", Found: change.ID, Message: fmt.Sprintf("the schema change ID %q must match %s: lower kebab case", change.ID, kebabName)})
		case seen[change.ID]:
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-kind", Path: path + ".id", Found: change.ID, Message: fmt.Sprintf("the schema change ID %q is used twice; no change can use the ID of another", change.ID)})
		}
		seen[change.ID] = true
		if !contains(schemaChangeKinds, change.Kind) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-kind", Path: path + ".kind", Found: change.Kind, Allowed: schemaChangeKinds, Message: fmt.Sprintf("schema change %q has the kind %q; its kind must be add, rename, retype, require, drop or unique", change.ID, change.Kind)})
			continue
		}
		for _, value := range []string{"field", "from", "conversion", "inverse_conversion", "fill_value", "definition"} {
			takes, has := schemaValues[value][change.Kind], schemaValueOf(change, value)
			switch {
			case takes == valueRequired && !has:
				faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-kind", Path: path + "." + value, Message: fmt.Sprintf("schema change %q of kind %s has no %s; a %s needs it", change.ID, change.Kind, value, change.Kind)})
			case takes == valueRefused && has:
				faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-kind", Path: path + "." + value, Found: schemaValueText(change, value), Message: fmt.Sprintf("schema change %q of kind %s has a %s, which a %s does not take; remove it", change.ID, change.Kind, value, change.Kind)})
			}
		}
	}
	return faults
}

func schemaValueText(change types.SchemaChange, value string) string {
	switch value {
	case "field":
		return change.Field
	case "from":
		return change.From
	case "conversion":
		return change.Conversion
	case "inverse_conversion":
		return change.InverseConversion
	case "fill_value":
		return fmt.Sprint(change.FillValue)
	}
	return change.Definition
}

func findEntity(entities []types.Entity, name string) (types.Entity, bool) {
	for _, entity := range entities {
		if entity.Name == name {
			return entity, true
		}
	}
	return types.Entity{}, false
}

func findField(entity types.Entity, name string) (types.Field, bool) {
	for _, field := range entity.Fields {
		if field.Name == name {
			return field, true
		}
	}
	return types.Field{}, false
}

// validateSchemaChangeEntity checks that the entity and the field of each schema change agree with the declaration
// (ST-DECL-INV58): for add, rename, retype and require the entity declares the field, for rename it does not
// declare From, for drop it does not declare the field, a drop with no field names an entity that the declaration
// does not declare, and for unique the field names a declared invariant of kind unique on the entity.
func validateSchemaChangeEntity(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for i, change := range service.SchemaChanges {
		path := schemaChangePath(i)
		fault := func(where, message string) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-entity", Path: path + where, Found: change.Entity + "." + change.Field, Message: fmt.Sprintf("schema change %q on the entity %q and the field %q: %s", change.ID, change.Entity, change.Field, message)})
		}
		entity, declared := findEntity(service.Entities, change.Entity)
		if change.Kind == "drop" && change.Field == "" {
			if declared {
				fault(".entity", "the declaration still declares the entity that the change drops; remove the entity")
			}
			continue
		}
		if !declared {
			if contains(schemaChangeKinds, change.Kind) {
				fault(".entity", "the declaration declares no such entity")
			}
			continue
		}
		_, hasField := findField(entity, change.Field)
		switch change.Kind {
		case "add":
			if change.Field != "" && !hasField {
				fault(".field", "the entity does not declare the field that the change adds")
			}
		case "rename", "retype", "require":
			if !hasField {
				fault(".field", fmt.Sprintf("the entity does not declare the field, which a %s names by its name after the change", change.Kind))
			}
			if _, hasFrom := findField(entity, change.From); change.Kind == "rename" && change.From != "" && hasFrom {
				fault(".from", fmt.Sprintf("the entity still declares %q, the name before the rename; remove it", change.From))
			}
		case "drop":
			if hasField {
				fault(".field", "the entity still declares the field that the change drops; remove the field")
			}
		case "unique":
			found := false
			for _, invariant := range service.Invariants {
				if invariant.Name == change.Field && invariant.Kind == "unique" && invariant.On == change.Entity {
					found = true
				}
			}
			if !found {
				fault(".field", "the field names no declared invariant of kind unique on the entity")
			}
		}
	}
	return faults
}

// schemaPhases gives the phases of each schema change in the order of the schema history.
func schemaPhases(steps []types.SchemaStep) map[string][]string {
	phases := map[string][]string{}
	for _, step := range steps {
		phases[step.Change] = append(phases[step.Change], step.Phase)
	}
	return phases
}

// openSchemaChanges gives the schema changes whose last step is expand, in the order of the declaration.
func openSchemaChanges(service *types.ServiceDeclaration) []types.SchemaChange {
	phases := schemaPhases(service.SchemaHistory)
	var open []types.SchemaChange
	for _, change := range service.SchemaChanges {
		if steps := phases[change.ID]; len(steps) > 0 && steps[len(steps)-1] == "expand" {
			open = append(open, change)
		}
	}
	return open
}

// validateSchemaSteps checks that the schema history moves each change in order (ST-DECL-INV59): each step names a
// declared change, each change has exactly one expand step and at most one contract step after it, a change of kind
// add has no contract step, and no change has no step.
func validateSchemaSteps(service *types.ServiceDeclaration) []ValidationError {
	declared := map[string]string{}
	for _, change := range service.SchemaChanges {
		declared[change.ID] = change.Kind
	}
	var faults []ValidationError
	for i, step := range service.SchemaHistory {
		if _, known := declared[step.Change]; !known {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-step", Path: schemaStepPath(i) + ".change", Found: step.Change, Message: fmt.Sprintf("the schema step for %q names no declared schema change", step.Change)})
		}
		if step.Phase != "expand" && step.Phase != "contract" {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-step", Path: schemaStepPath(i) + ".phase", Found: step.Phase, Allowed: []string{"expand", "contract"}, Message: fmt.Sprintf("the schema step for %q has the phase %q; a phase is expand or contract", step.Change, step.Phase)})
		}
	}
	phases := schemaPhases(service.SchemaHistory)
	for i, change := range service.SchemaChanges {
		path := schemaChangePath(i)
		fault := func(message string) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-step", Path: path, Found: strings.Join(phases[change.ID], ", "), Message: fmt.Sprintf("schema change %q has the steps %q: %s", change.ID, strings.Join(phases[change.ID], ", "), message)})
		}
		steps := phases[change.ID]
		switch {
		case len(steps) == 0:
			fault("it has no step; put its expand step at the end of the schema history")
		case steps[0] != "expand":
			fault("its first step must be expand")
		case len(steps) > 2 || (len(steps) == 2 && steps[1] != "contract"):
			fault("it has exactly one expand step and at most one contract step after it")
		case len(steps) == 2 && change.Kind == "add":
			fault("a change of kind add has an expand step only")
		}
	}
	return faults
}

// validateSchemaChangeOpen checks that at most one open schema change names a given field of an entity, as Field or
// as From (ST-DECL-INV60).
func validateSchemaChangeOpen(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	owner := map[[2]string]string{}
	for _, change := range openSchemaChanges(service) {
		for _, field := range []string{change.Field, change.From} {
			if field == "" || (change.Kind == "retype" && field == change.From) {
				continue
			}
			key := [2]string{change.Entity, field}
			if first, taken := owner[key]; taken && first != change.ID {
				faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-open", Path: "schema_changes", Found: field, Message: fmt.Sprintf("the schema changes %q and %q are both open on the field %s.%s; finish %q with its contract step before %q moves the same column", first, change.ID, change.Entity, field, first, change.ID)})
				continue
			}
			owner[key] = change.ID
		}
	}
	return faults
}

// validateSchemaChangeMode checks that a declaration with an open schema change whose expand step starts a backfill
// runs the controller mode (ST-DECL-INV61). Checking does not add the mode.
func validateSchemaChangeMode(service *types.ServiceDeclaration) []ValidationError {
	if RunsMode(service, "controller") {
		return nil
	}
	var faults []ValidationError
	for _, change := range openSchemaChanges(service) {
		if backfillKinds[change.Kind] {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-mode", Path: "modes", Found: strings.Join(service.Modes, ", "), Allowed: []string{"controller"}, Message: fmt.Sprintf("schema change %q is at expand and its backfill runs only in the mode controller, which the modes %s do not hold; add controller to modes, or leave modes out to run all", change.ID, strings.Join(service.Modes, ", "))})
		}
	}
	return faults
}

// definitionField reads the Definition of a dropped field, as [name:] type[, constraint value]..., such as
// legacy: string, max_length 64, into a field. It gives a reason when the Definition cannot be read.
func definitionField(definition string) (types.Field, string) {
	parts := strings.Split(definition, ",")
	head := strings.TrimSpace(parts[0])
	if _, typ, named := strings.Cut(head, ":"); named {
		head = strings.TrimSpace(typ)
	}
	field := types.Field{Type: types.FieldType(head)}
	if !types.ValidFieldTypes[field.Type] {
		return field, fmt.Sprintf("the type %q is not a field type", head)
	}
	for _, part := range parts[1:] {
		key, value, _ := strings.Cut(strings.TrimSpace(part), " ")
		value = strings.TrimSpace(value)
		number, err := strconv.ParseFloat(value, 64)
		switch key {
		case "min_length", "max_length":
			length, err := strconv.Atoi(value)
			if err != nil {
				return field, fmt.Sprintf("the constraint %s %q is not a whole number", key, value)
			}
			if key == "min_length" {
				field.MinLength = &length
			} else {
				field.MaxLength = &length
			}
		case "min", "max":
			if err != nil {
				return field, fmt.Sprintf("the constraint %s %q is not a number", key, value)
			}
			if key == "min" {
				field.Min = &number
			} else {
				field.Max = &number
			}
		case "pattern":
			field.Pattern = value
		case "default", "optional", "unique":
		default:
			return field, fmt.Sprintf("the constraint %q is not one a field has", key)
		}
	}
	return field, ""
}

// valueSuits tells whether a value suits the type and the constraints of a field, and gives the reason when it
// does not.
func valueSuits(field types.Field, value any) (bool, string) {
	probe := field
	probe.Default = value
	if !defaultSuits(probe) {
		return false, fmt.Sprintf("it does not suit the type %s", field.Type)
	}
	if text, ok := value.(string); ok {
		length := utf8.RuneCountInString(text)
		if field.MinLength != nil && length < *field.MinLength {
			return false, fmt.Sprintf("it is shorter than the min_length %d", *field.MinLength)
		}
		if field.MaxLength != nil && length > *field.MaxLength {
			return false, fmt.Sprintf("it is longer than the max_length %d", *field.MaxLength)
		}
		if field.Pattern != "" {
			if pattern, err := regexp.Compile(field.Pattern); err == nil && !pattern.MatchString(text) {
				return false, fmt.Sprintf("it does not match the pattern %s", field.Pattern)
			}
		}
	}
	var number float64
	switch n := value.(type) {
	case int:
		number = float64(n)
	case int64:
		number = float64(n)
	case uint64:
		number = float64(n)
	case float64:
		number = n
	default:
		return true, ""
	}
	if field.Min != nil && number < *field.Min {
		return false, fmt.Sprintf("it is less than the min %v", *field.Min)
	}
	if field.Max != nil && number > *field.Max {
		return false, fmt.Sprintf("it is more than the max %v", *field.Max)
	}
	return true, ""
}

// validateSchemaChangeValues checks that the values of each schema change suit its field (ST-DECL-INV62): the field
// of a require is required with no default, and a fill value suits the type and the constraints of the field, or
// for a drop those of its Definition. Checking does not hold the conversions of a retype against the database
// component yet, as the registry does not declare them.
func validateSchemaChangeValues(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for i, change := range service.SchemaChanges {
		path := schemaChangePath(i)
		fault := func(where, found, message string) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-value", Path: path + where, Found: found, Message: fmt.Sprintf("schema change %q: %s", change.ID, message)})
		}
		var field types.Field
		switch change.Kind {
		case "require", "retype":
			entity, declared := findEntity(service.Entities, change.Entity)
			found := false
			if declared {
				field, found = findField(entity, change.Field)
			}
			if !found {
				continue
			}
			if change.Kind == "require" && (field.Optional || field.Default != nil) {
				fault(".field", change.Field, fmt.Sprintf("the field %s.%s of a require must be required with no default; a field that is optional or has a default is an add", change.Entity, change.Field))
			}
		case "drop":
			if change.Field == "" || change.Definition == "" {
				continue
			}
			var reason string
			if field, reason = definitionField(change.Definition); reason != "" {
				fault(".definition", change.Definition, fmt.Sprintf("the definition %q cannot be read: %s", change.Definition, reason))
				continue
			}
		default:
			continue
		}
		if change.FillValue == nil {
			continue
		}
		if ok, reason := valueSuits(field, change.FillValue); !ok {
			fault(".fill_value", fmt.Sprint(change.FillValue), fmt.Sprintf("the fill value %v does not suit the field %s: %s", change.FillValue, change.Field, reason))
		}
	}
	return faults
}

// validateSchemaChangeCount checks that at most 127 schema changes are open (ST-DECL-INV64).
func validateSchemaChangeCount(service *types.ServiceDeclaration) []ValidationError {
	open := len(openSchemaChanges(service))
	if open <= maxOpenSchemaChanges {
		return nil
	}
	return []ValidationError{{Category: "schema-change", Rule: "schema-change-count", Path: "schema_history", Found: strconv.Itoa(open), Message: fmt.Sprintf("%d schema changes are open at expand; at most %d may be open, so finish some with their contract steps first", open, maxOpenSchemaChanges)}}
}
