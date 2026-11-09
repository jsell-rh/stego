package declaration

import (
	"fmt"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

// validateAuthorizationCoverage checks that authorization fails closed (ST-DECL-INV31): when the declaration binds
// authorization, each entity a collection reads declares a resource type, and each operation of each collection
// names a permission or public with a reason. Each entity and each operation left out is its own fault, so no
// operation is unprotected by omission.
func validateAuthorizationCoverage(service *types.ServiceDeclaration) []ValidationError {
	if service.Authorization == nil {
		return nil
	}
	var faults []ValidationError
	reported := map[string]bool{}
	for _, collection := range service.Collections {
		ei, entity := findEntity(service.Entities, collection.Entity)
		if entity == nil {
			continue // The collection check reports an entity the declaration does not declare.
		}
		if entity.ResourceType == "" {
			if !reported[entity.Name] {
				reported[entity.Name] = true
				faults = append(faults, ValidationError{Category: "authorization", Rule: "authorization-coverage", Path: fmt.Sprintf("entities[%d].resource_type", ei), Found: entity.Name,
					Message: fmt.Sprintf("entity %q, which the collection %q reads, declares no resource type; with authorization bound, give it the type of the schema that its rows are checked as", entity.Name, collection.Name)})
			}
			continue
		}
		for _, operation := range collection.Operations {
			place := fmt.Sprintf("entities[%d].permissions.%s", ei, operation)
			permission, named := entity.Permissions[operation]
			switch {
			case !named || permission == "":
				faults = append(faults, ValidationError{Category: "authorization", Rule: "authorization-coverage", Path: fmt.Sprintf("entities[%d].permissions", ei), Found: collection.Name + "." + string(operation),
					Message: fmt.Sprintf("the collection %q allows the operation %q, but entity %q names no permission for it; name a permission of the resource type %s, or public with a reason", collection.Name, operation, entity.Name, entity.ResourceType)})
			case permission == types.PermissionPublic && strings.TrimSpace(entity.PublicReasons[operation]) == "":
				faults = append(faults, ValidationError{Category: "authorization", Rule: "authorization-coverage", Path: place, Found: collection.Name + "." + string(operation),
					Message: fmt.Sprintf("the collection %q declares the operation %q public with no reason; give the reason in public_reasons.%s of entity %q", collection.Name, operation, operation, entity.Name)})
			}
		}
	}
	return faults
}

// operationUnprotected is the predicate of ST-DECL-SP18: the declaration binds an authorization provider, and a
// collection reads an entity with no resource type or allows an operation that names neither a permission nor
// public with a reason.
func operationUnprotected(service *types.ServiceDeclaration) bool {
	return len(validateAuthorizationCoverage(service)) > 0
}

// findEntity gives the index and the entity of the name, or nil.
func findEntity(entities []types.Entity, name string) (int, *types.Entity) {
	for i := range entities {
		if entities[i].Name == name {
			return i, &entities[i]
		}
	}
	return -1, nil
}
