# Task 009: rest-api Component Generator

**Spec Reference:** "File Types — Component", "Code Generation Mechanism", "MVP Scope"

**Status:** `ready-for-review`

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
