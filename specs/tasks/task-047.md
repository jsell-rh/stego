# Task 047: CORS Middleware Generation and Assembler Wiring

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **CORS** section and **Generated Runtime Configuration**

**Status:** `not-started`

**Depends on:** task-046

**Blocks:** task-048, task-049

## Description

When `cors: enabled` is set in the archetype conventions, the `rest-api` component must generate a CORS middleware file and the assembler must wire it as the **outermost** middleware layer (before auth, before validation).

### What changes

**`internal/generator/restapi/generator.go`:**
- When `ctx.Conventions.CORS == "enabled"`, generate a `cors.go` file in the rest-api output namespace (`internal/api/`) containing:
  - A CORS middleware function that:
    - Reads `CORS_ALLOWED_ORIGINS` (default `*`), `CORS_ALLOWED_METHODS` (default `GET,POST,PATCH,PUT,DELETE,OPTIONS`), and `CORS_ALLOWED_HEADERS` (default `Content-Type,Authorization`) from environment variables
    - Sets `Access-Control-Allow-Origin`, `Access-Control-Allow-Methods`, `Access-Control-Allow-Headers` response headers on all requests
    - Handles `OPTIONS` preflight requests by returning `204 No Content` immediately
    - Wraps the next handler in the chain
  - A constructor function (e.g. `NewCORSMiddleware()`) that returns the middleware
- Return appropriate wiring metadata so the assembler knows to include CORS middleware.

**`internal/compiler/assembler.go`:**
- When CORS middleware wiring is present, insert it as the **outermost** middleware layer in the generated `main.go` — it wraps everything else (before auth, before validation).
- The generated `main.go` should construct and apply the CORS middleware.

**Tests:**
- `internal/generator/restapi/generator_test.go` — verify `cors.go` is generated when `CORS == "enabled"` and not generated when empty/missing.
- `internal/compiler/assembler_test.go` — verify CORS middleware appears as outermost wrapper in generated `main.go`.

### What does NOT change

- The `overrides: cors: disabled` mechanism — that's task-048.
- Example output — that's task-049.

## Spec Excerpt

> When `cors: enabled` is set in the archetype conventions (the default for `rest-crud`), the `rest-api` component generates CORS middleware that wraps the HTTP handler chain. The middleware:
>
> - Sets `Access-Control-Allow-Origin`, `Access-Control-Allow-Methods`, and `Access-Control-Allow-Headers` on all responses
> - Handles `OPTIONS` preflight requests with a `204 No Content` response
> - Is inserted as the outermost middleware layer (before auth, before validation)
>
> Runtime configuration via environment variables:
>
> | Variable | Default | Description |
> |----------|---------|-------------|
> | `CORS_ALLOWED_ORIGINS` | `*` | Comma-separated list of allowed origins, or `*` for all |
> | `CORS_ALLOWED_METHODS` | `GET,POST,PATCH,PUT,DELETE,OPTIONS` | Allowed HTTP methods |
> | `CORS_ALLOWED_HEADERS` | `Content-Type,Authorization` | Allowed request headers |

## Acceptance Criteria

1. When `CORS == "enabled"`, the rest-api generator emits a `cors.go` file with CORS middleware reading all 3 env vars with correct defaults.
2. The CORS middleware handles `OPTIONS` preflight with `204 No Content`.
3. The assembler wires CORS middleware as the outermost layer in generated `main.go`.
4. When `CORS` is empty/unset, no CORS middleware is generated or wired.
5. Generator tests verify `cors.go` generation and content.
6. Assembler tests verify CORS middleware placement in the middleware chain.
7. All tests pass: `go test ./...`
8. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
