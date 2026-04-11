# Task 046: Add CORS Convention Field to Domain Types

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Archetype Definition** (`cors: enabled` in conventions) and **CORS** section

**Status:** `ready-for-review`

**Review:** `specs/reviews/task-046.md`

**Depends on:** None (all prerequisites complete)

**Blocks:** task-047

## Description

The rest-crud archetype definition now includes `cors: enabled` in its conventions block. The `Convention` struct in `internal/types/types.go` must gain a `CORS` field so the framework can propagate this convention value through the generator pipeline.

### What changes

**`internal/types/types.go`:**
- Add `CORS string` field to the `Convention` struct (with appropriate YAML/JSON tag `cors`).

**Parsing/loading:**
- Verify that the existing YAML unmarshaling of archetype definitions picks up the new `cors` field automatically (it should, given struct tags). If any explicit field mapping exists, update it.

**Tests:**
- Add or update a test that parses a rest-crud archetype YAML with `cors: enabled` and asserts `Convention.CORS == "enabled"`.

### What does NOT change

- Generator behavior — no CORS middleware generation yet (that's task-047).
- Assembler — no wiring changes yet.
- Example output — unchanged.

## Spec Excerpt

> ```yaml
> conventions:
>   layout: flat
>   error_handling: problem-details-rfc
>   response_format: envelope
>   request_validation: openapi-schema
>   logging: structured-json
>   test_pattern: table-driven
>   cors: enabled
> ```

## Acceptance Criteria

1. `Convention` struct has a `CORS string` field with `yaml:"cors"` tag.
2. Parsing a rest-crud archetype YAML with `cors: enabled` populates `Convention.CORS` as `"enabled"`.
3. All tests pass: `go test ./...`
4. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- c4fdb76 feat(task-046): add CORS convention field to domain types
- 4109606 fix(task-046): add cors field to parser testdata and assertion
