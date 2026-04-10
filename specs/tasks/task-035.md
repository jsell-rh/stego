# Task 035: Fix Path Derivation to Use Entity Plural

**Spec Reference:** [specs/registry/archetypes/rest-crud/spec.md](../registry/archetypes/rest-crud/spec.md) -- "Path Derivation Rules (must be implemented)"

**Status:** `not-started`

**Depends on:** task-019 (collection-aware code generation pipeline)

## Description

Fix `collectionBasePath()` in `internal/generator/restapi/generator.go` to derive URL path segments from the entity name (pluralized, lowercased) instead of the collection name.

### Current Bug

The generator uses `eb.Name` (the collection name) as the URL segment. For scoped collections this produces incorrect paths:

- `cluster-nodepools` -> `/clusters/{cluster_id}/cluster-nodepools` (wrong)
- Should be: `/clusters/{cluster_id}/nodepools` (entity `NodePool` -> `nodepools`)

### Path Derivation Rules

1. **Unscoped collection:** path = `/{entity_plural}` (e.g. entity `Cluster` -> `/clusters`)
2. **Scoped collection:** path = `/{parent_plural}/{parent_id_param}/{entity_plural}` (e.g. entity `NodePool` scoped to `Cluster` -> `/clusters/{cluster_id}/nodepools`)
3. **Multi-level scope:** chains parent paths recursively (e.g. entity `AdapterStatus` scoped to `NodePool` which is scoped to `Cluster` -> `/clusters/{cluster_id}/nodepools/{nodepool_id}/statuses`)
4. **`path_prefix` override:** when set on a collection, replaces the derived path entirely (relative to `base_path`)

### Implementation Notes

- `collectionBasePath()` at `restapi/generator.go:296` needs to use `eb.Entity` (lowercased, pluralized) instead of `eb.Name`
- A `pluralize` function exists in `postgresadapter/generator.go:1165` but is package-private. Either extract to a shared package (e.g. `internal/naming`) or duplicate in `restapi` (spec says duplication is cheaper than coupling -- but this is internal code, not component-to-component)
- Parent path resolution also uses collection name and needs the same fix
- All downstream code that builds paths from `collectionBasePath` output (OpenAPI, handler routes, href expressions, wiring) will pick up the fix automatically
- Existing tests will need updating to expect entity-plural paths

## Spec Excerpt

> Collection paths must be derived from the entity name (pluralized, lowercased), not from the collection name. The collection name is an identifier for the service declaration; the URL path comes from the entity.
>
> Current bug: the generator uses the collection name (`cluster-nodepools`) as the URL segment instead of the entity plural (`nodepools`). This produces `/clusters/{cluster_id}/cluster-nodepools` instead of `/clusters/{cluster_id}/nodepools`.

## Acceptance Criteria

1. `collectionBasePath()` derives URL segments from entity name, not collection name.
2. Unscoped collection `clusters` with entity `Cluster` -> path `/clusters`.
3. Scoped collection `cluster-nodepools` with entity `NodePool` scoped to `Cluster` -> path `/clusters/{cluster_id}/nodepools`.
4. Multi-level scoped collection with entity `AdapterStatus` scoped through `NodePool` -> `Cluster` -> path `/clusters/{cluster_id}/nodepools/{nodepool_id}/statuses`.
5. `path_prefix` override still takes precedence over derived paths.
6. OpenAPI spec, handler routes, href expressions, and wiring all reflect the corrected paths.
7. Example service output updated to reflect correct entity-derived paths.
8. `go test ./...` passes from the repo root.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

