package declaration

import (
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// The defaults of every ga archetype that a declaration may leave only with a reason (ST-DECL-INV18).
const (
	tenancyPooled = "pooled"
	tenancySingle = "single"
	auditChange   = "change"
)

// DefaultLeftWithoutReason is satisfied by a declaration that says tenancy kind single or audit off and gives no
// reason (ST-DECL-SP01).
func DefaultLeftWithoutReason(service *types.ServiceDeclaration) bool {
	return len(defaultsLeftWithoutReason(service)) > 0
}

// validateDefaultsLeft enforces ST-DECL-INV18: a declaration that omits tenancy gets pooled tenancy and one that
// omits audit gets audit at level change, which the settings report gives back (FilledSettings); a declaration
// that says tenancy kind single or audit off must give a reason. It also refuses a tenancy kind other than single
// or pooled, because no default can be filled in for it.
func validateDefaultsLeft(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	if tenancy := service.Tenancy; tenancy != nil && tenancy.Kind != "" && tenancy.Kind != tenancyPooled && tenancy.Kind != tenancySingle {
		faults = append(faults, ValidationError{
			Category: "service", Rule: "tenancy-kind", Path: "tenancy.kind", Found: tenancy.Kind, Allowed: []string{tenancySingle, tenancyPooled},
			Message: fmt.Sprintf("tenancy kind %q is not single or pooled", tenancy.Kind),
		})
	}
	return append(faults, defaultsLeftWithoutReason(service)...)
}

// defaultsLeftWithoutReason gives one fault for each default that the declaration leaves with no reason.
func defaultsLeftWithoutReason(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	if tenancy := service.Tenancy; tenancy != nil && tenancy.Kind == tenancySingle && blank(tenancy.Reason) {
		faults = append(faults, ValidationError{
			Category: "service", Rule: "default-reason", Path: "tenancy.reason", Found: "tenancy kind single",
			Message: "tenancy kind single leaves the default pooled tenancy, so the tenancy setting must give a reason",
		})
	}
	if audit := service.Audit; audit != nil && audit.Enabled != nil && !*audit.Enabled && blank(audit.Reason) {
		faults = append(faults, ValidationError{
			Category: "service", Rule: "default-reason", Path: "audit.reason", Found: "audit off",
			Message: "audit off leaves the default audit at level change, so the audit setting must give a reason",
		})
	}
	return faults
}
