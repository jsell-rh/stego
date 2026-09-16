This component generates HTTP and gRPC request spans, metrics, and OpenTelemetry
request logs through one shared runtime. Export is enabled when
`OTEL_EXPORTER_OTLP_ENDPOINT` names an HTTPS collector. The collector certificate
must be trusted. Configuration errors prevent startup; collector failure does
not prevent application requests.

Version 1.10.0 adds fixed PostgreSQL driver signals through the same runtime.
The storage peer supplies the driver callbacks. SQL and connection data do not
enter the signal API. See the [database telemetry contract](../../../specs/database-observability.md).

Version 1.13.0 adds automatic metrics for the process database pool. The compiler
supplies the existing pool; no database is created for telemetry alone. The
runtime reports connection use, the configured limit, cumulative waits and
wait duration, and connection retirement. These are eight fixed series with
the runtime's service and instance identity. See the
[pool metric contract](../../../specs/database-pool-metrics.md).

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

[RPC client observability](../../../specs/rpc-client-observability.md) adds
common outbound signals through the active runtime. Generated clients select
method names from compiled contracts and preserve the complete stream lifetime.

Generated HTTPS clients use the telemetry runtime in their call context.
The [HTTP client contract](../../../specs/http-client-observability.md) defines
completion, propagation, privacy, and resource bounds. The generated CLI now owns its runtime; see the
[CLI contract](../../../specs/cli-observability.md).

`ExportEnvironment(service, directory)` captures the current collector settings
and returns validated environment values and files for another generated process.
The caller selects the service name and its private mount directory. Certificate
and token source paths do not enter the returned environment. The helper uses
fixed file names, `otel-ca.pem` and `otel-token`. Store these files only in the
process's protected mount. Each call reads current files, so reconciliation can
change a configuration digest when credentials rotate.

`STEGO_OTEL_TOKEN_FILE` enables bearer authentication for all three OTLP signals.
The runtime reads that file for each export call and requires TLS. The token file
must be a bounded regular file with no world access, group write, or execute
permission. Group read is allowed for Kubernetes Secret mounts. The file can be
replaced during rotation. Token values never belong in browser configuration.

Runtime and deployment settings use the same validation. Collector endpoints
require HTTPS and the gRPC protocol. CA files can contain certificate PEM only.
Unsupported OTEL settings fail validation, including when export is disabled.
A configured certificate or token requires a collector endpoint. Missing
collector configuration still permits local service logging.
