# Review: Task 034 — rh-sso-auth End-to-End Verification

## Summary

Round 1: Two findings. The generated main.go does not match the rh-sso-auth spec's Wiring section (env vars are read inside Build() instead of main.go), and the reconciler change introduces non-deterministic component ordering via map iteration.

Round 2: Both round 1 findings verified fixed. No new findings. All acceptance criteria satisfied: example exists with correct override, generated code matches spec (middleware.go, context.go, main.go wiring), code compiles and passes go vet, all "Differences from jwt-auth" table rows verified, go test ./... passes.

## Findings

- [x] [process-revision-complete] **Generated main.go does not read environment variables (AC #4, Verification Step 6).** The rh-sso-auth spec Wiring section states: "main.go reads JWK_CERT_URL, JWK_CERT_FILE, and AUTH_ENABLED from environment." Task-034 Verification Step 6 requires main.go to contain "Environment variable reading (JWK_CERT_URL, JWK_CERT_FILE, AUTH_ENABLED)." The generated `examples/user-management-rhsso/out/main.go` contains zero `os.Getenv` calls for these variables. Instead, they are read inside `Build()` in `middleware.go` (lines 88–98). The generated main.go at line 64 has `jWTHandler := auth.NewJWTHandler()` and at line 88 has `jWTHandler.Build()(validationMiddleware(mux))` with no env var reading or builder method calls between them. AC #4 ("Generated main.go wiring matches the spec's 'Wiring' section") is not satisfied.

- [x] [process-revision-complete] **Non-deterministic component ordering in collectComponentNames (reconciler.go:468–473).** The new loop iterates `servicePortOverrides` (a `map[string]string` derived from `svcDecl.Overrides` which is `map[string]any`) and appends unmatched override components to the ordered `names` slice. Go map iteration order is non-deterministic. If a service declares multiple port binding overrides where the override components are not replacements of archetype defaults, their order in the resulting component list varies across runs. The pre-existing code iterated only slices and strings (deterministic); the map iteration is a regression introduced by this commit. Sort the map keys before iterating to restore determinism.
