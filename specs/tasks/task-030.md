# Task 030: End-to-End Example with All rest-crud Spec Features

**Spec Reference:** "MVP Scope" (spec.md), all sections of rest-crud spec

**Status:** `needs-revision`

**Review:** [specs/reviews/task-030.md](../reviews/task-030.md)

## Description

Update the example service to demonstrate all rest-crud archetype features implemented in Tasks 018-029. This is the final integration verification: a single `service.yaml` + fills that exercises collections, base_path, GORM storage, envelope responses, RFC 9457 errors, request validation, patch, upsert, search, and short-circuit chain semantics. The generated service must compile and represent a realistic use case.

### What changes

**Example service (`examples/user-management/service.yaml`):**
- Add `base_path` (e.g. `/api/user-mgmt/v1`).
- Add `error_type_base` (e.g. `https://api.example.com/errors/`).
- Add `patch` operation to at least one collection with `patchable` fields.
- Add `upsert` operation to at least one collection with `upsert_key` and `concurrency`.
- Ensure `?search=` works on list endpoints.
- Add at least one slot with `chain` operator and `short_circuit: true` to demonstrate halt semantics.
- All collections use the named map format.

**Regenerate and verify:**
- `stego validate` — passes.
- `stego plan` — shows expected changeset.
- `stego apply` — generates all output files.
- `cd out && go build` — compiles successfully.
- Generated code includes:
  - GORM models with Meta embedding and struct tags.
  - DAO layer with Create/Get/Replace/Delete/List/Upsert/Exists.
  - RFC 9457 error handling with `application/problem+json`.
  - Envelope response format with pagination.
  - OpenAPI validation middleware.
  - TSL search integration in list handlers.
  - Patch handler with pointer-field request struct.
  - Upsert handler with conflict resolution and optimistic concurrency.
  - Short-circuit chain with halt semantics.
  - Collection-scoped slot wiring in `main.go`.

**Documentation update:**
- Ensure README quick-start and example sections reflect the final state.

## Spec Excerpt

> **Example service:** simplified hyperfleet-api or similar, producing a compilable, runnable Go service from a single `service.yaml` + fills.

## Acceptance Criteria

1. Example `service.yaml` exercises: collections (including multi-path), base_path, error_type_base, patch with patchable, upsert with upsert_key and concurrency, all CRUD operations, slots with gate, fan-out, and short-circuit chain.
2. `stego validate && stego plan && stego apply` succeeds.
3. `cd out && go build` compiles the generated service.
4. Generated `main.go` wires all fills, storage, auth, and search.
5. Generated code includes GORM models, DAO, RFC 9457 errors, envelope responses, validation middleware, TSL search, patch handlers, upsert handlers, short-circuit chain handlers.
6. README reflects final state.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
- 9e2fb11 feat(task-030): end-to-end example with all rest-crud spec features
- d5490a5 fix(task-030): address all 6 review findings from round 1
- ee58fa9 fix(task-030): address all 4 review findings from round 2
