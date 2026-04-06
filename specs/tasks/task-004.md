# Task 004: Registry Structure & Loading

**Spec Reference:** "Registry"

**Status:** `complete`

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

## Outstanding Revisions (Round 2)

The following code changes are still needed (review findings 5–7):

1. **Mixin `adds_slots` proto validation** — `loadMixins` must verify proto files exist for each `adds_slots` entry, matching the validation already done in `loadComponents`. Add proto files to the mixin test fixture.
2. **Fail on non-existent registry directory** — `Load()` must return an error if the top-level directory does not exist, rather than silently returning an empty registry.
3. **Remove unreachable duplicate-name code and fix test names** — The duplicate-name checks in `loadArchetypes`, `loadComponents`, and `loadMixins` are unreachable (directory names are unique on-disk and the identity check already enforces YAML name == dir name). Remove the dead code. Rename `TestLoadDuplicate*Name` tests to `TestLoad*NameMismatch` to reflect what they actually verify.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- `28c6579` feat(registry): implement registry loading and config parsing
- `47ed95d` fix(registry): address review findings for duplicate names, identity consistency, proto validation, and config validation
- `ff020ed` fix(registry): address round-2 review findings for mixin proto validation, non-existent dir, and dead code
