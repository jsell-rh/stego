# Task 019: Collection-Aware Code Generation Pipeline

**Spec Reference:** "Entity/Collection Separation", "Collections & Operations" (rest-crud spec), "Generated Code Structure"

**Status:** `needs-revision`

**Review:** [specs/reviews/task-019.md](../reviews/task-019.md)

## Description

With the Collection type system in place (Task 018), this task updates all code generators, the compiler, fill wiring, and CLI commands to work with named collections instead of expose blocks.

### What changes

**rest-api generator (`internal/generator/restapi/`):**
- Each collection generates its own handler file (e.g. `handler_org_users.go`, `handler_all_users.go`).
- Path derivation from collection name: `org-users` with `scope: { org_id: Organization }` becomes `/organizations/{org_id}/users`. `all-users` with no scope becomes `/users`. When `path_prefix` is set on a collection, it overrides the derived path.
- Multiple collections referencing the same entity share the entity struct but get separate handlers.
- Router registers routes per collection.
- OpenAPI spec generates paths per collection.
- Slot bindings reference collections: the generated handler for collection `org-users` wires the fills bound to that collection.

**postgres-adapter generator (`internal/generator/postgresadapter/`):**
- Entity models remain shared (one struct per entity, regardless of how many collections reference it).
- Update any references from expose blocks to collections.
- Storage interface functions operate on entities, not collections (collections are an API-layer concept).

**Fill wiring (`internal/slot/`, `internal/compiler/assembler.go`):**
- Slot interfaces are scoped to collections: a `before_create` slot on collection `org-users` generates a distinct interface from `before_create` on another collection.
- Generated `main.go` wires fills per collection.
- Operator generation (gate, chain, fan-out) uses collection names for type naming.

**Compiler (`internal/compiler/`):**
- `reconciler.go`: Plan generation iterates collections instead of expose blocks.
- `assembler.go`: main.go assembly wires per-collection handlers with their slot fills.
- `drift.go`: Drift detection works with collection-derived file paths.
- `state.go`: State tracking uses collection names.

**CLI (`cmd/stego/main.go`):**
- `stego init`: Generated scaffold uses `collections:` format.
- `stego fill create`: `--slot` flag works with collection-scoped slots (e.g. `--collection org-users --slot before_create`).
- `stego validate`: Validation uses collections throughout.
- All other commands pass through without structural changes.

**Validation (`internal/compiler/validate.go`):**
- Validate that the service declaration's `language` field matches the archetype's declared `language`. If they disagree, it is a validation error. Only `go` is supported in MVP; any other value is rejected. (Spec: "Stego is Written in Go")

**Tests:**
- Update all generator tests with collection-based fixtures.
- Update compiler tests.
- Update CLI tests.
- Add tests for language validation (match, mismatch, unsupported language).

### What does NOT change

- Entity type system (unchanged from Task 018).
- Registry structure (archetypes, components, mixins unchanged).
- Port resolution algorithm.

## Spec Excerpt

> Multiple collections referencing the same entity is the normal case. Each collection generates its own handler, routes, and wiring. The entity struct and storage are shared.
>
> Scoped collections generate nested routing. The `scope` field maps entity fields to parent entities. The compiler derives the URL path and generates parent existence verification at each level.

## Acceptance Criteria

1. rest-api generator produces one handler per collection (not per entity).
2. Path derivation: `org-users` with `scope: { org_id: Organization }` → `/organizations/{org_id}/users`. `all-users` with no scope → `/users`. `path_prefix` overrides derived paths.
3. Multiple collections referencing the same entity produce separate handlers sharing the entity struct.
4. Slot fills are wired per collection in generated `main.go`.
5. `stego plan` and `stego apply` work with collections.
6. `stego init` scaffolds with `collections:` format.
7. `stego fill create` accepts `--collection` flag.
8. `stego drift` detects changes in collection-derived files.
9. Language validation: service `language` must match archetype `language`; non-`go` values rejected with clear error.
10. All tests pass. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- `bc81b4b` fix(task-019): update rest-api generator tests for collection-based naming
- `79e20b1` feat(task-019): collection-aware code generation pipeline
