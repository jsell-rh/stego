Controller version 1.14.0 gives `RunSweep` the same private telemetry providers
as keyed controllers when `otel-tracing` is present. Consumers supply recovery
rules and retained pages. They do not supply common log callbacks, exporters,
span wrappers, or metric registries.

The scheduler validates all definitions before it starts telemetry. A canceled
run starts no provider. Overlapping sweeps and keyed controllers share the
existing provider owner. The last caller closes it after its workers stop.
Provider close retains the existing three-second budget. A component generated
without the telemetry peer keeps its standalone behavior and dependencies.

Each page read and page validation has operation `scan`. Each started action
has operation `reconcile`. These operations emit the existing correlated span,
completion log, and duration histogram. Retryable failures also increment the
existing retry counter. Local logs remain available without OTLP export. The
existing fixed fields, queue bounds, transport checks, and data policy apply.
Group names, stream names, cursors, items, provider details, and error text do
not enter these signals. A sweep does not register a keyed queue or report
keyed queue capacity.

A pass deadline can permit a later pass. It is recorded as a retry when the
parent run remains active and the error is not terminal. Parent cancellation
and cancellation from a terminal peer failure do not count as retries. The
retry flag describes eligibility for a later pass, not proof that another
attempt ran. The retained source still determines which items appear again.

Telemetry records a deadline even when a page callback returns no error after
its deadline. That observation does not become a new source error for the
terminal policy. Existing page validation, cursor progress, group rotation,
worker limits, callback observations, and error identity remain in force.
The scheduler still joins workers before the next page and before close.
Callback panics still propagate; their values do not enter telemetry.

Generated tests verify source and action failures, successful actions, invalid
pages, pass deadlines, parent cancellation, and terminal peer cancellation.
They check fixed local records and private-data exclusion. Existing sweep
fairness, cursor, validation, and worker tests pass with and without telemetry.
The full compiler race suite passed with PostgreSQL required on port 32917.
Static checks passed. The Hypershell probe first found no common recovery
telemetry on compiler `f7a630b` after service-account creation and a retained
revocation request. Application adoption is a separate gate.

This change uses the existing controller recording path. It is not a new
throughput or capacity result. Telemetry for the older `Run` loop, standalone
scan and cycle entry points, cleanup backlog export, database and outbound
operations, and process resource metrics remains open. It does not provide
durable retry history, distributed ownership, or provider fencing. The full
enterprise and observability goals remain active.
