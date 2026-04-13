# Task 052: Implicit Field Injection in Create and Upsert Handlers

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Implicit Fields** section: "On create/upsert: injects the implicit values into the entity before persisting. The client does not send these fields; if present in the request body, they are overwritten."

**Status:** `ready-for-review`

**Depends on:** task-050, task-051

## Description

Update the `rest-api` generator to inject implicit field values into the entity struct in create and upsert handlers.

### What changes

**`internal/generator/restapi/generator.go`:**

1. **`generateCreateMethod`** (~line 762, after the scope parent field assignment):
   - For each entry in `eb.Implicit`, emit: `entity.FieldName = "value"` (where `FieldName` is the PascalCase version of the implicit key).
   - This goes after `emitPopulateServerManagedFieldsCreate` and the parent ref field assignment, ensuring implicit values overwrite anything the client sent.

2. **`generateUpsertMethod`** (~line 1202, same position as create):
   - Same logic: for each entry in `eb.Implicit`, emit the field assignment.

3. Implicit fields should also be excluded from the request schema (they are server-managed). Update `isServerManagedField` or the create request schema generation so implicit fields for a given collection are not included in the request body. **However**, implicit fields vary per collection (not per entity), so the OpenAPI create-request schema is entity-scoped. The simpler approach per the spec is: "if present in the request body, they are overwritten" — meaning the overwrite in the handler is sufficient, and the OpenAPI schema doesn't need collection-specific variants. Leave OpenAPI as-is; the handler overwrite is the enforcement.

**`internal/generator/restapi/generator_test.go`:**
- Add test: collection with implicit generates field assignments in create handler.
- Add test: collection with implicit generates field assignments in upsert handler.
- Add test: implicit values overwrite position is after decode and before store call.

### What does NOT change

- List handler — that's task-053.
- Storage layer — no changes needed; the entity struct already has the field, and Create/Upsert receive the full struct.
- Other handlers (read, update, delete, patch) — implicit does not affect these.

## Acceptance Criteria

1. Generated create handler sets implicit fields on the entity before calling `store.Create`.
2. Generated upsert handler sets implicit fields on the entity before calling `store.Upsert`.
3. Implicit assignments use PascalCase field names matching the entity struct.
4. All tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- 0f43120 feat(task-052): inject implicit field values in create and upsert handlers
