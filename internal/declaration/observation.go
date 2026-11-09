package declaration

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

func observationPath(i int) string { return fmt.Sprintf("observations[%d]", i) }

var (
	observationName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	snakeName       = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	measureType     = regexp.MustCompile(`^measure\(\s*([^,\s]*)\s*(?:,\s*(-?[0-9]+)\.\.(-?[0-9]+)\s*)?\)$`)
	enumType        = regexp.MustCompile(`^enum\((.*)\)$`)
	entityFieldType = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*)\.([a-z][a-z0-9_]*)$`)
	// observationUnits are the units that a measure and a histogram value may have.
	observationUnits = []string{"s", "ms", "By", "1"}
	// reservedObservationFields are the attributes that the telemetry runtime sets on each signal.
	reservedObservationFields = map[string]bool{"value": true, "trace_id": true, "span_id": true, "tenant": true, "principal": true, "operation": true}
)

const (
	maxObservationFields = 8
	maxEnumValues        = 32
	// maxMetricSeries is the most series that a metric observation may make: the limit of 128 series for each
	// instrument (ST-SUP-RC17-OC17) counts the overflow series.
	maxMetricSeries = 127
)

func isUnit(unit string) bool {
	for _, known := range observationUnits {
		if unit == known {
			return true
		}
	}
	return false
}

// validateObservationNames checks that each observation's name is in lower snake case of at most 63 characters and
// is unique in the declaration (ST-DECL-INV48).
func validateObservationNames(observations []types.Observation) []ValidationError {
	var faults []ValidationError
	seen := map[string]bool{}
	for i, observation := range observations {
		switch {
		case !observationName.MatchString(observation.Name):
			faults = append(faults, ValidationError{Category: "observation", Rule: "observation-name", Path: observationPath(i) + ".name", Found: observation.Name, Message: fmt.Sprintf("the observation name %q must match %s: lower snake case, at most 63 characters", observation.Name, observationName)})
		case seen[observation.Name]:
			faults = append(faults, ValidationError{Category: "observation", Rule: "observation-name", Path: observationPath(i) + ".name", Found: observation.Name, Message: fmt.Sprintf("the observation name %q is used twice; give each observation its own name", observation.Name)})
		}
		seen[observation.Name] = true
	}
	return faults
}

// validateObservationKind checks that each observation's kind is event, counter or histogram and its severity is
// debug, info, warn or error (ST-DECL-INV49).
func validateObservationKind(observations []types.Observation) []ValidationError {
	kinds := []string{"event", "counter", "histogram"}
	severities := []string{"debug", "info", "warn", "error"}
	var faults []ValidationError
	for i, observation := range observations {
		if !contains(kinds, observation.Kind) {
			faults = append(faults, ValidationError{Category: "observation", Rule: "observation-kind", Path: observationPath(i) + ".kind", Found: observation.Kind, Allowed: kinds, Message: fmt.Sprintf("observation %q has the kind %q; its kind must be event, counter or histogram", observation.Name, observation.Kind)})
		}
		if !contains(severities, observation.Severity) {
			faults = append(faults, ValidationError{Category: "observation", Rule: "observation-severity", Path: observationPath(i) + ".severity", Found: observation.Severity, Allowed: severities, Message: fmt.Sprintf("observation %q has the severity %q; its severity must be debug, info, warn or error", observation.Name, observation.Severity)})
		}
	}
	return faults
}

// validateObservationValue checks that a histogram names exactly one value, as name: unit with a name in lower
// snake case and a known unit, and that an event or a counter names no value (ST-DECL-INV50).
func validateObservationValue(observations []types.Observation) []ValidationError {
	var faults []ValidationError
	for i, observation := range observations {
		fault := func(message string) {
			faults = append(faults, ValidationError{Category: "observation", Rule: "observation-value", Path: observationPath(i) + ".value", Found: observation.Value, Message: fmt.Sprintf("observation %q has the value %q: %s", observation.Name, observation.Value, message)})
		}
		if observation.Kind != "histogram" {
			if observation.Value != "" {
				fault("only a histogram records a value; remove it")
			}
			continue
		}
		if observation.Value == "" {
			fault("a histogram must record one value, as name: unit")
			continue
		}
		if strings.Contains(observation.Value, ",") {
			fault("a histogram records exactly one value; name one")
			continue
		}
		name, unit, found := strings.Cut(observation.Value, ":")
		name, unit = strings.TrimSpace(name), strings.TrimSpace(unit)
		if !found || !snakeName.MatchString(name) || !isUnit(unit) {
			fault(fmt.Sprintf("a value is name: unit, with a name in lower snake case and a unit of %s", strings.Join(observationUnits, ", ")))
		}
	}
	return faults
}

// observationFieldType is one field type of an observation, as checking reads it.
type observationFieldType struct {
	// bounded tells if the field is an enum or a bool, and values is the number of values it can take.
	bounded bool
	values  int
	// entity and field name the entity field that the probe method reads, for a field of type Entity.field.
	entity, field string
}

// parseObservationType reads one field type of an observation: Entity.field of a declared entity, enum(a | b),
// bool, or measure(unit, min..max). It gives a reason when the type is none of these.
func parseObservationType(text string, entities []types.Entity) (observationFieldType, string) {
	text = strings.TrimSpace(text)
	switch {
	case text == "bool":
		return observationFieldType{bounded: true, values: 2}, ""
	case enumType.MatchString(text):
		var values []string
		seen := map[string]bool{}
		for _, value := range strings.Split(enumType.FindStringSubmatch(text)[1], "|") {
			value = strings.TrimSpace(value)
			if !snakeName.MatchString(value) || seen[value] {
				return observationFieldType{}, fmt.Sprintf("an enum has 1 to %d distinct values in lower snake case", maxEnumValues)
			}
			seen[value] = true
			values = append(values, value)
		}
		if len(values) > maxEnumValues {
			return observationFieldType{}, fmt.Sprintf("an enum has 1 to %d distinct values in lower snake case, not %d", maxEnumValues, len(values))
		}
		return observationFieldType{bounded: true, values: len(values)}, ""
	case strings.HasPrefix(text, "measure("):
		match := measureType.FindStringSubmatch(text)
		if match == nil || !isUnit(match[1]) || match[2] == "" {
			return observationFieldType{}, fmt.Sprintf("a measure is measure(unit, min..max), with a unit of %s and a range of integers", strings.Join(observationUnits, ", "))
		}
		low, errLow := strconv.ParseInt(match[2], 10, 64)
		high, errHigh := strconv.ParseInt(match[3], 10, 64)
		if errLow != nil || errHigh != nil || low > high {
			return observationFieldType{}, "the range of a measure is min..max of integers, with min not greater than max"
		}
		return observationFieldType{}, ""
	case entityFieldType.MatchString(text):
		match := entityFieldType.FindStringSubmatch(text)
		for _, entity := range entities {
			if entity.Name != match[1] {
				continue
			}
			for _, field := range entity.Fields {
				if field.Name == match[2] {
					return observationFieldType{entity: entity.Name, field: field.Name}, ""
				}
			}
			return observationFieldType{}, fmt.Sprintf("the entity %s declares no field %s", entity.Name, match[2])
		}
		return observationFieldType{}, fmt.Sprintf("the declaration declares no entity %s", match[1])
	}
	return observationFieldType{}, "a field type is Entity.field, enum(a | b), bool or measure(unit, min..max); free text and a plain int are not types"
}

// validateObservationFields checks the fields of each observation (ST-DECL-INV51): at most eight, each with a name
// in lower snake case that is unique in the observation and not reserved, and with a known type.
func validateObservationFields(observations []types.Observation, entities []types.Entity) []ValidationError {
	var faults []ValidationError
	for i, observation := range observations {
		if len(observation.Fields) > maxObservationFields {
			faults = append(faults, ValidationError{Category: "observation", Rule: "observation-field", Path: observationPath(i) + ".fields", Found: strconv.Itoa(len(observation.Fields)), Message: fmt.Sprintf("observation %q has %d fields; it may have at most %d", observation.Name, len(observation.Fields), maxObservationFields)})
		}
		seen := map[string]bool{}
		for j, field := range observation.Fields {
			at := fmt.Sprintf("%s.fields[%d]", observationPath(i), j)
			fault := func(where, found, message string) {
				faults = append(faults, ValidationError{Category: "observation", Rule: "observation-field", Path: at + where, Found: found, Message: fmt.Sprintf("observation %q has the field %q: %s", observation.Name, field.Name, message)})
			}
			switch {
			case !snakeName.MatchString(field.Name):
				fault(".name", field.Name, "its name must be in lower snake case")
			case reservedObservationFields[field.Name]:
				fault(".name", field.Name, "its name is one that the telemetry runtime sets on each signal; choose another name")
			case seen[field.Name]:
				fault(".name", field.Name, "its name is used twice in the observation")
			}
			seen[field.Name] = true
			if _, reason := parseObservationType(field.Type, entities); reason != "" {
				fault(".type", field.Type, fmt.Sprintf("its type %q is not known: %s", field.Type, reason))
			}
		}
	}
	return faults
}

// privateSensitivities are the sensitivities of entity fields whose values no observation may carry.
var privateSensitivities = map[string]bool{"pii": true, "secret": true}

// validateObservationPrivacy checks that no field of an observation reads an entity field whose sensitivity is pii
// or secret (ST-DECL-INV52), because the probe method reads that field from the loaded entity value and sends it.
func validateObservationPrivacy(observations []types.Observation, entities []types.Entity) []ValidationError {
	sensitivity := map[[2]string]string{}
	for _, entity := range entities {
		for _, field := range entity.Fields {
			sensitivity[[2]string{entity.Name, field.Name}] = field.Sensitivity
		}
	}
	var faults []ValidationError
	for i, observation := range observations {
		for j, field := range observation.Fields {
			typ, reason := parseObservationType(field.Type, entities)
			if reason != "" || typ.entity == "" {
				continue
			}
			if level := sensitivity[[2]string{typ.entity, typ.field}]; privateSensitivities[level] {
				faults = append(faults, ValidationError{Category: "observation", Rule: "observation-private-field", Path: fmt.Sprintf("%s.fields[%d].type", observationPath(i), j), Found: field.Type, Message: fmt.Sprintf("observation %q has the field %q, which reads the entity field %s.%s of sensitivity %s; an observation may not send a %s value, so remove the field or read another entity field", observation.Name, field.Name, typ.entity, typ.field, level, level)})
			}
		}
	}
	return faults
}

// validateObservationSeries checks that each field of a counter or a histogram is an enum or a bool, and that the
// number of value combinations of its fields, times two when it counts each attempt, times the number of its
// emitters, is at most 127 (ST-DECL-INV53).
func validateObservationSeries(observations []types.Observation, entities []types.Entity) []ValidationError {
	var faults []ValidationError
	for i, observation := range observations {
		if observation.Kind != "counter" && observation.Kind != "histogram" {
			continue
		}
		series, bounded := 1, true
		for j, field := range observation.Fields {
			typ, reason := parseObservationType(field.Type, entities)
			if reason != "" {
				bounded = false
				continue
			}
			if !typ.bounded {
				bounded = false
				faults = append(faults, ValidationError{Category: "observation", Rule: "observation-series", Path: fmt.Sprintf("%s.fields[%d].type", observationPath(i), j), Found: field.Type, Allowed: []string{"enum(a | b)", "bool"}, Message: fmt.Sprintf("the %s %q has the field %q of type %q; each field of a counter or a histogram must be an enum or a bool, so that its series stay bounded", observation.Kind, observation.Name, field.Name, field.Type)})
				continue
			}
			series *= typ.values
		}
		if !bounded {
			continue
		}
		if observation.Counts != "committed" {
			series *= 2
		}
		if len(observation.EmittedBy) > 0 {
			series *= len(observation.EmittedBy)
		}
		if series > maxMetricSeries {
			faults = append(faults, ValidationError{Category: "observation", Rule: "observation-series", Path: observationPath(i) + ".fields", Found: strconv.Itoa(series), Message: fmt.Sprintf("the %s %q makes %d series (the value combinations of its fields, times 2 when it counts each attempt, times its %d emitters); it may make at most %d", observation.Kind, observation.Name, series, max(len(observation.EmittedBy), 1), maxMetricSeries)})
		}
	}
	return faults
}

// validateObservationEmitters checks that each observation names at least one emitter, and that each emitter is a
// command or a query of the declaration, or a slot binding as collection.slot, named once (ST-DECL-INV54).
func validateObservationEmitters(service *types.ServiceDeclaration) []ValidationError {
	declared := map[string]bool{}
	for _, command := range service.Commands {
		declared[command.Name] = true
	}
	for _, query := range service.Queries {
		declared[query.Name] = true
	}
	for _, binding := range service.Slots {
		declared[binding.Collection+"."+binding.Slot] = true
	}
	var faults []ValidationError
	for i, observation := range service.Observations {
		if len(observation.EmittedBy) == 0 {
			faults = append(faults, ValidationError{Category: "observation", Rule: "observation-emitter", Path: observationPath(i) + ".emitted_by", Allowed: sortedNames(declared), Message: fmt.Sprintf("observation %q names no emitter; name the commands, queries or slot bindings (as collection.slot) whose fills emit it", observation.Name)})
			continue
		}
		seen := map[string]bool{}
		for j, emitter := range observation.EmittedBy {
			at := fmt.Sprintf("%s.emitted_by[%d]", observationPath(i), j)
			switch {
			case !declared[emitter]:
				faults = append(faults, ValidationError{Category: "observation", Rule: "observation-emitter", Path: at, Found: emitter, Allowed: sortedNames(declared), Message: fmt.Sprintf("observation %q names the emitter %q, which the declaration does not declare", observation.Name, emitter)})
			case seen[emitter]:
				faults = append(faults, ValidationError{Category: "observation", Rule: "observation-emitter", Path: at, Found: emitter, Message: fmt.Sprintf("observation %q names the emitter %q twice; name each emitter once", observation.Name, emitter)})
			}
			seen[emitter] = true
		}
	}
	return faults
}

// validateObservationCounts checks that each observation's Counts is attempt or committed, attempt when it is
// omitted, and that an observation with an outcome measured states it (ST-DECL-INV55).
func validateObservationCounts(observations []types.Observation) []ValidationError {
	var faults []ValidationError
	for i, observation := range observations {
		switch {
		case observation.Counts != "" && observation.Counts != "attempt" && observation.Counts != "committed":
			faults = append(faults, ValidationError{Category: "observation", Rule: "observation-counts", Path: observationPath(i) + ".counts", Found: observation.Counts, Allowed: []string{"attempt", "committed"}, Message: fmt.Sprintf("observation %q has the counts %q; it must be attempt or committed", observation.Name, observation.Counts)})
		case observation.Counts == "" && observation.OutcomeMeasured != "":
			faults = append(faults, ValidationError{Category: "observation", Rule: "observation-counts", Path: observationPath(i) + ".counts", Allowed: []string{"attempt", "committed"}, Message: fmt.Sprintf("observation %q measures the outcome %s and states no counts; state attempt or committed, so the measure counts what it means to", observation.Name, observation.OutcomeMeasured)})
		}
	}
	return faults
}

// ObservationCounts gives when an emission of the observation is sent: its Counts, or attempt when it is omitted.
func ObservationCounts(observation types.Observation) string {
	if observation.Counts == "" {
		return "attempt"
	}
	return observation.Counts
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
