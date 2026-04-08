# Review: Task 015 — CLI — All 11 Commands

## Round 1

- [-] [process-revision-complete] **`stego fill create --slot <s>` does not detect ambiguous slot names.** The spec states: "Each component owns its own slot protos (no sharing between components -- duplication is cheaper than coupling)." This means multiple components can define slots with the same name. At `cmd/stego/main.go:271-282`, the implementation iterates `reg.Components()` (a Go map) and picks the first component whose slot list contains the given name. Map iteration order in Go is non-deterministic, so when two or more components define the same slot name, the command silently picks an arbitrary component — producing different `fill.yaml` `implements` values across invocations. The implementation should detect the ambiguity (count of matching components > 1) and return an error listing the matching components, so the user can disambiguate.
