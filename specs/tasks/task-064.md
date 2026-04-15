# Task 064: Use UUID v7 Instead of v4 for Entity ID Generation

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — Response Format (`uuid.NewV7()`), Known Generator Bugs

**Status:** `complete`

**Review:** [specs/reviews/task-064.md](../reviews/task-064.md)

**Depends on:** task-062

## Description

The spec requires UUID v7 (time-ordered) for entity IDs:

> `id` -- auto-generated UUID v7 (time-ordered), assigned on create. The generator must use `uuid.NewV7()` (not `uuid.New()` which produces v4).

The Known Generator Bugs section documents this as an open defect:

> **UUID v4 instead of v7**: The generator uses `uuid.New()` (v4). It should use `uuid.NewV7()` per this spec.

Three generator sites currently emit `uuid.New().String()`:

1. **`internal/generator/restapi/generator.go:776`** — create handler (envelope mode)
2. **`internal/generator/restapi/generator.go:1240`** — upsert handler insert path (envelope mode)
3. **`internal/generator/postgresadapter/generator.go:249`** — GORM `BeforeCreate` hook on `Meta`

### Fix

`uuid.NewV7()` returns `(uuid.UUID, error)` (unlike `uuid.New()` which returns only `uuid.UUID`), so each call site must handle the error:

- **restapi create handler**: return an internal error via `handleError(w, r, InternalError(...))` if `NewV7()` fails.
- **restapi upsert handler**: same pattern — return internal error on failure.
- **postgresadapter BeforeCreate hook**: return the error from the hook (GORM propagates it as a transaction failure).

The `github.com/google/uuid v1.6.0` dependency already supports `NewV7()` — no dependency change needed.

Tests in `internal/generator/restapi/generator_test.go` (line 5574, 5997) and `internal/generator/postgresadapter/generator_test.go` (line 288) assert on `uuid.New().String()` and must be updated to match the new pattern.

## Acceptance Criteria

1. All three generator sites emit `uuid.NewV7()` instead of `uuid.New()`.
2. Generated restapi handlers propagate `NewV7()` errors as HTTP 500 internal errors.
3. Generated postgresadapter `BeforeCreate` hook propagates `NewV7()` errors via the return value.
4. Bare-mode test (`generator_test.go:5997`) still verifies no UUID generation in bare mode (update assertion to match `NewV7` pattern).
5. Envelope-mode test (`generator_test.go:5574`) verifies the generated create handler uses `uuid.NewV7()`.
6. Postgres adapter test (`generator_test.go:288`) verifies the `BeforeCreate` hook uses `uuid.NewV7()`.
7. All existing tests pass: `go test ./...`
8. Example output regenerated (both `examples/user-management/` and `examples/user-management-rhsso/`).
9. The "UUID v4 instead of v7" entry is removed from the Known Generator Bugs section in `specs/registry/archetypes/rest-crud/spec.md` once the fix is verified.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
- b104d67 fix(task-064): use UUID v7 instead of v4 for entity ID generation
- eab80ba fix(task-064): restore deleted .stego/config.yaml files
