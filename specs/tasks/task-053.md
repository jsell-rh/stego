# Task 053: Implicit Field Filtering in List Handlers and Storage Layer

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Implicit Fields** section: "On list: adds the implicit values as WHERE clause filters, so the collection only returns rows matching its implicit context."

**Status:** `not-started`

**Depends on:** task-050, task-051

## Description

Update the `rest-api` generator and `postgres-adapter` generator to pass implicit field filters through the List call chain.

### What changes

**Approach:** Extend `ListOptions` with an `ImplicitFilters map[string]string` field. The rest-api handler populates it from the collection's implicit declaration; the postgres-adapter applies each entry as an additional `WHERE field = ?` clause.

**`internal/generator/restapi/generator.go`:**

1. **`ListOptions` struct generation** (~line 1490 area where types are emitted):
   - Add `ImplicitFilters map[string]string` field to the generated `ListOptions` struct.

2. **`generateListMethod`** (~line 1076, where `ListOptions` is constructed):
   - When `len(eb.Implicit) > 0`, populate `ImplicitFilters` in the `opts` initialization:
     ```go
     opts := ListOptions{..., ImplicitFilters: map[string]string{"resource_type": "Cluster"}}
     ```
   - The implicit map is known at generation time (compile-time constants), so the values are hardcoded string literals.

**`internal/generator/restapi/router.go` (or wherever the Storage interface is emitted):**
   - No change to `List` signature — `ListOptions` already carries the filters.

**`internal/generator/postgresadapter/generator.go`:**

1. **`List` method** (~line 728, after scope WHERE clause):
   - After the scope filter block, add a loop over `opts.ImplicitFilters`:
     ```go
     for field, value := range opts.ImplicitFilters {
         if !validCols[field] {
             return ListResult{}, fmt.Errorf("invalid implicit filter field %q for entity %s", field)
         }
         query = query.Where(field+" = ?", value)
     }
     ```
   - This reuses the existing `validCols` map for column name validation (SQL injection prevention).

**Tests:**
- `internal/generator/restapi/generator_test.go`: collection with implicit generates `ImplicitFilters` in ListOptions.
- `internal/generator/postgresadapter/generator_test.go`: List method applies implicit filters as WHERE clauses.

### What does NOT change

- The `List` function signature (still `scopeField`, `scopeValue`, `opts`).
- Scope handling — implicit filters are additive, applied alongside scope filters.

## Acceptance Criteria

1. Generated `ListOptions` struct includes `ImplicitFilters map[string]string`.
2. Generated list handlers populate `ImplicitFilters` from collection's implicit declaration.
3. Generated `List` method in postgres-adapter applies each implicit filter as a WHERE clause.
4. Implicit filter field names are validated against valid columns (no SQL injection).
5. Implicit filters combine correctly with scope filters (both applied as AND conditions).
6. All tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

