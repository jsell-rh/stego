# Task 021: base_path and error_type_base Service Declaration Fields

**Spec Reference:** "Base Path" (rest-crud spec), "Error Handling (RFC 9457)" (rest-crud spec)

**Status:** `not-started`

## Description

The rest-crud archetype spec defines `base_path` as a service-level field prepended to all collection-derived paths, and `error_type_base` as the URI prefix for RFC 9457 error type URIs. Neither is currently in the service declaration schema.

### What changes

**Core types (`internal/types/types.go`):**
- Add `BasePath string` to `ServiceDeclaration`.
- Add `ErrorTypeBase string` to `ServiceDeclaration`.

**Parser:**
- Parse `base_path` and `error_type_base` from service.yaml.

**rest-api generator:**
- When `base_path` is set, prepend it to all collection-derived routes. A collection `clusters` with `entity: Cluster` derives `/clusters`; with `base_path: /api/hyperfleet/v1`, the full path becomes `/api/hyperfleet/v1/clusters`.
- Scoped collection paths are also relative to `base_path`.
- When `path_prefix` is set on a collection, it is also relative to `base_path`.
- If `base_path` is omitted, paths are served from root.
- OpenAPI spec paths include `base_path`.

**Validation:**
- `base_path` must start with `/` if provided.
- `error_type_base` is optional; no format validation beyond non-empty if provided.

## Spec Excerpt

> The service declaration includes a `base_path` that is prepended to all collection-derived paths:
> ```yaml
> kind: service
> name: hyperfleet-api
> archetype: rest-crud
> base_path: /api/hyperfleet/v1
> ```
> Collection paths are relative to `base_path`. If `base_path` is omitted, collection paths are served from the root.

## Acceptance Criteria

1. `base_path` and `error_type_base` fields parse from service.yaml.
2. rest-api generator prepends `base_path` to all routes.
3. OpenAPI paths include `base_path`.
4. `base_path` validation: must start with `/` if set.
5. When `base_path` is omitted, behavior is unchanged (paths from root).
6. Tests cover base_path routing and OpenAPI path generation.
7. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
