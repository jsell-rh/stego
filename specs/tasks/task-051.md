# Task 051: Validate Implicit Fields on Collections

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Implicit Fields** section

**Status:** `not-started`

**Depends on:** task-050

## Description

Add validation for the `implicit` field on collections in `internal/compiler/validate.go`. The spec states: "The keys must be fields on the entity. The values are string literals."

### What changes

**`internal/compiler/validate.go` — inside `validateCollectionReferences`:**
- After the existing scope validation block (~line 498), add validation for `c.Implicit`:
  - Each key in `Implicit` must be a field name on the referenced entity (use the existing `entityFields[c.Entity]` map).
  - Each value must be a non-empty string (they are string literals).
  - Implicit field keys must not overlap with computed fields (implicit sets concrete values; computed fields are filled by slots — these are contradictory).
  - Implicit field keys must not overlap with scope keys (scope is set from the URL path parameter; implicit is set from the collection declaration — double-setting the same field is ambiguous).
- Error messages should follow the existing pattern: `fmt.Sprintf("collection %q ...")`.

**`internal/compiler/validate_test.go`:**
- Add test cases for:
  - Valid implicit (no error).
  - Implicit key not a field on entity → error.
  - Implicit key overlaps with computed field → error.
  - Implicit key overlaps with scope key → error.
  - Empty implicit value → error.

### What does NOT change

- The multi-field scope cardinality check — implicit is unrelated to scope cardinality.
- Generator code — that comes in task-052/053.

## Acceptance Criteria

1. `stego validate` rejects implicit keys that are not entity fields.
2. `stego validate` rejects implicit keys that overlap with scope keys.
3. `stego validate` rejects implicit keys on computed fields.
4. `stego validate` rejects empty implicit string values.
5. Valid implicit declarations pass validation.
6. All tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

