# Task 050: Add Implicit Field to Collection Domain Type

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Implicit Fields** section

**Status:** `ready-for-review`

**Depends on:** none (all prior tasks complete)

## Description

Add the `Implicit` field to the `Collection` struct in `internal/types/types.go` so that the parser can deserialize `implicit` from `service.yaml` collections.

### What changes

**`internal/types/types.go`:**
- Add `Implicit map[string]string \`yaml:"implicit,omitempty"\`` to the `Collection` struct (after `Scope`).

No helper methods needed — implicit is a simple key→value map iterated directly by generators.

### What does NOT change

- Parser (`internal/parser/parser.go`) — YAML unmarshaling is generic; adding the struct field is sufficient.
- Validators, generators, tests — those come in subsequent tasks.

## Acceptance Criteria

1. `Collection` struct has `Implicit map[string]string` with yaml tag `implicit,omitempty`.
2. Parsing a service.yaml with `implicit: { resource_type: "Cluster" }` on a collection populates the field.
3. `go build ./...` compiles.
4. All existing tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

