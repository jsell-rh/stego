# Task 012: Fill Wiring & main.go Assembly

**Spec Reference:** "Fill Wiring", "Generated Code Structure"

**Status:** `needs-revision`

**Review:** [specs/reviews/task-012.md](../reviews/task-012.md)

## Description

Implement the compiler's ability to wire fills into generated code via constructor injection.

- Generate Go interfaces from slot proto definitions
- Fills implement generated interfaces
- Generated `main.go` wires concrete fills into constructors
- Wiring struct from each component assembled into shared files:
  - `cmd/main.go` — imports, constructor calls, route registration
  - `go.mod` — module definition
- Gate operator: all fills must return Ok for operation to proceed
- Chain operator: sequential execution with short-circuit halt support
- Fan-out operator: concurrent execution of all fills
- `fills/` directory is human-owned, never overwritten

## Spec Excerpt

> Fills are wired via constructor injection using generated interfaces. No DI framework, no reflection, no runtime lookup.
> Generated `main.go` wires concrete fills into constructors.

## Acceptance Criteria

- Generated main.go shows all fills and connections
- Gate, chain, fan-out operators work correctly
- Short-circuit halt stops chain and returns status
- fills/ directory is never touched by generator
- Tests verify generated wiring compiles

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits

- f0a27c2 feat(task-012): implement fill wiring & main.go assembly
- a6494f8 fix(task-012): address all 6 review findings for fill wiring & assembly
- ced5a8f fix(task-012): address round 2 review findings for assembler
- 5df5ad3 fix(task-012): address round 3 review findings for assembler
- 8a8cde0 fix(task-012): seed assembler-internal template variables into constructor disambiguation
- 75a82dc fix(task-012): address round 5 review findings for assembler
- f78c785 fix(task-012): address round 6 review findings for assembler
