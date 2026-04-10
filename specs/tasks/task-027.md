# Task 027: tsl-search Component Generator

**Spec Reference:** "Components > tsl-search" (rest-crud spec)

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-027.md](../reviews/task-027.md)

## Description

The rest-crud archetype spec includes `tsl-search` as a default component that integrates the Tree Search Language library into list handlers. This task creates the tsl-search component generator and adds it to the rest-crud archetype.

### What changes

**Component YAML (`registry/components/tsl-search/component.yaml`):**
```yaml
kind: component
name: tsl-search
version: 1.0.0
output_namespace: internal/search

requires:
  - storage-adapter

provides:
  - search-engine

slots:
  - name: resolve_field
    proto: stego.components.tsl_search.slots.ResolveField
    default: column-name-lookup
```

**Slot proto (`registry/components/tsl-search/slots/resolve_field.proto`):**
- Define `ResolveField` service for custom field-to-column mapping.

**Archetype YAML (`registry/archetypes/rest-crud/archetype.yaml`):**
- Add `tsl-search` to components list.

**Generator (`internal/generator/tslsearch/`):**
- Generate TSL expression parsing function that converts `?search=` expressions to parameterized WHERE clauses.
- Generate field name validation against entity field definitions (disallowed/unknown fields rejected with 400).
- Generate field-to-column mapping (entity field names to SQL column names, including table prefixes for JOINs).
- Use squirrel for parameterized query construction (SQL injection prevention).
- Integration with GenericDao (from Task 022) for building queries with the WHERE clause.

**rest-api generator update:**
- When `tsl-search` component is resolved, generate `?search=` query parameter handling in all list handlers.
- Parse the search expression and pass it to the tsl-search generated code.

**Port resolution:**
- `tsl-search` requires `storage-adapter` and provides `search-engine`.
- Port resolution validates the dependency.

**resolve_field slot:**
- Default behavior maps field names directly to column names.
- Fills can override for JSONB path queries, label queries, or cross-entity JOINs.

**Wiring:**
- Generated go.mod includes TSL library (`github.com/yaacov/tree-search-language`) and squirrel dependencies.

## Spec Excerpt

> Integrates the Tree Search Language library into list handlers. Generates SQL helper functions for parsing `?search=` expressions into parameterized WHERE clauses.
>
> The `resolve_field` slot allows fills to customize how specific field types map to SQL. Default behavior maps field names directly to column names.

## Acceptance Criteria

1. `tsl-search` component YAML and slot proto created in registry.
2. `tsl-search` added to rest-crud archetype components list.
3. Generator produces TSL parsing and field validation code.
4. Field-to-column mapping generated from entity definitions.
5. Parameterized queries via squirrel (no SQL injection).
6. List handlers gain `?search=` query parameter support.
7. `resolve_field` slot generated with default column-name-lookup behavior.
8. Port resolution passes (requires storage-adapter, provides search-engine).
9. Generated go.mod includes TSL and squirrel dependencies.
10. Tests cover search parsing, field validation, and SQL generation.
11. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- f70b449 feat(task-027): add tsl-search component generator
