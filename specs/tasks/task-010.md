# Task 010: postgres-adapter Component Generator

**Spec Reference:** "File Types — Component", "Migration Diffing", "MVP Scope"

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-010.md](../reviews/task-010.md)

## Description

Implement the `postgres-adapter` component as a Go package with a `Generator`.

- `registry/components/postgres-adapter/component.yaml`
- Generator produces:
  - Model structs from entity definitions
  - Query functions (CRUD + list + upsert)
  - Migration SQL files (desired state diffing is component-owned)
  - Upsert with natural-key conflict resolution and optimistic concurrency
  - Computed/derived field support (read-only, populated by fills)
- Output namespace: `internal/storage`
- Provides: `storage-adapter`

## Spec Excerpt

> Migration generation is a component concern, not a stego concern. The compiler passes entity definitions (desired state) to the storage component's generator. The component owns the diffing strategy.

## Acceptance Criteria

- Generator produces compilable Go model and query code
- Migration SQL generated for entity definitions
- Upsert support with conflict resolution
- Computed fields are read-only in generated queries
- Tests verify generated code compiles

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits

- `09b5273` feat(task-010): implement postgres-adapter component generator
- `c4be0d2` chore(task-010): mark task ready-for-review
- `bf25d30` fix(task-010): address round 1 review findings
- `26a7881` fix(task-010): address round 2 review findings
- `af140b4` fix(task-010): address round 3 review findings
- `2581bfb` fix(task-010): address round 4 review findings
- `47d744f` fix(task-010): address round 5 review findings
- `df7c753` fix(task-010): address round 6 review findings
- `64209e0` fix(task-010): address round 7 review findings
