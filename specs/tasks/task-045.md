# Task 045: Regenerate Example Output with List Parameter Changes

## Spec Reference

`specs/registry/archetypes/rest-crud/spec.md` — **Response Format > List query parameters**

## Spec Excerpt

Follows tasks 043 and 044. The example services must be regenerated to reflect:
- `pageSize` query parameter (was `size`)
- Default sort `created_time desc` when `orderBy` omitted
- New `order` direction override query parameter

## Acceptance Criteria

1. Run `stego apply` (or equivalent regeneration) for both example services
2. `examples/user-management/out/` reflects all list parameter changes from tasks 043–044
3. `examples/user-management-rhsso/out/` reflects all list parameter changes from tasks 043–044
4. Generated handlers use `pageSize` query parameter
5. Generated handlers apply default sort `created_time desc`
6. Generated handlers parse `order` query parameter
7. Generated OpenAPI specs declare `pageSize` and `order` parameters
8. No other changes to examples beyond the list parameter updates
9. `go test ./...` passes

## Files Likely Affected

- `examples/user-management/out/internal/api/*.go`
- `examples/user-management-rhsso/out/internal/api/*.go`
- `examples/user-management/out/openapi.yaml` (if generated)
- `examples/user-management-rhsso/out/openapi.yaml` (if generated)

## Progress

`not-started`

## Commits

_(none yet)_
