# Task 041: Use Email Attribute for created_by/updated_by Fields

**Spec Reference:** "Server-Managed Fields and Request Schemas" (rest-crud spec, line 367)

**Status:** `not-started`

**Depends on:** task-039 (server-managed fields implementation)

**Blocks:** task-042 (example regeneration)

## Description

The rest-crud archetype spec clarifies how `created_by` and `updated_by` server-managed fields are populated: use `Attributes["email"]` from the auth identity, falling back to `UserID` if email is empty. This ensures compatibility with OpenAPI specs that type these fields as email format.

The current implementation (from task-039) uses `authID.UserID` directly. It must be updated to prefer `Attributes["email"]`.

### What changes

**`internal/generator/restapi/generator.go`:**

- `emitPopulateServerManagedFieldsCreate` (line ~2498): change the `created_by`/`updated_by` population from:
  ```go
  if authID := auth.IdentityFromContext(r.Context()); authID.UserID != "" {
      cluster.CreatedBy = authID.UserID
  }
  ```
  to:
  ```go
  if authID := auth.IdentityFromContext(r.Context()); authID.UserID != "" {
      if email := authID.Attributes["email"]; email != "" {
          cluster.CreatedBy = email
      } else {
          cluster.CreatedBy = authID.UserID
      }
  }
  ```

- `emitPopulateServerManagedFieldsUpdate` (line ~2528): same change for `updated_by`.

**`internal/generator/restapi/generator_test.go`:**

- Update `TestServerManagedFieldsCreateHandler` assertions: expect `Attributes["email"]` with `UserID` fallback instead of direct `UserID` assignment.
- Update `TestServerManagedFieldsUpdateHandler` assertions: same.
- Update `TestServerManagedFieldsUpdateHandlerPreservesCreateOnly` assertions if applicable.

### What does NOT change

- The `Identity` struct in auth generators (jwt-auth, rh-sso-auth) — it already has `Attributes map[string]string`.
- The `isServerManaged()` classification logic — unchanged.
- OpenAPI schema generation — unchanged.

## Spec Excerpt

> `created_by`, `updated_by` -- extract from JWT identity in request context using `Attributes["email"]` (falling back to `UserID` if email is empty). This ensures compatibility with OpenAPI specs that type these fields as email format.

## Acceptance Criteria

1. Generated create handler populates `created_by` and `updated_by` using `Attributes["email"]`, falling back to `UserID`.
2. Generated update handler populates `updated_by` using `Attributes["email"]`, falling back to `UserID`.
3. Tests verify email-preferred extraction with UserID fallback.
4. `go test ./...` passes from the repo root.
5. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

