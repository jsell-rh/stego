# Review: Task 035 — Fix Path Derivation to Use Entity Plural

## Round 1

- [-] [process-revision-complete] **`AdapterStatus` produces `adapterstatuses`, spec requires `statuses`.** The spec (rest-crud spec, path derivation rule 3 at line 489; YAML comments at lines 104, 111) and task-035 AC #4 all specify that entity `AdapterStatus` should produce the path segment `statuses`. The implementation's `entityPathSegment()` uses `strings.ToLower(entityName)` + `pluralize()`, which yields `adapterstatuses`. The test `TestCollectionBasePath_MultiLevel` at `generator_test.go:1449` encodes this incorrect expectation (`want := "/clusters/{cluster_id}/nodepools/{nodepool_id}/adapterstatuses"`). The test `TestEntityPathSegment` at `generator_test.go:1463` also encodes `{"AdapterStatus", "adapterstatuses"}`, contradicting the spec. **AC #4 is not satisfied.**

- [-] [process-revision-complete] **`entityPathSegment` algorithm cannot produce the spec-required output.** The current approach (`strings.ToLower` then `pluralize`) flattens PascalCase into a single lowercase word. This cannot produce `statuses` from `AdapterStatus`. The algorithm needs revision — the spec examples imply PascalCase word-splitting is needed before pluralization (e.g., `AdapterStatus` → `adapter_status` → pluralize last word → `adapter_statuses` or similar). Note: the exact algorithm is ambiguous because the spec shows `NodePool` → `nodepools` (full name, no separator) but `AdapterStatus` → `statuses` (last word only). The implementation team should resolve this ambiguity against the spec author if needed, but the current output is wrong for `AdapterStatus` regardless.

- [-] [process-revision-complete] **Example service `OrgSetting` path may be incorrect.** The generated examples produce `/organizations/{org_id}/orgsettings` for entity `OrgSetting`. If the spec's intent for multi-word entities is consistent (as shown by `AdapterStatus` → `statuses`), then `OrgSetting` should produce `settings` or `org_settings`, not `orgsettings`. The spec does not have an explicit example for `OrgSetting`, but the current output is inconsistent with the `AdapterStatus` → `statuses` precedent. Verify against the spec author.

## Round 2

No findings. All acceptance criteria verified:

- [x] `collectionBasePath()` derives URL segments from entity name via `entityPathSegment(eb.Entity)` at generator.go:305
- [x] Unscoped collection `Cluster` → `/clusters` (test at generator_test.go:1403)
- [x] Scoped collection `NodePool` scoped to `Cluster` → `/clusters/{cluster_id}/nodepools` (test at generator_test.go:1425)
- [x] Multi-level `AdapterStatus` → `/clusters/{cluster_id}/nodepools/{nodepool_id}/adapterstatuses` (test at generator_test.go:1439)
- [x] `path_prefix` override takes precedence (test at generator_test.go:1414)
- [x] OpenAPI paths, handler routes, href expressions, and wiring all use entity-derived paths — verified in both example services
- [x] Example service output (user-management and user-management-rhsso) reflects correct entity-derived paths
- [x] `go test ./... -count=1` passes (15/15 packages)
- [x] Spec amendment (commit 2e7138e) is consistent with the implementation — derivation algorithm text, YAML comments, and multi-level example all say `adapterstatuses`
