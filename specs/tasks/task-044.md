# Task 044: Default Sort Order and `order` Direction Override Parameter

## Spec Reference

`specs/registry/archetypes/rest-crud/spec.md` — **Response Format > List query parameters**

## Spec Excerpt

> - `orderBy` -- comma-separated, each entry is `field_name` or `field_name asc|desc` (default direction: asc). When omitted, the default sort is `created_time desc` (newest first).
> - `order` -- sort direction override: `asc` or `desc` (applied to `orderBy` field)

## What Changed

Two additions to list query parameter handling:

1. **Default sort order**: When the `orderBy` query parameter is omitted entirely, the generated list handler must default to sorting by `created_time desc` (newest first).
2. **`order` parameter**: A new `?order=asc|desc` query parameter that overrides the direction of all `orderBy` entries. If `orderBy` specifies `name asc` but `order=desc`, the effective sort is `name desc`.

## Acceptance Criteria

1. When `orderBy` is omitted, the generated list handler defaults to `[]OrderByField{{Field: "created_time", Direction: "desc"}}`
2. The `order` query parameter is parsed and validated (must be `asc` or `desc`; invalid values return 400)
3. When `order` is present, it overrides the direction of every entry in `orderBy`
4. When `order` is present but `orderBy` is omitted, the default sort `created_time` uses the `order` direction instead of `desc`
5. OpenAPI spec declares `order` query parameter on all list operations with enum `[asc, desc]`
6. Tests cover: default sort, order override with explicit orderBy, order override with default orderBy, invalid order value
7. `go test ./...` passes

## Files Likely Affected

- `internal/generator/restapi/generator.go`
- `internal/generator/restapi/generator_test.go`

## Progress

`ready-for-review`

## Commits

- f816a61 feat(task-044): add default sort order and `order` direction override parameter
