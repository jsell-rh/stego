package declaration

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/types"
)

// knownModes are the modes that the one binary runs (ST-DECL-EN01), in the order of the format.
var knownModes = []string{"api", "worker", "controller"}

// The mixins that depend on each other (ST-DECL-INV28): kafka-events sends what durable-events stores.
const (
	mixinKafkaEvents   = "kafka-events"
	mixinDurableEvents = "durable-events"
)

// controllerSources are the ways a controller learns of work (ST-DECL-EN16).
var controllerSources = []string{"scan", "watch"}

// invariantKinds are the kinds of a declared invariant, each with one emission in the store (ST-DECL-EN17): unique
// emits a unique index, no_overlap an exclusion constraint and sum_limit a constraint trigger.
var invariantKinds = []string{"unique", "no_overlap", "sum_limit"}

// invariantNameForm is the form of the name of a declared invariant. The store names its constraint, index or
// trigger by it, so the declaration holds it to a form that SQL cannot read as code (ST-DECL-INV86).
var invariantNameForm = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// invariantFieldForm is the form of a field that a declared invariant names in by, range or parent.
var invariantFieldForm = fieldNameForm

// whereTerm is one term of the Where of a declared invariant: a field, an operator and a literal, such as
// status = 'active' or done = false. A string literal holds no quote. The generated migration writes the condition
// into a partial index or a constraint, so checking takes only this form and no other SQL.
var whereTerm = regexp.MustCompile(`^([a-z][a-z0-9_]{0,62}) *(=|<>|!=) *(true|false|null|-?[0-9]{1,18}|'[A-Za-z0-9 _.:-]{0,255}')$`)

// ModeUnknown is the specification ST-DECL-SP14: Modes names a mode other than api, worker or controller.
func ModeUnknown(service *types.ServiceDeclaration) bool {
	if service == nil {
		return false
	}
	for _, mode := range service.Modes {
		if !slices.Contains(knownModes, mode) {
			return true
		}
	}
	return false
}

// MixinRequirementUnmet is the specification ST-DECL-SP15: the declaration binds kafka-events without
// durable-events.
func MixinRequirementUnmet(service *types.ServiceDeclaration) bool {
	return service != nil && slices.Contains(service.Mixins, mixinKafkaEvents) && !slices.Contains(service.Mixins, mixinDurableEvents)
}

// InvariantWithoutEmission is the specification ST-DECL-SP16: a declared invariant has a kind other than unique,
// no_overlap or sum_limit, or lacks the fields its kind needs.
func InvariantWithoutEmission(service *types.ServiceDeclaration) bool {
	if service == nil {
		return false
	}
	for _, invariant := range service.Invariants {
		if !slices.Contains(invariantKinds, invariant.Kind) || len(missingInvariantFields(invariant)) > 0 {
			return true
		}
	}
	return false
}

// ControllerWithoutLease is the specification ST-DECL-SP17: a controller has no lease TTL, a source other than scan
// or watch, or tenants other than each.
func ControllerWithoutLease(service *types.ServiceDeclaration) bool {
	if service == nil {
		return false
	}
	for _, controller := range service.Controllers {
		if controller.LeaseTTL == "" || controller.Tenants != "each" {
			return true
		}
		for _, source := range controller.Sources {
			if !slices.Contains(controllerSources, source) {
				return true
			}
		}
	}
	return false
}

// missingInvariantFields gives the fields that the kind of the declared invariant needs and that it leaves out: on
// for every kind, by for unique and no_overlap, range for no_overlap, and parent for sum_limit.
func missingInvariantFields(invariant types.DeclaredInvariant) []string {
	var missing []string
	if invariant.On == "" {
		missing = append(missing, "on")
	}
	switch invariant.Kind {
	case "unique":
		if len(invariant.By) == 0 {
			missing = append(missing, "by")
		}
	case "no_overlap":
		if invariant.Range == "" {
			missing = append(missing, "range")
		}
		if len(invariant.By) == 0 {
			missing = append(missing, "by")
		}
	case "sum_limit":
		if invariant.Parent == "" {
			missing = append(missing, "parent")
		}
	}
	return missing
}

// validateModesAndMixins holds the declaration to ST-DECL-INV28: each mode is api, worker or controller, once, and
// kafka-events is bound together with durable-events. Each fault names the mode or the missing mixin.
func validateModesAndMixins(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	seen := map[string]bool{}
	for i, mode := range service.Modes {
		path := fmt.Sprintf("modes[%d]", i)
		switch {
		case !slices.Contains(knownModes, mode):
			faults = append(faults, ValidationError{Category: "service", Rule: "mode", Path: path, Found: mode, Allowed: knownModes,
				Message: fmt.Sprintf("the mode %q is not a mode of the binary; the modes are api, worker and controller, and leaving modes out runs all three", mode)})
		case seen[mode]:
			faults = append(faults, ValidationError{Category: "service", Rule: "mode", Path: path, Found: mode, Allowed: knownModes,
				Message: fmt.Sprintf("the mode %q is named more than once", mode)})
		}
		seen[mode] = true
	}
	if MixinRequirementUnmet(service) {
		faults = append(faults, ValidationError{Category: "mixin", Rule: "mixin-requirement", Path: fmt.Sprintf("mixins[%d]", slices.Index(service.Mixins, mixinKafkaEvents)), Found: mixinKafkaEvents, Allowed: []string{mixinDurableEvents},
			Message: fmt.Sprintf("the mixin %s needs the mixin %s, which stores the events that %s sends; add %s to mixins", mixinKafkaEvents, mixinDurableEvents, mixinKafkaEvents, mixinDurableEvents)})
	}
	return faults
}

// validateControllers holds the declaration to ST-DECL-INV29: each controller declares its sources from scan and
// watch, a resync and a lease TTL that are durations greater than zero, and at least one replica. A controller that
// declares tenants other than each is refused by ST-DECL-INV20. Each fault names the controller and the field.
func validateControllers(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for i, controller := range service.Controllers {
		path := fmt.Sprintf("controllers[%d]", i)
		fault := func(field, found string, allowed []string, format string, args ...any) {
			faults = append(faults, ValidationError{Category: "service", Rule: "controller", Path: path + "." + field, Found: found, Allowed: allowed,
				Message: fmt.Sprintf("controller %q ", controller.Name) + fmt.Sprintf(format, args...)})
		}
		if len(controller.Sources) == 0 {
			fault("sources", "", controllerSources, "declares no sources; give scan, watch or both")
		}
		for j, source := range controller.Sources {
			if !slices.Contains(controllerSources, source) {
				faults = append(faults, ValidationError{Category: "service", Rule: "controller", Path: fmt.Sprintf("%s.sources[%d]", path, j), Found: source, Allowed: controllerSources,
					Message: fmt.Sprintf("controller %q has the source %q; a source is scan or watch", controller.Name, source)})
			}
		}
		for _, d := range []struct{ field, value, example string }{{"resync", controller.Resync, "10m"}, {"lease_ttl", controller.LeaseTTL, "15s"}} {
			name := strings.ReplaceAll(d.field, "_", " ")
			if d.field == "lease_ttl" {
				name = "lease TTL"
			}
			if d.value == "" {
				fault(d.field, "", nil, "declares no %s; give a duration, such as %s", name, d.example)
			} else if duration, err := time.ParseDuration(d.value); err != nil || duration <= 0 {
				fault(d.field, d.value, nil, "has the %s %q, which is not a duration greater than zero, such as %s", name, d.value, d.example)
			}
		}
		if controller.Replicas < 1 {
			fault("replicas", fmt.Sprint(controller.Replicas), nil, "declares no replicas; give the number of replicas that stand by under the lease, at least 1")
		}
	}
	return faults
}

// validateDeclaredInvariants holds the declaration to ST-DECL-INV30: each declared invariant has a name of the
// form, once, the kind unique, no_overlap or sum_limit, and the fields its kind needs; on names a declared entity,
// range names two fields, parent names entity.field, and where is a condition of the strict form whereTerm over
// fields of the entity. A rule that has no such emission stays a slot binding. Each fault names the invariant and
// the kind or the field.
func validateDeclaredInvariants(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	seen := map[string]bool{}
	for i, invariant := range service.Invariants {
		path := fmt.Sprintf("invariants[%d]", i)
		fault := func(field, found string, allowed []string, format string, args ...any) {
			faults = append(faults, ValidationError{Category: "entity", Rule: "declared-invariant", Path: path + "." + field, Found: found, Allowed: allowed,
				Message: fmt.Sprintf("declared invariant %q ", invariant.Name) + fmt.Sprintf(format, args...)})
		}
		switch {
		case !invariantNameForm.MatchString(invariant.Name):
			fault("name", invariant.Name, []string{invariantNameForm.String()}, "has a name that does not match %s", invariantNameForm)
		case seen[invariant.Name]:
			fault("name", invariant.Name, nil, "is declared more than once; give each invariant its own name")
		}
		seen[invariant.Name] = true
		if !slices.Contains(invariantKinds, invariant.Kind) {
			fault("kind", invariant.Kind, invariantKinds, "has the kind %q, which has no emission in the store; the kind is unique, no_overlap or sum_limit, and any other rule stays a slot binding", invariant.Kind)
			continue
		}
		for _, field := range missingInvariantFields(invariant) {
			fault(field, "", nil, "of the kind %s needs %s", invariant.Kind, field)
		}
		var entity *types.Entity
		if invariant.On != "" {
			name, field, dotted := strings.Cut(invariant.On, ".")
			for ei := range service.Entities {
				if service.Entities[ei].Name == name {
					entity = &service.Entities[ei]
				}
			}
			switch {
			case entity == nil:
				fault("on", invariant.On, entityNameList(service.Entities), "is on %q, which names no declared entity", invariant.On)
			case dotted && !slices.Contains(fieldNameList(*entity), field):
				fault("on", invariant.On, fieldNameList(*entity), "is on %q, and %s has no field %q", invariant.On, name, field)
			}
		}
		for j, field := range invariant.By {
			if !invariantFieldForm.MatchString(field) {
				fault(fmt.Sprintf("by[%d]", j), field, []string{invariantFieldForm.String()}, "is scoped by %q, which is not a field name of the form %s", field, invariantFieldForm)
			}
		}
		if invariant.Range != "" {
			bounds := strings.Split(invariant.Range, ",")
			valid := len(bounds) == 2
			for _, bound := range bounds {
				valid = valid && invariantFieldForm.MatchString(strings.TrimSpace(bound))
			}
			if !valid {
				fault("range", invariant.Range, nil, "has the range %q; a range is the two fields that bound it, such as starts_at, ends_at", invariant.Range)
			}
		}
		if invariant.Parent != "" {
			parent, limit, dotted := strings.Cut(invariant.Parent, ".")
			if !dotted || !entityNameForm.MatchString(parent) && !invariantFieldForm.MatchString(parent) || !invariantFieldForm.MatchString(limit) {
				fault("parent", invariant.Parent, nil, "has the parent %q; the parent is the entity.field that holds the limit, such as customer.credit_limit", invariant.Parent)
			}
		}
		if invariant.Where != "" {
			if field, bad := badWhereTerm(invariant.Where, entity); bad && field == "" {
				fault("where", invariant.Where, nil, "has the condition %q; a condition is field = value terms joined by and, where a value is true, false, null, a whole number or a quoted text with no quote, such as status = 'active'", invariant.Where)
			} else if bad {
				fault("where", invariant.Where, fieldNameList(*entity), "has a condition on %q, which is not a field of %s", field, entity.Name)
			}
		}
	}
	return faults
}

// badWhereTerm finds the first term of a Where that is not of the form whereTerm, and gives no field for it; or the
// first term on a field that the entity does not have, and gives that field. The entity is nil when the invariant
// names no declared entity, and then no field is looked up.
func badWhereTerm(where string, entity *types.Entity) (string, bool) {
	for _, term := range strings.Split(where, " and ") {
		match := whereTerm.FindStringSubmatch(strings.TrimSpace(term))
		if match == nil {
			return "", true
		}
		if entity != nil && !slices.Contains(fieldNameList(*entity), match[1]) {
			return match[1], true
		}
	}
	return "", false
}
