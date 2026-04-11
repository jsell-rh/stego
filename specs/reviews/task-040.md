# Review: Task 040 — Regenerate Example Output with Spec Amendments

**Verdict:** PASS — no findings.

## Verification Summary

- [x] AC1: Both examples regenerated (commit 4da1da0 modifies files in both example dirs)
- [x] AC2: `main.go` reads `PORT` from `os.Getenv("PORT")` with `"8080"` fallback; no hardcoded port
- [x] AC3: OpenAPI includes `OrganizationCreateRequest`, `UserCreateRequest`, `OrgSettingCreateRequest` schemas; POST/PUT `$ref` points to `{Entity}CreateRequest`; server-managed and scope fields correctly excluded
- [x] AC4: `go build ./...` succeeds in both `examples/user-management/out` and `examples/user-management-rhsso/out`
- [x] AC5: `go test ./...` passes from repo root (15 packages)

## Additional Checks

- Error prefix `USERMANAGEMENT` correct (no `-api`/`-service`/`-server` suffix to strip from `user-management`)
- Handler files identical between both examples (diff verified)
- openapi.json identical between both examples (diff verified)
- Kind validation present on Create, Update, Patch, and Upsert handlers
- `orgsetting.Generation = 0` set in Upsert handler (server-managed field zeroed)
- state.yaml updated in both examples with correct component lists (`jwt-auth` vs `rh-sso-auth`)
