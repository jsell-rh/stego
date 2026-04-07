# Task 009: rest-api Component Generator

**Spec Reference:** "File Types — Component", "Code Generation Mechanism", "MVP Scope"

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-009.md](../reviews/task-009.md)

## Description

Implement the `rest-api` component as a Go package with a `Generator`.

- `registry/components/rest-api/component.yaml` — config schema, ports (requires: auth-provider, storage-adapter; provides: http-server, openapi-spec), slots
- Generator produces:
  - HTTP handler files per entity (using `net/http` or chi — standard library preferred)
  - Route registration
  - Middleware wiring (auth)
  - OpenAPI spec generation
  - Support for all operations: create, read, update, delete, list, upsert
  - Nested routing via `parent` on expose blocks
  - Scope filtering
- Output namespace: `internal/api`
- Returns `Wiring` for main.go assembly

## Spec Excerpt

> `rest-api` component (handlers, routes, middleware, OpenAPI)

## Acceptance Criteria

- `component.yaml` defined and parseable
- Generator produces compilable Go handler code for CRUD operations
- Nested routing generates parent existence verification
- OpenAPI spec generated
- Tests verify generated code compiles

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits

- b9c3bc5 feat(task-009): implement rest-api component generator
- 4be9256 chore(task-009): mark task ready-for-review
- 9751749 fix(task-009): address all 5 review findings on rest-api component generator
- 626d10e fix(task-009): address round 2 findings — scope filtering, OpenAPI params, computed fields
- d86da4a fix(task-009): address round 3 findings — JSON header, upsert parent ID, conditional imports
- 9273255 fix(task-009): address round 4 findings — Content-Type header, timestamp zero value, expose schema
- 1de3e33 fix(task-009): address round 5 findings — OpenAPI response content, exported handler methods
- 875ff0b fix(task-009): address round 6 findings — raw field names in parent-only List, OpenAPI scope query param
- 4d8d107 fix(task-009): address round 7 findings — parent ref errors, OpenAPI required, keyword safety
- 328f10e fix(task-009): address round 8 findings — OpenAPI constraints/format and multi-level ancestor verification
- 618498b fix(task-009): address round 9 findings — circular parent detection and OpenAPI default attribute
- a004e98 fix(task-009): address round 10 finding — update handler parent ID assignment
- 63e4cdc fix(task-009): address round 11 finding — reject entity names that collide with generator-internal identifiers
- c0d1d81 fix(task-009): address round 12 finding — guard safeVarName against function-scoped identifiers
- 7ef6a4f fix(task-009): address round 13 findings — namespace parameterization and exposed-only OpenAPI schemas
- 10977c8 fix(task-009): address round 14 finding — reject parent not in expose list
- 20f18d6 fix(task-009): address round 15 finding — extract path parameter names from prefix template
- 2339cb8 chore(task-009): mark task ready-for-review
