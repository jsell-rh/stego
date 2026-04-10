# Task 034: rh-sso-auth End-to-End Verification

**Spec Reference:** [specs/registry/components/rh-sso-auth/spec.md](../registry/components/rh-sso-auth/spec.md) -- "Differences from jwt-auth (MVP)" section

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-034.md](../reviews/task-034.md)

**Depends on:** task-033 (assembler integration)

## Description

Verify the rh-sso-auth component works end-to-end by creating an example service variant that uses it, running `stego plan` and `stego apply`, and confirming the generated code compiles.

### Example Service

Create `examples/user-management-rhsso/` (or add an override variant to the existing example) that uses rh-sso-auth instead of jwt-auth:

```yaml
kind: service
name: user-management
archetype: rest-crud
language: go
base_path: /api/users/v1

# ... same entities and collections as existing example ...

overrides:
  auth-provider: rh-sso-auth
```

### Verification Steps

1. Run `stego validate` on the example service -- must pass.
2. Run `stego plan` -- must show rh-sso-auth as the auth-provider component.
3. Run `stego apply` -- must generate all files without errors.
4. Verify generated `out/internal/auth/middleware.go` contains:
   - JWTHandler struct with builder pattern
   - JWK key fetching and caching
   - RSA signature validation
   - Public path bypass with exact matching
   - Stop() method
5. Verify generated `out/internal/auth/context.go` contains:
   - Payload struct with all fields
   - Claim fallback chains
   - Context accessors
6. Verify generated `out/main.go` contains:
   - Environment variable reading (JWK_CERT_URL, JWK_CERT_FILE, AUTH_ENABLED)
   - Builder pattern construction
   - Public paths configuration
   - AUTH_ENABLED passthrough
   - Stop() cleanup
7. Verify generated `out/go.mod` includes `github.com/golang-jwt/jwt/v4`.
8. Verify generated code compiles: `cd examples/user-management-rhsso/out && go build ./...` (or equivalent).

### Spec Compliance Check

Verify each row in the "Differences from jwt-auth" table:

| Concern | jwt-auth (MVP) | rh-sso-auth | Verified |
|---------|---------------|-------------|----------|
| Signature verification | None (decodes payload only) | RSA signature validation | |
| Key source | None | JWK URL or file with auto-refresh | |
| Claim extraction | user_id, role | Full payload with fallback chains | |
| Public paths | None | Configurable, exact match | |
| Auth disable | No | AUTH_ENABLED=false | |
| Context type | Custom Identity struct | *jwt.Token + Payload extraction | |

## Acceptance Criteria

1. Example service with `rh-sso-auth` override exists and passes `stego validate`.
2. `stego apply` generates all expected files without errors.
3. Generated `middleware.go` and `context.go` match the spec's "Generated Code" section.
4. Generated `main.go` wiring matches the spec's "Wiring" section.
5. Generated code compiles successfully.
6. All rows in the "Differences from jwt-auth" table are verified.
7. `go test ./...` passes from the repo root.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- a04680d feat(task-034): rh-sso-auth end-to-end verification example
- f3c1bf9 fix(task-034): move env var reading to main.go wiring, fix map iteration order
