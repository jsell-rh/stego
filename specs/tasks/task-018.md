# Task 018: Collection Type System — Replace Expose Blocks with Named Collections

**Spec Reference:** "Glossary" (Collection noun), "Entity/Collection Separation", "Service Declaration example"

**Status:** `needs-revision`

**Review:** [specs/reviews/task-018.md](../reviews/task-018.md)

## Description

The spec defines `Collection` as the 6th core noun: "A scoped, operation-constrained access pattern over an entity. Multiple collections can reference the same entity. Each collection generates its own handler, routes, and wiring." The current implementation uses `ExposeBlock` — an anonymous, entity-centric list — which cannot express multiple access paths to the same entity. This task replaces `ExposeBlock` with named `Collection` and updates the service declaration schema from `expose:` to `collections:`.

### What changes

**Core types (`internal/types/types.go`):**
- Rename `ExposeBlock` to `Collection`. Collections gain an implicit `Name` field (populated from the map key during parsing).
- `Scope` changes from `string` to `map[string]string` to support multi-field scoping (e.g. `scope: { org_id: Organization }`).
- Remove `Parent` field — parent nesting is now derived from the `Scope` map.
- `ServiceDeclaration.Expose []ExposeBlock` becomes `ServiceDeclaration.Collections map[string]Collection` (or an order-preserving representation — implementer's choice, but code generation must be deterministic).
- `SlotDeclaration.Entity` becomes `SlotDeclaration.Collection` (string referencing a collection name, not an entity name).
- `Fill.Entity` becomes `Fill.Collection`.

**Parser (`internal/parser/`):**
- Parse `collections:` as a named map in service.yaml:
  ```yaml
  collections:
    org-users:
      entity: User
      scope: { org_id: Organization }
      operations: [create, read, update, list]
    all-users:
      entity: User
      operations: [list]
  ```
- Reject `expose:` with a clear migration error message.
- Each collection's name is populated from its map key.

**Validation (`internal/compiler/validate.go`):**
- Validate that each collection's `entity` references an existing entity.
- Validate that scope field names exist on the referenced entity and that scope values reference existing entities.
- Validate that slot declarations reference existing collection names.
- Validate that fill declarations reference existing collection names.
- Validate that collection names are unique (enforced by map structure).

**Tests:**
- Update all parser tests for the new `collections:` format.
- Update validation tests for collection-specific rules.
- Update any test fixtures that use `expose:`.

### What does NOT change

- Entity definitions remain unchanged.
- The `UpsertKey`, `Concurrency`, `PathPrefix` fields carry over from ExposeBlock to Collection.
- Generator implementations are updated in Task 019.
- Example service is updated in Task 020.

## Spec Excerpt

> **Collection** | A scoped, operation-constrained access pattern over an entity. Multiple collections can reference the same entity. Each collection generates its own handler, routes, and wiring. | Product team / LLM
>
> Entities define data (fields, types, constraints). Collections define access patterns (which entity, what scope, what operations, what URL). This separation is load-bearing:
> - An entity is declared once. A collection references it.
> - Multiple collections can reference the same entity with different scopes and operations.
> - Each collection generates its own handler. The entity struct is shared.
> - Slots bind to collections, not entities. Different access paths can have different business logic.
> - Paths are derived from collection names and scopes, or declared explicitly via `path_prefix`.

## Acceptance Criteria

1. `ExposeBlock` type is renamed to `Collection` with a `Name` field.
2. `Scope` is `map[string]string`, not `string`. `Parent` field is removed.
3. `ServiceDeclaration` uses `Collections` (named map or ordered representation) instead of `Expose`.
4. `SlotDeclaration` and `Fill` use `Collection` field instead of `Entity`.
5. Parser handles `collections:` named map format. `expose:` is rejected with a helpful error.
6. Validation checks collection entity refs, scope field/entity refs, slot collection refs, fill collection refs.
7. All existing tests are updated and pass.
8. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- `103fc68` refactor(restapi): rename ExposeBlock to Collection and update field accessors
- `1ad115a` refactor(validate): update validate_test.go for Collection types
- `a2d3f91` fix(restapi): update generator and tests for Collection type changes
- `beba95d` feat(types): replace ExposeBlock with Collection type system
- `6e5b8b8` fix(task-018): address round 1 review findings for Collection type system
- `878ed94` fix(task-018): address round 2 review findings for Collection type system
- `db527ac` fix(task-018): address round 3 review findings — migration errors and test fixtures
- `467058c` fix(task-018): enforce scope cardinality on Reconcile path (round 4)
- `b8f3aa6` fix(task-018): update generator error messages from "expose block" to "collection" (round 5)
