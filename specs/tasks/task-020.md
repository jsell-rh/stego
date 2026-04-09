# Task 020: Update Example Service and README for Collections

**Spec Reference:** "Entity/Collection Separation", "Glossary" (six nouns), "Service Declaration example"

**Status:** `complete`

## Description

With the collection-aware code generation pipeline complete (Tasks 018-019), update the example service and README to use the new `collections:` format. This task also demonstrates multi-path access (multiple collections referencing the same entity), which was impossible with the old `expose:` format.

### What changes

**Example service (`examples/user-management/service.yaml`):**
- Replace `expose:` with `collections:` named map.
- Add an `all-users` collection that provides a flat `/users` list endpoint alongside the scoped `org-users` collection (`/organizations/{org_id}/users`). This demonstrates the key Collection capability: same entity, different access paths.
- Update slot bindings from `entity: User` to `collection: org-users`.
- Update fill.yaml files from `entity:` to `collection:`.

Example target:
```yaml
collections:
  organizations:
    entity: Organization
    operations: [create, read]

  org-users:
    entity: User
    scope: { org_id: Organization }
    operations: [create, read, update, list]

  all-users:
    entity: User
    operations: [list]

slots:
  - collection: org-users
    slot: before_create
    gate:
      - rbac-policy
      - admin-creation-policy

  - collection: org-users
    slot: on_entity_changed
    fan-out:
      - user-change-notifier
      - audit-logger
```

**Regenerate output (`examples/user-management/out/`):**
- Run `stego apply` to regenerate all output files.
- Verify `go build` succeeds in the output directory.
- Generated `main.go` shows collection-scoped fill wiring.

**README.md:**
- Change "Five nouns" to "Six nouns" and add Collection to the list.
- Update the quick-start `service.yaml` example from `expose:` to `collections:`.
- Update the `stego fill create` example to show `--collection` flag if applicable.

## Spec Excerpt

> Six nouns, seven operators.
>
> | Term | What it is |
> |------|-----------|
> | **Collection** | A scoped, operation-constrained access pattern over an entity. Multiple collections can reference the same entity. Each collection generates its own handler, routes, and wiring. |
>
> This makes multi-path access the default case, not an exception. REST APIs project entity graphs onto URL trees; that projection is inherently 1:N.

## Acceptance Criteria

1. `examples/user-management/service.yaml` uses `collections:` with at least three collections, including two that reference the same entity (`User`).
2. Slot bindings use `collection:` not `entity:`.
3. Fill YAML files use `collection:` not `entity:`.
4. `stego validate` passes on the updated service.yaml.
5. `stego apply` produces output that compiles with `go build`.
6. Generated `main.go` wires fills to the correct collection-scoped handlers.
7. README lists six nouns and uses `collections:` in examples.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

All acceptance criteria were already satisfied by task-019's revision rounds:

- `263650a` fix(task-019): round 4 revisions — path param from scope field, README collections format, stale terminology
- `79d587f` fix(task-019): round 5 — regenerate example output with collection-aware generators

Verification commit for this task:
- `3cd9e61` chore(task-020): mark ready-for-review — all ACs satisfied by task-019
