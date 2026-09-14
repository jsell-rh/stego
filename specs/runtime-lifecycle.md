Generated services can run HTTP handlers and background tasks in one process.
A service can also contain background tasks without HTTP routes.

A generator lists task constructor indexes in `Wiring.BackgroundTasks`. Each
constructed value must have a `Run(context.Context) error` method. The compiler
checks indexes and rejects duplicates. It retains each task constructor and its
dependencies. Generated method assignments let the Go compiler check the task
signature. STEGO does not yet type-check all output before apply.

The service creates a signal context before resource construction. SIGINT and
SIGTERM cancel this context. All tasks receive a shared child context. A task
that fails causes cancellation of the other tasks. A task that returns without
an error before cancellation is a service failure. Errors include the component
name and constructor index. A plain cancellation error during shutdown is normal.
Errors that include other failures remain errors.

The runner checks both the signal context and its task context when it classifies
a task return. Parent cancellation can be visible before cancellation reaches
the child. A normal return in that interval is part of shutdown. A plain
`context.Canceled` result is also normal. Other errors, joined cleanup errors,
panics, and abnormal exits remain failures. The generated regression test delays
child cancellation explicitly; it does not depend on a timing loop.

On 2026-09-14, bounded jshell Job `stego-placement-90694ac2/check` reproduced
the parent-cancellation failure with the previous compiler. The corrected
compiler passed `TestGeneratedBackgroundLifecycle` for worker-only and HTTP
services in 8.908 seconds. Those generated tests use the race detector and retain
the cleanup-error, abnormal-exit, and process-signal checks. The Job completed;
its namespace and local private launch files were removed. Local evidence is in
`/tmp/stego-cancel-order-569yo7r5`.

The service waits for every task before it returns. Deferred cleanup therefore
runs after task use of the resources ends. HTTP shutdown retains its ten-second
request drain limit. A worker failure also starts this drain. A listener is
closed even if cancellation occurs before the HTTP task starts.

Every task must honor cancellation. A task that does not return can prevent
shutdown. The service does not detach such a task and close its resources while
it can still use them. Task implementations must bound external operations.
Constructor deadlines and forced process termination policy remain open work.
The task boundary treats panics and abnormal exits as service failures. It does
not resume a failed task. See `health-probes.md` for readiness behavior.

Generated tests check task errors, unexpected return, cancellation, delayed
cleanup, preserved cleanup errors, shared dependencies, name collisions, and
services with and without HTTP routes. A process test checks SIGTERM shutdown.
An HTTP test checks request draining after a worker failure. These tests run
with the race detector on Linux. Generated test programs also compile for
Windows amd64 and macOS arm64; runtime tests on those systems remain open.

Generators can request constructor arguments through `ConstructorResources`.
The supported values are the service context and a `database/sql` connection.
The compiler supplies the raw connection for SQL services and the underlying
connection for GORM services. Invalid indexes and unknown resources fail
assembly. PostgreSQL tests execute both forms. Raw SQL services use the pgx
driver and include its module requirement.

The Kafka component now uses these resources to construct one outbox worker.
Startup checks queue columns and read, update, and delete permissions before it
connects to Kafka. It does not apply the queue migration. The generated runtime
owns the publisher, cancellation, and worker cleanup. It can run once. Close
cancels delivery and waits for cleanup.

Deployment settings use the `STEGO_KAFKA_` prefix: `BROKERS`, `TOPIC`,
`AUTHENTICATION`, `CA_FILE`, `CLIENT_CERTIFICATE_FILE`, `CLIENT_KEY_FILE`,
`USERNAME`, and `PASSWORD_FILE`. The publisher validates these settings. Secrets
remain in files. Each worker delivers the fixed `kafka` outbox destination to
the configured topic. Runtime tests check delivery after restart and rejection
of a missing queue schema. Real broker deployment checks remain open.
