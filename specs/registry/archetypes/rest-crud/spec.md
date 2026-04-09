# rest-crud Archetype Specification

The `rest-crud` archetype generates REST API services with PostgreSQL storage, JWT authentication, OpenTelemetry tracing, and health checks.

## Archetype Definition

```yaml
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
  response_format: envelope
  logging: structured-json
  test_pattern: table-driven

compatible_mixins:
  - event-publisher
  - async-worker

bindings:
  storage-adapter: postgres-adapter
  auth-provider: jwt-auth
```

## Entity Type System

Types and constraints are essential complexity -- they describe what the data IS, not how it's stored or validated. They belong in the IR. The component decides HOW to enforce them (SQL constraints, middleware, ORM annotations).

Primitive types (aligned with protobuf): `string`, `int32`, `int64`, `float`, `double`, `bool`, `bytes`, `timestamp`.

Stego-specific types: `enum`, `ref`, `jsonb`.

Constraint attributes (finite set, not extensible):
- `min_length`, `max_length`, `pattern` (strings)
- `min`, `max` (numerics)
- `unique`, `unique_composite: [field1, field2]`
- `optional`, `default`
- `values: [a, b, c]` (enums)
- `computed: true, filled_by: <fill>` (derived fields)

```yaml
fields:
  - { name: email, type: string, max_length: 255, unique: true }
  - { name: name, type: string, min_length: 3, max_length: 53, pattern: "^[a-z0-9]" }
  - { name: role, type: enum, values: [admin, member] }
  - { name: org_id, type: ref, to: Organization }
  - { name: metadata, type: jsonb, optional: true }
  - { name: status, type: jsonb, computed: true, filled_by: status-aggregator }
```

If a constraint can't be expressed with these attributes, it becomes a fill -- that's the boundary between "what the data is" and "domain logic."

## Collections & Operations

**Operations** include `create`, `read`, `update`, `delete`, `list`, `upsert`, and `patch`. Upsert supports natural-key conflict resolution and optimistic concurrency:
```yaml
collections:
  cluster-statuses:
    entity: AdapterStatus
    scope: { resource_type: Cluster, resource_id: Cluster }
    operations: [list, upsert]
    upsert_key: [resource_type, resource_id, adapter]
    concurrency: optimistic    # only update if generation is newer
```

**Scoped collections** generate nested routing. The `scope` field maps entity fields to parent entities. The compiler derives the URL path and generates parent existence verification at each level:
```yaml
collections:
  clusters:
    entity: Cluster
    operations: [create, read, list]
    # path derived: /clusters

  cluster-nodepools:
    entity: NodePool
    scope: { cluster_id: Cluster }
    operations: [create, read, list]
    # path derived: /clusters/{cluster_id}/nodepools

  all-nodepools:
    entity: NodePool
    operations: [list]
    # path derived: /nodepools

  cluster-statuses:
    entity: AdapterStatus
    scope: { resource_type: Cluster, resource_id: Cluster }
    operations: [list, upsert]
    upsert_key: [resource_type, resource_id, adapter]
    # path derived: /clusters/{cluster_id}/statuses

  nodepool-statuses:
    entity: AdapterStatus
    scope: { resource_type: NodePool, resource_id: NodePool }
    operations: [list, upsert]
    upsert_key: [resource_type, resource_id, adapter]
    # path derived: /clusters/{cluster_id}/nodepools/{nodepool_id}/statuses
```

Multiple collections referencing the same entity is the normal case. Each collection generates its own handler, routes, and wiring. The entity struct and storage are shared.

**Patch (partial update)** is distinct from update (full replace). When a collection includes `patch` in its operations, it must also declare `patchable` -- the list of fields that can be partially updated:
```yaml
collections:
  clusters:
    entity: Cluster
    operations: [create, read, list, patch]
    patchable: [spec, labels]
```

The generator produces a patch request struct with pointer fields for only the listed fields (`*string`, `*int32`, `*json.RawMessage`, etc.). A get-then-merge handler fetches the existing record, applies non-nil fields from the patch request, and saves. `patchable` fields must exist on the entity and must not be computed or ref fields. `patch` requires `patchable` and vice versa (bidirectional dependency).

## Base Path

The service declaration includes a `base_path` that is prepended to all collection-derived paths:

```yaml
kind: service
name: hyperfleet-api
archetype: rest-crud
base_path: /api/hyperfleet/v1
```

Collection paths are relative to `base_path`. A collection `clusters` with `entity: Cluster` derives the relative path `/clusters`; the full URL becomes `/api/hyperfleet/v1/clusters`. A scoped collection with `scope: { cluster_id: Cluster }` derives `/clusters/{cluster_id}/nodepools`; the full URL becomes `/api/hyperfleet/v1/clusters/{cluster_id}/nodepools`.

When `path_prefix` is set on a collection, it is also relative to `base_path`.

If `base_path` is omitted, collection paths are served from the root (e.g. `/clusters`).

## Response Format

When `response_format: envelope` is set in the archetype conventions, the `rest-api` component wraps all responses:

**Single resource responses** include `id`, `kind`, and `href` metadata:
```json
{
  "id": "abc123",
  "kind": "Cluster",
  "href": "/api/hyperfleet/v1/clusters/abc123",
  "name": "my-cluster",
  "spec": {}
}
```

- `id` -- auto-generated UUID, assigned on create
- `kind` -- derived from entity name
- `href` -- `base_path` + collection path + `id`

**List responses** wrap items in a pagination envelope:
```json
{
  "kind": "ClusterList",
  "page": 1,
  "size": 10,
  "total": 42,
  "items": [...]
}
```

The `rest-api` component generates:
- A presenter function per entity that adds `id`, `kind`, `href` to the response
- List handlers that accept `page`, `size`, `orderBy`, `order` query parameters
- List responses with `kind` (entity name + "List"), `page`, `size`, `total`, and `items`
- The storage interface gains a `ListOptions` parameter for pagination and ordering

When `response_format` is not set or set to `bare`, entities are returned as plain JSON without wrapping.

**Computed/derived fields** are read-only fields populated by a fill, never written via the API:
```yaml
entities:
  - name: Cluster
    fields:
      - { name: name, type: string, unique: true }
      - { name: spec, type: jsonb }
      - { name: status_conditions, type: jsonb, computed: true, filled_by: status-aggregator }
```

## Slot/Fill Contract

Slots are defined as protobuf service definitions. Each component owns its own slot protos (no sharing between components -- duplication is cheaper than coupling). Fills implement the generated interface in the target language.

Common stable types (Identity, SlotResult) live in a shared `stego.common` proto package. The bar for inclusion is very high.

```protobuf
// registry/components/rest-api/slots/before_create.proto
syntax = "proto3";
package stego.components.rest_api.slots;
import "stego/common/types.proto";

service BeforeCreate {
  rpc Evaluate(BeforeCreateRequest) returns (stego.common.SlotResult);
}

message BeforeCreateRequest {
  stego.common.CreateRequest input = 1;
  stego.common.Identity caller = 2;
}
```

**Short-circuit chains** allow a step to halt the pipeline and return a result early. The slot proto includes a `halt` field:
```yaml
slots:
  - collection: cluster-statuses
    slot: process_adapter_status
    chain:
      - validate-mandatory-conditions    # can halt with 400
      - discard-stale-generation         # can halt with 204 (no-op)
      - persist-status
      - aggregate-resource-status
    short_circuit: true                  # enables halt semantics
```
```protobuf
message SlotResult {
  bool ok = 1;
  string error_message = 2;
  bool halt = 3;           // stop the chain, return this result
  int32 status_code = 4;   // HTTP status for the halted response
}
```

## Components

### rest-api

Generates HTTP handlers (one per collection), route registration, OpenAPI spec, and a Storage interface.

```yaml
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
```

### postgres-adapter

Generates Go model structs, SQL queries, and database migrations. Migration diffing strategy is owned by this component (not by stego core).

```yaml
kind: component
name: postgres-adapter
version: 1.4.0
output_namespace: internal/storage

requires: []
provides:
  - storage-adapter

slots: []
```

### jwt-auth

Generates JWT middleware with configurable header and claim extraction.

```yaml
kind: component
name: jwt-auth
version: 1.0.0
output_namespace: internal/auth

requires: []
provides:
  - auth-provider

slots: []
```

## Open Questions

- How to handle complex query patterns (TSL search) -- reusable component or archetype concern?
- Collection path derivation rules need to be specified precisely (how does `scope: { cluster_id: Cluster }` become `/clusters/{cluster_id}/nodepools`?)
- Collection naming conventions need enforcement (e.g. `{scope}-{entity-plural}` or `all-{entity-plural}`)
- Envelope response format (id, kind, href wrapping; list pagination) -- component config or archetype convention?
- RFC 9457 Problem Details error responses -- the archetype declares `error_handling: problem-details-rfc` but the rest-api generator must implement it
