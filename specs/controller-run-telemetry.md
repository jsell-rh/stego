# Public controller telemetry

The public `controller.Run` function did not start the common telemetry runtime.
It supplied only the optional application observer. The keyed controller and
sweep entry points already used the common runtime.

The candidate adds the same runtime owner to `Run`. Each watch session, scan,
and reconciliation has an independent operation trace. Provider calls remain
children of that operation. The common provider exports fixed log fields,
metrics, and traces. Resource values and callback errors do not enter telemetry.
The application observer retains its existing callback and error contract.

Each session registers its queue. Capacity includes the configured waiting
buffer and one active action. The scan can hold one additional item while it
waits for the buffer. Queue registration ends after both source workers stop.
All sessions share the enclosing telemetry owner. The final owner performs the
existing bounded flush.

A failed scan ends its session. The watch session owns the reconnect decision;
scan telemetry does not independently count that reconnect as a scan retry.
The terminal error policy still runs on the main worker. A telemetry setup or
queue registration error stops work before the source opens.

Generated-service tests cover TLS export of logs, metrics, and traces through
the actual public entry point, with sampling enabled and disabled. They check
independent operation roots, provider ancestry, queue counts, complete flush,
private-data exclusion, and runtime ownership. Existing source ordering,
overflow, deadline, permission, and cancellation tests run with both peer modes.
Hosted results are required before qualification. No local Go test or benchmark
is required for this change.

This change covers `Run`. It does not establish telemetry coverage for every
standalone scan or durable cycle helper. The complete C6 audit remains open.
