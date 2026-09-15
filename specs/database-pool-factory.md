The generated browser backend exposed a database factory selection error.
It consumes a SQL pool and declares the PostgreSQL adapter as a peer, but it
does not consume the adapter's storage constructor. Assembly therefore skipped
the adapter's declared factory and emitted `sql.Open` directly. This bypassed
the adapter's pool limits, connection deadline, transport checks, and driver
telemetry. The new pool metrics made this missing boundary visible in the
generated browser process.

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
Hypershell does not need new pool code. Full compiler CI and the regenerated
browser workflow must verify this change before the application gate closes.
