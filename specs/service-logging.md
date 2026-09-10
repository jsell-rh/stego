The `otel-tracing` component version 1.3.0 adds a private service event logger.
The generated constructor records `telemetry.runtime.started`. Its `Close`
method stops new service events and records `telemetry.runtime.stopped` once.
These events describe the telemetry runtime. They do not prove that every
application dependency is ready or that all provider work completed.

Service events have fixed names, messages, and severity. `LogServiceEvent`
accepts the generated `ServiceEvent` type. The common catalog also includes
`ServiceReady`, `ServiceStopping`, and `ServiceFailed`. A caller must supply the
correct lifecycle meaning for these explicit events. Unknown event values are
rejected. The API accepts no message text, error object, or arbitrary fields.
Domain event declarations remain open; the fixed catalog does not replace them.

Every service event enters a local JSON queue. With OTLP log export enabled, it
also enters the existing private log provider under scope `stego/service`.
Both forms include service identity, time, severity, and a fixed event message.
An explicit call with a valid span context includes trace and span IDs. Other
context values are not recorded. Automatic runtime events have no request IDs.
The service name uses the existing validated `OTEL_SERVICE_NAME` setting or the
generated service name. It is now validated even when no collector is configured.
`OTEL_LOGS_EXPORTER=none` disables remote log export, not local service logs.

Local output uses stderr. It has one worker and at most 256 queued records.
Each record contains only fixed catalog text, a bounded service name, a time,
and optional fixed-length trace IDs. The queue retains no request context.
Full queues drop new records and increment `LocalLogDrops`. Write errors
increment `LocalLogFailures`. Neither path logs the error text or retries.
The OTLP queue and transport retain their existing independent limits.

Provider flush and local output share one three-second shutdown budget.
Provider shutdown errors increment `ShutdownFailures` and queue one fixed local
`telemetry.shutdown.incomplete` warning. This warning is not exported. A local
flush timeout also increments the counter. No global logger or OpenTelemetry
provider is replaced, so exporter diagnostics cannot enter this log API.

The runtime cannot cancel an operating-system write to stderr. If that write
stays blocked, the one local worker can remain until the write returns or the
process exits. The queued records remain bounded, callers continue, and `Close`
returns at its deadline. A released writer drains the queue and exits. Local
logging and OTLP are best effort; neither is a durable audit log.

Generated runtime tests cover local-only output, OTLP output, export disabling,
trace correlation, invalid events and service names, concurrent calls and close,
write errors, full queues, a blocked collector and local writer, and worker
termination after the writer is released. The Gateway regression first failed
with compiler `f89bf13`: event delivery succeeded, but no lifecycle logs appeared.

A local queue-pressure measurement used Go 1.26.8 on Linux amd64 with an Intel
Core Ultra 9 185H. Three 200 ms samples measured 80.00–95.06 ns per call and
21–24 bytes per call. The writer discarded JSON output. Between 86.12% and 87.96% of calls were dropped
because recording exceeded output throughput. This measures bounded admission
under overload. It is not evidence of delivered log throughput or production
capacity. The existing benchmark command also runs this measurement.

Complete process logging remains open. Generated main still uses its existing
plain startup and failure messages. Early constructor failures, automatic
application readiness, domain events, controller events, and request JSON output
need further shared contracts and application tests. The new catalog does not
capture arbitrary `log` or `slog` messages. See the
[full observability requirement](shared-observability.md).

The full compiler race suite passed with PostgreSQL required on port 32911.
Static checks and the generated runtime tests passed. The benchmark run passed.
Pinned Gateway checks and remote CI are separate evidence.
