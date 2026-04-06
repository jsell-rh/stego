# Task 005: Port Resolution Engine

**Spec Reference:** "Port Resolution"

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-005.md](../reviews/task-005.md)

## Description

Implement the port resolution algorithm that matches component `requires` to `provides`.

- Archetype declares default bindings (`storage-adapter: postgres-adapter`)
- Service declaration can override bindings via `overrides:`
- Validate every `requires` port has exactly one provider
- Unresolved or ambiguous ports are compile errors with clear messages
- Return a resolved dependency graph

## Spec Excerpt

> The compiler validates that every `requires` port has exactly one provider. Unresolved or ambiguous ports are a compile error.

## Acceptance Criteria

- Port resolution works for the `rest-crud` archetype default bindings
- Service-level overrides take precedence
- Clear error on unresolved port
- Clear error on ambiguous port (two providers for same port)
- Unit tests for happy path + both error cases

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits

- `b2c8ae8` feat(ports): implement port resolution engine
- `daaffdd` chore(tasks): mark task-005 ready-for-review
- `b4587ea` fix(ports): address review findings for port resolution engine
- `3aa54b7` chore(tasks): mark task-005 ready-for-review
