# Review: Task 026 — OpenAPI Request Body Validation Middleware

## Round 1

No findings. All 9 acceptance criteria verified:

1. `RequestValidation` added to `Convention` struct with `yaml:"request_validation,omitempty"` tag; `request_validation: openapi-schema` added to `registry/archetypes/rest-crud/archetype.yaml`.
2. Generated validation.go loads OpenAPI spec at startup via `//go:embed openapi.json` and `openapi3.NewLoader().LoadFromData()`.
3. Validation middleware filters on `http.MethodPost`, `http.MethodPut`, and `http.MethodPatch` — covering create, update, upsert (PUT), and patch operations.
4. Validation failures return RFC 9457 Problem Details via `handleError(w, r, Validation(valErrors))` with per-field `validation_errors` array.
5. Entity field constraints delegated to kin-openapi schema validation against the generated OpenAPI spec (which encodes required, type, min/max, pattern, enum from entity definitions).
6. Generation is conditional: skipped when `RequestValidation` is empty and when no body-accepting operations exist (read-only collections).
7. `GoModRequires["github.com/getkin/kin-openapi"] = "v0.128.0"` set when validation is active.
8. Tests cover: generation on/off, read-only skip, method filtering, error format (RequestError/MultiError/SchemaError unwrapping), body restore (double NopCloser), envelope interop, auth bypass (NoopAuthenticationFunc), assembler chaining with/without auth, and missing WrapExpr rejection.
9. `go build ./cmd/stego` succeeds; full test suite passes.

Assembler changes (MiddlewareSpec, chaining, consumed-constructor tracking) are correct and backward-compatible with the existing MiddlewareConstructor/MiddlewareWrapExpr mechanism.
