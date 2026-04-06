# Task 014: Validation & Drift Detection

**Spec Reference:** "CLI Interface" (validate, drift)

**Status:** `not-started`

## Description

Implement validation and drift detection commands.

**stego validate:**
- Check service.yaml against registry (archetype exists, components exist, slots exist)
- Validate entity type system constraints are valid
- Validate port resolution succeeds
- Validate all referenced fills exist in `fills/` directory
- Validate expose blocks reference defined entities
- Validate slot bindings match available slots

**stego drift:**
- Compare generated files in `out/` against what the compiler would produce
- Detect hand-edits to generated files
- Report which files have been modified

## Spec Excerpt

> `stego validate` — Check service.yaml against registry
> `stego drift` — Detect hand-edits to generated files

## Acceptance Criteria

- validate catches: missing archetype, missing component, invalid field type, unresolved port, missing fill, bad entity reference
- drift detects modified generated files
- Both produce clear, actionable error messages
- Unit tests for each validation rule

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits
