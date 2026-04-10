# Review: Task 037 — Error Code Prefix — Strip Common Suffixes

**Verdict:** PASS — no findings.

## Checklist

- [x] `deriveErrorPrefix` implements the 3-step algorithm: strip suffix, remove hyphens, uppercase
- [x] `break` after first suffix match prevents double-stripping (correct per spec)
- [x] All spec examples verified: `hyperfleet-api` -> `HYPERFLEET`, `user-management` -> `USERMANAGEMENT`, `order-service` -> `ORDER`
- [x] AC1: `deriveErrorPrefix("hyperfleet-api")` returns `"HYPERFLEET"`
- [x] AC2: `deriveErrorPrefix("order-service")` returns `"ORDER"`
- [x] AC3: `deriveErrorPrefix("my-cool-server")` returns `"MYCOOL"`
- [x] AC4: `deriveErrorPrefix("user-management")` returns `"USERMANAGEMENT"`
- [x] AC5: `deriveErrorPrefix("api")` returns `"API"` (no leading hyphen, not stripped)
- [x] AC6: `go test ./internal/generator/restapi/...` passes
- [x] AC7: `go build ./cmd/stego` compiles
- [x] Full test suite passes with no regressions
- [x] `TestGenerate_ErrorsFileGenerated` updated to expect `HYPERFLEET-*` codes (not `HYPERFLEETAPI-*`)
- [x] No stale `HYPERFLEETAPI` references in Go source files
- [x] Edge cases tested: empty string, bare `api`, short names like `just-api`
