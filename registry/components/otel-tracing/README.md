This component generates HTTP and gRPC request spans, metrics, and OpenTelemetry
request logs through one shared runtime. Export is enabled when
`OTEL_EXPORTER_OTLP_ENDPOINT` names an HTTPS collector. The collector certificate
must be trusted. Configuration errors prevent startup; collector failure does
not prevent application requests.

The runtime records methods, registered routes when available, status, duration,
and active requests. Request logs correlate with spans. Metrics and logs also
cover unsampled requests. Raw URLs, bodies, credentials, and identity fields are
excluded. Queues, metric series, export time, and shutdown time have fixed limits.

See the [request observability contract](../../../specs/request-observability.md)
for settings, privacy rules, evidence, and remaining observability requirements.
Complete service, controller, database, and outbound instrumentation remains open.

Version 1.3.0 also emits fixed runtime lifecycle events as local JSON and OTLP
logs. Local service logs remain available without a collector. See the
[service logging contract](../../../specs/service-logging.md) for the declared
event API, queue limits, shutdown behavior, and remaining process logging work.

[Shared controller telemetry](../../../specs/controller-observability.md) now
records keyed actions, scans, watch sessions, retries, and aggregate queue state.
The generated runtime owns logging, metrics, spans, and provider lifetime.

Version 1.5.0 adds a random `service.instance.id` shared by all signals and local
logs in one runtime. Replacements get a new identity. See the
[instance identity contract](../../../specs/telemetry-instance-identity.md).

Version 1.6.0 supplies the generated HTTP server's diagnostic logger. It discards
raw panic and server error text, then queues a fixed local and OTLP service
event. See [HTTP diagnostics](../../../specs/http-diagnostics.md) for ownership,
queue limits, request correlation, and remaining work.
