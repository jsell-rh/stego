# Task 043: Rename `size` Query Parameter to `pageSize`

## Spec Reference

`specs/registry/archetypes/rest-crud/spec.md` — **Response Format > List query parameters**

## Spec Excerpt

> **List query parameters** (following the rh-trex pattern):
> - `page` -- 1-indexed page number (default: 1)
> - `pageSize` -- items per page (default: 100, max: 65500)
>
> **Pagination mechanics:**
> - Fetch page via `OFFSET (page-1)*pageSize LIMIT pageSize`
> - `pageSize` capped at 65500 (PostgreSQL parameter limit); values above are silently clamped

## What Changed

The query parameter for items-per-page was renamed from `size` to `pageSize`. The **response** JSON field `"size"` (actual number of items returned) is unchanged — only the request-side query parameter name changes.

## Acceptance Criteria

1. `rest-api` generator emits `r.URL.Query().Get("pageSize")` instead of `r.URL.Query().Get("size")` in all list handlers
2. Generated variable names updated accordingly (`pageSizeStr`, `pageSize` locals)
3. OpenAPI spec declares `pageSize` query parameter (not `size`) on all list operations
4. `ListOptions.Size` internal field name may remain unchanged (internal concern)
5. Response envelope `"size"` JSON key is **not** renamed (it means actual count, not requested page size)
6. All existing tests updated to assert the new parameter name
7. `go test ./...` passes

## Files Likely Affected

- `internal/generator/restapi/generator.go`
- `internal/generator/restapi/generator_test.go`

## Progress

`complete`

## Commits

- `3caed67` feat(task-043): rename `size` query parameter to `pageSize` in list operations
