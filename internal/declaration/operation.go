package declaration

import (
	"fmt"
	"sort"

	"github.com/jsell-rh/stego/internal/types"
)

// Facts are what checking of a service's composition reads beside the declaration: the slot each fill of the
// project implements and the outbox destinations that the active components declare. The compile check of
// Generating reads them from the project's fills and the active component set, and a test gives them as values.
type Facts struct {
	// FillSlots gives, for each fill that the project holds, the slot it implements as component.slot.
	FillSlots map[string]string
	// Destinations holds the outbox destinations that a component of the active component set declares.
	Destinations map[string]bool
	// GRPCMethods gives the kind of each method, as package.Service/Method, of the services of the proto files of the
	// grpc-application component. It is nil when that component is not active (ST-DECL-INV73).
	GRPCMethods map[string]MethodKind
}

// The slots that the fills of commands, queries and workers implement. Each capability component (ST-SUP-RC32,
// ST-SUP-RC33, ST-SUP-RC35) is named for its one slot.
const (
	CommandSlot = "command.command"
	QuerySlot   = "query.query"
	WorkerSlot  = "worker.worker"
)

// ValidateComposition runs the checks of a service's composition (ST-DECL-UC09): its commands, queries, workers, schema changes and observations. It reads the
// declaration and the facts, and it returns every fault it finds.
func ValidateComposition(service *types.ServiceDeclaration, facts Facts) []ValidationError {
	var faults []ValidationError
	faults = append(faults, validateCommandNames(service.Commands)...)
	faults = append(faults, validateCommandWrites(service.Commands, service.Entities)...)
	faults = append(faults, validateCommandFill(service.Commands, facts)...)
	faults = append(faults, validateCommandEffects(service, stagingFacts(service.Workers, facts))...)
	faults = append(faults, validateQueryNames(service.Queries, service.Commands)...)
	faults = append(faults, validateQueryReads(service.Queries, service.Entities)...)
	faults = append(faults, validateQueryFill(service.Queries, facts)...)
	faults = append(faults, validateGRPCMethods(service, facts)...)
	faults = append(faults, validateOperationRoutes(service)...)
	faults = append(faults, validateOperationRefusals(service)...)
	faults = append(faults, validateCommandRetry(service.Commands)...)
	faults = append(faults, validateWorkerNames(service)...)
	faults = append(faults, validateWorkerTrigger(service.Workers, facts)...)
	faults = append(faults, validateWorkerWrites(service.Workers, service.Entities)...)
	faults = append(faults, validateWorkerFill(service.Workers, facts)...)
	faults = append(faults, validateWorkerEffects(service, facts)...)
	faults = append(faults, validateWorkerMode(service)...)
	faults = append(faults, validateWorkerLease(service.Workers)...)
	faults = append(faults, validateWorkerCycles(service.Workers)...)
	faults = append(faults, validateWorkerCount(service.Workers)...)
	faults = append(faults, validateSchemaChangeKind(service.SchemaChanges)...)
	faults = append(faults, validateSchemaChangeEntity(service)...)
	faults = append(faults, validateSchemaSteps(service)...)
	faults = append(faults, validateSchemaChangeOpen(service)...)
	faults = append(faults, validateSchemaChangeMode(service)...)
	faults = append(faults, validateSchemaChangeValues(service)...)
	faults = append(faults, validateSchemaChangeCount(service)...)
	faults = append(faults, validateObservationNames(service.Observations)...)
	faults = append(faults, validateObservationKind(service.Observations)...)
	faults = append(faults, validateObservationValue(service.Observations)...)
	faults = append(faults, validateObservationFields(service.Observations, service.Entities)...)
	faults = append(faults, validateObservationPrivacy(service.Observations, service.Entities)...)
	faults = append(faults, validateObservationSeries(service.Observations, service.Entities)...)
	faults = append(faults, validateObservationEmitters(service)...)
	faults = append(faults, validateObservationCounts(service.Observations)...)
	return faults
}

// fillFault checks that an operation names exactly one fill and that the project holds that fill at the slot the
// operation needs. kind is command, query or worker, and path is the operation's place in the declaration.
func fillFault(kind, name, path string, fills types.FillNames, slot string, facts Facts) []ValidationError {
	rule := kind + "-fill"
	switch len(fills) {
	case 0:
		return []ValidationError{{Category: kind, Rule: rule, Path: path + ".fill", Message: fmt.Sprintf("%s %q names no fill; name the one fill of the slot %s that carries it out", kind, name, slot)}}
	case 1:
	default:
		return []ValidationError{{Category: kind, Rule: rule, Path: path + ".fill", Found: fmt.Sprint([]string(fills)), Message: fmt.Sprintf("%s %q names %d fills %q; name exactly one fill of the slot %s", kind, name, len(fills), []string(fills), slot)}}
	}
	fill := fills[0]
	implements, held := facts.FillSlots[fill]
	if !held {
		return []ValidationError{{Category: kind, Rule: rule, Path: path + ".fill", Found: fill, Message: fmt.Sprintf("%s %q names the fill %q, but fills/%s/fill.yaml does not exist; write the fill of the slot %s there", kind, name, fill, fill, slot)}}
	}
	if implements != slot {
		return []ValidationError{{Category: kind, Rule: rule, Path: path + ".fill", Found: fill, Allowed: []string{slot}, Message: fmt.Sprintf("%s %q names the fill %q, which implements %s; its fill must implement %s", kind, name, fill, implements, slot)}}
	}
	return nil
}

// entityFaults checks that an operation names at least one entity and only entities that the declaration declares.
// field is the operation's key that names them, such as writes.
func entityFaults(kind, name, path, field string, names []string, entities []types.Entity) []ValidationError {
	rule := kind + "-" + field
	if len(names) == 0 {
		return []ValidationError{{Category: kind, Rule: rule, Path: path + "." + field, Allowed: entityNameList(entities), Message: fmt.Sprintf("%s %q names no entity in %s; name at least one declared entity", kind, name, field)}}
	}
	declared := map[string]bool{}
	for _, entity := range entities {
		declared[entity.Name] = true
	}
	var faults []ValidationError
	for i, entity := range names {
		if !declared[entity] {
			faults = append(faults, ValidationError{Category: kind, Rule: rule, Path: fmt.Sprintf("%s.%s[%d]", path, field, i), Found: entity, Allowed: entityNameList(entities), Message: fmt.Sprintf("%s %q names the entity %q in %s, which the declaration does not declare", kind, name, entity, field)})
		}
	}
	return faults
}

// sortedNames gives the keys of a set in order.
func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
