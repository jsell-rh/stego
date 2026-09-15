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
