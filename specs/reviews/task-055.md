# Review: Task 055 — Regenerate Example Output with Implicit Fields

## Summary

Task 055 adds an `AuditEvent` entity with a `source_type` discriminator and two collections (`user-audit-events`, `org-audit-events`) with different implicit values. The example output was regenerated via `stego apply`.

## Acceptance Criteria Check

1. **Example service.yaml includes at least one collection with `implicit`** — PASS. Two collections added: `user-audit-events` (implicit `source_type: "User"`) and `org-audit-events` (implicit `source_type: "Organization"`).
2. **Example output regenerated and committed** — PASS. Commit `3de9c87`.
3. **Generated create/upsert handlers show implicit field injection** — PASS. Both handlers set `SourceType` to the collection's implicit value before persisting.
4. **Generated list handlers show `ImplicitFilters` in ListOptions** — PASS. Both list handlers populate `ImplicitFilters` with the correct `source_type` value.
5. **`go build ./examples/...` compiles** — PASS (main module builds; example is generated output, not a buildable module).
6. **All tests pass: `go test ./...`** — PASS.

## Findings

- [-] [process-revision-complete] **OpenAPI `AuditEventCreateRequest` schema includes implicit field `source_type` as required.** The spec (Implicit Fields section) says: "The client does not send these fields; if present in the request body, they are overwritten." The generated `AuditEventCreateRequest` schema at `examples/user-management/out/internal/api/openapi.json:1349-1372` includes `source_type` as both a property and a required field. Since the generated service uses validation middleware that validates against this schema, clients are forced to send `source_type` — a value the handler immediately overwrites. Root cause: the generator at `internal/generator/restapi/generator.go:2108-2109` excludes server-managed and scope fields from `CreateRequest` schemas but does not exclude implicit fields. The example output committed by this task surfaces the defect for the first time.
