# Task 007: Slot/Fill Proto Contract & Interface Generation

**Spec Reference:** "Slot/Fill Contract", "Fill Wiring"

**Status:** `complete`

**Review:** [specs/reviews/task-007.md](../reviews/task-007.md)

## Description

Implement slot proto definitions and Go interface generation from them.

- Define shared `stego.common` proto package with `Identity`, `SlotResult`, `CreateRequest` etc.
- Define slot protos for `rest-api` component: `before_create.proto`, `validate.proto`
- Generate Go interfaces from slot protos (can be done programmatically for MVP — no need for full protoc pipeline)
- `SlotResult` includes `halt` and `status_code` fields for short-circuit chains
- Each component owns its own slot protos (no sharing between components)

## Spec Excerpt

> Slots are defined as protobuf service definitions. Each component owns its own slot protos.
> Common stable types (Identity, SlotResult) live in a shared `stego.common` proto package.

## Acceptance Criteria

- Proto files exist for common types and rest-api slots
- Go interfaces generated from slot definitions
- SlotResult supports halt semantics
- Tests verify interface generation produces compilable Go

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits

- d2d2a4e feat(slot): implement slot/fill proto contract and interface generation
- 5882734 fix(slot): route GenerateInterface through gen.File pipeline for header enforcement
