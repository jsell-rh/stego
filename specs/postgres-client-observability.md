The PostgreSQL client now uses the private telemetry runtime in the caller's
context. The old client used global providers and the default logger. Generated
workers do not install global providers, so their database lifecycle signals
could be missing from export. The client no longer creates instruments per call
or writes through an unbounded default log handler.

With an `otel-tracing` peer, the compiler connects the client to
`TracePostgresOperation`. This boundary accepts only five operation values:
`read`, `server-identity`, `ensure`, `delete`, and `quarantine`. Unknown values
become `_OTHER`. Outcomes are `success`, `failure`, `busy`, `canceled`, `deadline`,
or `aborted`. Unknown outcomes become `failure`. The client maps errors to fixed
outcomes before it calls the runtime. Error text, SQLSTATE, SQL, arguments,
credentials, server addresses, database names, and resource IDs are not inputs
to this boundary.

The scope is `stego/postgres-client`. Each operation emits a CLIENT span named
`postgres.database.<operation>`, a correlated local and OTLP event named
`postgres.database.completed`, a `stego.postgres.database.duration` histogram
in seconds, and an aggregate `stego.postgres.database.active` count. Fixed
attributes are `db.system.name`, `operation`, `outcome`, and, for failure,
`error.type`. Logs also carry the duration. Runtime resource identity is shared
with the enclosing worker. Caller cancellation does not set an error span.

These timings include connection, SQL, and cleanup work within the public
operation. They remain separate from service database driver timings. Nested
quarantine operations have their own spans; their durations must not be added
to the enclosing deletion duration. The former global scope `stego.postgres`
and outcome `error` are replaced by this private scope and outcome `failure`.

The runtime shares its bounded local queue, private exporters, verified TLS,
and shutdown deadline with other service signals. Completion is idempotent.
With export disabled, a bound runtime still writes fixed local events. Without
a telemetry peer or runtime context, the client emits no signals and does not
fall back to global providers. SQL access and lifecycle behavior are unchanged.

Focused generated tests check real TLS export from the generated client,
correlation, fixed error outcomes, private-data exclusion, inactive totals,
repeated completion, sampling, disabled export, and collector failure. Invalid
database identities stop before a SQL connection in these focused tests.
Full compiler CI must retain the real SQL lifecycle checks. The complete
Hypershell browser workflow must prove successful database creation and deletion,
cleanup denial and recovery, and retained signal identity across worker restart.

[Full compiler CI](https://github.com/jsell-rh/stego/actions/runs/34961995199)
passed at `f2b09c0`, including race checks and generated PostgreSQL provisioning.
Hypershell `2af1b46` adopts this source and requires SQL operation signals in its
complete browser workflow. That application result remains separate and required.

The [Gateway API gate](hypershell-postgres-telemetry-api.json) passed at `2af1b46`
in run `34962176232`: all 31 required tests, 902 source files, 230 generated files,
four matching generation records, and complete cleanup. Acceptance took 153.598
seconds. The independent cluster check found no remaining Job, Pods, or private
fixtures. The complete browser SQL signal check remains required. The later
worker startup change has separate compiler and application checks.
