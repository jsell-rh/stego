# Task 013: Compiler Core — Plan/Apply Reconciler

**Spec Reference:** "Compiler Process (Reconciler Pattern)"

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-013.md](../reviews/task-013.md)

## Description

Implement the core compiler that follows the Terraform-style plan/apply pattern.

- `stego plan`: diff desired state (service.yaml) vs current state (.stego/state.yaml), show changeset
  - List files to generate, update, or leave unchanged
  - Show entity field changes
- `stego apply`: execute the plan — run all component generators, assemble shared files, write output
- State tracking in `.stego/state.yaml`:
  - `last_applied` with registry SHA and per-component version/SHA
- Output to `out/` directory (layout determined by archetype conventions)
- Assemble shared files from all component Wiring structs
- Validate service.yaml against registry before planning

## Spec Excerpt

> Follows Terraform-style plan/apply, not one-shot generation.
> State tracked in `.stego/state.yaml`, pinned to exact registry SHAs for full auditability.
> Output is plain Go (or target language). No runtime dependency on STEGO.

## Acceptance Criteria

- `plan` shows accurate changeset
- `apply` generates all files correctly
- State file written after successful apply
- Subsequent plan with no changes shows "no changes"
- `go build` works on generated output

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits

- d22658b feat(task-013): implement compiler core — plan/apply reconciler
- ea324bb fix(task-013): address all 7 review findings
- 1670c51 fix(task-013): address round 2 review findings 8-10
- 1d888aa fix(task-013): address round 3 review findings 11-13
- 7b3d195 fix(task-013): address round 4 review findings 14-16
- 167a2e0 fix(task-013): address round 5 review findings 17-18
