package declaration

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"

	"github.com/jsell-rh/stego/internal/types"
)

// The checks of the authorization that a declaration binds (ST-DECL-UC06): the authorization setting, the .zed
// schema in the service's own source, which checking compiles with the schema compiler of the SpiceDB provider, and
// every name the declaration takes from it.

// Providers are the authorization providers a declaration may bind.
var Providers = []string{"spicedb"}

// MaxSchemaBytes limits the size of the .zed schema that checking reads.
const MaxSchemaBytes = 1 << 20

// AuthzSchema holds what checking reads from a compiled .zed schema: each definition with its relations and
// permissions, and the parameters of its caveats.
type AuthzSchema struct {
	// Path is the schema's path in the project folder.
	Path string
	// definitions maps each definition name to its relations and permissions.
	definitions map[string]schemaDefinition
	// caveatParameters holds the parameter names of every caveat.
	caveatParameters map[string]bool
	// wildcards holds each type#name of a relation or permission that can grant a wildcard subject
	// (wildcardReach).
	wildcards map[string]bool
}

// schemaDefinition is one definition of the schema: each relation with the subject types it admits, and the names
// of its permissions.
type schemaDefinition struct {
	relations   map[string][]string
	permissions map[string]bool
	// subjects maps each relation to the subjects it admits, each with its relation and whether it is a wildcard
	// such as user:*.
	subjects map[string][]schemaSubject
	// rewrites maps each permission to its expression.
	rewrites map[string]*core.UsersetRewrite
}

// schemaSubject is one subject that a relation admits: a type, the relation of a userset such as group#member, and
// whether it is the wildcard of the type, such as user:*.
type schemaSubject struct {
	namespace string
	relation  string
	wildcard  bool
}

// SchemaError is a .zed schema that does not compile: its path, the line where compiling stopped and why.
type SchemaError struct {
	Path    string
	Line    int
	Message string
}

func (e *SchemaError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s line %d: %s", e.Path, e.Line, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Message)
}

// CompileAuthzSchema compiles the .zed schema source at the path with the SpiceDB schema compiler, and then checks
// that every type, relation and caveat the schema refers to is one it defines. It returns a *SchemaError, which
// names the line, for a schema that does not compile.
func CompileAuthzSchema(schemaPath string, source []byte) (*AuthzSchema, error) {
	compiled, err := compiler.Compile(compiler.InputSchema{Source: input.Source(schemaPath), SchemaString: string(source)},
		compiler.AllowUnprefixedObjectType(), compiler.DisallowImportFlag())
	if err != nil {
		fault := &SchemaError{Path: schemaPath, Message: err.Error()}
		var located compiler.WithContextError
		if errors.As(err, &located) {
			if line, _, lineErr := located.SourceRange.Start().LineAndColumn(); lineErr == nil {
				fault.Line = line + 1
			}
			fault.Message = located.BaseMessage
		}
		return nil, fault
	}
	schema := &AuthzSchema{Path: schemaPath, definitions: map[string]schemaDefinition{}, caveatParameters: map[string]bool{}}
	caveats := map[string]bool{}
	for _, caveat := range compiled.CaveatDefinitions {
		caveats[caveat.GetName()] = true
		for parameter := range caveat.GetParameterTypes() {
			schema.caveatParameters[parameter] = true
		}
	}
	for _, definition := range compiled.ObjectDefinitions {
		entry := schemaDefinition{relations: map[string][]string{}, permissions: map[string]bool{}, subjects: map[string][]schemaSubject{}, rewrites: map[string]*core.UsersetRewrite{}}
		for _, relation := range definition.GetRelation() {
			if relation.GetUsersetRewrite() != nil {
				entry.permissions[relation.GetName()] = true
				entry.rewrites[relation.GetName()] = relation.GetUsersetRewrite()
				continue
			}
			var subjects []string
			for _, allowed := range relation.GetTypeInformation().GetAllowedDirectRelations() {
				subjects = append(subjects, allowed.GetNamespace())
				entry.subjects[relation.GetName()] = append(entry.subjects[relation.GetName()], schemaSubject{namespace: allowed.GetNamespace(), relation: allowed.GetRelation(), wildcard: allowed.GetPublicWildcard() != nil})
			}
			entry.relations[relation.GetName()] = subjects
		}
		schema.definitions[definition.GetName()] = entry
	}
	if err := schema.checkReferences(compiled.ObjectDefinitions, caveats); err != nil {
		return nil, err
	}
	schema.wildcards = schema.wildcardReach()
	return schema, nil
}

// checkReferences checks that each subject type a relation admits is a definition of the schema, with the relation
// it names, that each caveat a relation requires is defined, and that each relation a permission computes is one of
// its own definition. The compiler parses the schema but leaves these to the provider, which would refuse the
// schema only when the service writes it.
func (s *AuthzSchema) checkReferences(definitions []*core.NamespaceDefinition, caveats map[string]bool) error {
	for _, definition := range definitions {
		own := s.definitions[definition.GetName()]
		for _, relation := range definition.GetRelation() {
			for _, allowed := range relation.GetTypeInformation().GetAllowedDirectRelations() {
				subject, defined := s.definitions[allowed.GetNamespace()]
				switch {
				case !defined:
					return s.referenceError(allowed.GetSourcePosition(), fmt.Sprintf("relation %s of %s admits the type %s, which the schema does not define", relation.GetName(), definition.GetName(), allowed.GetNamespace()))
				case allowed.GetRelation() != "" && allowed.GetRelation() != "..." && !subject.has(allowed.GetRelation()):
					return s.referenceError(allowed.GetSourcePosition(), fmt.Sprintf("relation %s of %s admits %s#%s, which the schema does not define", relation.GetName(), definition.GetName(), allowed.GetNamespace(), allowed.GetRelation()))
				case allowed.GetRequiredCaveat() != nil && !caveats[allowed.GetRequiredCaveat().GetCaveatName()]:
					return s.referenceError(allowed.GetSourcePosition(), fmt.Sprintf("relation %s of %s requires the caveat %s, which the schema does not define", relation.GetName(), definition.GetName(), allowed.GetRequiredCaveat().GetCaveatName()))
				}
			}
			for _, name := range computedRelations(relation.GetUsersetRewrite()) {
				if !own.has(name.relation) {
					return s.referenceError(name.position, fmt.Sprintf("permission %s of %s uses %s, which %s does not define", relation.GetName(), definition.GetName(), name.relation, definition.GetName()))
				}
			}
		}
	}
	return nil
}

// referenceError is the schema error at a position of the compiled schema.
func (s *AuthzSchema) referenceError(position *core.SourcePosition, message string) error {
	fault := &SchemaError{Path: s.Path, Message: message}
	if position != nil {
		fault.Line = int(position.GetZeroIndexedLineNumber()) + 1
	}
	return fault
}

// computedName is a relation that a permission computes in its own definition, with where the schema names it.
type computedName struct {
	relation string
	position *core.SourcePosition
}

// computedRelations gives the relations of its own definition that a permission's expression names: each computed
// relation and the left side of each arrow.
func computedRelations(rewrite *core.UsersetRewrite) []computedName {
	if rewrite == nil {
		return nil
	}
	var operation *core.SetOperation
	switch {
	case rewrite.GetUnion() != nil:
		operation = rewrite.GetUnion()
	case rewrite.GetIntersection() != nil:
		operation = rewrite.GetIntersection()
	default:
		operation = rewrite.GetExclusion()
	}
	var names []computedName
	for _, child := range operation.GetChild() {
		switch {
		case child.GetComputedUserset() != nil:
			names = append(names, computedName{child.GetComputedUserset().GetRelation(), child.GetSourcePosition()})
		case child.GetTupleToUserset() != nil:
			names = append(names, computedName{child.GetTupleToUserset().GetTupleset().GetRelation(), child.GetSourcePosition()})
		case child.GetFunctionedTupleToUserset() != nil:
			names = append(names, computedName{child.GetFunctionedTupleToUserset().GetTupleset().GetRelation(), child.GetSourcePosition()})
		case child.GetUsersetRewrite() != nil:
			names = append(names, computedRelations(child.GetUsersetRewrite())...)
		}
	}
	return names
}

// has tells whether the definition has a relation or a permission of the name.
func (d schemaDefinition) has(name string) bool {
	_, relation := d.relations[name]
	return relation || d.permissions[name]
}

// Defines tells whether the schema has a definition of the type.
func (s *AuthzSchema) Defines(typeName string) bool {
	_, ok := s.definitions[typeName]
	return ok
}

// grants tells whether a relation of the schema admits the subject type.
func (s *AuthzSchema) grants(subjectType string) bool {
	for _, definition := range s.definitions {
		for _, subjects := range definition.relations {
			for _, subject := range subjects {
				if subject == subjectType {
					return true
				}
			}
		}
	}
	return false
}

// CheckAuthorization runs the checks of the authorization that the declaration binds, reading the schema from the
// project folder: the authorization setting (ST-DECL-UC06-R01), the schema path and its compile, every name the
// declaration takes from the schema (ST-DECL-INV22), the relations a slot binding may write (ST-DECL-INV23), the
// system principal (ST-DECL-INV24), the relationships of each entity (ST-DECL-UC06-R04) and the coverage of every
// operation (ST-DECL-INV31). A declaration that binds no authorization and names none of it gets no fault. project
// is the project folder; a schema path that leaves it is refused.
func CheckAuthorization(service *types.ServiceDeclaration, project fs.FS) []ValidationError {
	if service.Authorization == nil {
		return authorizationUnbound(service)
	}
	faults := validateAuthorizationSetting(service.Authorization)
	faults = append(faults, validateAuthorizationTenancy(service)...)
	faults = append(faults, validateSystemEntities(service.Entities)...)
	faults = append(faults, validateRelationshipParts(service.Entities)...)
	faults = append(faults, validateAuthorizationCoverage(service)...)
	schema, fault := readAuthzSchema(service.Authorization.SchemaPath, project)
	if fault != nil {
		faults = append(faults, operationsAuthorization(service, nil)...)
		return append(faults, *fault)
	}
	if schema == nil {
		return append(faults, operationsAuthorization(service, nil)...)
	}
	faults = append(faults, operationsAuthorization(service, schema)...)
	faults = append(faults, validateSchemaNames(service, schema)...)
	faults = append(faults, validateWrittenRelations(service, schema)...)
	faults = append(faults, validateSystemPrincipal(service, schema)...)
	faults = append(faults, checkProvisioningSchema(service, schema)...)
	return faults
}

// readAuthzSchema reads and compiles the schema at the path in the project folder. It gives no schema and no fault
// when the path is empty, which validateAuthorizationSetting reports.
func readAuthzSchema(schemaPath string, project fs.FS) (*AuthzSchema, *ValidationError) {
	if schemaPath == "" {
		return nil, nil
	}
	fault := func(message string) *ValidationError {
		return &ValidationError{Category: "authorization", Rule: "schema-path", Path: "authorization.schema_path", Found: schemaPath, Message: message}
	}
	if !fs.ValidPath(schemaPath) || path.Clean(schemaPath) != schemaPath {
		return nil, fault(fmt.Sprintf("the schema path %q is not a clean relative path inside the service's own source; give the path of the .zed schema relative to the project folder, such as authz/schema.zed", schemaPath))
	}
	file, err := project.Open(schemaPath)
	if err != nil {
		return nil, fault(fmt.Sprintf("the schema %q cannot be read inside the service's own source: %v", schemaPath, err))
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fault(fmt.Sprintf("the schema %q is not a regular file inside the service's own source", schemaPath))
	}
	source, err := io.ReadAll(io.LimitReader(file, MaxSchemaBytes+1))
	if err != nil {
		return nil, fault(fmt.Sprintf("the schema %q cannot be read: %v", schemaPath, err))
	}
	if len(source) > MaxSchemaBytes {
		return nil, fault(fmt.Sprintf("the schema %q is larger than %d bytes", schemaPath, MaxSchemaBytes))
	}
	schema, err := CompileAuthzSchema(schemaPath, source)
	var compileFault *SchemaError
	if errors.As(err, &compileFault) {
		found := schemaPath
		if compileFault.Line > 0 {
			found = fmt.Sprintf("%s:%d", schemaPath, compileFault.Line)
		}
		return nil, &ValidationError{Category: "authorization", Rule: "schema-compile", Path: "authorization.schema_path", Found: found,
			Message: fmt.Sprintf("the schema does not compile: %s", compileFault.Error())}
	}
	if err != nil {
		return nil, fault(err.Error())
	}
	return schema, nil
}

// validateAuthorizationTenancy checks that a declaration that binds authorization declares pooled tenancy
// (ST-DECL-INV84): every resource ID at the provider is tenant-qualified (ST-SUP-RP06-LC22), and only a stated
// pooled tenancy selects the tenancy component that gives each table its tenant column (ST-GEN-INV37), so an
// omitted tenancy, which relies on the default, is refused as well as kind single.
func validateAuthorizationTenancy(service *types.ServiceDeclaration) []ValidationError {
	tenancy := service.Tenancy
	switch {
	case tenancy == nil:
		return []ValidationError{{Category: "authorization", Rule: "authorization-tenancy", Path: "tenancy",
			Message: "the declaration binds authorization but does not declare tenancy; every resource ID at the provider is tenant-qualified, so declare tenancy with kind pooled, its key and its claim"}}
	case tenancy.Kind != "" && tenancy.Kind != "pooled":
		return []ValidationError{{Category: "authorization", Rule: "authorization-tenancy", Path: "tenancy.kind", Found: tenancy.Kind,
			Allowed: []string{"pooled"}, Message: fmt.Sprintf("the declaration binds authorization with tenancy kind %q; every resource ID at the provider is tenant-qualified, so declare tenancy with kind pooled", tenancy.Kind)}}
	}
	return nil
}

// validateAuthorizationSetting checks that the authorization setting names a known provider, the schema path and
// the subject type of a request's identity (ST-DECL-UC06-R01).
func validateAuthorizationSetting(setting *types.Authorization) []ValidationError {
	var faults []ValidationError
	missing := func(key, name, hint string) {
		faults = append(faults, ValidationError{Category: "authorization", Rule: "authorization-setting", Path: "authorization." + key,
			Message: fmt.Sprintf("the authorization setting %s is missing; %s", name, hint)})
	}
	switch {
	case setting.Provider == "":
		missing("provider", "provider", "name the provider of the decision and relationship ports, such as spicedb")
		faults[len(faults)-1].Allowed = append([]string(nil), Providers...)
	case !contains(Providers, setting.Provider):
		faults = append(faults, ValidationError{Category: "authorization", Rule: "authorization-setting", Path: "authorization.provider", Found: setting.Provider,
			Allowed: append([]string(nil), Providers...), Message: fmt.Sprintf("the authorization provider %q is not one that STEGO binds", setting.Provider)})
	}
	if setting.SchemaPath == "" {
		missing("schema_path", "schema path", "give the path of the .zed schema in the service's own source, such as authz/schema.zed")
	}
	if setting.SubjectFromIdentity == "" {
		missing("subject_from_identity", "subject from identity", "name the subject type of the schema that a request's identity maps to, such as user")
	}
	return faults
}

// authorizationUnbound gives a fault for each place that names authorization when the declaration binds none: an
// entity's resource type, permissions, context fields or relationships, and a slot binding's relationships written.
// With no schema, none of these names can exist in it (ST-DECL-INV22).
func authorizationUnbound(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	fault := func(place, message string) {
		faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: place, Found: place,
			Message: message + ", but the declaration binds no authorization; add the authorization setting with its provider, schema path and subject from identity, or remove it"})
	}
	for ei, entity := range service.Entities {
		a := entity.EntityAuthorization
		if a.ResourceType != "" {
			fault(fmt.Sprintf("entities[%d].resource_type", ei), fmt.Sprintf("entity %q names the resource type %q", entity.Name, a.ResourceType))
		}
		if len(a.Permissions) > 0 {
			fault(fmt.Sprintf("entities[%d].permissions", ei), fmt.Sprintf("entity %q names permissions", entity.Name))
		}
		if len(a.ContextFields) > 0 {
			fault(fmt.Sprintf("entities[%d].context_fields", ei), fmt.Sprintf("entity %q names context fields", entity.Name))
		}
		if len(a.Relationships) > 0 {
			fault(fmt.Sprintf("entities[%d].relationships", ei), fmt.Sprintf("entity %q declares relationships", entity.Name))
		}
	}
	operations := func(kind, path, resourceType, permission string, relations []string) {
		if resourceType != "" || permission != "" || len(relations) > 0 {
			fault(path, fmt.Sprintf("the %s at %s names a resource type, a permission or relationships written", kind, path))
		}
	}
	for i, command := range service.Commands {
		operations("command", commandPath(i), command.ResourceType, command.Permission, command.RelationshipsWritten)
	}
	for i, query := range service.Queries {
		operations("query", queryPath(i), query.ResourceType, query.Permission, nil)
	}
	for i, worker := range service.Workers {
		operations("worker", workerPath(i), worker.ResourceType, worker.Permission, nil)
	}
	for si, slot := range service.Slots {
		if len(slot.RelationshipsWritten) > 0 {
			fault(fmt.Sprintf("slots[%d].relationships_written", si), fmt.Sprintf("the binding of %s at %s lists relationships written", slot.Collection, slot.Slot))
		}
	}
	return faults
}

// validateRelationshipParts checks that each relationship of an entity names its relation, its subject source, the
// subject field when the source is field, and its subject type (ST-DECL-UC06-R04).
func validateRelationshipParts(entities []types.Entity) []ValidationError {
	var faults []ValidationError
	for ei, entity := range entities {
		for ri, relationship := range entity.Relationships {
			place := fmt.Sprintf("entities[%d].relationships[%d]", ei, ri)
			name := relationshipName(entity, relationship)
			missing := func(key, part, hint string, allowed ...string) {
				faults = append(faults, ValidationError{Category: "authorization", Rule: "relationship", Path: place + "." + key, Found: name, Allowed: allowed,
					Message: fmt.Sprintf("the relationship %s of entity %q has no %s; %s", name, entity.Name, part, hint)})
			}
			if relationship.Relation == "" {
				missing("relation", "relation", "name the relation of the schema under the entity's resource type")
			}
			switch relationship.SubjectSource {
			case "":
				missing("subject_source", "subject source", "say where the subject comes from", types.SubjectSourceField, types.SubjectSourceCaller)
			case types.SubjectSourceField:
				if relationship.SubjectField == "" {
					missing("subject_field", "subject field", "name the field of the entity that holds the subject")
				} else if !hasField(entity, relationship.SubjectField) {
					faults = append(faults, ValidationError{Category: "authorization", Rule: "relationship", Path: place + ".subject_field", Found: relationship.SubjectField,
						Allowed: fieldNames(entity), Message: fmt.Sprintf("the relationship %s of entity %q takes its subject from the field %q, which the entity does not declare", name, entity.Name, relationship.SubjectField)})
				}
			case types.SubjectSourceCaller:
			default:
				faults = append(faults, ValidationError{Category: "authorization", Rule: "relationship", Path: place + ".subject_source", Found: relationship.SubjectSource,
					Allowed: []string{types.SubjectSourceField, types.SubjectSourceCaller}, Message: fmt.Sprintf("the relationship %s of entity %q has the subject source %q, which is not field or caller", name, entity.Name, relationship.SubjectSource)})
			}
			if relationship.SubjectType == "" {
				missing("subject_type", "subject type", "name the schema type of the subject, such as user")
			}
		}
	}
	return faults
}

// relationshipName names a relationship in a fault: its resource type and relation, as far as it states them.
func relationshipName(entity types.Entity, relationship types.Relationship) string {
	relation := relationship.Relation
	if relation == "" {
		relation = "(no relation)"
	}
	if entity.ResourceType == "" {
		return relation
	}
	return entity.ResourceType + "#" + relation
}

// validateSchemaNames checks that every name the declaration takes from the schema is in it (ST-DECL-INV22): the
// subject type of a request's identity, each entity's resource type, the permission of each operation, the
// relation and subject type of each relationship, and the caveat parameter of each context field.
func validateSchemaNames(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	lacks := func(place, kind, name, where string, allowed []string) {
		faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: place, Found: name, Allowed: allowed,
			Message: fmt.Sprintf("%s names the %s %q, which the schema %s lacks", where, kind, name, schema.Path)})
	}
	if schema.Defines(types.FloorMarkerType) {
		faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: "authorization.schema_path", Found: types.FloorMarkerType,
			Message: fmt.Sprintf("the schema %s defines %s, the floor marker that STEGO adds to the schema it writes to the provider; remove the definition %s from the schema", schema.Path, types.FloorMarkerType, types.FloorMarkerType)})
	}
	if subject := service.Authorization.SubjectFromIdentity; subject != "" && !schema.Defines(subject) {
		lacks("authorization.subject_from_identity", "subject type", subject, "the authorization setting", schema.typeNames())
	}
	for ei, entity := range service.Entities {
		a := entity.EntityAuthorization
		where := fmt.Sprintf("entity %q", entity.Name)
		definition, defined := schema.definitions[a.ResourceType]
		if a.ResourceType != "" && !defined {
			lacks(fmt.Sprintf("entities[%d].resource_type", ei), "resource type", a.ResourceType, where, schema.typeNames())
		}
		if defined {
			for _, operation := range sortedOperations(a.Permissions) {
				permission := a.Permissions[operation]
				if permission != types.PermissionPublic && !definition.permissions[permission] {
					lacks(fmt.Sprintf("entities[%d].permissions.%s", ei, operation), "permission", permission,
						fmt.Sprintf("%s for the operation %s", where, operation), sortedKeys(definition.permissions))
				}
			}
		}
		for ri, relationship := range a.Relationships {
			place := fmt.Sprintf("entities[%d].relationships[%d]", ei, ri)
			subjects, isRelation := definition.relations[relationship.Relation]
			if defined && relationship.Relation != "" && !isRelation {
				lacks(place+".relation", "relation", relationship.Relation, fmt.Sprintf("%s in a relationship of the resource type %s", where, a.ResourceType), sortedKeys(definition.relations))
				continue
			}
			if relationship.SubjectType == "" {
				continue
			}
			if !schema.Defines(relationship.SubjectType) {
				lacks(place+".subject_type", "subject type", relationship.SubjectType, where+" in a relationship", schema.typeNames())
				continue
			}
			if isRelation && !contains(subjects, relationship.SubjectType) {
				faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: place + ".subject_type", Found: relationship.SubjectType,
					Allowed: unique(subjects), Message: fmt.Sprintf("%s relates the subject type %q by %s#%s, which the schema %s does not admit for that relation", where, relationship.SubjectType, a.ResourceType, relationship.Relation, schema.Path)})
			}
		}
		for ci, field := range a.ContextFields {
			parameter := strings.TrimPrefix(field, "request.")
			if !schema.caveatParameters[parameter] {
				lacks(fmt.Sprintf("entities[%d].context_fields[%d]", ei, ci), "caveat parameter", field, where+" as a context field", prefixed("request.", sortedKeys(schema.caveatParameters)))
			}
		}
	}
	return faults
}

// SchemaLacksName is the predicate of ST-DECL-SP05: an entity, a collection's operation or a slot binding names a
// resource type, a permission, a relation or a caveat parameter that the compiled schema does not declare.
func SchemaLacksName(service *types.ServiceDeclaration, schema *AuthzSchema) bool {
	if service.Authorization == nil {
		service = withAuthorization(service)
	}
	for _, fault := range validateSchemaNames(service, schema) {
		if fault.Path != "authorization.subject_from_identity" && fault.Path != "authorization.schema_path" && !strings.HasSuffix(fault.Path, ".subject_type") {
			return true
		}
	}
	for _, slot := range service.Slots {
		for _, written := range slot.RelationshipsWritten {
			resourceType, relation, ok := strings.Cut(written, "#")
			if !ok {
				continue
			}
			if _, isRelation := schema.definitions[resourceType].relations[relation]; !isRelation {
				return true
			}
		}
	}
	return false
}

// withAuthorization is a copy of the declaration with an empty authorization setting, so that the name checks read
// a declaration that binds none.
func withAuthorization(service *types.ServiceDeclaration) *types.ServiceDeclaration {
	copied := *service
	copied.Authorization = &types.Authorization{}
	return &copied
}

// validateWrittenRelations checks that each relation a slot binding may write is resource_type#relation, that the
// resource type is the type of an entity the service declares, and that the relation is a relation of that type in
// the schema (ST-DECL-INV23).
func validateWrittenRelations(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	declared := declaredResourceTypes(service.Entities)
	for si, slot := range service.Slots {
		binding := fmt.Sprintf("the binding of %s at %s", slot.Collection, slot.Slot)
		for wi, written := range slot.RelationshipsWritten {
			place := fmt.Sprintf("slots[%d].relationships_written[%d]", si, wi)
			if message := writtenRelationUnknown(written, declared, schema, "the service declares"); message != "" {
				fault := ValidationError{Category: "authorization", Rule: "written-relations", Path: place, Found: written, Message: fmt.Sprintf("%s lists %q, %s", binding, written, message)}
				if resourceType, _, ok := strings.Cut(written, "#"); ok && declared[resourceType] {
					fault.Allowed = prefixed(resourceType+"#", sortedKeys(schema.definitions[resourceType].relations))
				} else {
					fault.Allowed = sortedKeys(declared)
				}
				faults = append(faults, fault)
			}
		}
	}
	return faults
}

// writtenRelationUnknown says why a relation a slot binding may write is unknown, or gives "" when it is a relation
// of the schema under the resource type of a declared entity.
func writtenRelationUnknown(written string, declared map[string]bool, schema *AuthzSchema, owner string) string {
	resourceType, relation, ok := strings.Cut(written, "#")
	switch {
	case !ok || resourceType == "" || relation == "" || strings.Contains(relation, "#"):
		return "which is not of the form resource_type#relation"
	case !schema.Defines(resourceType):
		return fmt.Sprintf("but the schema %s lacks the resource type %s", schema.Path, resourceType)
	case !declared[resourceType]:
		return fmt.Sprintf("but %s is the resource type of no entity %s", resourceType, owner)
	}
	if _, isRelation := schema.definitions[resourceType].relations[relation]; !isRelation {
		return fmt.Sprintf("but the schema %s lacks the relation %s of %s", schema.Path, relation, resourceType)
	}
	return ""
}

// SlotRelationUnknown is the predicate of ST-DECL-SP06: a slot binding lists a relation in Relationships written
// that the schema lacks, or that belongs to no resource type of a declared entity.
func SlotRelationUnknown(service *types.ServiceDeclaration, schema *AuthzSchema) bool {
	declared := declaredResourceTypes(service.Entities)
	for _, slot := range service.Slots {
		for _, written := range slot.RelationshipsWritten {
			if writtenRelationUnknown(written, declared, schema, "the service declares") != "" {
				return true
			}
		}
	}
	return false
}

// validateSystemPrincipal checks the system principal half of ST-DECL-INV24: the system subject type is
// service_account, the one subject type the authorization runtime gives a system principal, and the schema defines
// it and grants it relations; and when a slot binding runs as system, the authorization setting names it. So the
// system principal is a subject the schema grants relations to, and never a bypass.
func validateSystemPrincipal(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	subjectType := service.Authorization.SystemSubjectType
	if subjectType != "" {
		fault := ValidationError{Category: "authorization", Rule: "no-bypass", Path: "authorization.system_subject_type", Found: subjectType}
		switch {
		case !schema.Defines(subjectType):
			fault.Allowed = []string{types.SystemSubjectType}
			fault.Message = fmt.Sprintf("the system subject type %q is a type that the schema %s does not define; a system principal acts as %s", subjectType, schema.Path, types.SystemSubjectType)
		case !schema.grants(subjectType):
			fault.Allowed = []string{types.SystemSubjectType}
			fault.Message = fmt.Sprintf("the system subject type %q is a type that no relation of the schema %s grants; a system principal is a subject the schema grants relations to", subjectType, schema.Path)
		case subjectType != types.SystemSubjectType:
			fault.Allowed = []string{types.SystemSubjectType}
			fault.Message = fmt.Sprintf("the system subject type %q is not %s, the one subject type that the authorization runtime gives a system principal", subjectType, types.SystemSubjectType)
		default:
			return nil
		}
		var bindings []string
		for _, slot := range service.Slots {
			if slot.RunsAs == types.RunsAsSystem {
				bindings = append(bindings, fmt.Sprintf("the binding of %s at %s", slot.Collection, slot.Slot))
			}
		}
		if len(bindings) > 0 {
			fault.Message += "; it is the principal of " + strings.Join(bindings, " and ") + ", which runs as system"
		}
		return append(faults, fault)
	}
	for si, slot := range service.Slots {
		if slot.RunsAs != types.RunsAsSystem {
			continue
		}
		faults = append(faults, ValidationError{Category: "authorization", Rule: "no-bypass", Path: fmt.Sprintf("slots[%d].runs_as", si), Found: types.RunsAsSystem, Allowed: []string{types.SystemSubjectType},
			Message: fmt.Sprintf("the binding of %s at %s runs as system, but the authorization setting names no system subject type; name %s, the subject type of the schema that a system principal acts as", slot.Collection, slot.Slot, types.SystemSubjectType)})
	}
	return faults
}

// validateSystemEntities checks that no entity whose tenant is system names a resource type or permissions: a
// resource ID at the provider is tenant-qualified (ST-SUP-RP06-LC22), and an entity with no tenant has no tenant to
// qualify it with. Such an entity is never exposed through a tenant request, so it needs no permission.
func validateSystemEntities(entities []types.Entity) []ValidationError {
	var faults []ValidationError
	for ei, entity := range entities {
		if entity.Tenant != types.TenantSystem {
			continue
		}
		if entity.ResourceType != "" {
			faults = append(faults, systemEntityFault(ei, entity, "resource_type", entity.ResourceType, "the resource type"))
		}
		if len(entity.Permissions) > 0 {
			faults = append(faults, systemEntityFault(ei, entity, "permissions", entity.Name, "permissions"))
		}
	}
	return faults
}

func systemEntityFault(ei int, entity types.Entity, key, found, what string) ValidationError {
	return ValidationError{Category: "authorization", Rule: "system-entity-protected", Path: fmt.Sprintf("entities[%d].%s", ei, key), Found: found,
		Message: fmt.Sprintf("entity %q has the tenant system and names %s; every resource ID at the provider is tenant-qualified, and an entity with no tenant has none to qualify it with, so remove %s or give the entity the tenant owned or shared", entity.Name, what, what)}
}

// declaredResourceTypes gives the resource types of the declared entities.
func declaredResourceTypes(entities []types.Entity) map[string]bool {
	declared := map[string]bool{}
	for _, entity := range entities {
		if entity.ResourceType != "" {
			declared[entity.ResourceType] = true
		}
	}
	return declared
}

// typeNames gives the names of the schema's definitions, in order.
func (s *AuthzSchema) typeNames() []string { return sortedKeys(s.definitions) }

func sortedOperations(m map[types.Operation]string) []types.Operation {
	operations := make([]types.Operation, 0, len(m))
	for operation := range m {
		operations = append(operations, operation)
	}
	sort.Slice(operations, func(i, j int) bool { return operations[i] < operations[j] })
	return operations
}

func prefixed(prefix string, names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = prefix + name
	}
	return out
}

func unique(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func hasField(entity types.Entity, name string) bool {
	for _, field := range entity.Fields {
		if field.Name == name {
			return true
		}
	}
	return false
}

func fieldNames(entity types.Entity) []string {
	names := make([]string, 0, len(entity.Fields))
	for _, field := range entity.Fields {
		names = append(names, field.Name)
	}
	return names
}

// slotSettings resolves the principal that the fills of each slot binding run as: tenant when the binding omits it
// (ST-DECL-EN05). A binding that runs as system states it.
func slotSettings(service *types.ServiceDeclaration) []Setting {
	var settings []Setting
	for i, slot := range service.Slots {
		settings = append(settings, resolved(fmt.Sprintf("slots[%d].runs_as", i), slot.RunsAs, types.RunsAsTenant))
	}
	return settings
}

// reachesWildcard tells whether the relation or permission name of the type can grant a wildcard subject, such as
// user:*, so that every subject of that type holds it (wildcardReach).
func (s *AuthzSchema) reachesWildcard(typeName, name string) bool {
	return s.wildcards[typeName+"#"+name]
}

// wildcardReach computes, for every relation and permission of the schema, whether it can grant a wildcard subject:
// a relation that admits a wildcard; a relation that admits a userset whose relation reaches one; or a permission
// whose expression reaches one through its relations and arrows. A union reaches one when any part does, an
// intersection when every part does, and an exclusion when its base does. Each rule is monotone, so the answer is
// the least fixed point: every node starts at no wildcard, and each pass over the schema sets the nodes that now
// reach one, until a pass sets none. A cycle with no wildcard stays at no wildcard. Each pass is linear in the
// size of the schema, and there are at most as many passes as nodes, so no schema makes checking slow.
func (s *AuthzSchema) wildcardReach() map[string]bool {
	reach := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for typeName, definition := range s.definitions {
			for name := range definition.subjects {
				if !reach[typeName+"#"+name] && s.relationReaches(definition.subjects[name], reach) {
					reach[typeName+"#"+name], changed = true, true
				}
			}
			for name, rewrite := range definition.rewrites {
				if !reach[typeName+"#"+name] && s.rewriteReaches(typeName, rewrite, reach) {
					reach[typeName+"#"+name], changed = true, true
				}
			}
		}
	}
	return reach
}

// relationReaches tells whether a relation with the subjects given reaches a wildcard, by the nodes found so far.
func (s *AuthzSchema) relationReaches(subjects []schemaSubject, reach map[string]bool) bool {
	for _, subject := range subjects {
		if subject.wildcard || subject.relation != "" && subject.relation != "..." && reach[subject.namespace+"#"+subject.relation] {
			return true
		}
	}
	return false
}

// rewriteReaches tells whether a permission expression of the type reaches a wildcard, by the nodes found so far.
func (s *AuthzSchema) rewriteReaches(typeName string, rewrite *core.UsersetRewrite, reach map[string]bool) bool {
	child := func(c *core.SetOperation_Child) bool {
		switch {
		case c.GetComputedUserset() != nil:
			return reach[typeName+"#"+c.GetComputedUserset().GetRelation()]
		case c.GetTupleToUserset() != nil || c.GetFunctionedTupleToUserset() != nil:
			tupleset, computed := c.GetTupleToUserset().GetTupleset().GetRelation(), c.GetTupleToUserset().GetComputedUserset().GetRelation()
			if f := c.GetFunctionedTupleToUserset(); f != nil {
				tupleset, computed = f.GetTupleset().GetRelation(), f.GetComputedUserset().GetRelation()
			}
			for _, subject := range s.definitions[typeName].subjects[tupleset] {
				if subject.wildcard || reach[subject.namespace+"#"+computed] {
					return true
				}
			}
			return false
		case c.GetUsersetRewrite() != nil:
			return s.rewriteReaches(typeName, c.GetUsersetRewrite(), reach)
		}
		return false
	}
	switch {
	case rewrite.GetUnion() != nil:
		for _, c := range rewrite.GetUnion().GetChild() {
			if child(c) {
				return true
			}
		}
		return false
	case rewrite.GetIntersection() != nil:
		children := rewrite.GetIntersection().GetChild()
		for _, c := range children {
			if !child(c) {
				return false
			}
		}
		return len(children) > 0
	case rewrite.GetExclusion() != nil:
		children := rewrite.GetExclusion().GetChild()
		return len(children) > 0 && child(children[0])
	}
	return false
}
