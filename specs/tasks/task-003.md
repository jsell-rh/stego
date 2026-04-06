# Task 003: YAML Schema Parsing

**Spec Reference:** "File Types" (all subsections)

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-003.md](../reviews/task-003.md)

## Description

Implement YAML parsing for all file types into the core domain types from Task 002.

Parsers needed:
- `archetype.yaml` → `Archetype`
- `component.yaml` → `Component`
- `mixin.yaml` → `Mixin`
- `service.yaml` → `ServiceDeclaration`
- `fill.yaml` → `Fill`

Each parser should:
- Validate `kind:` field matches expected type
- Return structured errors with file path and line context
- Handle all fields shown in spec examples

Create a unified `Parse(path string) (interface{}, error)` that dispatches on `kind`.

## Spec Excerpt

> The spec "File Types" section shows the complete YAML schema for each kind.

## Acceptance Criteria

- All five file types parse correctly from YAML
- Round-trip test: marshal → unmarshal → compare
- Error messages include file path
- Test fixtures for each file type based on spec examples

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits

- `8ddfd78` feat: implement YAML schema parsing for all five file types
- `5be6aea` chore: mark task-003 ready-for-review
- `8806a55` fix: address task-003 review findings for YAML parsing
- `7cb3b51` fix: use errors.As for yaml.TypeError unwrapping and add integration test
- `d18962a` chore: mark task-003 ready-for-review
