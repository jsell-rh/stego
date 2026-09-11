The generated PostgreSQL adapter and process own database startup limits.
Applications do not need a connection wrapper or a startup probe in domain code.

Each call to the adapter's database connector has a five-second deadline. This
is one budget for the driver connection operation, including name lookup,
connection attempts, TLS negotiation, authentication, and host fallbacks. A
shorter caller deadline takes precedence. Caller context values are preserved.
The connector rejects and closes a connection returned after its deadline.
Canceling the connection context after success does not shorten the session
lifetime or a later query's own deadline.

The pool remains lazy: `OpenDatabase` configures the pool without connecting.
The generated process then calls `PingContext` with a five-second deadline.
This startup budget includes obtaining a connection and completing the ping.
It inherits the generated process signal context, so a process stop cancels it.
GORM automatic ping is disabled, so it cannot use a separate unbounded call.
The rule also applies to generated SQL processes and registered pool factories.
It runs before migrations, domain constructors, background tasks, and API
listeners. On failure, the process closes the pool and reports the fixed
`database.ping` stage. The failure record excludes connection and driver data.

The connection wrapper uses the standard
[Go connector contract](https://pkg.go.dev/database/sql/driver#Connector) and
the pinned pgx connector. It does not replay transactions or application work.
A supplied driver must honor context cancellation. STEGO cannot interrupt an
arbitrary driver or application callback that ignores its context.

The Hypershell test first creates a Gateway, verifies its owner grant, and reads
its event. It then stops the application and tests restart with two database
faults. One endpoint stops before authentication. The other accepts protocol
startup and stops at the first query. Both starts must exit before the external
test deadline, report one safe failure, and start no API listener. A restart
with the real database must retain the Gateway and deny an unrelated user.

Separate generated-code checks cover the pool with and without telemetry,
caller context and deadline preservation, stalled authentication, and a query
that continues beyond the connection budget on an established session. Generated
SQL and GORM process tests check the startup ping deadline and pool closure.

This change does not set database TLS policy, validate all DSN or environment
settings, bound arbitrary startup callbacks, or add startup telemetry providers.
Migration policy, LISTEN connections, and production capacity require separate
evidence. The wider enterprise goal remains open.

The jshell probe on the preceding compiler failed after successful Gateway
creation, owner-grant checks, and event delivery. Database restart remained
stuck until the external test context killed it at eight seconds. The initial
application archive was
`87b088bc7761128c0c39b8f91e4da32fea88eff0d8d2b853ef3edb29d7eb0abd`.

On 2026-09-11, Job `db-startup` in namespace `stego-db-startup-20260911` ran
Go 1.26.8 and PostgreSQL 18.6. The test container had one CPU and a 3 GiB memory
limit. The database had half a CPU and a 512 MiB memory limit. The job deadline
was 30 minutes. All builds and runtime checks ran in the cluster.

The first candidate passed the complete PostgreSQL adapter race suite in
93.139 seconds, compiler tests in 58.552 seconds, and registry tests in 2.256
seconds. The final candidate added signal cancellation, a late-connection
closure check, and component version 3.16.0. Its generated driver and pool
checks passed in 29.128 seconds, compiler tests in 40.642 seconds, and registry
tests in 2.190 seconds. Static checks and the compiler build passed.
The final tested compiler archive was
`336d814d0e22e73229e65638c5903363679c03b9376aa17dc56e3a5dd28b8f22`.
Later compiler edits changed evidence documents only. Full CI and the pinned
Hypershell workflow are separate checks. These package times do not measure
production request performance.
