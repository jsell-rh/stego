# Review: Task 003 — YAML Schema Parsing

## Findings

- [x] [process-revision-complete] **Parse errors lack line context.** The task description requires "Return structured errors with file path and line context." `ParseError` (`parser.go:15-18`) only stores `Path` and `Err` — no line number or surrounding line context is included. When a YAML file fails to parse, the user sees the file path but has no indication of where in the file the problem is. Add line number (and optionally a snippet of the offending line) to `ParseError` and its `Error()` output.

- [x] [process-revision-complete] **Round-trip tests are shallow — they only compare a subset of fields.** The acceptance criteria require "Round-trip test: marshal -> unmarshal -> compare." The existing round-trip tests verify only a few top-level scalar fields and skip nested/complex structures:
  - `TestRoundTripFill` (`parser_test.go:517`): checks `Name`, `Implements`, `Entity` — skips `QualifiedBy` and `QualifiedAt`.
  - `TestRoundTripComponent` (`parser_test.go:451`): checks `Name`, `Version`, `Requires` count, `Slots` count — skips `Config` entirely (the most complex nested structure with custom unmarshalers).
  - `TestRoundTripServiceDeclaration` (`parser_test.go:493`): checks `Name`, `Archetype`, `Entities` count, `Slots` count — skips `Expose` details (scope, parent, operations), `Slots` details (gate, fan-out, chain), `Overrides`, and `Mixins`.
  - `TestRoundTripArchetype` (`parser_test.go:427`): checks `Name`, `Language`, `Version`, `Components` count, `Bindings` count — skips `Conventions` fields, `CompatibleMixins`, and `DefaultAuth`.

- [x] [process-revision-complete] **Component Config round-trip is broken (masked by shallow test).** `ConfigFieldItems` (`types.go:138`) has a custom `UnmarshalYAML` that accepts a flat map of named sub-fields, but has no corresponding `MarshalYAML`. When marshaled, it produces `fields:` and `inline:` struct keys instead of the flat map the custom unmarshaler expects. Verified: marshaling the parsed `component.yaml` and unmarshaling the result causes `expose.items.Fields` to be `nil` (the data lands in `Inline` instead). The round-trip test for Component does not cover `Config`, so this breakage is invisible. While the missing `MarshalYAML` is a types concern (task-002), the parser's round-trip test should have caught it — that is squarely a task-003 gap.
