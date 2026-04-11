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
  - tsl-search
  - otel-tracing
  - health-check

default_auth: jwt-auth

conventions:
  layout: flat
  error_handling: problem-details-rfc
  response_format: envelope
  request_validation: openapi-schema
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
    # path derived: /clusters/{cluster_id}/adapterstatuses

  nodepool-statuses:
    entity: AdapterStatus
    scope: { resource_type: NodePool, resource_id: NodePool }
    operations: [list, upsert]
    upsert_key: [resource_type, resource_id, adapter]
    # path derived: /clusters/{cluster_id}/nodepools/{nodepool_id}/adapterstatuses
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

**List query parameters** (following the rh-trex pattern):
- `page` -- 1-indexed page number (default: 1)
- `size` -- items per page (default: 100, max: 65500)
- `orderBy` -- comma-separated, each entry is `field_name` or `field_name asc|desc` (default direction: asc)
- `search` -- TSL filter expression (see Open Questions)
- `fields` -- sparse fieldset selection, comma-separated field names (`id` is always included even if not listed)

**List response fields:**
- `kind` -- entity name + "List" (e.g. "ClusterList")
- `page` -- the requested page number
- `size` -- the actual number of items returned (may be less than requested on the last page)
- `total` -- total count of matching records across all pages
- `items` -- array of presented entities

**Pagination mechanics:**
- Count total matching records first (`SELECT COUNT(*)`)
- Fetch page via `OFFSET (page-1)*size LIMIT size`
- `orderBy` field names are validated against entity fields; invalid fields are rejected with 400
- SQL injection prevented by field name validation + hardcoded direction strings (only `asc` or `desc`)
- `size` capped at 65500 (PostgreSQL parameter limit); values above are silently clamped

The `rest-api` component generates:
- A presenter function per entity that adds `id`, `kind`, `href` to the response
- List handlers that parse and validate pagination query parameters
- The storage interface gains a `ListOptions` parameter for pagination and ordering

When `response_format` is not set or set to `bare`, entities are returned as plain JSON without wrapping or pagination.

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

Generates GORM-based model structs, DAO layer, and database migrations. Uses GORM as the ORM, following the proven rh-trex/hyperfleet-api pattern.

```yaml
kind: component
name: postgres-adapter
version: 2.0.0
output_namespace: internal/storage

requires: []
provides:
  - storage-adapter

slots: []
```

**Generated model structs** embed a `Meta` base and use GORM tags:
```go
// Meta is the base model, embedded in all entities.
type Meta struct {
    ID          string
    CreatedTime time.Time
    UpdatedTime time.Time
    DeletedAt   gorm.DeletedAt `gorm:"index"`
}

type Cluster struct {
    Meta
    Name   string         `json:"name" gorm:"uniqueIndex;size:53;not null"`
    Spec   datatypes.JSON `json:"spec" gorm:"type:jsonb;not null"`
    Labels datatypes.JSON `json:"labels,omitempty" gorm:"type:jsonb"`
    // ...
}
```

- GORM tags are derived from entity field constraints (unique, min/max length, optional, type)
- `jsonb` fields use `gorm.io/datatypes.JSON`
- `ref` fields generate foreign key relationships
- `computed` fields are included in the model but excluded from create/update inputs (see **Server-Managed Fields and Request Schemas**)
- Soft delete via `gorm.DeletedAt` (all deletes are soft by default)

## Server-Managed Fields and Request Schemas

Not all entity fields belong in API request bodies. The `rest-api` component must generate separate OpenAPI schemas for create and update requests that exclude **server-managed fields** -- fields whose values are assigned or derived by the server, not provided by the client.

A field is server-managed if any of the following apply:

1. **`computed: true`** -- filled by a slot, never client-written
2. **`type: timestamp`** -- server-assigned timestamps (`created_time`, `updated_time`, etc.)
3. **Named `created_by` or `updated_by`** -- populated from the authenticated identity (JWT claims)
4. **Named `generation`** -- server-managed version counter, incremented on mutation

The `id` field (from `Meta`) is always server-assigned and excluded from all request schemas.

**Generated OpenAPI schemas per entity:**

| Schema | Used by | Includes | Excludes |
|--------|---------|----------|----------|
| `{Entity}` | GET responses | All fields + `id`, `kind`, `href` envelope | Nothing |
| `{Entity}CreateRequest` | POST request body | Client-provided fields only | Server-managed fields, `id` |
| `{Entity}PatchRequest` | PATCH request body | `patchable` fields as optional | Everything else |

Example for the `Cluster` entity:

```yaml
# Entity fields from service.yaml
fields:
  - { name: name, type: string, unique: true }
  - { name: spec, type: jsonb }
  - { name: labels, type: jsonb, optional: true }
  - { name: status, type: jsonb, computed: true, filled_by: status-aggregator }
  - { name: generation, type: int32, default: 1 }
  - { name: created_by, type: string }
  - { name: updated_by, type: string }
  - { name: created_time, type: timestamp }
  - { name: updated_time, type: timestamp }
```

Generated `ClusterCreateRequest` schema includes only: `name` (required), `spec` (required), `labels` (optional). The remaining fields are server-managed.

The create handler must populate server-managed fields before persisting:
- `id` -- generate UUID v7
- `created_by`, `updated_by` -- extract from JWT identity in request context using `Attributes["email"]` (falling back to `UserID` if email is empty). This ensures compatibility with OpenAPI specs that type these fields as email format.
- `created_time`, `updated_time` -- set to `time.Now()`
- `generation` -- use the declared `default` value (e.g. `1`)
- `computed` fields -- leave nil/zero; filled asynchronously by their declared fill

The `kind` field in the request body (if present) is validated against the entity name but is not persisted -- it is a client-side type assertion. If absent, the server does not reject the request. If present and wrong, the server returns 400.

**Generated DAO layer** provides per-entity data access:
- `Create(ctx, entity)` -- `g2.Create(entity)`
- `Get(ctx, id)` -- `g2.First(&entity, id)`
- `Replace(ctx, entity)` -- `g2.Save(entity)` (used by both update and patch)
- `Delete(ctx, id)` -- soft delete via GORM
- `List(ctx, listArgs)` -- `g2.Offset(offset).Limit(limit).Find(&list)` with pagination
- `Upsert(ctx, entity, upsertKey, concurrency)` -- `g2.Clauses(clause.OnConflict{...})`
- `Exists(ctx, id)` -- existence check for parent verification

**GenericDao** provides the base for the `tsl-search` component to build queries with ordering, filtering, JOINs, and pagination.

**Migrations** use GORM AutoMigrate at startup. The component generates migration structs registered in order, following the hyperfleet-api migration pattern:
```go
// internal/storage/migrations/001_initial.go
func init() {
    Register("001_initial", func(g2 *gorm.DB) error {
        return g2.AutoMigrate(&Cluster{}, &NodePool{}, &AdapterStatus{})
    })
}
```

**Session factory** manages database connections with a `SessionFactory` interface, supporting both production (PostgreSQL) and test (testcontainers) configurations.

### tsl-search

Integrates the Tree Search Language library (`github.com/yaacov/tree-search-language`) into list handlers. Generates SQL helper functions for parsing `?search=` expressions into parameterized WHERE clauses.

```yaml
kind: component
name: tsl-search
version: 1.0.0
output_namespace: internal/search

requires:
  - storage-adapter

provides:
  - search-engine

slots:
  - name: resolve_field
    proto: stego.components.tsl_search.slots.ResolveField
    default: column-name-lookup
```

The component generates:
- TSL expression parsing and SQL WHERE clause generation (wraps the TSL library)
- Field name validation against entity field definitions (disallowed fields rejected with 400)
- Field-to-column mapping (entity field names to SQL column names, including table prefixes)
- Parameterized queries via squirrel (SQL injection prevention)
- The `rest-api` component generates `?search=` query parameter handling in all list handlers

The `resolve_field` slot allows fills to customize how specific field types map to SQL. Default behavior maps field names directly to column names. Fills can override this for:
- JSONB path queries (`status.conditions.Ready.status` -> `jsonb_path_query_first(...)`)
- Label queries (`labels.region` -> `labels->>'region'`)
- Related table JOINs (field references that cross entity boundaries)

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

## Error Handling (RFC 9457)

The archetype convention `error_handling: problem-details-rfc` directs the `rest-api` component to generate RFC 9457 Problem Details error responses.

**Error response format:**
```json
{
  "type": "https://api.hyperfleet.io/errors/not-found",
  "title": "Not Found",
  "status": 404,
  "detail": "Cluster with id 'abc123' not found",
  "code": "HYPERFLEET-NTF-001",
  "instance": "/api/hyperfleet/v1/clusters/abc123",
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "timestamp": "2026-04-09T12:00:00Z"
}
```

- Content-Type: `application/problem+json`
- `type` -- error category URI (identifier, not necessarily dereferenceable)
- `title` -- human-readable error category
- `status` -- HTTP status code
- `detail` -- context-specific error description
- `code` -- structured error code: `{SERVICE_PREFIX}-{CATEGORY}-{NUMBER}`
- `instance` -- the request path that caused the error
- `trace_id` -- OpenTelemetry trace ID when available
- `timestamp` -- UTC timestamp of the error

**Service declaration configures:**
```yaml
kind: service
name: hyperfleet-api
base_path: /api/hyperfleet/v1
error_type_base: https://api.hyperfleet.io/errors/   # optional, for RFC 9457 type URIs
```

The error code prefix is derived from the service name by: (1) stripping common suffixes (`-api`, `-service`, `-server`), (2) removing remaining hyphens, (3) uppercasing. Examples: `hyperfleet-api` -> `HYPERFLEET`, `user-management` -> `USERMANAGEMENT`, `order-service` -> `ORDER`. Error categories follow a fixed set:

| Category | Codes | HTTP Status | Example |
|----------|-------|-------------|---------|
| VAL | Validation errors | 400 | `HYPERFLEET-VAL-001` |
| AUT | Authentication | 401 | `HYPERFLEET-AUT-001` |
| AUZ | Authorization | 403 | `HYPERFLEET-AUZ-001` |
| NTF | Not found | 404 | `HYPERFLEET-NTF-001` |
| CNF | Conflict | 409 | `HYPERFLEET-CNF-001` |
| INT | Internal error | 500 | `HYPERFLEET-INT-001` |

The `rest-api` component generates:
- A `ServiceError` type with RFC 9457 fields
- Error constructors: `NotFound()`, `BadRequest()`, `Conflict()`, `Validation()`, etc.
- A `handleError` function that serializes errors as Problem Details with `application/problem+json`
- Validation errors include a `validation_errors` array with per-field details

## Request Validation

When `request_validation: openapi-schema` is set in the archetype conventions, the `rest-api` component generates middleware that validates request bodies against the generated OpenAPI spec at runtime.

- The generated OpenAPI spec is loaded at server startup
- POST, PUT, PATCH, and upsert request bodies are validated against the schema
- Validation uses `kin-openapi` (or equivalent) to check required fields, type constraints, min/max/pattern
- Validation failures return RFC 9457 Problem Details with per-field `validation_errors`:

```json
{
  "type": "https://api.hyperfleet.io/errors/validation-error",
  "title": "Validation Error",
  "status": 400,
  "detail": "Invalid ClusterSpec",
  "code": "HYPERFLEET-VAL-000",
  "validation_errors": [
    { "field": "spec.region", "message": "property 'region' is missing" },
    { "field": "spec.diskSize", "message": "number must be at least 10" }
  ]
}
```

Entity field constraints (min_length, max_length, pattern, min, max, required) are already encoded in the generated OpenAPI spec. The validation middleware enforces them at runtime without hand-coded validation functions per entity.

## Generated Runtime Configuration

The generated `main.go` reads runtime configuration from environment variables. These are not configurable in `service.yaml` -- they are deployment concerns.

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | Yes | -- | PostgreSQL connection string |
| `PORT` | No | `8080` | HTTP listen port |

The generated server must read `PORT` from the environment (falling back to `8080`) rather than hardcoding the port. This is required for testability (integration tests need to run the server on a non-default port) and for container orchestration (port assignment by the platform).

```go
port := os.Getenv("PORT")
if port == "" {
    port = "8080"
}
```

Additional environment variables are defined by individual components (e.g. `JWK_CERT_URL` by `rh-sso-auth`, `AUTH_ENABLED` by auth components).

## Open Questions

- Collection naming conventions need enforcement (e.g. `{scope}-{entity-plural}` or `all-{entity-plural}`)

## Path Derivation Rules (must be implemented)

Collection paths must be derived from the entity name (pluralized, lowercased), not from the collection name. The collection name is an identifier for the service declaration; the URL path comes from the entity.

The derivation algorithm: `lowercase(entityName)` then `pluralize`. Multi-word PascalCase names are treated as a single token (e.g. `NodePool` -> `nodepool` -> `nodepools`, `AdapterStatus` -> `adapterstatus` -> `adapterstatuses`). Use `path_prefix` on the collection to override the derived segment when a shorter path is preferred.

Rules:
1. **Unscoped collection:** path = `/{entity_plural}` (e.g. entity `Cluster` -> `/clusters`)
2. **Scoped collection:** path = `/{parent_plural}/{parent_id_param}/{entity_plural}` (e.g. entity `NodePool` scoped to `Cluster` -> `/clusters/{cluster_id}/nodepools`)
3. **Multi-level scope:** chains parent paths recursively (e.g. entity `AdapterStatus` scoped to `NodePool` which is scoped to `Cluster` -> `/clusters/{cluster_id}/nodepools/{nodepool_id}/adapterstatuses`)
4. **`path_prefix` override:** when set on a collection, replaces the derived path entirely (relative to `base_path`)
