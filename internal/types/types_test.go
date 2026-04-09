package types

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFieldTypeValidation(t *testing.T) {
	valid := []FieldType{
		FieldTypeString, FieldTypeInt32, FieldTypeInt64,
		FieldTypeFloat, FieldTypeDouble, FieldTypeBool,
		FieldTypeBytes, FieldTypeTimestamp,
		FieldTypeEnum, FieldTypeRef, FieldTypeJsonb,
	}
	for _, ft := range valid {
		if !ValidFieldTypes[ft] {
			t.Errorf("expected %q to be a valid field type", ft)
		}
	}
	if ValidFieldTypes["invalid"] {
		t.Error("expected 'invalid' to not be a valid field type")
	}
}

func TestOperationValidation(t *testing.T) {
	valid := []Operation{OpCreate, OpRead, OpUpdate, OpDelete, OpList, OpUpsert}
	for _, op := range valid {
		if !ValidOperations[op] {
			t.Errorf("expected %q to be a valid operation", op)
		}
	}
	if ValidOperations["invalid"] {
		t.Error("expected 'invalid' to not be a valid operation")
	}
}

func TestFieldUnmarshal(t *testing.T) {
	input := `
name: email
type: string
max_length: 255
unique: true
`
	var f Field
	if err := yaml.Unmarshal([]byte(input), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Name != "email" {
		t.Errorf("name = %q, want %q", f.Name, "email")
	}
	if f.Type != FieldTypeString {
		t.Errorf("type = %q, want %q", f.Type, FieldTypeString)
	}
	if f.MaxLength == nil || *f.MaxLength != 255 {
		t.Errorf("max_length = %v, want 255", f.MaxLength)
	}
	if !f.Unique {
		t.Error("unique = false, want true")
	}
}

func TestFieldUnmarshalEnum(t *testing.T) {
	input := `
name: role
type: enum
values: [admin, member]
`
	var f Field
	if err := yaml.Unmarshal([]byte(input), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Type != FieldTypeEnum {
		t.Errorf("type = %q, want %q", f.Type, FieldTypeEnum)
	}
	if len(f.Values) != 2 || f.Values[0] != "admin" || f.Values[1] != "member" {
		t.Errorf("values = %v, want [admin, member]", f.Values)
	}
}

func TestFieldUnmarshalRef(t *testing.T) {
	input := `
name: org_id
type: ref
to: Organization
`
	var f Field
	if err := yaml.Unmarshal([]byte(input), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Type != FieldTypeRef {
		t.Errorf("type = %q, want %q", f.Type, FieldTypeRef)
	}
	if f.To != "Organization" {
		t.Errorf("to = %q, want %q", f.To, "Organization")
	}
}

func TestFieldUnmarshalComputed(t *testing.T) {
	input := `
name: status_conditions
type: jsonb
computed: true
filled_by: status-aggregator
`
	var f Field
	if err := yaml.Unmarshal([]byte(input), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !f.Computed {
		t.Error("computed = false, want true")
	}
	if f.FilledBy != "status-aggregator" {
		t.Errorf("filled_by = %q, want %q", f.FilledBy, "status-aggregator")
	}
}

func TestEntityUnmarshal(t *testing.T) {
	input := `
name: User
fields:
  - { name: email, type: string, unique: true }
  - { name: role, type: enum, values: [admin, member] }
`
	var e Entity
	if err := yaml.Unmarshal([]byte(input), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Name != "User" {
		t.Errorf("name = %q, want %q", e.Name, "User")
	}
	if len(e.Fields) != 2 {
		t.Fatalf("fields count = %d, want 2", len(e.Fields))
	}
}

func TestCollectionUnmarshal(t *testing.T) {
	input := `
entity: AdapterStatus
operations: [list, upsert]
upsert_key: [resource_type, resource_id, adapter]
concurrency: optimistic
path_prefix: /clusters/{cluster_id}/nodepools/{nodepool_id}/statuses
scope:
  nodepool_id: NodePool
`
	var c Collection
	if err := yaml.Unmarshal([]byte(input), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Entity != "AdapterStatus" {
		t.Errorf("entity = %q, want %q", c.Entity, "AdapterStatus")
	}
	if len(c.Operations) != 2 {
		t.Fatalf("operations count = %d, want 2", len(c.Operations))
	}
	if c.Operations[0] != OpList || c.Operations[1] != OpUpsert {
		t.Errorf("operations = %v, want [list, upsert]", c.Operations)
	}
	if len(c.UpsertKey) != 3 {
		t.Errorf("upsert_key count = %d, want 3", len(c.UpsertKey))
	}
	if c.Concurrency != ConcurrencyOptimistic {
		t.Errorf("concurrency = %q, want %q", c.Concurrency, ConcurrencyOptimistic)
	}
	if c.Scope["nodepool_id"] != "NodePool" {
		t.Errorf("scope[nodepool_id] = %q, want %q", c.Scope["nodepool_id"], "NodePool")
	}
}

func TestSlotDeclarationUnmarshal(t *testing.T) {
	input := `
slot: before_create
collection: org-users
gate:
  - rbac-policy
  - admin-creation-policy
`
	var sb SlotDeclaration
	if err := yaml.Unmarshal([]byte(input), &sb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sb.Slot != "before_create" {
		t.Errorf("slot = %q, want %q", sb.Slot, "before_create")
	}
	if sb.Collection != "org-users" {
		t.Errorf("collection = %q, want %q", sb.Collection, "org-users")
	}
	if len(sb.Gate) != 2 {
		t.Fatalf("gate count = %d, want 2", len(sb.Gate))
	}
}

func TestSlotDeclarationChainUnmarshal(t *testing.T) {
	input := `
slot: process_adapter_status
chain:
  - validate-mandatory-conditions
  - discard-stale-generation
  - persist-status
  - aggregate-resource-status
short_circuit: true
`
	var sb SlotDeclaration
	if err := yaml.Unmarshal([]byte(input), &sb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sb.Chain) != 4 {
		t.Fatalf("chain count = %d, want 4", len(sb.Chain))
	}
	if !sb.ShortCircuit {
		t.Error("short_circuit = false, want true")
	}
}

func TestServiceDeclarationUnmarshal(t *testing.T) {
	input := `
kind: service
name: user-management
archetype: rest-crud
language: go
entities:
  - name: User
    fields:
      - { name: email, type: string, unique: true }
      - { name: role, type: enum, values: [admin, member] }
      - { name: org_id, type: ref, to: Organization }
  - name: Organization
    fields:
      - { name: name, type: string, unique: true }
collections:
  organizations:
    entity: Organization
    operations: [create, read]
  org-users:
    entity: User
    scope: { org_id: Organization }
    operations: [create, read, update, list]
slots:
  - slot: before_create
    collection: org-users
    gate:
      - rbac-policy
      - admin-creation-policy
  - slot: on_entity_changed
    collection: org-users
    fan-out:
      - user-change-notifier
      - audit-logger
mixins:
  - event-publisher
overrides:
  jwt-auth:
    header: X-Internal-Token
`
	var sd ServiceDeclaration
	if err := yaml.Unmarshal([]byte(input), &sd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sd.Kind != "service" {
		t.Errorf("kind = %q, want %q", sd.Kind, "service")
	}
	if sd.Name != "user-management" {
		t.Errorf("name = %q, want %q", sd.Name, "user-management")
	}
	if sd.Archetype != "rest-crud" {
		t.Errorf("archetype = %q, want %q", sd.Archetype, "rest-crud")
	}
	if len(sd.Entities) != 2 {
		t.Errorf("entities count = %d, want 2", len(sd.Entities))
	}
	if len(sd.Collections) != 2 {
		t.Errorf("collections count = %d, want 2", len(sd.Collections))
	}
	if sd.Collections[0].Name != "organizations" {
		t.Errorf("collections[0].name = %q, want %q", sd.Collections[0].Name, "organizations")
	}
	if sd.Collections[0].Entity != "Organization" {
		t.Errorf("collections[0].entity = %q, want %q", sd.Collections[0].Entity, "Organization")
	}
	if sd.Collections[1].Name != "org-users" {
		t.Errorf("collections[1].name = %q, want %q", sd.Collections[1].Name, "org-users")
	}
	if sd.Collections[1].Scope["org_id"] != "Organization" {
		t.Errorf("collections[1].scope = %v, want {org_id: Organization}", sd.Collections[1].Scope)
	}
	if len(sd.Slots) != 2 {
		t.Errorf("slots count = %d, want 2", len(sd.Slots))
	}
	if sd.Slots[0].Collection != "org-users" {
		t.Errorf("slots[0].collection = %q, want %q", sd.Slots[0].Collection, "org-users")
	}
	if len(sd.Mixins) != 1 || sd.Mixins[0] != "event-publisher" {
		t.Errorf("mixins = %v, want [event-publisher]", sd.Mixins)
	}
	if sd.Overrides == nil {
		t.Error("overrides = nil, want non-nil")
	}
}

func TestArchetypeUnmarshal(t *testing.T) {
	input := `
kind: archetype
name: rest-crud
language: go
version: 3.0.0
components:
  - rest-api
  - postgres-adapter
  - otel-tracing
  - health-check
default_auth: jwt-auth
conventions:
  layout: flat
  error_handling: problem-details-rfc
  logging: structured-json
  test_pattern: table-driven
compatible_mixins:
  - event-publisher
  - async-worker
bindings:
  storage-adapter: postgres-adapter
  auth-provider: jwt-auth
`
	var a Archetype
	if err := yaml.Unmarshal([]byte(input), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Name != "rest-crud" {
		t.Errorf("name = %q, want %q", a.Name, "rest-crud")
	}
	if len(a.Components) != 4 {
		t.Errorf("components count = %d, want 4", len(a.Components))
	}
	if a.DefaultAuth != "jwt-auth" {
		t.Errorf("default_auth = %q, want %q", a.DefaultAuth, "jwt-auth")
	}
	if a.Conventions.Layout != "flat" {
		t.Errorf("conventions.layout = %q, want %q", a.Conventions.Layout, "flat")
	}
	if len(a.Bindings) != 2 {
		t.Errorf("bindings count = %d, want 2", len(a.Bindings))
	}
}

func TestComponentUnmarshalWithConfig(t *testing.T) {
	// Uses the spec's full component YAML, including nested items for both
	// map-of-fields (expose.items) and leaf-list (operations.items) forms.
	input := `
kind: component
name: rest-api
version: 2.1.0
config:
  port:
    type: int
    default: 8080
  expose:
    type: list
    items:
      entity:
        type: entity-ref
      operations:
        type: list
        items:
          enum: [create, read, update, delete, list]
      scope:
        type: field-ref
        optional: true
requires:
  - auth-provider
provides:
  - http-server
`
	var c Component
	if err := yaml.Unmarshal([]byte(input), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	expose, ok := c.Config["expose"]
	if !ok {
		t.Fatal("config missing 'expose' key")
	}
	if expose.Type != "list" {
		t.Errorf("config.expose.type = %q, want %q", expose.Type, "list")
	}
	// expose.items is a map of named sub-fields
	if expose.Items == nil || expose.Items.Fields == nil {
		t.Fatal("config.expose.items.Fields is nil, want map with 3 entries")
	}
	if len(expose.Items.Fields) != 3 {
		t.Fatalf("config.expose.items.Fields count = %d, want 3", len(expose.Items.Fields))
	}
	entityItem, ok := expose.Items.Fields["entity"]
	if !ok {
		t.Fatal("config.expose.items missing 'entity'")
	}
	if entityItem.Type != "entity-ref" {
		t.Errorf("config.expose.items.entity.type = %q, want %q", entityItem.Type, "entity-ref")
	}
	scopeItem := expose.Items.Fields["scope"]
	if !scopeItem.Optional {
		t.Error("config.expose.items.scope.optional = false, want true")
	}
	// operations.items is a single inline ConfigField (leaf-list)
	opsItem, ok := expose.Items.Fields["operations"]
	if !ok {
		t.Fatal("config.expose.items missing 'operations'")
	}
	if opsItem.Type != "list" {
		t.Errorf("config.expose.items.operations.type = %q, want %q", opsItem.Type, "list")
	}
	if opsItem.Items == nil || opsItem.Items.Inline == nil {
		t.Fatal("config.expose.items.operations.items.Inline is nil, want inline ConfigField")
	}
	if len(opsItem.Items.Inline.Enum) != 5 {
		t.Errorf("operations.items.enum count = %d, want 5", len(opsItem.Items.Inline.Enum))
	}
	if opsItem.Items.Inline.Enum[0] != "create" {
		t.Errorf("operations.items.enum[0] = %q, want %q", opsItem.Items.Inline.Enum[0], "create")
	}
}

func TestComponentUnmarshal(t *testing.T) {
	input := `
kind: component
name: rest-api
version: 2.1.0
output_namespace: internal/api
requires:
  - auth-provider
  - storage-adapter
provides:
  - http-server
  - openapi-spec
slots:
  - name: before_create
    proto: stego.components.rest_api.slots.BeforeCreate
    default: passthrough
  - name: validate
    proto: stego.components.rest_api.slots.Validate
    default: passthrough
`
	var c Component
	if err := yaml.Unmarshal([]byte(input), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Name != "rest-api" {
		t.Errorf("name = %q, want %q", c.Name, "rest-api")
	}
	if c.OutputNamespace != "internal/api" {
		t.Errorf("output_namespace = %q, want %q", c.OutputNamespace, "internal/api")
	}
	if len(c.Requires) != 2 {
		t.Errorf("requires count = %d, want 2", len(c.Requires))
	}
	if c.Requires[0].Name != "auth-provider" {
		t.Errorf("requires[0].name = %q, want %q", c.Requires[0].Name, "auth-provider")
	}
	if c.Requires[1].Name != "storage-adapter" {
		t.Errorf("requires[1].name = %q, want %q", c.Requires[1].Name, "storage-adapter")
	}
	if len(c.Provides) != 2 {
		t.Errorf("provides count = %d, want 2", len(c.Provides))
	}
	if c.Provides[0].Name != "http-server" {
		t.Errorf("provides[0].name = %q, want %q", c.Provides[0].Name, "http-server")
	}
	if len(c.Slots) != 2 {
		t.Errorf("slots count = %d, want 2", len(c.Slots))
	}
	if c.Slots[0].Default != "passthrough" {
		t.Errorf("slots[0].default = %q, want %q", c.Slots[0].Default, "passthrough")
	}
}

func TestMixinUnmarshal(t *testing.T) {
	input := `
kind: mixin
name: event-publisher
version: 1.0.0
adds_components:
  - kafka-producer
adds_slots:
  - name: on_entity_changed
    proto: stego.mixins.event_publisher.slots.OnEntityChanged
    default: noop
overrides: none
`
	var m Mixin
	if err := yaml.Unmarshal([]byte(input), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Name != "event-publisher" {
		t.Errorf("name = %q, want %q", m.Name, "event-publisher")
	}
	if len(m.AddsComponents) != 1 || m.AddsComponents[0] != "kafka-producer" {
		t.Errorf("adds_components = %v, want [kafka-producer]", m.AddsComponents)
	}
	if len(m.AddsSlots) != 1 {
		t.Errorf("adds_slots count = %d, want 1", len(m.AddsSlots))
	}
	if m.Overrides != "none" {
		t.Errorf("overrides = %q, want %q", m.Overrides, "none")
	}
}

func TestFillUnmarshal(t *testing.T) {
	input := `
kind: fill
name: admin-creation-policy
implements: rest-api.before_create
collection: org-users
qualified_by: jsell
qualified_at: 2026-04-06T00:00:00Z
`
	var f Fill
	if err := yaml.Unmarshal([]byte(input), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Name != "admin-creation-policy" {
		t.Errorf("name = %q, want %q", f.Name, "admin-creation-policy")
	}
	if f.Implements != "rest-api.before_create" {
		t.Errorf("implements = %q, want %q", f.Implements, "rest-api.before_create")
	}
	if f.Collection != "org-users" {
		t.Errorf("collection = %q, want %q", f.Collection, "org-users")
	}
	if f.QualifiedBy != "jsell" {
		t.Errorf("qualified_by = %q, want %q", f.QualifiedBy, "jsell")
	}
}
