# Task 060: Regenerate Example Output with Lifecycle Slots

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Lifecycle Slots** section (lines 304–351)

**Status:** `complete`

**Review:** [specs/reviews/task-060.md](../reviews/task-060.md)

**Depends on:** task-058, task-059

## Description

After lifecycle slot support is fully wired into the generator (tasks 058 and 059), update the example service to demonstrate at least one new lifecycle slot and regenerate the example output.

### Example service updates

**`examples/user-management/service.yaml`:**
Add slot bindings that demonstrate the new lifecycle slots. Good candidates from the spec:

- `after_patch` on a collection with `chain: [increment-generation]` — demonstrates recomputing derived fields after a patch
- `before_delete` on a collection with `gate: [check-dependencies]` — demonstrates preventing deletion of active resources
- `after_create` on a collection with `fan-out: [send-welcome]` — demonstrates post-creation side effects

At minimum, add one before-slot and one after-slot that aren't `before_create` to show the lifecycle coverage.

### Example regeneration

- Run the compiler against the updated service declaration to regenerate output
- Verify the generated handlers include the lifecycle slot invocations at the correct points
- Verify compilation succeeds

### What does NOT change

- Core implementation — all lifecycle slot logic is done in tasks 057-059
- Non-example source files

## Acceptance Criteria

1. Example service.yaml includes at least one new lifecycle slot binding (not before_create/validate)
2. Example output regenerated and committed
3. Generated handlers show lifecycle slot invocations at correct lifecycle points
4. `go build ./examples/...` compiles (or equivalent verification)
5. All tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- 276f606 feat(task-060): add lifecycle slot examples and regenerate output
