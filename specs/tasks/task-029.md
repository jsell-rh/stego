# Task 029: Short-Circuit Chain Semantics

**Spec Reference:** "Slot/Fill Contract > Short-circuit chains" (rest-crud spec)

**Status:** `not-started`

## Description

The rest-crud archetype spec defines short-circuit chain semantics where a chain step can halt the pipeline and return a result early. This task adds `short_circuit: true` support to slot declarations and `halt`/`status_code` fields to `SlotResult`, and updates the chain operator to stop processing when a step halts.

### What changes

**Proto (`registry/common/types.proto`):**
- `halt` (bool) and `status_code` (int32) fields already exist on `SlotResult` — verify they are present and correctly documented.

**Core types (`internal/types/types.go`):**
- Add `ShortCircuit bool` field to `SlotDeclaration`.

**Parser:**
- Parse `short_circuit: true` from slot declarations in service YAML.

**Validation:**
- `short_circuit` is only valid on slots using the `chain` operator.
- Reject `short_circuit` on `gate` or `fan-out` operators.

**Slot interface generation (`internal/slot/generate.go`):**
- Generated `SlotResult` struct includes `Halt bool` and `StatusCode int32` fields.

**Chain operator (`internal/slot/operators.go`):**
- When `short_circuit: true`, the chain operator checks `result.Halt` after each step.
- If `Halt` is true, stop processing remaining steps and return the result immediately.
- `StatusCode` on the halted result determines the HTTP response status (e.g. 204 No Content, 400 Bad Request).

**rest-api generator (`internal/generator/restapi/`):**
- When a slot has `short_circuit: true`, the generated handler checks the `Halt` and `StatusCode` fields on the chain result.
- If halted, respond with the `StatusCode` and appropriate body (error or empty).

## Spec Excerpt

> **Short-circuit chains** allow a step to halt the pipeline and return a result early. The slot proto includes a `halt` field:
> ```yaml
> slots:
>   - collection: cluster-statuses
>     slot: process_adapter_status
>     chain:
>       - validate-mandatory-conditions    # can halt with 400
>       - discard-stale-generation         # can halt with 204 (no-op)
>       - persist-status
>       - aggregate-resource-status
>     short_circuit: true                  # enables halt semantics
> ```
> ```protobuf
> message SlotResult {
>   bool ok = 1;
>   string error_message = 2;
>   bool halt = 3;           // stop the chain, return this result
>   int32 status_code = 4;   // HTTP status for the halted response
> }
> ```

## Acceptance Criteria

1. `SlotResult` proto includes `halt` and `status_code` fields.
2. `ShortCircuit` field added to `SlotDeclaration` type and parsed from YAML.
3. Validation enforces: `short_circuit` only valid with `chain` operator.
4. Generated `SlotResult` Go struct includes `Halt bool` and `StatusCode int32`.
5. Chain operator stops processing on `Halt == true`.
6. Generated handler responds with `StatusCode` when chain halts.
7. Existing non-short-circuit chains are unaffected.
8. Tests cover halt semantics, non-halt passthrough, and validation.
9. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
