The user requires complete OpenTelemetry logging, metrics, and tracing in STEGO.
The requirement was confirmed on 2026-09-10. Consumers must receive common
instrumentation through generated code. They must not need to build exporters,
span wrappers, metric registries, log bridges, or controller telemetry loops.
This requirement is part of C6, H2, and H3, and remains open.

The shared runtime must provide:

- Structured service logs with severity, service identity, and trace correlation.
- OpenTelemetry log export, request and runtime metrics, and distributed traces.
- Automatic HTTP and gRPC instrumentation, including denied requests, failures,
  cancellation, streaming calls, and shutdown.
- Common controller and reconciliation telemetry for attempts, work duration,
  pending work, retries, errors, and recovery. Domain code supplies only the
  resource meaning and provider action.
- Common database and outbound client instrumentation where STEGO owns those
  boundaries, with context propagation through generated clients.
- One validated deployment configuration, explicit resource ownership, bounded
  queues and attribute cardinality, verified export transport, and bounded flush.
- Failure isolation: an unavailable collector must not stop application work or
  cause unbounded memory growth, request delay, or shutdown delay.

Common fields must have a clear data policy. Do not export credentials, raw
request bodies, raw SQL, resource IDs as metric labels, arbitrary baggage,
provider error text, or user profile data by default. Domain telemetry needs
explicit declarations. Logging must remain useful when export is disabled.
Export settings belong to deployment configuration, not application handlers.

Acceptance must use a complete Gateway workflow through the generated process
and a real local OTLP collector. Verify log, metric, and trace correlation;
authorized and denied access; watch lifetime; event delivery and reconciliation;
restart; collector failure; queue pressure; and repeat generation. Verify the
same common behavior with an independent service. Measure enabled and disabled
cost, concurrent request behavior, and bounded resource use. A trace-only test
or a declaration file is not evidence for the complete requirement.

The [request observability contract](request-observability.md) adds automatic
OTLP request logs and metrics to the same runtime. This covers the request
boundary, not complete service or controller logging.

Earlier evidence covers [HTTP spans](http-tracing.md) and
[gRPC call and stream spans](grpc-tracing.md) with a shared private provider,
bounded queue, and verified TLS OTLP/gRPC export. The combined request gate
adds log and metric export. Complete service logging, database and outbound
spans, runtime metrics, and controller telemetry remain open.
The broader upstream observability requirements also remain in force.

The [service logging contract](service-logging.md) adds a private fixed-event API,
local JSON output, and automatic telemetry runtime lifecycle events. It preserves
bounded callers and shutdown when local output blocks. General process and
domain logging remain open; this step does not close the full requirement.

The [controller observability contract](controller-observability.md) adds common
keyed-work logs, metrics, spans, and aggregate queue gauges. Consumers no longer
need common event-to-log callbacks for these boundaries. Non-keyed work and the
remaining telemetry requirements stay open.

The [instance identity contract](telemetry-instance-identity.md) separates runtime
resources with random UUIDs. Request, controller, and local signals share their
runtime identity. Replica aggregation can now retain separate OTLP streams;
backend storage and production capacity remain separate gates.

The [process failure policy](process-failure-privacy.md) adds safe local failure
records before telemetry is available and removes GORM's raw query and error
output. Bootstrap export and complete process and database telemetry remain open.
