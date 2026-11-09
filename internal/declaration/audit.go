package declaration

import (
	"fmt"
	"sort"

	"github.com/jsell-rh/stego/internal/types"
)

// auditLevels are the audit levels (ST-DECL-EN10), and sensitivities the field sensitivities (ST-DECL-EN03).
var (
	auditLevels   = []string{"metadata", auditChange}
	sensitivities = []string{"public", "pii", "secret"}
	auditPII      = []string{"redact", "keep"}
)

// AuditValueUnknown is satisfied by a declaration whose audit level is not metadata or change, whose audit sink is
// not a port binding of the declaration, or that has a field whose sensitivity is not public, pii or secret
// (ST-DECL-SP04).
func AuditValueUnknown(service *types.ServiceDeclaration) bool {
	for _, fault := range validateAuditValues(service) {
		switch fault.Rule {
		case "audit-level", "audit-sink", "field-sensitivity":
			return true
		}
	}
	return false
}

// validateAuditValues enforces ST-DECL-INV21: an audit level is metadata or change, an audit sink is a port
// binding of the declaration, a field sensitivity is public, pii or secret, and the audit annotations of a
// binding are a string map. It also refuses an audit pii setting other than redact or keep.
func validateAuditValues(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	if audit := service.Audit; audit != nil {
		if audit.Level != "" && !contains(auditLevels, audit.Level) {
			faults = append(faults, ValidationError{
				Category: "service", Rule: "audit-level", Path: "audit.level", Found: audit.Level, Allowed: auditLevels,
				Message: fmt.Sprintf("audit level %q is not metadata or change", audit.Level),
			})
		}
		if audit.Sink != "" {
			bound := portBindings(service)
			if !contains(bound, audit.Sink) {
				faults = append(faults, ValidationError{
					Category: "service", Rule: "audit-sink", Path: "audit.sink", Found: audit.Sink, Allowed: bound,
					Message: fmt.Sprintf("audit sink %q is not a port that the declaration binds", audit.Sink),
				})
			}
		}
		if audit.PII != "" && !contains(auditPII, audit.PII) {
			faults = append(faults, ValidationError{
				Category: "service", Rule: "audit-pii", Path: "audit.pii", Found: audit.PII, Allowed: auditPII,
				Message: fmt.Sprintf("audit pii %q is not redact or keep", audit.PII),
			})
		}
	}
	for i, entity := range service.Entities {
		for j, field := range entity.Fields {
			if field.Sensitivity != "" && !contains(sensitivities, field.Sensitivity) {
				faults = append(faults, ValidationError{
					Category: "field-type", Rule: "field-sensitivity", Path: fieldPath(i, j) + ".sensitivity", Found: field.Sensitivity, Allowed: sensitivities,
					Message: fmt.Sprintf("field %q of entity %q has the sensitivity %q, which is not public, pii or secret", field.Name, entity.Name, field.Sensitivity),
				})
			}
		}
	}
	for i, slot := range service.Slots {
		faults = append(faults, annotationFaults(slotPath(i)+".audit_annotations", slot)...)
	}
	return faults
}

// annotationFaults refuses audit annotations that are not a map of text to text.
func annotationFaults(path string, slot types.SlotDeclaration) []ValidationError {
	if slot.AuditAnnotations == nil {
		return nil
	}
	annotations, ok := slot.AuditAnnotations.(map[string]any)
	if !ok {
		return []ValidationError{{
			Category: "slot-binding", Rule: "audit-annotations", Path: path, Found: fmt.Sprint(slot.AuditAnnotations),
			Message: fmt.Sprintf("the audit annotations of the binding on %q at %s must be a map of text to text", slot.Collection, slot.Slot),
		}}
	}
	keys := make([]string, 0, len(annotations))
	for key := range annotations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var faults []ValidationError
	for _, key := range keys {
		if _, text := annotations[key].(string); text {
			continue
		}
		faults = append(faults, ValidationError{
			Category: "slot-binding", Rule: "audit-annotations", Path: path + "." + key, Found: fmt.Sprint(annotations[key]),
			Message: fmt.Sprintf("the audit annotation %q of the binding on %q at %s must be text", key, slot.Collection, slot.Slot),
		})
	}
	return faults
}

// portBindings gives the ports that the declaration binds, in order: the keys of its overrides that bind a port to
// a component.
func portBindings(service *types.ServiceDeclaration) []string {
	var ports []string
	for key, value := range service.Overrides {
		if _, component := value.(string); component && !IsConventionOverrideKey(key) {
			ports = append(ports, key)
		}
	}
	sort.Strings(ports)
	return ports
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
