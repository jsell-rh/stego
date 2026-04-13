# Task 056: Exclude Implicit Fields from Request Schemas

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Implicit Fields** section, **Server-Managed Fields and Request Schemas** section

**Status:** `ready-for-review`

**Depends on:** task-052

**Blocks:** task-055 (needs-revision)

## Description

The `rest-api` generator builds per-entity `{Entity}CreateRequest` OpenAPI schemas that exclude server-managed fields and scope fields. However, it does not exclude **implicit fields** — fields whose values are determined by the collection, not by the client.

The spec (Implicit Fields section) states: "The client does not send these fields; if present in the request body, they are overwritten." Including implicit fields in the CreateRequest schema forces clients to send values the handler immediately overwrites. When `request_validation: openapi-schema` is active, the validation middleware rejects requests that omit these fields — making the API unusable for the intended pattern.

### Root cause

At `internal/generator/restapi/generator.go:2108-2109`, the CreateRequest schema loop checks `isServerManaged(f) || scopeFields[f.Name]` but does not check whether the field appears in any collection's `implicit` map.

### What changes

**`internal/generator/restapi/generator.go`:**
- Before building each entity's CreateRequest schema, collect all implicit field names from every collection that references the entity.
- Add those field names to the exclusion check at line 2109 (alongside `isServerManaged` and `scopeFields`).

### What does NOT change

- The implicit field injection logic in create/upsert handlers (done in task 052).
- The implicit field filtering in list handlers (done in task 053).
- The entity response schema (`{Entity}`) — implicit fields appear in responses since they are stored on the entity.

## Acceptance Criteria

1. `{Entity}CreateRequest` schema excludes fields that appear in any collection's `implicit` map for that entity.
2. If a field is implicit in one collection but not another (both referencing the same entity), the field is still excluded from the entity-wide CreateRequest schema (conservative: exclude if implicit anywhere).
3. Existing tests pass: `go test ./...`
4. New or updated test in `internal/generator/restapi/generator_test.go` verifying implicit fields are excluded from CreateRequest schemas.
5. The generated OpenAPI spec for the example service no longer includes `source_type` in `AuditEventCreateRequest`.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- d36049b feat(task-056): exclude implicit fields from request schemas
