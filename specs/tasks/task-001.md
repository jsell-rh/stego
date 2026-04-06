# Task 001: Go Module & Project Structure

**Spec Reference:** "Stego is Written in Go", "Generated Code Structure"

**Status:** `not-started`

## Description

Initialize the Go module and establish the project directory structure for the stego compiler.

- `go mod init` with appropriate module path
- Create directory skeleton:
  - `cmd/stego/` — CLI entrypoint
  - `internal/types/` — core domain types
  - `internal/compiler/` — compiler/reconciler
  - `internal/registry/` — registry loading/resolution
  - `internal/generator/` — Generator interface and gen.Context
  - `registry/archetypes/` — built-in archetypes
  - `registry/components/` — built-in components
- Minimal `cmd/stego/main.go` that prints version

## Spec Excerpt

> Stego itself is Go. Components are Go packages implementing `Generator`. The compiler imports and calls them directly -- the registry is a Go module. Single binary distribution.

## Acceptance Criteria

- `go build ./cmd/stego` compiles successfully
- Directory structure exists per above

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits
