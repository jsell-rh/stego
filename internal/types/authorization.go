package types

// Authorization is the authorization setting of a declaration (ST-DECL-EN11): the provider that supplies the decision
// and relationship ports, the path of the service's own .zed schema in its source, the subject type that a request's
// identity maps to, and the subject type that a system principal acts as.
type Authorization struct {
	// Provider names the provider of the decision and relationship ports, such as spicedb.
	Provider string `yaml:"provider,omitempty" json:",omitempty"`
	// SchemaPath is the path of the .zed schema in the project folder, such as authz/schema.zed.
	SchemaPath string `yaml:"schema_path,omitempty" json:",omitempty"`
	// SubjectFromIdentity is the subject type of the schema that a request's verified identity maps to, such as user.
	SubjectFromIdentity string `yaml:"subject_from_identity,omitempty" json:",omitempty"`
	// SystemSubjectType is the subject type of the schema that a system principal acts as, such as service_account.
	// A declaration needs it when a slot binding runs as system.
	SystemSubjectType string `yaml:"system_subject_type,omitempty" json:",omitempty"`
}

// Relationship is one relationship tuple that the generated code writes for an entity on create and removes on
// delete, through the outbox (ST-DECL-EN12): the relation under the entity's resource type, where the subject comes
// from, and the subject's type in the schema.
type Relationship struct {
	// Relation is the relation of the schema, under the resource type of the entity, such as owner.
	Relation string `yaml:"relation,omitempty" json:",omitempty"`
	// SubjectSource is field or caller: where the subject identifier comes from.
	SubjectSource string `yaml:"subject_source,omitempty" json:",omitempty"`
	// SubjectField is the field of the entity that holds the subject identifier, when the source is field.
	SubjectField string `yaml:"subject_field,omitempty" json:",omitempty"`
	// SubjectType is the type of the subject in the schema, such as user.
	SubjectType string `yaml:"subject_type,omitempty" json:",omitempty"`
}

// Subject sources of a relationship.
const (
	SubjectSourceField  = "field"
	SubjectSourceCaller = "caller"
)

// PermissionPublic is the value of an operation in Permissions that declares the operation public. A public
// operation needs a reason in PublicReasons.
const PermissionPublic = "public"

// EntityAuthorization holds what an entity states for authorization (part of ST-DECL-EN02): its resource type in
// the schema, the permission checked per operation, the reason for each public operation, the request fields
// passed to caveats and the relationships its rows write. It is inline in Entity.
type EntityAuthorization struct {
	// ResourceType is the type of the entity in the schema, such as document.
	ResourceType string `yaml:"resource_type,omitempty" json:",omitempty"`
	// Permissions maps each operation to the permission of the resource type that it checks, or to public.
	Permissions map[Operation]string `yaml:"permissions,omitempty" json:",omitempty"`
	// PublicReasons gives the reason for each operation that Permissions declares public.
	PublicReasons map[Operation]string `yaml:"public_reasons,omitempty" json:",omitempty"`
	// ContextFields are the request fields passed to caveats, such as request.ip. The name after request. is a
	// caveat parameter of the schema.
	ContextFields []string `yaml:"context_fields,omitempty" json:",omitempty"`
	// ExpectedCardinality is small or large; empty is large. A list of an entity of small expected cardinality reads
	// the rows that the provider's resource search gives; a large one pages the table and decides each page with one
	// batch evaluation (ST-DECL-EN02, ST-SUP-RP11-LC20).
	ExpectedCardinality string `yaml:"expected_cardinality,omitempty" json:",omitempty"`
	// Relationships are the tuples the generated code writes for each row.
	Relationships []Relationship `yaml:"relationships,omitempty" json:",omitempty"`
	// References maps each field that refers to an entity with a resource type to the relation of the service's
	// schema that the runtime writes from this row to the referenced row, and the permission that a create and a move
	// decide on the referenced row (ST-DECL-EN02, ST-DECL-INV85).
	References map[string]Reference `yaml:"references,omitempty" json:",omitempty"`
}

// Reference is the relation and the permission of one reference to an entity with a resource type, named as the
// service's own schema names them (ST-DECL-EN02).
type Reference struct {
	Relation   string `yaml:"relation"`
	Permission string `yaml:"permission"`
}

// The expected cardinalities of an entity (ST-DECL-EN02).
const (
	CardinalitySmall = "small"
	CardinalityLarge = "large"
)

// Principals that the fills of a slot binding run as.
const (
	RunsAsTenant = "tenant"
	RunsAsSystem = "system"
)

// TenantSystem is the tenant of an entity that has no tenant and is never exposed through a tenant request.
const TenantSystem = "system"

// SystemSubjectType is the one subject type that a system principal acts as: the authorization runtime calls every
// system principal service_account:<name> (ST-SUP-RP06-LC07).
const SystemSubjectType = "service_account"

// FloorMarkerType is the definition of the floor marker that the authorization generator adds to the schema it
// writes to the provider (ST-SUP-RP06-LC13), so the service's own schema does not define it.
const FloorMarkerType = "stego_floor"

// SlotAuthorization holds what a slot binding states for authorization (part of ST-DECL-EN05): the relations, as
// resource_type#relation, that its fills may write. It is inline in SlotDeclaration.
type SlotAuthorization struct {
	// RelationshipsWritten are the relations that a fill bound to the slot may write, as resource_type#relation.
	RelationshipsWritten []string `yaml:"relationships_written,omitempty" json:",omitempty"`
}
