# Task 037: Error Code Prefix — Strip Common Suffixes

**Spec Reference:** "Error Handling (RFC 9457)" (rest-crud spec, line 484)

**Status:** `ready-for-review`

**Depends on:** None (all prerequisites complete)

**Blocks:** task-039 (example regeneration)

## Description

The rest-crud archetype spec has amended the error code prefix derivation algorithm. The current implementation (`deriveErrorPrefix` in `internal/generator/restapi/generator.go:1474`) removes hyphens and uppercases the service name. The spec now requires a three-step algorithm:

1. Strip common suffixes: `-api`, `-service`, `-server`
2. Remove remaining hyphens
3. Uppercase

**Current behavior:**
- `hyperfleet-api` → `HYPERFLEETAPI`
- `order-service` → `ORDERSERVICE`
- `user-management` → `USERMANAGEMENT`

**Spec-required behavior:**
- `hyperfleet-api` → `HYPERFLEET` (strip `-api`)
- `order-service` → `ORDER` (strip `-service`)
- `user-management` → `USERMANAGEMENT` (no suffix to strip)

### What changes

**`internal/generator/restapi/generator.go`:**
- Update `deriveErrorPrefix()` to strip `-api`, `-service`, `-server` suffixes before removing hyphens and uppercasing.

**`internal/generator/restapi/generator_test.go`:**
- Update `TestDeriveErrorPrefix` test cases:
  - `hyperfleet-api` → `HYPERFLEET` (was `HYPERFLEETAPI`)
  - `my-cool-service` → `MYCOOL` (was `MYCOOLSERVICE`)
  - Add test case: `order-server` → `ORDER`
  - Add test case: `just-api` → `JUST`
  - Edge case: `api` alone → `API` (no stripping — suffix requires leading hyphen)

## Spec Excerpt

> The error code prefix is derived from the service name by: (1) stripping common suffixes (`-api`, `-service`, `-server`), (2) removing remaining hyphens, (3) uppercasing. Examples: `hyperfleet-api` -> `HYPERFLEET`, `user-management` -> `USERMANAGEMENT`, `order-service` -> `ORDER`.

## Acceptance Criteria

1. `deriveErrorPrefix("hyperfleet-api")` returns `"HYPERFLEET"`.
2. `deriveErrorPrefix("order-service")` returns `"ORDER"`.
3. `deriveErrorPrefix("my-cool-server")` returns `"MYCOOL"`.
4. `deriveErrorPrefix("user-management")` returns `"USERMANAGEMENT"` (no suffix to strip).
5. `deriveErrorPrefix("api")` returns `"API"` (no leading hyphen, not stripped).
6. All tests pass: `go test ./internal/generator/restapi/...`
7. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- b176ed0 fix(restapi): strip common suffixes in deriveErrorPrefix
