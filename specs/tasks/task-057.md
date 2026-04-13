# Task 057: Lifecycle Slot Proto Definitions and Component Registry Update

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **Lifecycle Slots** section (lines 304–351) and **rest-api component** definition (lines 355–400)

**Status:** `ready-for-review`

**Depends on:** none (all prior tasks complete)

## Description

The rest-crud spec defines 9 lifecycle slots for the `rest-api` component. Currently only `before_create` and `validate` have proto files and component.yaml entries. This task creates the missing 7 proto files and updates the component registry to declare all slots.

### Proto files to create

Create these proto files in `registry/components/rest-api/slots/`:

1. **`before_upsert.proto`** — mirrors `before_create.proto` pattern (Input + Caller)
2. **`before_patch.proto`** — Input (patch fields) + Caller
3. **`before_delete.proto`** — entity ID string + Caller (no Input/Fields map — delete only needs the ID)
4. **`after_create.proto`** — Entity name + persisted entity fields + Caller
5. **`after_upsert.proto`** — Entity name + persisted entity fields + Caller
6. **`after_patch.proto`** — Entity name + updated entity fields + Caller
7. **`after_delete.proto`** — Entity name + deleted entity ID + Caller

All protos use `package stego.components.rest_api.slots` and import `stego/common/types.proto`. Each defines a `service` with an `Evaluate` RPC returning `stego.common.SlotResult`.

### Component.yaml updates

**`registry/components/rest-api/component.yaml`:**
- Bump version to `3.0.0`
- Add all 9 lifecycle slots to `slots:` list (before_create, before_upsert, before_patch, before_delete, after_create, after_upsert, after_patch, after_delete, validate) — each with `default: passthrough`
- Add `patch` to the operations enum in the config schema

**`internal/registry/testdata/registry/components/rest-api/component.yaml`:**
- Mirror the production component.yaml changes

**Copy proto files to testdata:**
- Copy all new protos into `internal/registry/testdata/registry/components/rest-api/slots/`

### What does NOT change

- Generator code — wiring lifecycle slots into the generator is task 058/059
- Example services — regeneration is task 060
- Other components or archetypes

## Acceptance Criteria

1. 7 new proto files exist in `registry/components/rest-api/slots/`
2. `rest-api` component.yaml lists all 9 lifecycle slots with `default: passthrough`
3. `rest-api` component.yaml version is `3.0.0`
4. Testdata mirrors production registry
5. All tests pass: `go test ./...`

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

