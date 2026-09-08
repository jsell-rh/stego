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

The service waits for every task before it returns. Deferred cleanup therefore
runs after task use of the resources ends. HTTP shutdown retains its ten-second
request drain limit. A worker failure also starts this drain. A listener is
closed even if cancellation occurs before the HTTP task starts.

Every task must honor cancellation. A task that does not return can prevent
shutdown. The service does not detach such a task and close its resources while
it can still use them. Task implementations must bound external operations.
Constructor deadlines, forced process termination policy, and readiness remain
open work. This contract does not recover from a panic in application code.

Generated tests check task errors, unexpected return, cancellation, delayed
cleanup, preserved cleanup errors, shared dependencies, name collisions, and
services with and without HTTP routes. A process test checks SIGTERM shutdown.
An HTTP test checks request draining after a worker failure. These tests run
with the race detector on Linux. Generated test programs also compile for
Windows amd64 and macOS arm64; runtime tests on those systems remain open.

This task contract is common service infrastructure. It contains no Hypershell
rules. Outbox and Kafka service composition still require storage integration,
configuration, migrations, and complete runtime checks.
