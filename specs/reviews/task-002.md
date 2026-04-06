# Review: Task 002 — Core Domain Types

## Findings

- [ ] **`Port` struct is dead code.** `Port` is defined (`types.go:85-87`) but never referenced by `Component` or any other type. `Component.Requires` and `Component.Provides` are `[]string`, not `[]Port`. The task description specifies "Port — name (for requires/provides)", implying it should be used by `Component`. Either wire `[]Port` into `Component.Requires`/`Component.Provides`, or remove `Port` entirely. Dead types pollute the API surface and will confuse consumers.

- [ ] **`ConfigField` cannot represent nested config schemas.** The spec shows recursive config structures (e.g., `expose.items` containing typed sub-fields `entity`, `operations`, `scope`, each with their own `type`). `ConfigField.Items` is typed as `any` (`types.go:121`), so nested schemas unmarshal into untyped `map[string]interface{}` rather than `ConfigField` values. This means config schemas cannot be validated or traversed without runtime type assertions, undermining the purpose of having typed Go structs. `Items` should be `map[string]ConfigField` (or similar recursive type) to model the spec's nested config schemas.
