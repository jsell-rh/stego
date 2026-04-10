# Task 028: Upsert Operation with Conflict Resolution

**Spec Reference:** "Collections & Operations" (rest-crud spec), "Components > postgres-adapter" (rest-crud spec)

**Status:** `complete`

**Review:** [specs/reviews/task-028.md](../reviews/task-028.md)

## Description

The rest-crud archetype spec defines `upsert` as an operation alongside `create`, `read`, `update`, `delete`, `list`, and `patch`. Upsert supports natural-key conflict resolution and optimistic concurrency. Task 022 generates the GORM DAO `Upsert()` method; this task adds the `upsert` operation to the type system, collection parsing, validation, and rest-api handler generation.

### What changes

**Core types (`internal/types/types.go`):**
- Add `OpUpsert Operation = "upsert"` to the Operation enum and `ValidOperations` map.
- Add `UpsertKey []string` field to `Collection` — the natural-key fields for conflict resolution.
- Add `Concurrency string` field to `Collection` — currently only `"optimistic"` is supported.

**Parser:**
- Parse `upsert_key` and `concurrency` from collection YAML.

**Validation:**
- `upsert` in operations requires `upsert_key` to be set.
- `upsert_key` requires `upsert` in operations.
- Each field in `upsert_key` must exist on the referenced entity.
- `upsert_key` fields must not be `computed`.
- `concurrency` must be `"optimistic"` or empty. When `concurrency: optimistic`, the entity should have a generation or version field for conditional updates.

**rest-api generator:**
- When a collection includes `upsert`, generate a `PUT /{path}` handler (no `{id}` — upsert identifies by natural key).
- Handler: decode request body, call DAO `Upsert()` with the collection's `upsert_key` and `concurrency` setting.
- When `concurrency: optimistic`, the handler returns `200 OK` on update, `201 Created` on insert, and `409 Conflict` (or `204 No Content` via short-circuit) when the incoming generation is not newer.
- OpenAPI schema includes the upsert endpoint with the entity's request body.

**postgres-adapter generator (coordination with Task 022):**
- The DAO `Upsert()` method (from Task 022) accepts `upsertKey []string` and `concurrency string`.
- `upsert_key` maps to `clause.OnConflict{ Columns: [...] }` in GORM.
- `concurrency: optimistic` adds a WHERE condition to the ON CONFLICT UPDATE clause (e.g. `WHERE excluded.generation > target.generation`).

## Spec Excerpt

> **Operations** include `create`, `read`, `update`, `delete`, `list`, `upsert`, and `patch`. Upsert supports natural-key conflict resolution and optimistic concurrency:
> ```yaml
> collections:
>   cluster-statuses:
>     entity: AdapterStatus
>     scope: { resource_type: Cluster, resource_id: Cluster }
>     operations: [list, upsert]
>     upsert_key: [resource_type, resource_id, adapter]
>     concurrency: optimistic    # only update if generation is newer
> ```

## Acceptance Criteria

1. `upsert` added to Operation enum and ValidOperations.
2. `UpsertKey` and `Concurrency` fields added to Collection type and parsed from YAML.
3. Validation enforces: upsert requires upsert_key; upsert_key fields exist on entity; upsert_key fields not computed; concurrency value is valid.
4. Upsert handler generated with `PUT /{path}` route (no `{id}` suffix).
5. Handler calls DAO `Upsert()` with upsert_key and concurrency.
6. Optimistic concurrency returns appropriate status codes (200/201/409).
7. OpenAPI schema includes upsert endpoint.
8. Tests cover upsert generation, validation, and edge cases.
9. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- `db56356` feat(task-028): add upsert operation with conflict resolution
- `a2ba2fd` fix(task-028): wrap upsert COUNT+INSERT in transaction and check COUNT error
- `ca04452` fix(task-028): set SERIALIZABLE isolation and add upsert scope identifiers
