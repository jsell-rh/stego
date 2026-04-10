# Task 033: rh-sso-auth Assembler Integration & Wiring

**Spec Reference:** [specs/registry/components/rh-sso-auth/spec.md](../registry/components/rh-sso-auth/spec.md) -- "Wiring" and "Environment Variables" sections

**Status:** `not-started`

**Depends on:** task-032 (rh-sso-auth generator)

**Blocks:** task-034

## Description

Update the compiler assembler to handle `rh-sso-auth` wiring in generated `main.go` and `go.mod`. The rh-sso-auth wiring differs from jwt-auth in several ways: environment variable configuration, builder pattern construction, public paths, auth disable toggle, and goroutine cleanup.

### Assembler Changes (`internal/compiler/assembler.go`)

When the resolved auth-provider is `rh-sso-auth`, the assembler must generate:

**Environment variable reading in `main.go`:**
```go
jwkCertURL := os.Getenv("JWK_CERT_URL")
jwkCertFile := os.Getenv("JWK_CERT_FILE")
authEnabled := os.Getenv("AUTH_ENABLED")
```

**Builder pattern construction:**
```go
jwtHandler := auth.NewJWTHandler().
    WithKeysURL(jwkCertURL).
    WithKeysFile(jwkCertFile).
    WithPublicPath("/healthcheck").
    WithPublicPath("/metrics").
    WithPublicPath(basePath + "/openapi").
    Build()
```

- Public paths come from the component config `public_paths` default list
- The `base_path + /openapi` path is always added as public

**AUTH_ENABLED passthrough:**
When `AUTH_ENABLED=false`, the middleware is a passthrough (no auth check). The generated code should check `authEnabled != "false"` before applying the middleware.

**Middleware application:**
```go
handler := jwtHandler(mux)  // wraps the HTTP mux
```

**Cleanup on shutdown:**
```go
defer jwtHandler.Stop()  // or appropriate cleanup call
```

**go.mod dependency:**
Add `github.com/golang-jwt/jwt/v4` to the generated `go.mod` when rh-sso-auth is the auth provider.

### Validation

The validator should accept `rh-sso-auth` as a valid auth-provider override:
```yaml
overrides:
  auth-provider: rh-sso-auth
```

## Spec Excerpt

> The assembler wraps the HTTP mux with the JWT middleware: `jwtHandler.Build()` returns the middleware, applied via `middleware(mux)`.
> `main.go` reads `JWK_CERT_URL`, `JWK_CERT_FILE`, and `AUTH_ENABLED` from environment.
> When `AUTH_ENABLED=false`, the middleware is a passthrough (no auth check).
> Public paths are configured from the component config, plus the `base_path + /openapi` path is always public.

## Acceptance Criteria

1. Generated `main.go` reads `JWK_CERT_URL`, `JWK_CERT_FILE`, and `AUTH_ENABLED` from environment when auth-provider is rh-sso-auth.
2. Generated `main.go` constructs JWTHandler via builder pattern with configured public paths.
3. Generated `main.go` applies JWT middleware to the HTTP mux.
4. Generated `main.go` includes `AUTH_ENABLED=false` passthrough logic.
5. Generated `main.go` includes cleanup (`Stop()`) for the JWTHandler.
6. Generated `go.mod` includes `github.com/golang-jwt/jwt/v4` dependency.
7. `base_path + /openapi` is always added as a public path.
8. `stego plan` and `stego apply` work correctly with rh-sso-auth as auth-provider.
9. Assembler tests cover rh-sso-auth wiring generation.
10. `go test ./...` passes from the repo root.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
