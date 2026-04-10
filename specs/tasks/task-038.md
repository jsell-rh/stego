# Task 038: Generated Runtime Configuration — PORT Environment Variable

**Spec Reference:** "Generated Runtime Configuration" (rest-crud spec, lines 526-544)

**Status:** `needs-revision`

**Review:** [specs/reviews/task-038.md](../reviews/task-038.md)

**Depends on:** None (all prerequisites complete)

**Blocks:** task-039 (example regeneration)

## Description

The rest-crud archetype spec adds a new "Generated Runtime Configuration" section requiring the generated `main.go` to read the HTTP listen port from the `PORT` environment variable, falling back to `8080`. Currently, the assembler hardcodes the port as `8080` via `fmt.Sprintf(":%d", 8080)`.

### What changes

**`internal/compiler/assembler.go`:**
- Remove the `input.Port` default-fallback logic at line 67 (or deprecate the `Port` field on `AssemblerInput` — the port is no longer a compile-time value, it is a runtime environment variable).
- Update the generated `main.go` to read `PORT` from the environment:
  ```go
  port := os.Getenv("PORT")
  if port == "" {
      port = "8080"
  }
  addr := ":" + port
  ```
- The `"os"` import is already present in generated main.go (used for `DATABASE_URL`).

**`internal/compiler/assembler_test.go`:**
- Update tests that check for the hardcoded port (`8080`) to instead verify the generated code reads `PORT` from `os.Getenv` with `"8080"` as the fallback default.

### What does NOT change

- `DATABASE_URL` handling — already correct per the spec.
- Component-specific env vars (`JWK_CERT_URL`, `AUTH_ENABLED`) — already handled by their respective generators.

## Spec Excerpt

> The generated server must read `PORT` from the environment (falling back to `8080`) rather than hardcoding the port. This is required for testability (integration tests need to run the server on a non-default port) and for container orchestration (port assignment by the platform).
>
> ```go
> port := os.Getenv("PORT")
> if port == "" {
>     port = "8080"
> }
> ```

## Acceptance Criteria

1. Generated `main.go` reads `PORT` from `os.Getenv("PORT")` with `"8080"` fallback.
2. Generated `main.go` no longer hardcodes the port value.
3. Assembler tests verify the `PORT` env var reading pattern in generated code.
4. All tests pass: `go test ./internal/compiler/...`
5. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- `11925e1` feat(assembler): read PORT from env var instead of hardcoding
