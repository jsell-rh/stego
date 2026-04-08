# Task 016: Example Service — End-to-End Demonstration

**Spec Reference:** "MVP Scope"

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-016.md](../reviews/task-016.md)

## Description

Create an example service that demonstrates the full STEGO pipeline end-to-end.

### Prerequisite: Add event-publisher mixin to live registry

The live registry (`registry/`) is missing the `event-publisher` mixin needed for the fan-out fill demonstration. The testdata fixtures (`internal/registry/testdata/registry/mixins/event-publisher/`) have reference files. Add to the live registry:

- `registry/mixins/event-publisher/mixin.yaml` — kind: mixin, adds_components: [kafka-producer], adds_slots: [on_entity_changed], overrides: none
- `registry/mixins/event-publisher/slots/on_entity_changed.proto` — proto service definition for the on_entity_changed slot

### Example service

- `examples/user-management/service.yaml` — the service declaration from the spec
- Entities: User (email, role, org_id), Organization (name)
- Expose: Organization [create, read], User [create, read, update, list] scoped by org_id, nested under Organization
- Fills:
  - `admin-creation-policy` — gate fill for before_create on User (only admins create admins)
  - `audit-logger` — fan-out fill for on_entity_changed on User
- Run full pipeline: `cd examples/user-management && stego validate && stego plan && stego apply`
- Verify: `cd out && go build ./...` succeeds
- Verify: generated main.go shows all fills wired via constructor injection

## Spec Excerpt

> Example service: simplified hyperfleet-api or similar, producing a compilable, runnable Go service from a single `service.yaml` + fills.
> At least one gate fill (e.g. `admin-creation-policy`)
> At least one fan-out fill (e.g. `audit-logger`)

## Acceptance Criteria

- event-publisher mixin exists in live registry with mixin.yaml and slots/on_entity_changed.proto
- service.yaml matches spec example
- `stego apply` produces complete, compilable Go service
- Generated main.go shows admin-creation-policy and audit-logger wired
- `go build` succeeds on generated output
- Fill tests pass via `stego test`
- An auditor can read main.go and see every fill and every connection

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- `3645b6b` feat(task-016): add event-publisher mixin and kafka-producer stub to registry
- `51f46b4` fix(task-016): support mixin-added slots and fix multi-slot package issues
- `b628769` feat(task-016): add user-management example with fills and end-to-end pipeline
