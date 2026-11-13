package declaration

import (
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// statedPooled tells whether the declaration states a tenancy that is pooled. A declaration that omits the tenancy
// selects no tenancy component (ST-GEN-INV37), so it has no tenant list to provision.
func statedPooled(service *types.ServiceDeclaration) bool {
	return service.Tenancy != nil && (service.Tenancy.Kind == "" || service.Tenancy.Kind == tenancyPooled)
}

// validateProvisioning holds the declaration to the parts of ST-DECL-INV78 that need no schema: a declaration that
// states a pooled tenancy declares its tenant provisioning; when it binds authorization, the provisioning names a
// resource type, a Provision permission and a Retire permission, and neither permission is public; and a tenancy
// that is not pooled declares no provisioning. Each fault names the tenant provisioning and what is missing or
// public. checkProvisioningSchema holds the names to the schema.
func validateProvisioning(service *types.ServiceDeclaration) []ValidationError {
	const path = "tenancy.provisioning"
	fault := func(at, found, format string, args ...any) ValidationError {
		return ValidationError{Category: "service", Rule: "tenant-provisioning", Path: at, Found: found, Message: fmt.Sprintf(format, args...)}
	}
	if service.Tenancy == nil {
		return nil
	}
	provisioning := service.Tenancy.Provisioning
	switch {
	case !statedPooled(service) && provisioning != nil:
		return []ValidationError{fault(path, "", "the tenancy %q has one tenant, so it declares no tenant provisioning; remove tenancy.provisioning", service.Tenancy.Kind)}
	case !statedPooled(service):
		return nil
	case provisioning == nil:
		return []ValidationError{fault(path, "", "a pooled service declares its tenant provisioning, through which an operator adds and retires tenants; add tenancy.provisioning, such as {resource_type: service, provision: provision, retire: retire}")}
	case service.Authorization == nil:
		// Provision and Retire are decided commands, so a service that cannot decide them is refused now, and
		// does not wait for a generator to fail closed (security review of PR 253, L2).
		return []ValidationError{fault(path, "", "the tenant provisioning needs authorization, because Provision and Retire are decided on the service-level resource; bind authorization")}
	}
	var faults []ValidationError
	for _, part := range []struct{ key, value, what string }{
		{"resource_type", provisioning.ResourceType, "the resource type of the service-level resource that Provision and Retire are decided on"},
		{"provision", provisioning.Provision, "the Provision permission"},
		{"retire", provisioning.Retire, "the Retire permission"},
	} {
		switch {
		case part.value == "":
			faults = append(faults, fault(path+"."+part.key, "", "the tenant provisioning names no %s; the service binds authorization, so it needs one", part.what))
		case part.key != "resource_type" && part.value == types.PermissionPublic:
			faults = append(faults, fault(path+"."+part.key, part.value, "the tenant provisioning declares %s public; Provision and Retire are never public, so name a permission of the schema", part.what))
		}
	}
	return faults
}

// checkProvisioningSchema holds the names of the tenant provisioning to the authorization schema (ST-DECL-INV78):
// the schema defines the resource type, and the resource type has the Provision and the Retire permission. Each
// fault names the tenant provisioning and the name that the schema lacks.
func checkProvisioningSchema(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	if !statedPooled(service) || service.Tenancy.Provisioning == nil {
		return nil
	}
	provisioning := service.Tenancy.Provisioning
	const path = "tenancy.provisioning"
	definition, defined := schema.definitions[provisioning.ResourceType]
	if provisioning.ResourceType == "" {
		return nil
	}
	if !defined {
		return []ValidationError{{Category: "authorization", Rule: "tenant-provisioning", Path: path + ".resource_type", Found: provisioning.ResourceType, Allowed: schema.typeNames(),
			Message: fmt.Sprintf("the tenant provisioning names the resource type %q, which the schema %s lacks", provisioning.ResourceType, schema.Path)}}
	}
	var faults []ValidationError
	// The resource of the provisioning has a definition of its own: one that is a subject type, or the resource
	// type of an entity, would share its IDs with principals or with tenant data (security review of PR 253, L1).
	if provisioning.ResourceType == service.Authorization.SubjectFromIdentity || schema.grants(provisioning.ResourceType) || declaredResourceTypes(service.Entities)[provisioning.ResourceType] {
		faults = append(faults, ValidationError{Category: "authorization", Rule: "tenant-provisioning", Path: path + ".resource_type", Found: provisioning.ResourceType,
			Message: fmt.Sprintf("the tenant provisioning names the resource type %q, which is a subject type or the resource type of an entity; give the service-level resource a definition of its own, such as service", provisioning.ResourceType)})
	}
	for _, part := range []struct{ key, value, what string }{{"provision", provisioning.Provision, "Provision"}, {"retire", provisioning.Retire, "Retire"}} {
		switch {
		case part.value == "" || part.value == types.PermissionPublic:
		case !definition.permissions[part.value]:
			faults = append(faults, ValidationError{Category: "authorization", Rule: "tenant-provisioning", Path: path + "." + part.key, Found: part.value, Allowed: sortedKeys(definition.permissions),
				Message: fmt.Sprintf("the tenant provisioning names the %s permission %q, which the resource type %s of the schema %s lacks", part.what, part.value, provisioning.ResourceType, schema.Path)})
		case schema.reachesWildcard(provisioning.ResourceType, part.value, map[string]bool{}):
			// A wildcard such as user:* makes the permission public in effect (security review of PR 253, M1).
			faults = append(faults, ValidationError{Category: "authorization", Rule: "tenant-provisioning", Path: path + "." + part.key, Found: part.value,
				Message: fmt.Sprintf("the tenant provisioning names the %s permission %q, which reaches a wildcard subject of the schema %s, so anyone could hold it; Provision and Retire are never public", part.what, part.value, schema.Path)})
		}
	}
	return faults
}
