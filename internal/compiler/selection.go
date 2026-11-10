package compiler

import (
	"fmt"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// selection is one infrastructure component that a declared setting implies (ST-GEN-INV37), with the setting that
// selects it.
type selection struct {
	Component string
	Setting   string
}

// The infrastructure components that the declared settings select (ST-GEN-INV37).
const (
	auditComponent       = "audit-log"
	idempotencyComponent = "idempotency"
	ledgerComponent      = "ledger"
)

// selectComponents gives the infrastructure components that the declared settings of svc imply, in a fixed order:
// a stated tenancy that is not single selects tenancy, a stated audit that is on selects audit-log, a stated
// authorization selects spicedb-authz, a stated idempotency selects idempotency, and an entity of kind ledger
// selects ledger (ST-GEN-INV37). A setting that the declaration omits selects nothing, so a declaration that relies
// on the defaults keeps the components of its archetype and mixins.
func selectComponents(svc *types.ServiceDeclaration) []selection {
	var selected []selection
	if t := svc.Tenancy; t != nil && t.Kind != "single" {
		selected = append(selected, selection{Component: gen.TenancyComponent, Setting: "tenancy"})
	}
	if a := svc.Audit; a != nil && (a.Enabled == nil || *a.Enabled) {
		selected = append(selected, selection{Component: auditComponent, Setting: "audit"})
	}
	if svc.Authorization != nil {
		selected = append(selected, selection{Component: authorizationComponent, Setting: "authorization"})
	}
	if svc.Idempotency != nil {
		selected = append(selected, selection{Component: idempotencyComponent, Setting: "idempotency"})
	}
	for _, entity := range svc.Entities {
		if entity.Kind == "ledger" {
			selected = append(selected, selection{Component: ledgerComponent, Setting: "entities." + entity.Name + ".kind"})
			break
		}
	}
	return selected
}

// acceptanceClock is the clock that the expiry of an acceptance reads; tests set a fixed time.
var acceptanceClock = time.Now

// acceptanceDate is the form of the date of an acceptance (ST-DECL-EN07).
const acceptanceDate = "2006-01-02"

// validateSelectedComponents enforces ST-GEN-INV37 on the components that the declared settings select and the
// registry holds: one that is not ga needs an experimental acceptance (ST-DECL-EN07) that names it and has not run
// out, which it holds to the end of its date in UTC. Each fault names the component, its stage, the setting that
// selects it and the acceptance to add. A selected component that the registry does not hold is a refusal from
// outside the declaration, at generation (selectedComponentMissing).
func validateSelectedComponents(svc *types.ServiceDeclaration, reg *registry.Registry) []ValidationError {
	today := acceptanceClock().UTC().Format(acceptanceDate)
	var faults []ValidationError
	for _, s := range selectComponents(svc) {
		component := reg.Component(s.Component)
		if component == nil || registry.FitForGoldenPath(component) {
			continue
		}
		add := fmt.Sprintf("add an experimental acceptance: experimental_acceptances: [{component: %s, accepted_until: <a date no later than %s>, reason: <why the service accepts it>}]", s.Component, component.Sunset)
		var accepted *types.ExperimentalAcceptance
		for i := range svc.ExperimentalAcceptances {
			if svc.ExperimentalAcceptances[i].Component == s.Component {
				accepted = &svc.ExperimentalAcceptances[i]
			}
		}
		switch {
		case accepted == nil:
			faults = append(faults, ValidationError{
				Category: "component", Rule: "experimental-acceptance", Path: s.Setting, Found: s.Component,
				Message: fmt.Sprintf("the %s setting selects the component %s, which is %s; %s", s.Setting, s.Component, component.Stage, add),
			})
		case !validDate(accepted.AcceptedUntil) || accepted.AcceptedUntil < today:
			faults = append(faults, ValidationError{
				Category: "component", Rule: "experimental-acceptance", Path: "experimental_acceptances", Found: accepted.AcceptedUntil,
				Message: fmt.Sprintf("the %s setting selects the component %s, which is %s, and its experimental acceptance until %q has run out or is not a date; %s", s.Setting, s.Component, component.Stage, accepted.AcceptedUntil, add),
			})
		}
	}
	return faults
}

func validDate(text string) bool {
	_, err := time.Parse(acceptanceDate, text)
	return err == nil
}

// selectedComponentMissing refuses generation of a declaration whose settings select a component that the registry
// does not hold (ST-GEN-INV37, ST-DECL-UC03-R03): the refusal is of the registry, not of the declaration, so
// checking accepts the declaration.
func selectedComponentMissing(svc *types.ServiceDeclaration, reg *registry.Registry) error {
	for _, s := range selectComponents(svc) {
		if reg.Component(s.Component) == nil {
			return fmt.Errorf("generation refused: the %s setting selects the component %s, which the registry does not hold; generation runs once the registry holds %s", s.Setting, s.Component, s.Component)
		}
	}
	return nil
}
