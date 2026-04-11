# Task 042: Regenerate Example Output with Email Attribute Change

**Spec Reference:** "Server-Managed Fields and Request Schemas" (rest-crud spec, line 367)

**Status:** `ready-for-review`

**Depends on:** task-041 (email attribute for created_by/updated_by)

## Description

Regenerate the example services (`examples/user-management/` and `examples/user-management-rhsso/`) to incorporate the updated `created_by`/`updated_by` population logic from task-041. Verify the generated output compiles and reflects the email-preferred extraction.

### What changes

**Regenerate both examples:**
- `cd examples/user-management && stego apply`
- `cd examples/user-management-rhsso && stego apply`

**Verify in generated handler files:**
- `created_by` and `updated_by` population uses `Attributes["email"]` with `UserID` fallback.

**Verify generated code compiles:**
- `cd examples/user-management/out && go build ./...`
- `cd examples/user-management-rhsso/out && go build ./...`

## Spec Excerpt

> `created_by`, `updated_by` -- extract from JWT identity in request context using `Attributes["email"]` (falling back to `UserID` if email is empty). This ensures compatibility with OpenAPI specs that type these fields as email format.

## Acceptance Criteria

1. Both example services regenerated via `stego apply`.
2. Generated handlers use `Attributes["email"]` with `UserID` fallback for `created_by`/`updated_by`.
3. Generated code compiles in both examples.
4. `go test ./...` passes from the repo root.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Verification Notes

Both examples regenerated via `STEGO_REGISTRY=../../registry stego apply` — both reported "No changes. Infrastructure is up-to-date." This is expected because neither example's entities declare `created_by`/`updated_by` fields, so the task-041 generator change (email attribute extraction) does not affect the generated output. The feature is verified by task-041's unit tests (`TestServerManagedFieldsCreateHandler`, `TestServerManagedFieldsUpdateHandler`).

Generated code compiles: `cd out && go build ./...` succeeds for both examples. `go test ./...` passes from the repo root.

## Commits

- `3755865` chore(task-042): verify examples unchanged after email attribute change
