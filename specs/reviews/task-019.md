# Review: Task 019 — Collection-Aware Code Generation Pipeline

## Round 1

- [-] [process-revision-complete] **`validateExposeOperations` reports entity name instead of collection name** (`internal/generator/restapi/generator.go:1491`): When a collection has no operations, the error collects `eb.Entity` instead of `eb.Name`. With multiple collections referencing the same entity (the normal case per spec), this error is ambiguous — the user cannot determine which collection needs fixing. Should use `eb.Name`.

- [-] [process-revision-complete] **`validateFieldReferences` reports entity name instead of collection name** (`internal/generator/restapi/generator.go:1292-1294`): Error messages use `eb.Entity` as the collection identifier (`"collection for %q references scope field…"`). When two collections reference the same entity with different scopes, the user cannot tell which collection triggered the error. Should use `eb.Name` as the first format argument.

- [-] [process-revision-complete] **`validateOperationUniqueness` reports entity name instead of collection name** (`internal/generator/restapi/generator.go:1629-1631`): Error says `"collection for entity %q has duplicate operations"` using `eb.Entity`. Should include `eb.Name` to identify the specific collection.

- [-] [process-revision-complete] **`validateScopeParentConsistency` reports entity name instead of collection name** (`internal/generator/restapi/generator.go:1569-1571`): Error uses `eb.Entity` as the primary identifier (`"collection for %q sets scope…"`). With multiple collections for the same entity, this is ambiguous. Should use `eb.Name`.

- [-] [process-revision-complete] **`collectEntitySlotParams` parameter and doc comment not updated for collection-aware naming** (`internal/generator/restapi/generator.go:1693-1700`): The function parameter is named `entityName` and the doc comment says "collects slot binding parameters for a specific entity." The caller passes `eb.Name` (a collection name) and the function matches against `sb.Collection`. The parameter should be renamed to `collectionName` and the doc comment updated to say "for a specific collection."

- [-] [process-revision-complete] **`Generate` function doc comment is stale** (`internal/generator/restapi/generator.go:22`): Says "one per exposed entity" but the implementation now generates one handler per collection. Should say "one per collection."
