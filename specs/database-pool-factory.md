The generated browser backend exposed two database factory gaps. It consumes
a SQL pool and declares the PostgreSQL adapter as a peer, but has no entities.
The adapter returned no files or factory metadata for this case. Assembly also
skipped a declared factory when its storage constructor was unused. The browser
therefore used `sql.Open` directly. This bypassed the adapter's pool limits,
connection deadline, transport checks, and driver telemetry. Review of the new
pool metric wiring found this missing boundary in the generated browser process.

Assembly now selects the one declared database factory whenever a consumed
component requires a database. It imports the factory package with its assigned
alias. It does not import other packages or emit unused storage constructors
and migration calls. The consumer still selects SQL or GORM. An unused GORM
storage component cannot change a SQL consumer into a GORM consumer. A service
with no consumed database resource still creates no pool.

The regression failed for independent SQL and GORM consumers before the fix.
The corrected check verifies the factory call, its actual import alias, absence
of unrelated imports and storage calls, and the selected backend. A generated
process check verifies that the independent consumer uses the factory, retains
the startup ping deadline, and closes the pool on startup failure. Environment
and private-file database settings are both covered. These focused checks pass.

The factory implementation remains shared in STEGO's PostgreSQL adapter.
Adapter version 4.4.1 emits the pool factory and its metadata when there are no
entities. It emits no storage models, constructor, or migration calls in that
case. The namespace remains subject to validation. The complete browser
composition regression failed before this change and now passes.

Hypershell does not need new pool code. Full compiler CI and the regenerated
browser workflow must verify both changes before the application gate closes.

[Factory CI](https://github.com/jsell-rh/stego/actions/runs/34960738104) passed at
`1a81d80`. [Combined CI](https://github.com/jsell-rh/stego/actions/runs/34960890553)
at `e5b9931` passed the compiler, generated-runtime, and SQL provisioning checks,
but failed one registry assertion that still required adapter version 4.4.0.
Commit `5c5e7c9` changes that assertion to 4.4.1. Its focused registry check passed.
The [full repeat](https://github.com/jsell-rh/stego/actions/runs/34961472255) passed
at `5c5e7c9`, including compiler race and SQL provisioning checks. The earlier
failed combined run remains failed. Application pool results are separate.
