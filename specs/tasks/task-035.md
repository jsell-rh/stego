# Task 035: Fix Path Derivation to Use Entity Plural

**Spec Reference:** [specs/registry/archetypes/rest-crud/spec.md](../registry/archetypes/rest-crud/spec.md) -- "Path Derivation Rules (must be implemented)"

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-035.md](../reviews/task-035.md)

**Depends on:** task-019 (collection-aware code generation pipeline)

## Description

Fix `collectionBasePath()` in `internal/generator/restapi/generator.go` to derive URL path segments from the entity name (pluralized, lowercased) instead of the collection name.

### Spec Clarification (resolved)

The rest-crud spec had an internal inconsistency: the stated derivation rule says "entity name (pluralized, lowercased)" but the YAML examples showed `AdapterStatus → statuses`, which no mechanical application of that rule can produce (`lowercase + pluralize` yields `adapterstatuses`).

**Resolution:** The spec has been updated so examples are consistent with the stated rule. The derivation algorithm is: `lowercase(entityName)` then `pluralize`. Multi-word PascalCase names are treated as a single token:
- `NodePool` → `nodepool` → `nodepools`
- `AdapterStatus` → `adapterstatus` → `adapterstatuses`
- `Cluster` → `cluster` → `clusters`

If a shorter path segment is preferred (e.g. `statuses` instead of `adapterstatuses`), the collection should use `path_prefix` to override.

### Path Derivation Rules

1. **Unscoped collection:** path = `/{entity_plural}` (e.g. entity `Cluster` → `/clusters`)
2. **Scoped collection:** path = `/{parent_plural}/{parent_id_param}/{entity_plural}` (e.g. entity `NodePool` scoped to `Cluster` → `/clusters/{cluster_id}/nodepools`)
3. **Multi-level scope:** chains parent paths recursively (e.g. entity `AdapterStatus` scoped to `NodePool` which is scoped to `Cluster` → `/clusters/{cluster_id}/nodepools/{nodepool_id}/adapterstatuses`)
4. **`path_prefix` override:** when set on a collection, replaces the derived path entirely (relative to `base_path`)

### Review Round 1 Findings (to address)

1. **`entityPathSegment()` algorithm:** The `strings.ToLower(entityName) + pluralize()` approach is correct for the updated spec. The previous review flagged `adapterstatuses` as wrong because the old spec examples showed `statuses` — the spec has been corrected.
2. **Test `TestCollectionBasePath_MultiLevel`:** The test at `generator_test.go:1449` expects `/clusters/{cluster_id}/nodepools/{nodepool_id}/adapterstatuses` — verify this matches the current implementation output.
3. **Test `TestEntityPathSegment`:** The test at `generator_test.go:1463` expects `{"AdapterStatus", "adapterstatuses"}` — verify this matches the current implementation output.
4. **Example service `OrgSetting`:** The generated output produces `/organizations/{org_id}/orgsettings` for entity `OrgSetting`. Per the updated rule, this is correct (`orgsetting` → `orgsettings`).

### Implementation Notes

- `collectionBasePath()` at `restapi/generator.go:296` needs to use `eb.Entity` (lowercased, pluralized) instead of `eb.Name`
- A `pluralize` function exists in `postgresadapter/generator.go:1165` but is package-private. Either extract to a shared package (e.g. `internal/naming`) or duplicate in `restapi` (spec says duplication is cheaper than coupling -- but this is internal code, not component-to-component)
- Parent path resolution also uses collection name and needs the same fix
- All downstream code that builds paths from `collectionBasePath` output (OpenAPI, handler routes, href expressions, wiring) will pick up the fix automatically
- Existing tests will need updating to expect entity-plural paths

## Spec Excerpt

> Collection paths must be derived from the entity name (pluralized, lowercased), not from the collection name. The collection name is an identifier for the service declaration; the URL path comes from the entity.
>
> The derivation algorithm: `lowercase(entityName)` then `pluralize`. Multi-word PascalCase names are treated as a single token (e.g. `NodePool` -> `nodepool` -> `nodepools`, `AdapterStatus` -> `adapterstatus` -> `adapterstatuses`). Use `path_prefix` on the collection to override the derived segment when a shorter path is preferred.

## Acceptance Criteria

1. `collectionBasePath()` derives URL segments from entity name, not collection name.
2. Unscoped collection `clusters` with entity `Cluster` → path `/clusters`.
3. Scoped collection `cluster-nodepools` with entity `NodePool` scoped to `Cluster` → path `/clusters/{cluster_id}/nodepools`.
4. Multi-level scoped collection with entity `AdapterStatus` scoped through `NodePool` → `Cluster` → path `/clusters/{cluster_id}/nodepools/{nodepool_id}/adapterstatuses`.
5. `path_prefix` override still takes precedence over derived paths.
6. OpenAPI spec, handler routes, href expressions, and wiring all reflect the corrected paths.
7. Example service output updated to reflect correct entity-derived paths.
8. `go test ./...` passes from the repo root.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- 0cb87ef fix(task-035): derive URL paths from entity name, not collection name
