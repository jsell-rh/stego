# Review: Task 031 — Scoped Collection Access Enforcement in Generators

## Round 1

- [-] [process-revision-complete] **Scoped Read handler response drops storage-layer fields (`created_time`, `updated_time`) that unscoped Read preserves.** In `generateReadMethod` (`generator.go:688-715`), the scoped path stores the `store.Get()` result in `existing` (type `any`), performs a JSON roundtrip into the api-type struct (`var user User` at line 710), checks the scope field, then encodes `user` in the response (line 723). The original `existing` value — which contains storage metadata fields (`created_time`, `updated_time` from the postgres-adapter's `Meta` struct, `generator.go:239-241`) — is discarded. In the non-scoped path (line 693), the `store.Get()` result is stored directly in `user` (type `any`) and encoded as-is, preserving all storage-layer fields. This means a client reading the same entity through a scoped collection URL (e.g., `GET /organizations/1/users/123`) receives a response missing `created_time` and `updated_time`, while reading via an unscoped collection URL (e.g., `GET /users/123`) includes them. The inconsistency applies in both bare and envelope response modes — `presentEntity` (line 1396-1399) marshals whatever type it receives, so the field difference propagates. The scope check could extract the scope field value without altering the response by unmarshaling into a `map[string]any` for the field lookup and then encoding the original `existing` value.

## Round 2

- [-] [process-revision-complete] **Scoped delete-only collections generate uncompilable code — missing `encoding/json` import.** In `generateHandler()` (`generator.go:346-349`), the `needJSON` flag is set to `true` only when the operation is not `OpDelete`: `if op != types.OpDelete { needJSON = true }`. The scoped Delete handler (`generator.go:854,860`) emits `json.Marshal(existing)` and `json.Unmarshal(scopeData, &user)` for scope verification — both require `encoding/json`. When a scoped collection has only the `delete` operation, `needJSON` remains `false`, the `encoding/json` import is omitted, and the generated file references `json.Marshal`/`json.Unmarshal` without importing their package. This is a Go compile error. The test `TestGenerate_ScopedDeleteVerifiesScope` (`generator_test.go:7623`) exercises exactly this configuration (operations: `[OpDelete]`, scoped) but only checks string content via `strings.Contains`, not import completeness, so the bug passes the test suite. Analogous scoped paths in Update (`generator.go:777`) and Patch (`generator.go:1217`) are not affected because those operations are not `OpDelete` and always set `needJSON = true`.

## Round 3

No findings. All acceptance criteria verified:

1. Scoped Read handler verifies scope via `emitMapScopeCheck` (map[string]any), returns 404 on mismatch, preserves storage metadata in response.
2. Scoped Update handler pre-fetches entity, verifies scope via `emitMapScopeCheck` before mutation.
3. Scoped Patch handler verifies scope via `emitScopeCheck` (typed struct) after existing entity unmarshal.
4. Scoped Delete handler pre-fetches entity, verifies scope via `emitMapScopeCheck` before deletion.
5. Unscoped collections emit no scope check code (`isScoped` flag guards all branches).
6. `needJSON` initialized to `isScoped`, fixing the delete-only import issue from Round 2.
7. Tests cover all four scoped operations plus unscoped negative case. `go test ./...` passes.
