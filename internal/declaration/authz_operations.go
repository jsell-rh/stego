package declaration

import (
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// The authorization of the service's own operations (ST-DECL-UC09): each command, query and worker decides one
// permission of one resource type of the schema, and a command writes only the relations it declares. Each check
// takes the compiled schema, or nil when the schema did not compile; with nil it reports only what is missing.

// operationAuthz is what a command, a query or a worker states for authorization, with its place.
type operationAuthz struct {
	kind, name, path         string
	resourceType, permission string
}

// operationFaults checks that an operation names a resource type and a permission, and that the schema holds both.
func operationFaults(rule string, op operationAuthz, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	missing := func(key, what string) {
		faults = append(faults, ValidationError{Category: "authorization", Rule: rule, Path: op.path + "." + key, Found: op.name,
			Message: fmt.Sprintf("%s %q names no %s; with authorization bound, each %s decides a permission of a resource type of the schema", op.kind, op.name, what, op.kind)})
	}
	if op.resourceType == "" {
		missing("resource_type", "resource type")
	}
	if op.permission == "" {
		missing("permission", "permission")
	}
	if schema == nil {
		return faults
	}
	definition, defined := schema.definitions[op.resourceType]
	switch {
	case op.resourceType != "" && !defined:
		faults = append(faults, ValidationError{Category: "authorization", Rule: rule, Path: op.path + ".resource_type", Found: op.resourceType, Allowed: schema.typeNames(),
			Message: fmt.Sprintf("%s %q names the resource type %q, which the schema %s lacks", op.kind, op.name, op.resourceType, schema.Path)})
	case defined && op.permission != "" && !definition.permissions[op.permission]:
		faults = append(faults, ValidationError{Category: "authorization", Rule: rule, Path: op.path + ".permission", Found: op.permission, Allowed: sortedKeys(definition.permissions),
			Message: fmt.Sprintf("%s %q names the permission %q, which the resource type %s of the schema %s lacks", op.kind, op.name, op.permission, op.resourceType, schema.Path)})
	}
	return faults
}

// validateCommandAuthorization checks that each command names a resource type and a permission that the schema
// holds and a resource member of its request (ST-DECL-INV40).
func validateCommandAuthorization(commands []types.Command, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	for i, command := range commands {
		faults = append(faults, operationFaults("command-authorization", operationAuthz{"command", command.Name, commandPath(i), command.ResourceType, command.Permission}, schema)...)
		if command.ResourceMember == "" {
			faults = append(faults, ValidationError{Category: "authorization", Rule: "command-authorization", Path: commandPath(i) + ".resource_member", Found: command.Name,
				Message: fmt.Sprintf("command %q names no resource member; name the member of its request that holds the ID of the resource the permission is decided on", command.Name)})
		}
	}
	return faults
}

// validateCommandRelations checks that each relation in a command's Relationships written is a relation of the
// schema under the resource type of an entity the command writes (ST-DECL-INV41).
func validateCommandRelations(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	for i, command := range service.Commands {
		written := map[string]bool{}
		for _, name := range command.Writes {
			if _, entity := entityIndex(service.Entities, name); entity != nil && entity.ResourceType != "" {
				written[entity.ResourceType] = true
			}
		}
		for ri, relation := range command.RelationshipsWritten {
			message := writtenRelationUnknown(relation, written, schema, "the command writes")
			if message == "" {
				continue
			}
			faults = append(faults, ValidationError{Category: "authorization", Rule: "command-relations", Path: fmt.Sprintf("%s.relationships_written[%d]", commandPath(i), ri), Found: relation,
				Allowed: sortedKeys(written), Message: fmt.Sprintf("command %q lists %q, %s", command.Name, relation, message)})
		}
	}
	return faults
}

// validateQueryAuthorization checks that each query names a resource type and a permission that the schema holds
// (ST-DECL-INV46). A query with no resource member is a list query, decided per row.
func validateQueryAuthorization(queries []types.Query, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	for i, query := range queries {
		faults = append(faults, operationFaults("query-authorization", operationAuthz{"query", query.Name, queryPath(i), query.ResourceType, query.Permission}, schema)...)
	}
	return faults
}

// validateWorkerAuthorization checks that each worker names a resource type and a permission that the schema holds
// (ST-DECL-INV69).
func validateWorkerAuthorization(workers []types.Worker, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	for i, worker := range workers {
		faults = append(faults, operationFaults("worker-authorization", operationAuthz{"worker", worker.Name, workerPath(i), worker.ResourceType, worker.Permission}, schema)...)
	}
	return faults
}

// operationsAuthorization runs the authorization checks of the commands, queries and workers.
func operationsAuthorization(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	faults := validateCommandAuthorization(service.Commands, schema)
	if schema != nil {
		faults = append(faults, validateCommandRelations(service, schema)...)
	}
	faults = append(faults, validateQueryAuthorization(service.Queries, schema)...)
	faults = append(faults, validateReferences(service.Entities, schema)...)
	return append(faults, validateWorkerAuthorization(service.Workers, schema)...)
}
