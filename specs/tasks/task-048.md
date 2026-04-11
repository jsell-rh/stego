# Task 048: Service Override to Disable CORS

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **CORS** section (override subsection)

**Status:** `ready-for-review`

**Depends on:** task-047

**Blocks:** task-049

## Description

A service declaration can disable CORS via `overrides: cors: disabled` for internal-only services (e.g. behind a gateway that handles CORS). The reconciler must process this override and suppress CORS middleware generation even when the archetype convention defaults to `cors: enabled`.

### What changes

**Service declaration schema:**
- Support `overrides.cors` field in service YAML:
  ```yaml
  overrides:
    cors: disabled
  ```

**`internal/compiler/reconciler.go`:**
- When processing overrides, check for `cors: disabled`.
- If present, set `ctx.Conventions.CORS = ""` (or equivalent) so the rest-api generator and assembler skip CORS middleware generation.

**`internal/types/types.go` (if needed):**
- Ensure the `Overrides` type can hold a `CORS` string field if it doesn't already support arbitrary convention overrides.

**Tests:**
- Reconciler test: a service with `overrides: cors: disabled` using a `rest-crud` archetype (which defaults `cors: enabled`) should produce output with no CORS middleware.
- Verify the override only affects CORS — other conventions remain intact.

### What does NOT change

- Default behavior — `cors: enabled` still works as before when no override is present.
- The CORS middleware implementation itself (task-047).

## Spec Excerpt

> A service declaration can disable CORS via an override if the service is internal-only (e.g. behind a gateway that handles CORS):
>
> ```yaml
> overrides:
>   cors: disabled
> ```

## Acceptance Criteria

1. Service declarations with `overrides: cors: disabled` suppress CORS middleware generation.
2. Service declarations without the override (using rest-crud archetype) still get CORS middleware.
3. The override does not affect other conventions.
4. Reconciler tests verify both override and default paths.
5. All tests pass: `go test ./...`
6. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- `4f28053` feat(task-048): CORS override to disable CORS middleware generation
