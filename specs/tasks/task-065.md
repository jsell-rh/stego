# Task 065: Fix Route Collision for Scoped Collections with path_prefix

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — Path Derivation Rules (line 735–745), Known Generator Bugs

**Status:** `ready-for-review`

**Depends on:** none (independent of task-064)

## Description

The spec states (line 739):

> Use `path_prefix` on the collection to override the derived segment when a shorter path is preferred.

"The derived segment" is the entity-derived portion of the path (e.g. `adapterstatuses` from entity `AdapterStatus`). For scoped collections, the parent chain must be preserved — only the leaf segment is replaced.

**Current behavior:** `collectionBasePathWithVisited` (generator.go:346–348) returns `path_prefix` directly when set, ignoring the parent chain:

```go
if eb.PathPrefix != "" {
    return eb.PathPrefix, nil  // parent chain lost for scoped collections
}
```

This causes three downstream failures:

1. **`resolveAncestorParams`** (generator.go:3099–3126): extracts 0 path parameters from a segment-only prefix like `/statuses`, finds N ancestors, and errors with "path /statuses contains 0 path parameters but entity has N ancestors."
2. **`validateRouteCollisions`** (generator.go:3307–3343): computes collision paths from the wrong base path, producing false positives or missing real collisions.
3. **Route registration** (generator.go:152): `hrefBase` is computed from the wrong base path, producing incorrect HTTP routes.

**Expected behavior for scoped `path_prefix: /statuses`:**

Given entity `AdapterStatus` scoped to `NodePool` (scoped to `Cluster`):
- Current: path = `/statuses` (wrong — parent chain dropped)
- Expected: path = `/clusters/{cluster_id}/nodepools/{nodepool_id}/statuses`

### Backward Compatibility

Existing tests (generator_test.go:2870–3120) use full-path prefixes that include ancestor parameters with custom names, e.g.:

```go
PathPrefix: "/clusters/{cid}/pools/{pid}/statuses"
```

These must continue working. The fix must distinguish between:
- **Segment replacement:** `path_prefix: /statuses` → prepend auto-derived parent chain
- **Full-path override:** `path_prefix: /clusters/{cid}/pools/{pid}/statuses` → use as-is (current behavior)

A reasonable heuristic: if the prefix contains `{…}` path parameters matching or exceeding the ancestor count, treat it as a full-path override. Otherwise, treat it as a segment replacement and prepend the auto-derived parent chain.

### Spec Wording

Rule #4 (line 745) says "replaces the derived path entirely (relative to `base_path`)." This is ambiguous — it could mean "replaces the entity segment entirely" or "is the entire path." Line 739 is unambiguous: "override the derived segment." Once the fix is verified, update rule #4 wording to match line 739's intent: `path_prefix` replaces the entity-derived segment, not the full path including ancestors.

## Acceptance Criteria

1. A scoped collection with `path_prefix: /statuses` (no ancestor params) produces a path that includes the full auto-derived parent chain followed by `/statuses`.
2. A scoped collection with a full-path `path_prefix` containing ancestor params (e.g. `/clusters/{cid}/pools/{pid}/statuses`) continues to work as-is — no parent chain prepended.
3. `resolveAncestorParams` does not error when `path_prefix` is a segment replacement on a scoped collection; ancestor params are derived conventionally.
4. `validateRouteCollisions` operates on corrected paths — no false positives for scoped segment-replacement prefixes.
5. All existing tests pass (`go test ./...`), including the full-path prefix tests at generator_test.go:2870–3120.
6. New test: scoped collection with segment-only `path_prefix` (e.g. entity `AdapterStatus` scoped to `NodePool` with `path_prefix: /statuses`) produces correct path and compiles.
7. New test: segment-only `path_prefix` on a scoped collection does not falsely collide with an unscoped collection that has a different path.
8. Rule #4 in the Path Derivation Rules section of `specs/registry/archetypes/rest-crud/spec.md` is updated to clarify that `path_prefix` replaces the entity-derived segment, preserving the parent chain.
9. The "Route collision for scoped `path_prefix`" entry is removed from the Known Generator Bugs section in `specs/registry/archetypes/rest-crud/spec.md` once the fix is verified.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
- f686c67 fix(task-065): segment-replacement path_prefix preserves parent chain for scoped collections
