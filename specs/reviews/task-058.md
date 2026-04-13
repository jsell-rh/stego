# Review: Task 058 — Wire Before-Operation Lifecycle Slots into Rest-API Generator

**Reviewer:** Verifier
**Commit under review:** a2e6216

## Findings

- [-] [process-revision-complete] **`existing_entity` in `before_patch` contains post-merge state, not pre-patch state.** The `emitBeforePatchSlot` function builds `existingEntityMap` by reading from the entity variable (`lower`, e.g. `profile`) at Step 4 of the generated patch handler. But Step 3 has already applied non-nil patch fields to this same variable (`profile.DisplayName = *patch.DisplayName`). The proto field is named `existing_entity` and the spec (line 312) describes the input as "Patch fields + existing entity + caller." Both the proto name and spec description unambiguously indicate the pre-patch state. As implemented, a fill receiving `existing_entity` cannot determine the original field values before the patch was applied — it sees the merged result. This defeats the stated use case of "Enforce field-level permissions" because the fill cannot compare old vs. new values. **Fix:** Move the `before_patch` slot invocation to before Step 3 (before applying patch fields to the entity variable), or capture the existing entity state into a separate variable before mutation and use that for `existingEntityMap`. File: `internal/generator/restapi/generator.go`, lines 1392–1411.
