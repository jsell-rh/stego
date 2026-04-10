# Task 032: rh-sso-auth Component Definition & Generator

**Spec Reference:** [specs/registry/components/rh-sso-auth/spec.md](../registry/components/rh-sso-auth/spec.md)

**Status:** `ready-for-review`

**Depends on:** task-011 (jwt-auth pattern), task-009 (rest-api generator pattern)

**Blocks:** task-033

## Description

Add the `rh-sso-auth` component to the registry and implement its code generator. This component replaces the MVP `jwt-auth` with production-grade JWT authentication following the rh-trex pattern.

### Registry Definition

Create `registry/components/rh-sso-auth/component.yaml` matching the spec:

```yaml
kind: component
name: rh-sso-auth
version: 1.0.0
output_namespace: internal/auth

config:
  jwk_cert_url:
    type: string
    default: "https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/certs"
  jwk_cert_file:
    type: string
    optional: true
  public_paths:
    type: list
    items:
      type: string
    default: ["/healthcheck", "/metrics"]

requires: []
provides:
  - auth-provider
slots: []
```

### Generator Implementation

Create `internal/generator/rhssoauth/generator.go` implementing `gen.Generator`. The generator produces two files:

**`internal/auth/middleware.go`:**
- `JWTHandler` struct with builder pattern: `NewJWTHandler().WithKeysURL(...).WithKeysFile(...).WithPublicPath(...).Build()`
- `Build()` returns `func(http.Handler) http.Handler` middleware
- JWK key fetching from URL and file with automatic refresh (URL: hourly, file: every 5 minutes)
- JWT parsing and RSA signature validation using `github.com/golang-jwt/jwt/v4`
- Unknown `kid` handling: one-shot refresh with 30-second cooldown
- Thread safety: `sync.RWMutex` protecting key map; reads use `RLock`, writes use `Lock` with atomic map replacement
- Public path bypass with exact matching (no prefix matching)
- `Stop()` method to terminate key refresh goroutine

**`internal/auth/context.go`:**
- `Payload` struct with fields: Username, FirstName, LastName, Email, ClientID, Issuer
- Claim extraction with fallback chains:
  - Username: "username" -> "preferred_username" -> "sub"
  - FirstName: "first_name" -> "given_name" -> split("name")[0]
  - LastName: "last_name" -> "family_name" -> split("name")[1]
  - Email: from "email"
  - ClientID: from "clientId"
  - Issuer: from "iss"
- `GetAuthPayloadFromContext(ctx)` -- extracts payload from JWT claims in context
- `GetAuthPayload(r)` -- convenience wrapper for HTTP requests
- `GetUsernameFromContext(ctx)` / `SetUsernameContext(ctx, username)` -- username accessors
- `TokenFromContext(ctx)` -- raw JWT token accessor

### Wiring

The generator returns a `gen.Wiring` struct with:
- Imports for `github.com/golang-jwt/jwt/v4`
- Constructor info for the JWTHandler builder pattern
- Middleware application info

## Spec Excerpt

> Replaces the MVP `jwt-auth` component with production-grade JWT authentication following the rh-trex pattern. Supports JWK key discovery, RSA signature verification, claim extraction with fallback chains, and configurable public paths.

## Acceptance Criteria

1. `registry/components/rh-sso-auth/component.yaml` exists and matches the spec definition.
2. Generator produces compilable `internal/auth/middleware.go` with JWK key management, RSA validation, builder pattern, and public path bypass.
3. Generator produces compilable `internal/auth/context.go` with Payload struct, claim fallback chains, and context accessors.
4. Generator returns appropriate `gen.Wiring` struct for assembler consumption.
5. Generator tests verify generated code structure and correctness.
6. `go test ./...` passes from the repo root.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
