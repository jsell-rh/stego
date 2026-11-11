package declaration

import (
	"fmt"
	"slices"

	"github.com/jsell-rh/stego/internal/types"
)

// validateReferences checks the References of each entity when the service binds authorization (ST-DECL-INV85):
// each field that refers to an entity with a resource type names a relation that the schema declares on the
// entity's own resource type for the referenced type, and a permission that the schema declares on the referenced
// resource type. A reference that leaves either out, names one that the schema does not declare, or is given for a
// field that is no such reference is refused, naming the field.
func validateReferences(entities []types.Entity, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	byName := map[string]*types.Entity{}
	for i := range entities {
		byName[entities[i].Name] = &entities[i]
	}
	for ei, e := range entities {
		path := fmt.Sprintf("entities[%d]", ei)
		references := map[string]bool{}
		for _, f := range e.Fields {
			to := byName[f.To]
			if f.Type != types.FieldTypeRef || to == nil || to.ResourceType == "" {
				continue
			}
			references[f.Name] = true
			fault := func(key, found, message string, allowed []string) {
				faults = append(faults, ValidationError{Category: "authorization", Rule: "entity-references",
					Path: path + ".references." + f.Name + key, Found: found, Allowed: allowed,
					Message: fmt.Sprintf("entity %q field %q refers to %s, which has a resource type: %s", e.Name, f.Name, to.Name, message)})
			}
			ref, ok := e.References[f.Name]
			switch {
			case !ok:
				fault("", f.Name, "name its relation and its permission in references", nil)
				continue
			case ref.Relation == "":
				fault(".relation", f.Name, "name the relation that the runtime writes to the referenced row", nil)
			case schema != nil && !schema.relationAdmits(e.ResourceType, ref.Relation, to.ResourceType):
				fault(".relation", ref.Relation, fmt.Sprintf("the schema %s declares no relation %q on %q for %q", schema.Path, ref.Relation, e.ResourceType, to.ResourceType), nil)
			}
			switch {
			case ref.Permission == "":
				fault(".permission", f.Name, "name the permission that a create and a move decide on the referenced row", nil)
			case schema != nil && !schema.declaresPermission(to.ResourceType, ref.Permission):
				fault(".permission", ref.Permission, fmt.Sprintf("the schema %s declares no permission %q on %q", schema.Path, ref.Permission, to.ResourceType), schema.permissionsOf(to.ResourceType))
			}
		}
		for _, name := range sortedKeys(e.References) {
			if !references[name] {
				faults = append(faults, ValidationError{Category: "authorization", Rule: "entity-references", Path: path + ".references." + name, Found: name,
					Allowed: sortedKeys(references), Message: fmt.Sprintf("entity %q gives references for %q, which is no field that refers to an entity with a resource type", e.Name, name)})
			}
		}
	}
	return faults
}

// relationAdmits tells whether the definition resourceType declares relation with the subject type subjectType.
func (s *AuthzSchema) relationAdmits(resourceType, relation, subjectType string) bool {
	definition, ok := s.definitions[resourceType]
	return ok && slices.Contains(definition.relations[relation], subjectType)
}

// declaresPermission tells whether the definition resourceType declares the permission.
func (s *AuthzSchema) declaresPermission(resourceType, permission string) bool {
	definition, ok := s.definitions[resourceType]
	return ok && definition.permissions[permission]
}

// permissionsOf gives the permission names of the definition resourceType.
func (s *AuthzSchema) permissionsOf(resourceType string) []string {
	return sortedKeys(s.definitions[resourceType].permissions)
}
