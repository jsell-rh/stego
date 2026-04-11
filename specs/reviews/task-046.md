# Review: Task 046 — Add CORS Convention Field to Domain Types

## Round 1

- [x] [process-revision-complete] **Parser testdata not updated:** `internal/parser/testdata/archetype.yaml` does not include `cors: enabled` in its conventions block. The live registry YAML (`registry/archetypes/rest-crud/archetype.yaml`) and the registry testdata YAML (`internal/registry/testdata/registry/archetypes/rest-crud/archetype.yaml`) were both updated, but this third copy of the rest-crud archetype fixture was missed. The parser test (`internal/parser/parser_test.go:TestParseArchetype`) consequently has no assertion that `Conventions.CORS` is correctly parsed through the `ParseArchetype()` code path. Add `cors: enabled` to the parser testdata and an assertion in the parser test.

## Round 2

No findings. All four acceptance criteria verified: Convention struct has CORS field with correct YAML tag, all three fixture files updated, parser/registry/types tests assert the field, all tests pass, build compiles. Data flow through compiler reconciler confirmed automatic.
