package declaration

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

// The tenant kinds of an entity (ST-DECL-EN02).
const (
	tenantOwned  = "owned"
	tenantShared = "shared"
	tenantSystem = "system"
)

// tokenClaimPath is the token claim path grammar (ST-DECL-INV19): one or more claim names joined by dots, each a
// non-empty run of letters, digits, underscores, hyphens, colons or slashes.
var tokenClaimPath = regexp.MustCompile(`^[A-Za-z0-9_:/-]+(\.[A-Za-z0-9_:/-]+)*$`)

// requestParts are the first claim names that name a part of the request instead of a path in the verified
// identity token, compared in any letter case.
var requestParts = []string{"header", "headers", "body", "request", "query", "path", "cookie"}

// writeOperations are the collection operations that write the entity.
var writeOperations = map[types.Operation]bool{types.OpCreate: true, types.OpUpdate: true, types.OpDelete: true, types.OpUpsert: true, types.OpPatch: true}

// TenantFromClientField is satisfied by a declaration whose tenancy claim has a first name that is a request part,
// in any letter case (ST-DECL-SP02).
func TenantFromClientField(service *types.ServiceDeclaration) bool {
	return service.Tenancy != nil && claimRequestPart(service.Tenancy.Claim) != ""
}

// TenantlessPath is satisfied by a declaration with a controller that declares tenants none, a collection that
// reads a system entity, or a binding that runs as tenant and writes a shared entity (ST-DECL-SP03).
func TenantlessPath(service *types.ServiceDeclaration) bool {
	for _, controller := range service.Controllers {
		if controller.Tenants == "none" {
			return true
		}
	}
	return len(systemCollections(service))+len(tenantWritesToShared(service)) > 0
}

// validateTenancyClaim enforces ST-DECL-INV19: the tenancy claim must be a token claim path, and its first name
// must not be a request part. A pooled tenancy needs a claim and a key; tenancy kind single checks a claim only
// when it gives one.
func validateTenancyClaim(service *types.ServiceDeclaration) []ValidationError {
	tenancy := service.Tenancy
	if tenancy == nil {
		return nil
	}
	pooled := tenancy.Kind == "" || tenancy.Kind == tenancyPooled
	var faults []ValidationError
	if pooled && blank(tenancy.Key) {
		faults = append(faults, ValidationError{
			Category: "service", Rule: "tenancy-key", Path: "tenancy.key",
			Message: "pooled tenancy must name the key, the field that holds the tenant identifier",
		})
	}
	if !pooled && tenancy.Claim == "" {
		return faults
	}
	claim := tenancy.Claim
	switch {
	case !tokenClaimPath.MatchString(claim):
		faults = append(faults, ValidationError{
			Category: "service", Rule: "tenancy-claim", Path: "tenancy.claim", Found: claim,
			Message: fmt.Sprintf("tenancy claim %q is not a token claim path: one or more claim names joined by dots, each a non-empty run of letters, digits, underscores, hyphens, colons or slashes", claim),
		})
	case claimRequestPart(claim) != "":
		part := claimRequestPart(claim)
		faults = append(faults, ValidationError{
			Category: "service", Rule: "tenancy-claim", Path: "tenancy.claim", Found: claim,
			Message: fmt.Sprintf("tenancy claim %q names the request part %s, not a path in the verified identity token", claim, part),
		})
	}
	return faults
}

// claimRequestPart gives the request part that the first name of the claim names, or "" when it names none.
func claimRequestPart(claim string) string {
	first, _, _ := strings.Cut(claim, ".")
	for _, part := range requestParts {
		if strings.EqualFold(first, part) {
			return part
		}
	}
	return ""
}

// validateEntityTenants enforces ST-DECL-UC05-R04: the tenant of an entity is owned, shared or system, and an
// omitted one is owned.
func validateEntityTenants(entities []types.Entity) []ValidationError {
	var faults []ValidationError
	for i, entity := range entities {
		switch entity.Tenant {
		case "", tenantOwned, tenantShared, tenantSystem:
			continue
		}
		faults = append(faults, ValidationError{
			Category: "field-type", Rule: "entity-tenant", Path: entityPath(i) + ".tenant", Found: entity.Tenant, Allowed: []string{tenantOwned, tenantShared, tenantSystem},
			Message: fmt.Sprintf("entity %q has the tenant %q, which is not owned, shared or system", entity.Name, entity.Tenant),
		})
	}
	return faults
}

// validateTenantlessPaths enforces ST-DECL-INV20: a controller declares tenants each, a collection does not read
// a system entity, and a binding that writes a shared entity runs as system.
func validateTenantlessPaths(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for i, controller := range service.Controllers {
		if controller.Tenants == "each" {
			continue
		}
		faults = append(faults, ValidationError{
			Category: "service", Rule: "controller-tenants", Path: fmt.Sprintf("controllers[%d].tenants", i), Found: controller.Tenants, Allowed: []string{"each"},
			Message: fmt.Sprintf("controller %q declares tenants %q; a controller must declare tenants each, so that it opens one transaction per tenant", controller.Name, controller.Tenants),
		})
	}
	for _, collection := range systemCollections(service) {
		faults = append(faults, ValidationError{
			Category: "collection", Rule: "system-entity-collection", Path: collectionPath(collection.Name) + ".entity", Found: collection.Entity,
			Message: fmt.Sprintf("collection %q reads the entity %q, whose tenant is system; a system entity is never exposed through a tenant request", collection.Name, collection.Entity),
		})
	}
	for i, slot := range service.Slots {
		if slot.RunsAs != "" && slot.RunsAs != "tenant" && slot.RunsAs != "system" {
			faults = append(faults, ValidationError{
				Category: "slot-binding", Rule: "runs-as", Path: slotPath(i) + ".runs_as", Found: slot.RunsAs, Allowed: []string{"tenant", "system"},
				Message: fmt.Sprintf("the binding on %q at %s runs as %q, which is not tenant or system", slot.Collection, slot.Slot, slot.RunsAs),
			})
		}
	}
	for _, i := range tenantWritesToShared(service) {
		slot := service.Slots[i]
		entity := collectionEntity(service, slot.Collection)
		faults = append(faults, ValidationError{
			Category: "slot-binding", Rule: "shared-entity-write", Path: slotPath(i) + ".runs_as", Found: "tenant", Allowed: []string{"system"},
			Message: fmt.Sprintf("the binding on %q at %s runs as tenant and writes the shared entity %q; a binding that writes a shared entity must run as system", slot.Collection, slot.Slot, entity),
		})
	}
	return faults
}

// systemCollections gives the collections over an entity whose tenant is system.
func systemCollections(service *types.ServiceDeclaration) []types.Collection {
	var found []types.Collection
	for _, collection := range service.Collections {
		if entityTenant(service, collection.Entity) == tenantSystem {
			found = append(found, collection)
		}
	}
	return found
}

// tenantWritesToShared gives the index of each binding that runs as tenant on a collection that writes a shared
// entity.
func tenantWritesToShared(service *types.ServiceDeclaration) []int {
	var found []int
	for i, slot := range service.Slots {
		if slot.RunsAs != "" && slot.RunsAs != "tenant" {
			continue
		}
		for _, collection := range service.Collections {
			if collection.Name != slot.Collection || entityTenant(service, collection.Entity) != tenantShared {
				continue
			}
			for _, operation := range collection.Operations {
				if writeOperations[operation] {
					found = append(found, i)
					break
				}
			}
		}
	}
	return found
}

// entityTenant gives the tenant of the named entity, owned when the entity omits it, and "" for no such entity.
func entityTenant(service *types.ServiceDeclaration, name string) string {
	for _, entity := range service.Entities {
		if entity.Name == name {
			if entity.Tenant == "" {
				return tenantOwned
			}
			return entity.Tenant
		}
	}
	return ""
}

// collectionEntity gives the entity of the named collection.
func collectionEntity(service *types.ServiceDeclaration, name string) string {
	for _, collection := range service.Collections {
		if collection.Name == name {
			return collection.Entity
		}
	}
	return ""
}

// blank is true for text that holds nothing but space.
func blank(text string) bool { return strings.TrimSpace(text) == "" }
