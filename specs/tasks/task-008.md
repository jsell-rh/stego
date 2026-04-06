# Task 008: rest-crud Archetype Definition

**Spec Reference:** "File Types — Archetype", "MVP Scope"

**Status:** `ready-for-review`

## Description

Create the `rest-crud` archetype YAML definition and ensure all referenced components exist in the registry.

- `registry/archetypes/rest-crud/archetype.yaml`
- Components: rest-api, postgres-adapter, otel-tracing, health-check
- Default auth: jwt-auth
- Conventions: layout=flat, error_handling=problem-details-rfc, logging=structured-json, test_pattern=table-driven
- Compatible mixins: event-publisher, async-worker
- Default port bindings: storage-adapter→postgres-adapter, auth-provider→jwt-auth

**Stub components for non-MVP generators:**
- `registry/components/otel-tracing/component.yaml` — provides: `tracing`. No-op `Generator` that returns an empty file list. Stub ensures the archetype loads and port resolution succeeds.
- `registry/components/health-check/component.yaml` — provides: `health-endpoint`. No-op `Generator` that returns an empty file list.

These are intentionally minimal — full generators are post-MVP. The stubs satisfy the archetype's component list so that the registry, port resolution, and compiler don't error on missing components.

## Spec Excerpt

> The archetype YAML example in the spec.
> MVP defers: multiple archetypes, mixins, multiple registries, per-component SHA pinning, multi-language output.

## Acceptance Criteria

- `archetype.yaml` parses correctly using parser from Task 003
- All fields match spec
- Validation test confirms it loads via Registry from Task 004
- `otel-tracing` and `health-check` component.yaml files exist and parse correctly
- No-op generators for otel-tracing and health-check implement `Generator` interface (from Task 006) and return empty file lists
- Registry loads all four components referenced by the archetype

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits

- 0b05c7f feat(task-008): add rest-crud archetype definition and stub component generators
