# Task 039: Server-Managed Fields and Request Schemas

**Spec Reference:** "Server-Managed Fields and Request Schemas" (rest-crud spec, lines 326-372)

**Status:** `not-started`

**Depends on:** task-037 (error prefix fix — affects generated output), task-038 (PORT env var — affects generated output)

**Blocks:** task-040 (example regeneration)

## Description

The rest-crud archetype spec adds a new section defining server-managed fields — fields whose values are assigned by the server, not the client. The `rest-api` component must generate separate OpenAPI request schemas that exclude these fields, and the create handler must populate them server-side.

### Server-Managed Field Classification

A field is server-managed if any of the following apply:

1. **`computed: true`** — filled by a slot, never client-written
2. **`type: timestamp`** — server-assigned timestamps (`created_time`, `updated_time`, etc.)
3. **Named `created_by` or `updated_by`** — populated from the authenticated identity
4. **Named `generation`** — server-managed version counter, incremented on mutation

The `id` field (from `Meta`) is always server-assigned and excluded from all request schemas.

### What changes

**`internal/generator/restapi/generator.go`:**

**OpenAPI schema generation:**
- Add a helper function `isServerManaged(f types.Field) bool` that returns true when any server-managed criteria apply.
- Generate a `{Entity}CreateRequest` OpenAPI schema per entity that includes only client-provided fields (excludes server-managed fields, scope fields, and `id`).
- The existing `{Entity}` schema remains unchanged (used for GET responses, includes all fields + envelope metadata).
- `{Entity}PatchRequest` schema already exists — no changes needed (it already uses `patchable` fields only).
- POST/PUT request body `$ref` in OpenAPI paths references `{Entity}CreateRequest` instead of `{Entity}`.

**Create handler:**
- After decoding the request body into the entity struct, populate server-managed fields:
  - `id` — generate UUID (already done via GORM `BeforeCreate` hook on `Meta`, but verify)
  - `created_by`, `updated_by` — extract from JWT identity in request context. Use the auth context accessor (`auth.GetUsernameFromContext(ctx)` for rh-sso-auth, or equivalent for jwt-auth). If auth is not available (public endpoint), leave empty.
  - `created_time`, `updated_time` — set to `time.Now()` (already handled by GORM `Meta` hooks, but verify the fields are not overwritten from request body)
  - `generation` — use the declared `default` value from the field definition
  - `computed` fields — leave nil/zero

**Update handler:**
- `updated_by` — extract from JWT identity and set before persisting
- `updated_time` — set to `time.Now()`
- `generation` — increment (if present on entity)

**Request body `kind` validation:**
- When a `kind` field is present in the JSON request body, validate it matches the entity name. If wrong, return 400.
- When absent, do not reject the request.
- The `kind` field is not persisted — it is stripped after validation.

**Tests:**
- Add `isServerManaged()` unit tests covering each classification rule.
- Add generator tests verifying `{Entity}CreateRequest` schema excludes server-managed fields.
- Add generator tests verifying create handler populates server-managed fields.
- Add generator tests verifying `kind` field validation.

### What does NOT change

- Entity type definitions (`internal/types/types.go`) — no new fields needed.
- postgres-adapter generator — GORM model structs include all fields (server-managed fields are in the model, just not in request schemas).
- Patch handler — already uses `patchable` field list, unaffected.

## Spec Excerpt

> Not all entity fields belong in API request bodies. The `rest-api` component must generate separate OpenAPI schemas for create and update requests that exclude **server-managed fields** -- fields whose values are assigned or derived by the server, not provided by the client.
>
> A field is server-managed if any of the following apply:
> 1. `computed: true` -- filled by a slot, never client-written
> 2. `type: timestamp` -- server-assigned timestamps
> 3. Named `created_by` or `updated_by` -- populated from the authenticated identity
> 4. Named `generation` -- server-managed version counter
>
> Generated `ClusterCreateRequest` schema includes only: `name` (required), `spec` (required), `labels` (optional). The remaining fields are server-managed.

## Acceptance Criteria

1. `isServerManaged()` correctly classifies fields by all four rules.
2. `{Entity}CreateRequest` OpenAPI schema generated per entity, excluding server-managed fields, scope fields, and `id`.
3. POST request body `$ref` points to `{Entity}CreateRequest`, not `{Entity}`.
4. Create handler populates `created_by`/`updated_by` from auth context.
5. Create handler uses field `default` value for `generation` field.
6. Create handler does not allow client to set `computed`, `timestamp`, `created_by`, `updated_by`, or `generation` fields.
7. `kind` field in request body validated against entity name; wrong value returns 400; absent is accepted.
8. Update handler sets `updated_by` from auth context and `updated_time` to `time.Now()`.
9. Tests cover all server-managed field classifications and handler behaviors.
10. `go test ./...` passes from the repo root.
11. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
