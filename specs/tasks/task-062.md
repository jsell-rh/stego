# Task 062: Regenerate Example Output with Discovery Endpoints

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **OpenAPI and Metadata Endpoints** section (lines 669–700)

**Status:** `complete`

**Review:** [specs/reviews/task-062.md](../reviews/task-062.md)

**Depends on:** task-061

## Description

After the discovery endpoint generation is implemented in task-061, regenerate the example service output to include the three new endpoints.

### Example regeneration

- Run the compiler against `examples/user-management/service.yaml` to regenerate output
- Verify the generated output includes:
  - An OpenAPI spec handler serving the embedded spec at `GET {base_path}/openapi`
  - An OpenAPI UI HTML handler at `GET {base_path}/openapi.html`
  - A metadata handler at `GET {base_path}` listing top-level collections
  - Route registrations for all three endpoints (unauthenticated)
- Verify compilation succeeds

### What does NOT change

- Core implementation — all discovery endpoint logic is done in task-061
- Non-example source files
- `service.yaml` — no configuration needed for discovery endpoints

## Acceptance Criteria

1. Example output regenerated and committed
2. Generated `main.go` or router registers the three discovery routes
3. Generated handler files include the endpoint implementations
4. `go build ./examples/...` compiles (or equivalent verification)
5. All tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- 7f51ca8 fix(task-061): use {$} for exact root match and initialize collections slice (regenerated example output with discovery endpoints)
