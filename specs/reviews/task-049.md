# Review: Task 049 — Regenerate Example Output with CORS Middleware

**Reviewer:** Verifier
**Verdict:** PASS — no findings

## Checks Performed

- [x] `cors.go` exists at `examples/user-management/out/internal/api/cors.go` with correct generated header
- [x] `cors.go` content matches rest-api generator output (env vars, defaults, preflight handling)
- [x] `main.go` wires CORS as outermost middleware: `cORSMiddleware(authMiddleware(validationMiddleware(mux)))`
- [x] Variable name `cORSMiddleware` consistent with assembler's `rawConstructorVarName()` derivation
- [x] `state.yaml` updated with `cors.go` hash (SHA256 matches actual file) and `main.go` hash refreshed
- [x] Example `service.yaml` has no CORS override — correctly relies on archetype default `cors: enabled`
- [x] `go build ./cmd/stego` compiles
- [x] `go test ./...` — all 15 packages pass
