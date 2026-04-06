# Task 008: rest-crud Archetype Definition

**Spec Reference:** "File Types — Archetype", "MVP Scope"

**Status:** `not-started`

## Description

Create the `rest-crud` archetype YAML definition in the registry.

- `registry/archetypes/rest-crud/archetype.yaml`
- Components: rest-api, postgres-adapter, otel-tracing, health-check
- Default auth: jwt-auth
- Conventions: layout=flat, error_handling=problem-details-rfc, logging=structured-json, test_pattern=table-driven
- Compatible mixins: event-publisher, async-worker
- Default port bindings: storage-adapter→postgres-adapter, auth-provider→jwt-auth

## Spec Excerpt

> The archetype YAML example in the spec.

## Acceptance Criteria

- `archetype.yaml` parses correctly using parser from Task 003
- All fields match spec
- Validation test confirms it loads via Registry from Task 004

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits
