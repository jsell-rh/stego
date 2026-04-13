# Task 059: Wire After-Operation Lifecycle Slots into Rest-API Generator

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Lifecycle Slots / After slots** table (lines 317–327)

**Status:** `complete`

**Review:** [specs/reviews/task-059.md](../reviews/task-059.md)

**Depends on:** task-057

## Description

Wire the four after-operation lifecycle slots (`after_create`, `after_upsert`, `after_patch`, `after_delete`) into the rest-api generator. These slots fire after the database operation succeeds but before the HTTP response is sent. They receive the persisted entity and caller identity. They cannot reject the request (the mutation is committed).

### Generator changes (`internal/generator/restapi/generator.go`)

**`knownSlotRequestMeta`** — add entries for after-slots. After-slots receive the persisted entity + caller, so they need metadata flags that `emitAfterSlot` uses to populate the request.

**`slotAfterOps`** — add mappings:
- `after_create` → fires on `create`
- `after_upsert` → fires on `upsert`
- `after_patch` → fires on `patch`
- `after_delete` → fires on `delete`

**`emitAfterSlot` enhancement:**
The current `emitAfterSlot` builds a minimal request with `Entity` (string) and `Action` (string). The new after-slots need richer requests per the spec:
- `after_create`, `after_upsert`, `after_patch` — receive the persisted/updated entity (full field map) and the caller identity
- `after_delete` — receives the deleted entity ID and caller identity

This means `emitAfterSlot` needs to be enhanced (or a new variant created) to:
1. Populate entity fields from the persisted record (the entity variable is available in the handler after the store call)
2. Extract caller identity from the request context (same pattern as `emitBeforeSlot`)
3. Handle the delete case where only entity ID is available

The existing `on_entity_changed` after-slot (from the event-publisher mixin) should continue to work with its current simple {Entity, Action} pattern. The new lifecycle after-slots use a different request structure.

### Test updates

- Add test cases for each new after-slot binding
- Verify generated handlers invoke after_create after successful store.Create
- Verify generated handlers invoke after_upsert after successful store.Upsert
- Verify generated handlers invoke after_patch after successful store.Replace (patch)
- Verify generated handlers invoke after_delete after successful store.Delete
- Verify after-slot errors are logged/returned (they fire after mutation so behavior is: return 500 rather than silently swallowing)
- Verify nil-guard passthrough when no fill is bound

### What does NOT change

- Before-operation slots — handled by task 058
- Example services — regeneration is task 060
- Proto files — already created in task 057

## Acceptance Criteria

1. `after_create` slot fires after create operations in generated handlers
2. `after_upsert` slot fires after upsert operations in generated handlers
3. `after_patch` slot fires after patch operations in generated handlers
4. `after_delete` slot fires after delete operations in generated handlers
5. After-slots receive persisted entity data and caller identity
6. Existing `on_entity_changed` mixin slot continues to work unchanged
7. Nil-guard ensures passthrough when no fill is bound
8. All tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- ff37e87 feat(rest-api): wire after-operation lifecycle slots into handler generation
