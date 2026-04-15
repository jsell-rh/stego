# Review: Task 063 — Fix Missing `fmt` Import for After-Slot Emit Paths

## Verdict: needs-revision — 1 finding

## Acceptance Criteria Check

- [x] **AC#1**: `needFmt` flag accounts for `after_create`, `after_upsert`, `after_patch` — the `hasFieldSerializingSlots` loop at `generator.go:511-517` correctly checks after-slots from `slotsForOp()` and excludes `after_delete` and `on_entity_changed`.
- [x] **AC#2**: Generated handlers with non-string fields and after-slot bindings include `import "fmt"` — verified via `TestGenerate_AfterSlotNeedsFmtForNonStringFields`.
- [x] **AC#3**: String-only entities with after-slots do NOT include spurious `import "fmt"` — verified via `TestGenerate_AfterSlotStringOnlyNoFmt`.
- [x] **AC#4**: `after_delete` does NOT trigger `fmt` import on its own — verified via `TestGenerate_AfterDeleteSlotAloneNoFmt`.
- [x] **AC#5**: Test exercises `after_create`/`after_upsert` with non-string field (`int32`) and verifies `fmt` import — `TestGenerate_AfterSlotNeedsFmtForNonStringFields`.
- [x] **AC#6**: Degenerate-configuration compilation test — `TestGenerate_AfterCreateOnlyCompiles` writes generated output to temp dir and runs `go build ./...`.
- [x] **AC#7**: All existing tests pass — `go test ./...` passes (15 packages).
- [x] **AC#8**: Example output regeneration — not needed; the `user-management` example has `after_create` on `org-users` (entity `User`), but all User fields map to `string`/`json.RawMessage` — no `fmt` required.

## Code Review

The `needFmt` logic change is correct. `emitAfterCreateOrUpsertSlot` (line 3925) and `emitAfterPatchSlot` (line 3970) call `fieldToStringExpr` which emits `fmt.Sprintf` for non-string fields. `emitAfterDeleteSlot` and `emitAfterSlot` (`on_entity_changed`) do not. The exclusion filter at line 515 (`a.SlotName != "after_delete" && a.SlotName != "on_entity_changed"`) matches the dispatch logic at `dispatchAfterSlot` (line 3856-3867).

The `needAuth` move (pulling it outside the `hasFieldSerializingSlots` guard) is a correct ancillary fix — after-only slots with `HasCaller` would have missed the auth import under the old code. Not in ACs but logically required by the same root cause.

## Findings

- [process-revision-complete] **Stale Known Generator Bug entry not removed.** `specs/registry/archetypes/rest-crud/spec.md:731` still documents: "**Missing `fmt` import**: Handlers with `after_upsert` (or other after-slots) emit `fmt.Sprintf` for persisted field conversion but do not add `"fmt"` to the import block. Workaround: manually add the import after `stego apply`." This bug is exactly what task-063 fixes. The entry should be removed now that the fix is in place — leaving it implies the bug still exists. Compare task-064 AC#9 which explicitly requires removing the UUID v4 known-bug entry after the fix.
