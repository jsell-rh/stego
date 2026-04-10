# Task 025: Patch Operation with Patchable Fields

**Spec Reference:** "Collections & Operations > Patch" (rest-crud spec)

**Status:** `needs-revision`

**Review:** [specs/reviews/task-025.md](../reviews/task-025.md)

## Description

The rest-crud archetype spec defines `patch` as a distinct operation from `update` (full replace). Patch performs a partial update on only the fields listed in `patchable`. This task adds the `patch` operation and `patchable` field to collections.

### What changes

**Core types (`internal/types/types.go`):**
- Add `OpPatch Operation = "patch"` to the Operation enum and `ValidOperations` map.
- Add `Patchable []string` field to `Collection`.

**Parser:**
- Parse `patchable` field on collections.

**Validation:**
- `patch` in operations requires `patchable` to be set (and vice versa).
- Each field in `patchable` must exist on the referenced entity.
- `patchable` fields must not be `computed` or `ref` type.

**rest-api generator:**
- When a collection includes `patch`, generate a `PATCH /{path}/{id}` handler.
- Generate a patch request struct with pointer fields for only the `patchable` fields (`*string`, `*int32`, `*json.RawMessage`, etc.).
- Handler: fetch existing record (get), apply non-nil fields from patch request, save via Replace.
- OpenAPI schema includes the patch request type.

**postgres-adapter generator:**
- No new DAO function needed — `Replace` (GORM `Save`) handles the merged entity.

## Spec Excerpt

> **Patch (partial update)** is distinct from update (full replace). When a collection includes `patch` in its operations, it must also declare `patchable` -- the list of fields that can be partially updated:
> ```yaml
> collections:
>   clusters:
>     entity: Cluster
>     operations: [create, read, list, patch]
>     patchable: [spec, labels]
> ```
> The generator produces a patch request struct with pointer fields for only the listed fields. A get-then-merge handler fetches the existing record, applies non-nil fields from the patch request, and saves. `patchable` fields must exist on the entity and must not be computed or ref fields.

## Acceptance Criteria

1. `patch` added to Operation enum.
2. `patchable` field added to Collection type and parsed from YAML.
3. Validation enforces: patch requires patchable; patchable fields exist on entity; patchable fields not computed or ref.
4. Patch handler generated with `PATCH /{path}/{id}` route.
5. Patch request struct uses pointer fields for patchable fields only.
6. Get-then-merge logic: fetch, apply non-nil, save.
7. OpenAPI schema includes patch request type.
8. Tests cover patch generation, validation, and edge cases.
9. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- e34e173 feat(task-025): add patch operation with patchable fields
- e9807d6 fix(task-025): address round 1 — patchable timestamp import and OpenAPI 404
- 1a817ec fix(task-025): use pointer types for jsonb/bytes patchable fields per spec
