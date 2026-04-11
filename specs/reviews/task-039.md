# Review: Task 039 — Server-Managed Fields and Request Schemas

## Round 1

Two findings against the spec and acceptance criteria.

- [-] [process-revision-complete] **Update handler missing `kind` validation (AC7 violation).** The task's "Request body `kind` validation" section specifies that the `kind` field in the JSON request body must be validated against the entity name, returning 400 on mismatch. The create handler (`generateCreateMethod`, line 716) and upsert handler (`generateUpsertMethod`, line 1119) both use `io.ReadAll` + `emitKindValidation` to validate `kind` before struct decode. The update handler (`generateUpdateMethod`, line 843-847) still uses `json.NewDecoder(r.Body).Decode()` with no `kind` validation. AC7 says "kind field in request body validated against entity name; wrong value returns 400; absent is accepted" — this applies to all handlers accepting a request body. PUT accepts a request body and is missing validation. The `needIO` variable (line 509) is `hasCreate || hasUpsert`, excluding update, which is consistent with the missing validation but confirms the omission. The spec (rest-crud spec lines 372): "The `kind` field in the request body (if present) is validated against the entity name" — no handler restriction.
- [-] [process-revision-complete] **Update handler zeroes `created_by`, `created_time`, and `generation` without preserving existing values (data loss on Replace).** The update handler calls `emitClearServerManagedFields` (line 849) which zeros ALL server-managed fields including `created_by`, `created_time`, and `generation`. Then `emitPopulateServerManagedFieldsUpdate` (line 860) only restores `updated_by`, `updated_time`, and `generation++`. After clearing: `created_by = ""`, `created_time = time.Time{}`, `generation = 0`. After populate: `created_by` still `""`, `created_time` still zero, `generation = 1` (incremented from 0, not from existing value). Since `store.Replace` uses `gorm.Save(entity)` which writes all columns (spec line 377: "`Replace(ctx, entity)` -- `g2.Save(entity)`"), the original `created_by`, `created_time`, and correct `generation` values are overwritten on every update. For `generation`, the task says "increment (if present on entity)" — incrementing from zero is not incrementing the existing value. For `created_by`/`created_time`, the spec says these are "populated from the authenticated identity" and "set to `time.Now()`" on create — wiping them on update is destructive.

## Round 2

One finding against the spec.

- [-] [process-revision-complete] **Kind validation silently accepts non-string `kind` values (AC7 violation).** The `emitKindValidation` function (generator.go:2396-2409) generates code that only validates `kind` when it can be unmarshaled into a Go `string`. The generated code: `if err := json.Unmarshal(kindRaw, &kind); err == nil && kind != "Entity" { ... }`. When `kind` is a JSON number, boolean, array, or object (e.g., `{"kind": 42}`, `{"kind": true}`, `{"kind": [1]}`), `json.Unmarshal(kindRaw, &kind)` fails because the value is not a string. With `err != nil`, the condition `err == nil && kind != "Entity"` evaluates to false. The validation is skipped entirely, and the request proceeds as if no `kind` field was provided. Spec (rest-crud spec line 372): "If present and wrong, the server returns 400." A non-string kind value is present in the request body and does not match the entity name — it is "present and wrong." The handler must return 400. This affects all three handlers that call `emitKindValidation`: create (line 723), update (line 859), and upsert (line 1141). The fix: change the generated condition from `err == nil && kind != entityName` to `err != nil || kind != entityName`, so that both type errors (can't parse as string) and value mismatches (parsed but wrong) produce a 400.

## Round 3

One finding against the spec.

- [x] [process-revision-complete] **Patch handler does not validate `kind` field in request body (AC7 violation).** The rest-crud spec (line 372) states: "The `kind` field in the request body (if present) is validated against the entity name but is not persisted -- it is a client-side type assertion. If absent, the server does not reject the request. If present and wrong, the server returns 400." This is stated without handler restriction — it applies to all request bodies. The `generatePatchMethod` function (generator.go:1254) decodes the body directly via `json.NewDecoder(r.Body).Decode(&patch)` into a typed `PatchRequest` struct that only contains patchable fields. A `kind` field in the JSON body is silently ignored by Go's decoder (unknown fields are discarded by default). If a client sends `{"kind": "WrongEntity", "spec": {...}}` to PATCH, the server accepts the request without returning 400. `emitKindValidation` is called in create (line 723), update (line 859), and upsert (line 1141), but not in patch. AC7 says "kind field in request body validated against entity name; wrong value returns 400; absent is accepted" — no handler restriction. The patch handler must read the body with `io.ReadAll`, call `emitKindValidation`, then decode the patch struct from the buffered bytes, matching the pattern used by create, update, and upsert.

## Round 4

No findings. All acceptance criteria verified:

1. `isServerManaged()` correctly classifies all four rules (computed, timestamp, created_by/updated_by, generation). Unit tests cover all cases including negative cases.
2. `{Entity}CreateRequest` OpenAPI schema excludes server-managed fields, scope fields, and `id`. Verified in generated spec.
3. POST and PUT request body `$ref` correctly points to `{Entity}CreateRequest`.
4. Create handler populates `created_by`/`updated_by` from auth context via `IdentityFromContext`.
5. Create handler uses field `default` value for `generation` field.
6. Create handler clears all server-managed fields (`emitClearServerManagedFields`) before populating with server-derived values, preventing client from setting them.
7. `kind` field validation applied to all four request-body handlers (create, update, upsert, patch). Non-string values correctly rejected (`err != nil || kind != Entity`). Absent kind accepted.
8. Update handler sets `updated_by` from auth context and `updated_time` to `time.Now()` via `emitPopulateServerManagedFieldsUpdate`.
9. Update handler preserves `created_by`, `created_time`, and `generation` (incremented from existing value) via `emitPreserveCreateOnlyFields`.
10. Comprehensive test suite covers all classifications, handler behaviors, kind validation across handlers, and full compilation test.
11. `go test ./...` passes. `go build ./cmd/stego` compiles.
