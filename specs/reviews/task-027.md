# Task 027 Review: tsl-search Component Generator

## Round 1

- [-] [process-revision-complete] **Go module path mismatch between GoModRequires and generated import paths.** The generator's `GoModRequires` declares `github.com/yaacov/tree-search-language` at `v0.3.2`, but the generated search.go imports from `github.com/yaacov/tree-search-language/v5/pkg/tsl` and `github.com/yaacov/tree-search-language/v5/pkg/walkers/sql`. In Go modules, `github.com/yaacov/tree-search-language` and `github.com/yaacov/tree-search-language/v5` are distinct modules — the `/v5` import path requires `github.com/yaacov/tree-search-language/v5 v5.x.x` in go.mod. The generated go.mod (see `examples/user-management/go.mod:10`) lists `github.com/yaacov/tree-search-language v0.3.2` which will not satisfy the `/v5/` imports. Any real project resolving dependencies would fail to build.
  - **Files:** `internal/generator/tslsearch/generator.go:51-52` (GoModRequires), `internal/generator/tslsearch/generator.go:77-78` (import paths)

- [-] [process-revision-complete] **Squirrel does not parameterize the TSL SQL walker output.** AC #5 requires "Parameterized queries via squirrel (no SQL injection)." The TSL SQL walker `walker.Walk()` returns `(string, error)` — a complete SQL WHERE clause with literal values embedded (e.g. `name = 'foo' AND age > 5`). The subsequent call `sq.Expr(where).ToSql()` passes a string with no `?` placeholders and no bound args, so it returns `(where, nil, nil)` — the string unchanged and args empty. This means `SearchResult.Args` is always nil, and `query.Where(searchResult.Where, searchResult.Args...)` applies raw SQL with no parameterization. The squirrel dependency provides zero security benefit; SQL injection prevention depends entirely on the TSL library's internal value quoting, not squirrel parameterization as spec'd.
  - **Files:** `internal/generator/tslsearch/generator.go:133-143` (generated sq.Expr call)

- [-] [process-revision-complete] **Invalid search expressions and unknown fields return HTTP 500 instead of 400.** The spec states "Field name validation against entity field definitions (disallowed/unknown fields rejected with 400)." The generated list handlers pass the search expression to `store.List()`, which wraps parse/validation errors as `fmt.Errorf("search error: %w", err)`. The handler then catches all store errors with `handleError(w, r, InternalError(err.Error()))`, which returns 500. A malformed search expression or a reference to an unknown field produces 500 Internal Server Error instead of the 400 Bad Request required by the spec. The rest-api generator should either parse/validate the search expression at the handler layer (before calling store), or the store should return a typed error that the handler can distinguish from internal errors.
  - **Files:** `internal/generator/restapi/generator.go` (list handler error handling), `examples/user-management/out/internal/api/handler_all_users.go:87-89`, `examples/user-management/out/internal/api/handler_org_users.go` (same pattern)

## Round 2

No findings. All three round 1 issues were correctly resolved:

1. Module path: `GoModRequires` now uses `github.com/yaacov/tree-search-language/v5` at `v5.2.12`, consistent with the `/v5/` import paths in generated code and the example `go.mod`.
2. Parameterization: Generated code uses the TSL v5 SQL walker's `Sqlizer` return type, calling `filter.ToSql()` to extract parameterized SQL with bound args. Squirrel parameterization now happens correctly inside the walker.
3. Error codes: `ErrSearch` sentinel in `api` package, store wraps search errors via `fmt.Errorf("%w: %s", ErrSearch, err)`, list handlers use `errors.Is(err, ErrSearch)` to return 400 for client-input errors.

All tests pass (`go test -count=1 ./...`). Build succeeds (`go build ./cmd/stego`).
