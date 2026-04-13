# Task 058: Wire Before-Operation Lifecycle Slots into Rest-API Generator

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Lifecycle Slots / Before slots** table (lines 308–315)

**Status:** `ready-for-review`

**Depends on:** task-057

## Description

Wire the three new before-operation lifecycle slots (`before_upsert`, `before_patch`, `before_delete`) into the rest-api generator so that service declarations can bind fills to these slots and the generated handlers invoke them at the correct lifecycle point.

### Generator changes (`internal/generator/restapi/generator.go`)

**`knownSlotRequestMeta`** — add entries:
- `before_upsert`: `{HasCaller: true}` (same shape as before_create)
- `before_patch`: `{HasCaller: true}` (patch fields passed via Input)
- `before_delete`: `{HasCaller: true}` (entity ID passed, no fields map — see below)

**`slotBeforeOps`** — add mappings:
- `before_upsert` → fires on `upsert`
- `before_patch` → fires on `patch`
- `before_delete` → fires on `delete`

**`emitBeforeSlot` adaptation for `before_delete`:**
The `before_delete` slot fires in the delete handler, which has the entity ID but not a decoded entity struct. `emitBeforeSlot` currently builds a `CreateRequest{Fields: map[string]string{...}}` from the entity fields. For `before_delete`, the handler should populate `Input.Entity` with the entity name and `Input.Fields` with just `{"id": entityID}`. This may require a small conditional path in `emitBeforeSlot` or a separate emit function for delete-context slots.

**`emitBeforeSlot` for `before_patch`:**
The `before_patch` slot fires in the patch handler, which has the decoded patch request (pointer fields). The fields map should contain only the fields that were provided in the patch (non-nil pointer fields). This may need special handling since patch handlers work with optional pointer fields.

### Test updates

- Add test cases for each new before-slot binding
- Verify generated handlers invoke before_upsert in upsert handlers
- Verify generated handlers invoke before_patch in patch handlers  
- Verify generated handlers invoke before_delete in delete handlers
- Verify nil-guard passthrough when no fill is bound

### What does NOT change

- After-operation slots — handled by task 059
- Example services — regeneration is task 060
- Proto files — already created in task 057

## Acceptance Criteria

1. `before_upsert` slot fires before upsert operations in generated handlers
2. `before_patch` slot fires before patch operations in generated handlers
3. `before_delete` slot fires before delete operations in generated handlers
4. Each slot can reject the request (return `ok: false`) with an error
5. Short-circuit halt semantics work for all three slots
6. Nil-guard ensures passthrough when no fill is bound
7. All tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- a2e6216 feat(rest-api): wire before_upsert, before_patch, before_delete slots into handler generation
