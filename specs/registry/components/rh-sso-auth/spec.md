# rh-sso-auth Component Specification

Replaces the MVP `jwt-auth` component with production-grade JWT authentication following the rh-trex pattern. Supports JWK key discovery, RSA signature verification, claim extraction with fallback chains, and configurable public paths.

## Component Definition

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

## Environment Variables

The component reads configuration from environment variables at runtime:

- `JWK_CERT_URL` -- JWK endpoint URL for key discovery (overrides config default)
- `JWK_CERT_FILE` -- local JWK file path (overrides URL if set)
- `AUTH_ENABLED` -- set to `false` to disable auth entirely (development mode)

## JWK Key Management

The component fetches RSA public keys from a JWK (JSON Web Key Set) endpoint and caches them in memory.

**Key sources (in priority order):**
1. Local file (`JWK_CERT_FILE`) -- refreshed every 5 minutes (for kubelet-synced rotation)
2. Remote URL (`JWK_CERT_URL`) -- refreshed every hour

**Unknown key ID handling:** When a JWT contains a `kid` not in the cached set, the component attempts a one-shot refresh from the key source. A 30-second cooldown prevents refresh storms. If the key is still unknown after refresh, the request is rejected with 401.

**Thread safety:** Key map is protected by `sync.RWMutex`. Reads (token validation) use `RLock`. Writes (key refresh) use `Lock` and atomically replace the entire map.

## JWT Validation

The middleware:
1. Extracts the Bearer token from the `Authorization` header
2. Parses the JWT and validates the RSA signature against the cached public keys
3. Verifies the signing method is RSA (rejects HMAC, ECDSA, etc.)
4. Matches the `kid` header to a cached public key
5. On success, stores the parsed `*jwt.Token` in the request context

**Public paths** skip authentication entirely. Path matching is exact (no prefix matching) to prevent auth bypass via path traversal.

## Identity Extraction

After validation, the component extracts an identity payload from JWT claims with fallback chains:

```go
type Payload struct {
    Username  string  // from "username" -> "preferred_username" -> "sub"
    FirstName string  // from "first_name" -> "given_name" -> split("name")[0]
    LastName  string  // from "last_name" -> "family_name" -> split("name")[1]
    Email     string  // from "email"
    ClientID  string  // from "clientId"
    Issuer    string  // from "iss"
}
```

The fallback chains support both Red Hat SSO and RHD (Red Hat Developer) JWT formats.

The identity is stored in the request context and accessible via:
- `auth.GetUsernameFromContext(ctx)` -- returns the extracted username
- `auth.GetAuthPayload(r)` -- returns the full Payload struct

## Generated Code

The component generates:

### `internal/auth/middleware.go`
- `JWTHandler` struct with builder pattern: `NewJWTHandler().WithKeysURL(...).WithKeysFile(...).WithPublicPath(...).Build()`
- `Build()` returns `func(http.Handler) http.Handler` middleware
- JWK key fetching from URL and file with automatic refresh
- JWT parsing and RSA signature validation using `github.com/golang-jwt/jwt/v4`
- Public path bypass with exact matching
- `Stop()` method to terminate the key refresh goroutine

### `internal/auth/context.go`
- `Payload` struct with claim extraction and fallback chains
- `GetAuthPayloadFromContext(ctx)` -- extracts payload from JWT claims in context
- `GetAuthPayload(r)` -- convenience wrapper for HTTP requests
- `GetUsernameFromContext(ctx)` / `SetUsernameContext(ctx, username)` -- username accessors
- `TokenFromContext(ctx)` -- raw JWT token accessor

### Wiring
- The assembler wraps the HTTP mux with the JWT middleware: `jwtHandler.Build()` returns the middleware, applied via `middleware(mux)`
- `main.go` reads `JWK_CERT_URL`, `JWK_CERT_FILE`, and `AUTH_ENABLED` from environment
- When `AUTH_ENABLED=false`, the middleware is a passthrough (no auth check)
- Public paths are configured from the component config, plus the `base_path + /openapi` path is always public

## Dependencies

- `github.com/golang-jwt/jwt/v4` -- JWT parsing and validation

## Differences from jwt-auth (MVP)

| Concern | jwt-auth (MVP) | rh-sso-auth |
|---------|---------------|-------------|
| Signature verification | None (decodes payload only) | RSA signature validation |
| Key source | None | JWK URL or file with auto-refresh |
| Claim extraction | user_id, role | Full payload with fallback chains |
| Public paths | None | Configurable, exact match |
| Auth disable | No | AUTH_ENABLED=false |
| Context type | Custom Identity struct | *jwt.Token + Payload extraction |
