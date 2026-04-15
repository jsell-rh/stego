# Review: Task 061 — OpenAPI and Metadata Discovery Endpoint Generation

## Findings

- [-] [process-revision-complete] **Empty `base_path` metadata route creates Go 1.22+ subtree match that intercepts ALL GET requests.** In `generator.go:314-321`, when `ctx.BasePath` is empty, `metadataPath` is set to `"/"` and the route becomes `topMux.HandleFunc("GET /", discoveryHandler.ServeMetadata)`. In Go 1.22+ `ServeMux`, `"GET /"` is a subtree pattern (the path part `/` ends with `/`), matching ALL GET requests to any path — not just `GET /`. This causes the metadata handler to intercept every GET request (e.g. `GET /users`, `GET /openapi`) before it can reach the authenticated inner handler via `topMux.Handle("/", handler)`. The correct pattern for matching only the root path exactly is `"GET /{$}"`. This is reachable: `gen.Context.BasePath` documents "Empty string means paths are served from root" and the code explicitly handles this case. The test `TestGenerate_DiscoveryEmptyBasePath` verifies the route strings but does not catch the routing misbehavior.

- [-] [process-revision-complete] **Metadata `collections` field serializes as `null` instead of empty array when no unscoped collections exist.** In `generator.go:4215-4228`, `meta.Collections` is never initialized — it starts as a nil slice. When all collections are scoped, the append loop is never entered, and `json.MarshalIndent` serializes the nil slice as `"collections": null`. The spec (lines 685-696) shows `collections` as a JSON array. Standard JSON API convention and client expectations are that array fields are `[]` (empty array), not `null`. Initialize with `meta.Collections = []metadataCollection{}` or equivalent to ensure consistent array serialization.

## Round 2 — complete (no new findings)

Verified after fix commit 7f51ca8:

- Both prior findings resolved correctly: `/{$}` for exact root match, `make([]metadataCollection, 0)` for empty-array serialization
- All 7 acceptance criteria satisfied
- Generated examples (`user-management`, `user-management-rhsso`) produce correct discovery files, metadata JSON, and topMux wiring
- Discovery routes correctly bypass auth middleware via topMux pattern
- Constructor consumption, variable naming, and rename propagation all verified
- `go test ./...` passes
