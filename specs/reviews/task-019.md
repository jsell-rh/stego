# Review: Task 019 — Collection-Aware Code Generation Pipeline

## Round 1

- [-] [process-revision-complete] **`validateExposeOperations` reports entity name instead of collection name** (`internal/generator/restapi/generator.go:1491`): When a collection has no operations, the error collects `eb.Entity` instead of `eb.Name`. With multiple collections referencing the same entity (the normal case per spec), this error is ambiguous — the user cannot determine which collection needs fixing. Should use `eb.Name`.

- [-] [process-revision-complete] **`validateFieldReferences` reports entity name instead of collection name** (`internal/generator/restapi/generator.go:1292-1294`): Error messages use `eb.Entity` as the collection identifier (`"collection for %q references scope field…"`). When two collections reference the same entity with different scopes, the user cannot tell which collection triggered the error. Should use `eb.Name` as the first format argument.

- [-] [process-revision-complete] **`validateOperationUniqueness` reports entity name instead of collection name** (`internal/generator/restapi/generator.go:1629-1631`): Error says `"collection for entity %q has duplicate operations"` using `eb.Entity`. Should include `eb.Name` to identify the specific collection.

- [-] [process-revision-complete] **`validateScopeParentConsistency` reports entity name instead of collection name** (`internal/generator/restapi/generator.go:1569-1571`): Error uses `eb.Entity` as the primary identifier (`"collection for %q sets scope…"`). With multiple collections for the same entity, this is ambiguous. Should use `eb.Name`.

- [-] [process-revision-complete] **`collectEntitySlotParams` parameter and doc comment not updated for collection-aware naming** (`internal/generator/restapi/generator.go:1693-1700`): The function parameter is named `entityName` and the doc comment says "collects slot binding parameters for a specific entity." The caller passes `eb.Name` (a collection name) and the function matches against `sb.Collection`. The parameter should be renamed to `collectionName` and the doc comment updated to say "for a specific collection."

- [-] [process-revision-complete] **`Generate` function doc comment is stale** (`internal/generator/restapi/generator.go:22`): Says "one per exposed entity" but the implementation now generates one handler per collection. Should say "one per collection."

## Round 2

- [-] [process-revision-complete] **`validateExposeOperations` function not renamed to collection terminology** (`internal/generator/restapi/generator.go:1487`): Every other validation function was renamed during the collection migration (e.g. `validateExposeUniqueness` → `validateCollectionNameUniqueness`, `validateCaseInsensitiveUniqueness` → `validateCollectionDerivedUniqueness`). This function retains the "Expose" prefix. The caller site at line 76 invokes `validateExposeOperations(ctx.Collections)`. Should be renamed to `validateCollectionOperations` or similar.

- [-] [process-revision-complete] **Formatting error message uses `entity.Name` instead of collection name** (`internal/generator/restapi/generator.go:452`): `fmt.Errorf("formatting %s handler: %w", entity.Name, err)`. Since handlers are now generated per collection, and multiple collections can reference the same entity, this error message is ambiguous — the user cannot determine which collection's handler failed formatting. Should use `eb.Name`.

- [-] [process-revision-complete] **No test for `stego fill create --collection` flag** (`cmd/stego/main_test.go`): The task adds a `--collection` flag to `stego fill create` (acceptance criterion #7: "`stego fill create` accepts `--collection` flag"), but no test passes `--collection` or verifies the `Collection` field is populated in the generated fill.yaml. The existing `TestRunFillCreate` test only exercises `--slot`.

- [-] [process-revision-complete] **No reconciler test for language validation** (`internal/compiler/reconciler_test.go`): Language validation was added to `Reconcile()` at `reconciler.go:128-135`, but no test in `reconciler_test.go` verifies that a language mismatch or unsupported language causes `Reconcile()` to return an error. Tests exist only in `validate_test.go` for the `Validate()` path. Since `Reconcile` returns a hard `error` (aborts plan generation) while `Validate` returns soft `ValidationResult.Errors`, both paths should be tested independently. Acceptance criterion #5 says "`stego plan` and `stego apply` work with collections" — the language gate is part of that path.

- [-] [process-revision-complete] **Stale comment says "entities" instead of "collections"** (`internal/generator/restapi/generator.go:117-119`): Comment reads "Validate that no two entities produce the same route path. Collisions cause runtime panics (Go 1.22 ServeMux), OpenAPI path overwrites, and duplicate variable declarations." The validation now operates on collections, not entities, and "duplicate variable declarations" no longer applies (variables are collection-derived). Should say "no two collections" and drop the stale consequence.
