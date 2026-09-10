This component generates HTTP request tracing and bounded OTLP/gRPC export.
Tracing is disabled unless `OTEL_EXPORTER_OTLP_ENDPOINT` names an HTTPS collector.
The collector certificate must be trusted. Configuration errors prevent startup;
collector failure does not prevent application requests.

The runtime records method, registered route pattern when available, and status.
It does not copy raw URLs, request bodies, credentials, or identity fields.
Sampling, queue size, export time, and shutdown time have explicit limits.

See the [HTTP tracing contract](../../../specs/http-tracing.md) for supported
settings, privacy rules, evidence, and remaining observability requirements.
