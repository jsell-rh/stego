The original assessment required CI checks for the root module and both example
modules. The root `go test ./...` command does not enter nested Go modules.
The examples had retained old generated output and Go 1.22 dependency files.
The updated compiler tests alone did not verify those checked-in projects.

Both examples now use output from compiler `f97b315`, with a clean source
identity and Go 1.26.8. Their declarations, application fills, and project
configuration are preserved. Regeneration adds the current authentication,
transaction, durable event, server, health, and telemetry code. Dependency
resolution, repeated apply, and drift checks passed for both examples.

CI now has a separate job for each example. Each job builds the current compiler,
validates the declaration, applies generation, resolves dependencies, and checks
drift. Generated output and module files must match the committed files. Fill
and declaration files must remain unchanged, and untracked output fails the job.
A further apply must preserve the complete state record. The first apply can
update the compiler build identity to the revision under test.

Each job also verifies modules, checks vulnerabilities, runs the example module
tests with the race detector, and builds the service. The jobs have a 15-minute
limit and run in CI. Their results remain required before this update is verified.
These checks cover generation, build, and fill behavior. They do not prove a
deployed example workflow or replace the Hypershell application gates.

The first [CI run](https://github.com/jsell-rh/stego/actions/runs/34965629567)
failed the vulnerability checks for both examples. Their output still selected
`kin-openapi` 0.128.0; the SSO example also selected `jwt/v4` 4.5.1. The reported
calls reached the OpenAPI request validator and JWT parser. The failures remain
recorded and are not passing build or fill-test evidence.

The fix belongs in the shared generators. `rest-api` 3.1.0 selects `kin-openapi`
0.144.0 and declares Go 1.25 as its minimum target. `rh-sso-auth` 1.0.2 selects
`jwt/v4` 4.5.2. These versions include the fixes identified in
[GO-2026-6275](https://pkg.go.dev/vuln/GO-2026-6275),
[GO-2025-3533](https://pkg.go.dev/vuln/GO-2025-3533), and
[GO-2025-3553](https://pkg.go.dev/vuln/GO-2025-3553). Regeneration and repeat CI
must verify the resulting services; an upgraded version string is not sufficient.

Both examples were regenerated with clean compiler `5d24fad`. Their module files
now select the corrected dependency versions. Dependency resolution, repeated
apply, and drift checks pass. Repeat CI must still establish vulnerability,
build, and fill-test results for these files.
