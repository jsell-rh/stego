# Task 040: Regenerate Example Output with Spec Amendments

**Spec Reference:** "Server-Managed Fields and Request Schemas", "Error Handling (RFC 9457)", "Generated Runtime Configuration" (rest-crud spec)

**Status:** `complete`

**Depends on:** task-037, task-038, task-039

**Review:** [specs/reviews/task-040.md](../reviews/task-040.md)

## Description

Regenerate the example services (`examples/user-management/` and `examples/user-management-rhsso/`) to incorporate all three spec amendments from tasks 037-039. Verify the generated output compiles and reflects the updated behaviors.

### What changes

**Regenerate both examples:**
- `cd examples/user-management && stego apply`
- `cd examples/user-management-rhsso && stego apply`

**Verify in generated `main.go`:**
- `PORT` read from env var with `"8080"` fallback (task 038).
- No hardcoded port value.

**Verify in generated `internal/api/errors.go`:**
- Error code prefix for `user-management` is `USERMANAGEMENT` (unchanged — no suffix to strip).

**Verify in generated `internal/api/openapi.json`:**
- `{Entity}CreateRequest` schemas exist, excluding server-managed fields (task 039).
- POST request body `$ref` points to `{Entity}CreateRequest`.

**Verify generated code compiles:**
- `cd examples/user-management/out && go build ./...`
- `cd examples/user-management-rhsso/out && go build ./...`

## Spec Excerpt

> The generated server must read `PORT` from the environment (falling back to `8080`) rather than hardcoding the port.

## Acceptance Criteria

1. Both example services regenerated via `stego apply`.
2. Generated `main.go` reads `PORT` from env var.
3. Generated OpenAPI spec includes `{Entity}CreateRequest` schemas.
4. Generated code compiles in both examples.
5. `go test ./...` passes from the repo root.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- 4da1da0 chore(task-040): regenerate example output with spec amendments from tasks 037-039
