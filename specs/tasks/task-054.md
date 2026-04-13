# Task 054: Remove Multi-Field Scope Cardinality Restriction

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Implicit Fields** section (implicit uses multi-field patterns like `scope: { resource_id: Cluster }` + `implicit: { resource_type: "Cluster" }` which put multiple fields on the same entity, but scope itself remains single-field; the cardinality check is still valid for scope)

**Status:** `not-started`

**Depends on:** task-050

## Description

Review whether the multi-field scope cardinality restriction needs to be removed or relaxed. Per the user's instruction: "Remove the multi-field scope validation error (no longer needed for this pattern)."

The current restriction is in two places:
1. `internal/compiler/validate.go:476` — rejects `len(c.Scope) > 1`
2. `internal/generator/restapi/generator.go:2932` — `validateScopeCardinality()` also rejects `len(c.Scope) > 1`

The implicit fields pattern does NOT require multi-field scope — the polymorphic example uses single-field scope (`scope: { resource_id: Cluster }`) plus implicit (`implicit: { resource_type: "Cluster" }`). However, the user explicitly asked to remove this restriction, so we remove it.

### What changes

**`internal/compiler/validate.go`:**
- Remove the scope cardinality check at lines 473-481 (the `if len(c.Scope) > 1` block).

**`internal/generator/restapi/generator.go`:**
- Remove the `validateScopeCardinality` function (~lines 2926-2945).
- Remove the call to `validateScopeCardinality` at line 74.
- Update `ScopeField()` and `ParentEntity()` in `internal/types/types.go` if needed. Currently they return the first map entry (non-deterministic for multi-key maps). If we're removing the cardinality check, we need to either:
  - (a) Accept that multi-field scope is now allowed and update the path derivation to handle multiple scope levels, OR
  - (b) Simply remove the error but document that multi-field scope produces non-deterministic behavior (not recommended).
  
  **Recommended approach:** Remove the validation error. The `ScopeField()`/`ParentEntity()` helpers are only called after the check; without the check, multi-field scopes will still work non-deterministically. Add a TODO comment noting that multi-field scope path derivation is deferred to post-MVP. The immediate need is to unblock the implicit pattern, which uses single-field scope.

**Tests:**
- `internal/compiler/validate_test.go`: Remove or update the test case that expects the multi-field scope error.
- `internal/generator/restapi/generator_test.go`: Remove or update the test case for multi-field scope rejection.

### What does NOT change

- Single-field scope behavior — unchanged.
- Implicit field handling — orthogonal.

## Acceptance Criteria

1. `stego validate` no longer rejects collections with `len(scope) > 1`.
2. Generator no longer calls `validateScopeCardinality`.
3. Existing single-field scope tests still pass.
4. All tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

