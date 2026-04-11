# Task 049: Regenerate Example Output with CORS Middleware

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **CORS** section

**Status:** `complete`

**Review:** [`specs/reviews/task-049.md`](../reviews/task-049.md)

**Depends on:** task-047, task-048

## Description

After CORS middleware generation and override support are implemented, regenerate the example service output to reflect the new CORS middleware in the generated code.

### What changes

**Example regeneration:**
- Run `stego compile` (or equivalent) against the example service declaration to regenerate `examples/user-management/out/`.
- The regenerated output should include:
  - `cors.go` in the API output directory with CORS middleware implementation
  - CORS middleware wired as outermost layer in `main.go`
- Verify the generated code compiles and the CORS middleware is correctly placed.

### What does NOT change

- The example service declaration (`service.yaml`) — unless it needs `cors: enabled` explicitly (the archetype default should suffice).
- Any other generated files not affected by CORS.

## Acceptance Criteria

1. Example output regenerated with CORS middleware present.
2. Generated `cors.go` exists in the example API directory.
3. Generated `main.go` shows CORS middleware as outermost wrapper.
4. `go build ./examples/user-management/out/...` compiles (or equivalent verification).
5. All tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- `610d495` feat(task-049): regenerate example output with CORS middleware
