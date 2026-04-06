# Task 002: Core Domain Types

**Spec Reference:** "Glossary", "Entity Type System"

**Status:** `complete`

## Description

Define Go structs for the five core nouns and supporting types. These are the in-memory representations parsed from YAML files.

Types to define:
- `Archetype` — components list, conventions, compatible mixins, default auth, bindings
- `Component` — config schema, requires/provides ports, slots, version
- `Mixin` — adds_components, adds_slots
- `ServiceDeclaration` — archetype ref, entities, expose blocks, slots, mixins, overrides
- `Fill` — name, implements, entity, qualified_by, qualified_at
- `Entity` — name, fields list
- `Field` — name, type, constraints (min_length, max_length, pattern, min, max, unique, unique_composite, optional, default, values, computed, filled_by)
- `ExposeBlock` — entity, operations, scope, parent, path_prefix, upsert_key, concurrency
- `SlotDeclaration` — slot name, entity, gate/chain/fan-out bindings, short_circuit
- `Port` — name (for requires/provides)
- `Convention` — layout, error_handling, logging, test_pattern

Primitive types enum: string, int32, int64, float, double, bool, bytes, timestamp, enum, ref, jsonb.
Operations enum: create, read, update, delete, list, upsert.

## Spec Excerpt

> Five nouns, seven operators.
> Primitive types (aligned with protobuf): `string`, `int32`, `int64`, `float`, `double`, `bool`, `bytes`, `timestamp`.
> Stego-specific types: `enum`, `ref`, `jsonb`.

## Acceptance Criteria

- All types defined in `internal/types/`
- Types compile and have appropriate Go tags for YAML unmarshaling
- Unit tests for any non-trivial validation

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits

- `752f6e8` feat: define core domain types for all five nouns and supporting types
- `2845b57` fix: wire Port into Component and make ConfigField.Items recursive
- `97ed22a` chore: mark task-002 ready-for-review
- `7708b1e` fix: handle polymorphic ConfigField.Items, rename SlotBinding to SlotDeclaration
- `ad59881` chore: mark task-002 ready-for-review
