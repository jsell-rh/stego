# Task 013: Compiler Core — Plan/Apply Reconciler

**Spec Reference:** "Compiler Process (Reconciler Pattern)"

**Status:** `not-started`

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
