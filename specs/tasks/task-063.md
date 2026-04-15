# Task 063: Fix Missing `fmt` Import for After-Slot Emit Paths

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — Slot/Fill Contract

**Status:** `needs-revision`

**Review:** `specs/reviews/task-063.md`

**Depends on:** task-057

## Description

The `needFmt` flag in the rest-api generator (`internal/generator/restapi/generator.go`, lines 497–524) only checks **before-slot** operations when deciding whether to include `"fmt"` in the generated handler file's import block. However, `emitAfterCreateOrUpsertSlot` (line 3903) and `emitAfterPatchSlot` (line 3958) both emit `fmt.Sprintf` in the generated code for non-string entity fields via `fieldToStringExpr()`.

This means any entity with at least one non-string field (e.g. `int32`, `bool`, `timestamp`) and an `after_create`, `after_upsert`, or `after_patch` slot binding generates handler code that references `fmt.Sprintf` without importing `"fmt"` — a hard compile error in the generated output.

`after_delete` is NOT affected because `emitAfterDeleteSlot` only passes string values (`entity` name, `deleted_entity_id`) and does not call `fieldToStringExpr`.

### Root cause

The `needFmt` computation (lines 500–523) loops over operations and calls `slotsForOp()`, but only inspects the `before` return value:

```go
hasBeforeSlots := false
for _, op := range eb.Operations {
    before, _ := slotsForOp(op, slotParams)
    if len(before) > 0 {
        hasBeforeSlots = true
        break
    }
}
if hasBeforeSlots {
    needFmt = needsFmtForSlotFields(entity)
}
```

The `after` return value from `slotsForOp()` is discarded (`_`). After-slots that emit `fmt.Sprintf` via `fieldToStringExpr()` are never considered.

### Fix

Extend the `needFmt` check to also inspect after-slots. The `after` return from `slotsForOp()` must be checked: if any after-slot exists for `after_create`, `after_upsert`, or `after_patch` (the three emit functions that call `fieldToStringExpr`), and the entity has non-string fields (per `needsFmtForSlotFields`), set `needFmt = true`.

A clean approach:

```go
hasBeforeSlots := false
hasAfterFieldSlots := false // after-slots that serialize entity fields
for _, op := range eb.Operations {
    before, after := slotsForOp(op, slotParams)
    if len(before) > 0 {
        hasBeforeSlots = true
    }
    if len(after) > 0 {
        // after_create, after_upsert, after_patch all serialize entity fields.
        // after_delete does not (it only passes the entity ID string).
        for _, a := range after {
            if a.SlotName != "after_delete" {
                hasAfterFieldSlots = true
            }
        }
    }
}
if hasBeforeSlots || hasAfterFieldSlots {
    needFmt = needsFmtForSlotFields(entity)
}
```

This is a checklist item 119 violation (new operations must inherit conditional-import obligations from every type they use).

## Acceptance Criteria

1. The `needFmt` flag accounts for after-slot operations (`after_create`, `after_upsert`, `after_patch`) that emit `fmt.Sprintf` via `fieldToStringExpr`.
2. Generated handler files for entities with non-string fields and after-slot bindings include `import "fmt"`.
3. Generated handler files for entities with ONLY string/bytes/jsonb fields and after-slot bindings do NOT spuriously include `import "fmt"` (no unnecessary imports).
4. `after_delete` slots (which do not use `fieldToStringExpr`) do NOT trigger the `fmt` import on their own.
5. A test in `internal/generator/restapi/generator_test.go` exercises a collection with an `after_upsert` or `after_create` slot binding on an entity with a non-string field (e.g. `int32`), and verifies the generated handler imports `"fmt"`.
6. A degenerate-configuration test: collection with ONLY an `after_create` slot (no before-slots, no other operations that independently need `fmt`) on an entity with an `int32` field — verifies the generated output compiles (per checklist item 31, use actual compilation or `go build`, not `parser.ParseFile`).
7. All existing tests pass: `go test ./...`
8. Example output regenerated if any example service has after-slot bindings on entities with non-string fields.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
- e42b960 fix(rest-api): include fmt import for after-slot emit paths (task-063)
