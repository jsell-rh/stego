# Task 015: CLI — All 11 Commands

**Spec Reference:** "CLI Interface"

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-015.md](../reviews/task-015.md)

## Description

Wire all CLI commands into the `cmd/stego/main.go` entrypoint.

**Project lifecycle:**
- `stego init --archetype <name>` — create project from archetype (generates service.yaml scaffold, .stego/config.yaml)
- `stego fill create <name> --slot <s>` — scaffold a new fill with generated interface

**Reconciliation:**
- `stego plan` — diff desired vs current, show changeset (from Task 013)
- `stego apply` — generate/update code (from Task 013)
- `stego drift` — detect hand-edits (from Task 014)

**Validation:**
- `stego validate` — check service.yaml against registry (from Task 014)
- `stego test` — run all fill tests (delegates to `go test ./fills/...`)

**Registry:**
- `stego registry search` — query components by provides/requires/slots
- `stego registry inspect <component>` — show component details
- `stego registry fills --slot <s>` — find existing fills for a slot

Use standard library `flag` package or minimal CLI framework. No LLM integration.

## Spec Excerpt

> The full CLI Interface section from the spec.

## Acceptance Criteria

- All 11 commands are wired and respond to `--help`
- `stego init` generates a valid project scaffold
- `stego fill create` generates fill directory with interface stub
- Registry commands query and display results
- `stego test` delegates to Go test runner
- Single binary: `go build ./cmd/stego` produces working CLI

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits
