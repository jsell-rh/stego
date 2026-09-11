The `postgres-adapter` and `otel-tracing` components supply shared PostgreSQL
driver signals. The adapter generates a pool factory when the tracing peer is
present. A typed wiring record selects that factory. The compiler rejects an
invalid namespace, an undeclared import, an invalid function name, an unsupported
database resource, or more than one factory. The generated main owns the pool
and closes it if GORM initialization fails or the process returns.

Each operation uses the telemetry runtime in its context. The pool does not
create providers or change global providers. HTTP, gRPC, CLI, and controller
boundaries can therefore pass their runtime and trace to generated storage.
Contexts without a runtime produce no database telemetry. Startup migrations,
unbound workers, pool statistics, the separate event-listener connection, and
the `postgres-client` component still require coverage.

The pgx callbacks cover queries, row reads, execution, connection attempts,
prepare operations, batches, and copy operations. Query completion occurs when
the driver closes its rows. Transactions retain the context used at begin;
their begin, commit, and rollback statements use the query callbacks. A batch
produces one batch completion, rather than a copy of every contained query.
Pool wait time and automatic retries above the driver are outside these call
durations. A caller must close rows and finish its work before runtime shutdown.
Retries above this boundary can produce several driver records for one API
call. Result-conversion errors above the driver are outside its error result.
See the [pgx callback contract](https://pkg.go.dev/github.com/jackc/pgx/v5@v5.11.0#QueryTracer).

The scope is `stego/database`. The runtime emits CLIENT spans, local and OTLP
`db.client.operation.completed` records, `db.client.operation.duration` in
seconds, a separate `stego.db.connection.duration`, and an aggregate
`stego.db.active_calls` count. Call names are fixed: query, connect, prepare,
batch, copy, or `_OTHER`. Outcomes are success, failure, canceled, deadline,
or aborted. Unknown outcomes become failure. Failure, deadline, and aborted
produce an error span; caller cancellation does not. Completion is idempotent.

The runtime accepts no SQL, arguments, database names, addresses, credentials,
driver errors, statement names, table names, or command tags. It does not parse
SQL to infer labels. `db.system.name` is fixed to `postgresql`. The custom
`stego.db.call` attribute identifies the driver API. SQL operation names and
database identity are omitted under the privacy policy. This is a limited
profile of the [database conventions](https://opentelemetry.io/docs/specs/semconv/db/database-spans/),
not a claim that every recommended database field is present.

The existing private providers, bounded queues, TLS export, and shutdown budget
apply. Telemetry does not change application errors, SQL, transaction policy,
or configured database transport. Production database TLS policy and delivered
capacity still require separate evidence.
The factory uses pgx's parsed connection settings directly. Credential text
cannot set runtime parameters through GORM's former raw-string timezone scan.

The acceptance gate must check the same boundaries with real PostgreSQL and
with the Hypershell Gateway workflow. It must include atomic creation, an
event-write failure and rollback, denied reads, filtered lists, restart,
correlated signals, and exclusion of private values. Record results separately;
the broader enterprise and observability goals remain open.

The Hypershell probe on compiler `8ad255a` passed creation, owner-grant,
event-delivery, forced rollback, denied-read, and filtered-list assertions.
It then failed because the Gateway call had no correlated database signals.
The probe first required two fixture corrections: use the existing create
request shape, and count Gateway grants separately from global role grants.

The independent generated telemetry tests passed in `jshell`, with sampling
enabled and disabled. They checked fixed fields, private-data exclusion,
correlation, completion counts, idempotence, and a final active count of zero.
The first combined job failed because of a generator declaration-order error.
After correction, the full compiler suite passed in 80.521 seconds and the
registry suite passed. Generated storage transaction tests passed. The driver
fixture then required removal of an unused import. The revised driver tests
passed with PostgreSQL 18.6 in 1.81 seconds, including open rows, transaction
commit and rollback, deadlines, cancellation, batches, copy, and connection
setting privacy. Static analysis passed. The final driver source archive hash
was `9be9aa7a4736a2181ea6b9e47fe4ec49167eed7c8b2e393489ce4b3ae375a7b9`.
These results came from separate immutable source snapshots in the bounded
cluster job. The initial failed runs remain failed results. Application
adoption and full CI require separate results.

Three 200 ms helper samples used Go 1.26.8, Linux amd64, an Intel Xeon 6975P-C,
a one-CPU limit, a 3 GiB memory limit, and `GOMAXPROCS=1`. Unbound calls measured
3.681–3.852 ns with no allocations. Local recording measured 329.9–357.6 ns,
313 bytes, and four allocations. It dropped 99.50%–99.55% of local records under
this synthetic producer load. TLS OTLP recording measured 4.698–6.006
microseconds, 2,728–2,739 bytes, and 21 allocations. It dropped 92.38%–93.72% of
local records. OTLP queue drops were not measured. The writer discarded JSON.
These measurements exclude the driver callback, pool waits, SQL, network work,
and process startup. They do not establish delivered throughput or capacity.

Compiler commit `9cabca7` passed full CI in run `34603291206`.
A further validation check found that entity names could conflict with private
database helpers or import aliases. Adapter version 3.14.1 reserves these names
before generation. A regression test reads declarations and imports from the
generated database source, then requires preflight and generation to reject
each name without output. This also detects new helpers that lack validation.
The focused name and driver tests passed with the race detector in 2.847
seconds. Registry tests and static analysis also passed in the same bounded
cluster job. The source archive hash was
`61e72348f052a5b85b74410d44ac52297120c1934b5cabd6f579cff9b9208ba6`.
