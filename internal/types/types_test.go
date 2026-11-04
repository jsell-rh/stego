package types

import (
	"strings"
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
	valid := []Operation{OpCreate, OpRead, OpUpdate, OpDelete, OpList, OpUpsert, OpPatch}
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

func TestCollectionUnmarshalWithImplicit(t *testing.T) {
	input := `
entity: AdapterStatus
operations: [list, upsert]
upsert_key: [resource_type, resource_id, adapter]
scope:
  resource_id: Cluster
implicit:
  resource_type: "Cluster"
`
	var c Collection
	if err := yaml.Unmarshal([]byte(input), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Entity != "AdapterStatus" {
		t.Errorf("entity = %q, want %q", c.Entity, "AdapterStatus")
	}
	if len(c.Implicit) != 1 {
		t.Fatalf("implicit count = %d, want 1", len(c.Implicit))
	}
	if c.Implicit["resource_type"] != "Cluster" {
		t.Errorf("implicit[resource_type] = %q, want %q", c.Implicit["resource_type"], "Cluster")
	}
	if c.Scope["resource_id"] != "Cluster" {
		t.Errorf("scope[resource_id] = %q, want %q", c.Scope["resource_id"], "Cluster")
	}
}

func TestCollectionUnmarshalWithoutImplicit(t *testing.T) {
	input := `
entity: User
operations: [create, read]
`
	var c Collection
	if err := yaml.Unmarshal([]byte(input), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Implicit != nil {
		t.Errorf("implicit = %v, want nil (absent field should be nil)", c.Implicit)
	}
}

func TestServiceDeclarationUnmarshalWithImplicit(t *testing.T) {
	input := `
kind: service
name: adapter-service
archetype: rest-crud
language: go
entities:
  - name: AdapterStatus
    fields:
      - { name: resource_type, type: string }
      - { name: resource_id, type: ref, to: Cluster }
collections:
  cluster-statuses:
    entity: AdapterStatus
    scope: { resource_id: Cluster }
    implicit: { resource_type: "Cluster" }
    operations: [list, upsert]
    upsert_key: [resource_type, resource_id]
`
	var sd ServiceDeclaration
	if err := yaml.Unmarshal([]byte(input), &sd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sd.Collections) != 1 {
		t.Fatalf("collections count = %d, want 1", len(sd.Collections))
	}
	coll := sd.Collections[0]
	if coll.Name != "cluster-statuses" {
		t.Errorf("collection name = %q, want %q", coll.Name, "cluster-statuses")
	}
	if len(coll.Implicit) != 1 {
		t.Fatalf("implicit count = %d, want 1", len(coll.Implicit))
	}
	if coll.Implicit["resource_type"] != "Cluster" {
		t.Errorf("implicit[resource_type] = %q, want %q", coll.Implicit["resource_type"], "Cluster")
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

func TestSlotDeclarationRejectsDeprecatedEntityKey(t *testing.T) {
	input := `
slot: before_create
entity: User
gate:
  - my-policy
`
	var sb SlotDeclaration
	err := yaml.Unmarshal([]byte(input), &sb)
	if err == nil {
		t.Fatal("expected error for deprecated 'entity' key in slot declaration, got nil")
	}
	if !strings.Contains(err.Error(), "renamed to 'collection'") {
		t.Errorf("error message should mention renaming to 'collection', got: %v", err)
	}
}

func TestFillRejectsDeprecatedEntityKey(t *testing.T) {
	input := `
kind: fill
name: my-policy
implements: rest-api.before_create
entity: User
qualified_by: tester
qualified_at: 2026-04-01T00:00:00Z
`
	var f Fill
	err := yaml.Unmarshal([]byte(input), &f)
	if err == nil {
		t.Fatal("expected error for deprecated 'entity' key in fill declaration, got nil")
	}
	if !strings.Contains(err.Error(), "renamed to 'collection'") {
		t.Errorf("error message should mention renaming to 'collection', got: %v", err)
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
