# Task 031: Scoped Collection Access Enforcement in Generators

**Spec Reference:** "Entity/Collection Separation" (spec.md:24-31), "Collections & Operations" (rest-crud spec, lines 80-114)

**Status:** `needs-revision`

**Review:** [specs/reviews/task-031.md](../reviews/task-031.md)

**Depends on:** None (all prerequisites complete)

**Blocks:** task-030 (example service regeneration)

## Description

The spec defines a Collection as "a scoped, operation-constrained access pattern over an entity" (spec.md:17). The rest-crud spec states: "Scoped collections generate nested routing. The `scope` field maps entity fields to parent entities. The compiler derives the URL path and generates parent existence verification at each level."

Currently, the `rest-api` generator produces handlers where **only the List operation** enforces scope (filtering by `scopeField`/`scopeValue`). Individual resource operations — Read, Update, Patch, Delete — accept any entity ID regardless of whether it belongs to the URL's scope. This allows cross-scope data access:

- `GET /organizations/{orgB}/org-users/{user_in_orgA}` returns a user from a different org
- `PUT /organizations/{orgB}/org-users/{user_in_orgA}` silently reassigns the user to orgB
- `PATCH /organizations/{orgB}/org-users/{user_in_orgA}` patches a user from any org
- `DELETE /organizations/{orgB}/org-users/{user_in_orgA}` deletes a user from any org

### What changes

**rest-api generator (`internal/generator/restapi/generator.go`):**

For scoped collections, the generated handlers for Read, Update, Patch, and Delete must verify that the fetched entity belongs to the URL's scope before proceeding:

1. **Read handler** (`generateReadMethod`): After `store.Get()`, verify the entity's scope field matches the URL path parameter. If not, return 404.
2. **Update handler** (`generateUpdateMethod`): After `store.Get()` (add a pre-check Get), verify scope before calling `store.Replace()`. Do not silently overwrite the scope field from the URL.
3. **Patch handler** (`generatePatchMethod`): After `store.Get()`, verify the entity's scope field matches the URL path parameter before applying the patch.
4. **Delete handler** (`generateDeleteMethod`): Before `store.Delete()`, call `store.Get()` to verify the entity belongs to the URL's scope. If not, return 404.

The approach is to add a scope-check in the handler after retrieving the entity — this does NOT require changing the Storage interface. The handler already has access to the scope value from `r.PathValue()` and the entity's scope field from `store.Get()`.

**Generator tests (`internal/generator/restapi/generator_test.go`):**

Add tests verifying that generated handlers for scoped collections include scope verification code.

## Spec Excerpt

> **Collection** | A scoped, operation-constrained access pattern over an entity. Multiple collections can reference the same entity. Each collection generates its own handler, routes, and wiring. | Product team / LLM

> **Scoped collections** generate nested routing. The `scope` field maps entity fields to parent entities. The compiler derives the URL path and generates parent existence verification at each level.

## Acceptance Criteria

1. Generated Read handler for scoped collections verifies entity scope field matches URL scope parameter; returns 404 if mismatch.
2. Generated Update handler for scoped collections verifies scope before mutation; does not silently overwrite scope field from URL.
3. Generated Patch handler for scoped collections verifies scope before mutation.
4. Generated Delete handler for scoped collections verifies scope before deletion.
5. Unscoped collections are unaffected (no scope check generated).
6. Generator tests cover scope verification code generation.
7. `go test ./...` passes from the repo root.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
- 92ef2c0 feat(task-031): enforce scoped collection access in rest-api generator
- dd1f90a fix(task-031): preserve storage metadata in scoped Read response
