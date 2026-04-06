# Task 004: Registry Structure & Loading

**Spec Reference:** "Registry"

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-004.md](../reviews/task-004.md)

## Description

Implement the registry as a local directory structure (git-based, but for MVP we load from local filesystem). The registry contains archetypes, components, and mixins.

- Define registry directory layout:
  - `registry/archetypes/<name>/archetype.yaml`
  - `registry/components/<name>/component.yaml`
  - `registry/components/<name>/slots/*.proto`
- Implement `Registry` type that loads and indexes all archetypes, components, and mixins from a directory
- Support `.stego/config.yaml` for registry URL and pinned SHA (read-only for MVP, no git clone)
- Support `pins:` for per-component SHA overrides (parsed but not enforced for MVP)
- Resolution order: pinned SHA > registry ref

## Spec Excerpt

> The registry is a git repo. No database, no server. Versions are git tags for discovery, but all resolution pins to SHAs for auditability.
> Resolution order: pinned SHA > registry ref.

## Acceptance Criteria

- `Registry.Load(dir string)` returns indexed archetypes, components, mixins
- Lookup by name works for each kind
- `.stego/config.yaml` parsed
- Tests with fixture registry directory

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits

- `28c6579` feat(registry): implement registry loading and config parsing
- `47ed95d` fix(registry): address review findings for duplicate names, identity consistency, proto validation, and config validation
