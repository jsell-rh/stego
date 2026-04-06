# Task 016: Example Service — End-to-End Demonstration

**Spec Reference:** "MVP Scope"

**Status:** `not-started`

## Description

Create an example service that demonstrates the full STEGO pipeline end-to-end.

- `examples/user-management/service.yaml` — the service declaration from the spec
- Entities: User (email, role, org_id), Organization (name)
- Expose: Organization [create, read], User [create, read, update, list] scoped by org_id, nested under Organization
- Fills:
  - `admin-creation-policy` — gate fill for before_create on User (only admins create admins)
  - `audit-logger` — fan-out fill for on_entity_changed on User
- Run full pipeline: `stego validate && stego plan && stego apply`
- Verify: `cd out && go build ./...` succeeds
- Verify: generated main.go shows all fills wired via constructor injection

## Spec Excerpt

> Example service: simplified hyperfleet-api or similar, producing a compilable, runnable Go service from a single `service.yaml` + fills.
> At least one gate fill (e.g. `admin-creation-policy`)
> At least one fan-out fill (e.g. `audit-logger`)

## Acceptance Criteria

- service.yaml matches spec example
- `stego apply` produces complete, compilable Go service
- Generated main.go shows admin-creation-policy and audit-logger wired
- `go build` succeeds on generated output
- Fill tests pass via `stego test`
- An auditor can read main.go and see every fill and every connection

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits
