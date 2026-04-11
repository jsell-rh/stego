# Review: Task 047 — CORS Middleware Generation and Assembler Wiring

## Round 1

No findings. All eight acceptance criteria verified:

- [x] AC1: When `CORS == "enabled"`, the rest-api generator emits `internal/api/cors.go` with CORS middleware reading `CORS_ALLOWED_ORIGINS` (default `*`), `CORS_ALLOWED_METHODS` (default `GET,POST,PATCH,PUT,DELETE,OPTIONS`), and `CORS_ALLOWED_HEADERS` (default `Content-Type,Authorization`) from environment variables.
- [x] AC2: The CORS middleware handles `OPTIONS` preflight with `204 No Content` via `http.StatusNoContent`.
- [x] AC3: The assembler wires CORS middleware as the outermost layer via `OuterMiddlewares` — generated `main.go` produces `cORSMiddleware(authMiddleware(mux))` chain.
- [x] AC4: When `CORS` is empty/unset, no `cors.go` file is generated and no `OuterMiddlewares` appear in wiring.
- [x] AC5: Generator tests verify `cors.go` generation, all three env vars with defaults, CORS headers, OPTIONS handling, and negative cases (empty/unset).
- [x] AC6: Assembler tests verify CORS placement: `cors(auth(mux))`, `cors(auth(validation(mux)))`, `cors(mux)` without auth, and validation error for missing `WrapExpr`.
- [x] AC7: `go test ./...` passes — all 15 packages.
- [x] AC8: `go build ./cmd/stego` compiles.

## Additional Verification

- [x] `gen.Wiring.OuterMiddlewares` field added with correct documentation and type
- [x] `computeConsumedConstructors` marks outer middleware constructors as consumed (prevents pruning)
- [x] `generateMainGo` validates `WrapExpr` is non-empty for `OuterMiddlewares` entries
- [x] Constructor expression `api.NewCORSMiddleware()` follows existing pattern (`path.Base(ctx.OutputNamespace)` prefix)
- [x] Generated `cors.go` reads env vars at construction time (not per-request) — consistent with standard middleware pattern
- [x] Namespace validation applied to `cors.go` via existing `gen.ValidateNamespace` call
- [x] CORS convention flows from archetype through reconciler (`archetype.Conventions` passed directly to `gen.Context.Conventions`)
