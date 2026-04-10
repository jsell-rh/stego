# Task 036: Port Resolver Loads Override Components from Registry

## Spec Reference

**specs/spec.md — Port Resolution, item 3:**

> When an override references a component not in the archetype's component list, the resolver must load that component from the registry and include it in the active component set. This enables swapping components (e.g. `jwt-auth` -> `rh-sso-auth`) without modifying the archetype.

## Related Spec Sections

- Port Resolution (full section) — archetype defaults, service overrides, SHA pinning
- MVP Scope — `rest-crud` archetype, `jwt-auth` component, port override example
- Compiler Process — plan/apply reconciler pattern

## Current State

The override-component-loading behavior is currently split across two locations:

1. **`collectComponentNames()`** in `internal/compiler/reconciler.go` (lines 419-511): Detects service port overrides, replaces the archetype's default component with the override component, and adds the override component to the active component list. This works correctly for both `Reconcile()` and `Validate()`.

2. **`ports.Resolve()`** in `internal/ports/resolve.go`: A pure function that takes pre-loaded components. When a binding references a component not in the `Components` map, it returns an `InvalidBinding` error ("binding references non-existent component"). It does NOT load from the registry.

The spec says "the resolver must load that component from the registry." Currently the resolver does not — the loading happens upstream. This means:

- `ports.Resolve()` cannot be used standalone for port resolution with overrides
- The override-loading logic is coupled to the reconciler rather than the port resolution contract
- Tests for `ports.Resolve()` manually pre-load override components (e.g. `TestResolveServiceOverrideTakesPrecedence` adds `api-key-auth` to the map)

## Acceptance Criteria

1. `ports.Resolve()` accepts a registry (or component-loader function) via `ResolveInput`
2. When a service override binding references a component not in `ResolveInput.Components`, the resolver loads it from the registry and adds it to the active component set
3. The loaded component is validated (exists in registry, provides the required port)
4. The replaced default component (if any) is excluded from the active set to prevent namespace conflicts (both `jwt-auth` and `rh-sso-auth` declare `output_namespace: internal/auth`)
5. `ports.Resolve()` returns the final resolved component set (so callers know which components are active after resolution)
6. Unit tests in `internal/ports/resolve_test.go` cover:
   - Override loads component not in initial set
   - Override replaces archetype default component
   - Override references non-existent component in registry (error)
   - Override component doesn't provide the required port (error)
7. `collectComponentNames()` in the reconciler is simplified to delegate override-component logic to the resolver
8. All existing tests continue to pass
9. `stego plan` and `stego validate` work on the `examples/user-management-rhsso` example (auth-provider overridden to rh-sso-auth)

## Progress

`ready-for-review`

## Review

[specs/reviews/task-036.md](../reviews/task-036.md)

## Commits

- 413a753 feat(task-036): port resolver loads override components from registry
- c82e92a fix(task-036): handle empty Component in InvalidBinding error formatting
