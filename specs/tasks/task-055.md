# Task 055: Regenerate Example Output with Implicit Fields

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Implicit Fields** section

**Status:** `needs-revision`

**Review:** `specs/reviews/task-055.md`

**Depends on:** task-052, task-053, task-054

## Description

After implicit field support is fully implemented (parsing, validation, create/upsert injection, list filtering), update the example service to demonstrate implicit fields and regenerate the example output.

### What changes

**Example service declaration** (`examples/user-management/service.yaml` or equivalent):
- Add a collection that uses `implicit` to demonstrate the polymorphic association pattern. For example, if the example has entities that could benefit from implicit (e.g. an activity log with `source` discriminator), add `implicit: { source: "api" }` to the appropriate collection.
- Alternatively, if the existing example doesn't naturally need implicit, add a minimal demonstration collection that showcases the feature.

**Example regeneration:**
- Run `stego compile` (or equivalent) against the updated service declaration to regenerate the example output.
- Verify the generated code includes:
  - Implicit field assignments in create/upsert handlers.
  - `ImplicitFilters` populated in list handlers.
  - Storage layer applying implicit WHERE clauses.

### What does NOT change

- Core implementation — all implicit logic is done in tasks 050-054.
- Non-example source files.

## Acceptance Criteria

1. Example service.yaml includes at least one collection with `implicit`.
2. Example output regenerated and committed.
3. Generated create/upsert handlers show implicit field injection.
4. Generated list handlers show `ImplicitFilters` in ListOptions.
5. `go build ./examples/...` compiles (or equivalent verification).
6. All tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- 3de9c87 feat(task-055): add implicit field demo and regenerate example output
