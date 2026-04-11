# Review: Task 043 — Rename `size` Query Parameter to `pageSize`

## Verdict: PASS — no findings

All acceptance criteria verified:

- [x] AC1: Generator emits `r.URL.Query().Get("pageSize")` in list handler generation (`generator.go:957`)
- [x] AC2: Variable names updated to `pageSizeStr`/`pageSize` (`generator.go:957,962`)
- [x] AC3: OpenAPI spec declares `pageSize` query parameter (`generator.go:2040`)
- [x] AC4: `ListOptions.Size` internal field name unchanged (`generator.go:1038` — `Size: pageSize`)
- [x] AC5: Response envelope `"size"` JSON key NOT renamed (`generator.go:1115` — `"size": actualSize`)
- [x] AC6: All tests updated to assert new parameter name (6 test functions updated)
- [x] AC7: `go test ./...` passes (all 15 packages)

Additional verification:
- [x] `handlerScopeIdentifiers` correctly updated: `"size"`/`"sizeStr"` removed, `"pageSize"`/`"pageSizeStr"` added
- [x] No stale references to `Get("size")` remain in `internal/` (examples are out of scope — covered by task 045)
- [x] Single generation path (`generateListMethod`) ensures all list handlers emit the new parameter name
