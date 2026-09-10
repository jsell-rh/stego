The `otel-tracing` component now generates HTTP server spans and an OTLP/gRPC
exporter. Version 1.0.0 replaces the empty generator. Application handlers do not
need their own exporter, queue, response wrapper, or trace-context parser.

The runtime is disabled when `OTEL_EXPORTER_OTLP_ENDPOINT` is absent. In that
case it creates no provider or export connection and returns the application
handler directly. When enabled, the endpoint must be an HTTPS origin. Export
uses gRPC with verified TLS 1.3, no environment proxy, and no DNS service config.
The optional `OTEL_EXPORTER_OTLP_CERTIFICATE` selects a bounded, regular PEM file;
otherwise system trust roots apply. Invalid configuration prevents startup.
An unreachable collector does not prevent startup or request processing.

The supported environment settings are:

| Setting | Behavior |
| --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | HTTPS collector origin; absence disables tracing |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | Must be absent or `grpc` |
| `OTEL_EXPORTER_OTLP_CERTIFICATE` | Optional collector trust file |
| `OTEL_SERVICE_NAME` | Defaults to the declared service name; at most 128 simple name characters |
| `OTEL_TRACES_SAMPLER_ARG` | Local trace-ID sampling ratio from zero through one; default 0.1 |

Other nonempty `OTEL_` settings are rejected when tracing is enabled. They cannot
silently change resource fields, exporter headers, limits, transport, or sampling.
The runtime uses a private tracer provider and does not replace global providers.

A valid single W3C `traceparent` continues its trace and parent ID. Missing,
repeated, or malformed values start a new root for an ordinary HTTP request.
The remote sampling flag cannot override the local ratio. `tracestate` and
baggage are not imported. This is a deliberate trust boundary for public ingress.
It differs from Hypershell's upstream parent-based sampling and baggage policy.

The generated outer middleware creates one server span before authentication.
It records a bounded request method and final response status. An inner
middleware retains the registered Go route pattern when authentication copies
the request. Names use the method and that pattern when available; otherwise
they use `HTTP` and the method. Only bounded patterns with simple route characters
are accepted. Unknown routes and requests denied before routing can have no
route field. Raw request URLs, resource IDs, query values, headers, bodies,
identity fields, and provider error text are not copied. Server errors and
panics mark the span as failed without an exception message.

The response wrapper preserves optional HTTP interfaces, including streaming
flush support. Public discovery and health routes remain outside these wrappers.
The runtime uses OpenTelemetry Go 1.46.0 and its
[OTLP/gRPC exporter](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.46.0).
Generated dependencies include gRPC 1.83.1. The minimum Go target is 1.26.0.

The batch queue holds at most 256 spans, with at most 32 in a batch. Export has a
two-second deadline, no exporter retry, a one-MiB request limit, and a 64-KiB
response limit. Queue saturation drops spans instead of blocking requests.
Shutdown permits three seconds for the provider, then closes its connection.
Export is best effort. There is no durable trace queue or loss-free guarantee.
Failed batches increment `ExportFailures` and return a fixed safe error to the
SDK. This counter does not count SDK queue drops. Complete operational metrics
for tracing remain open.

Generated tests cover trusted TLS export, rejected untrusted certificates,
private-data exclusion, trace continuation, route patterns after request copies,
streaming responses, invalid settings, disabled operation, blocked export,
request progress under queue pressure, and bounded shutdown. The Gateway test
uses the generated process with PostgreSQL and a local TLS collector. It checks
successful and denied requests, restart, and continued access after collector
loss. The original application baseline served the request but exported no span.
The fixed workflow passed locally under race detection in 6.811 seconds. The
full compiler race suite passed with PostgreSQL required on port 32907. Static
checks passed. The application dependency scan reported no vulnerabilities.
Final pinned adoption is recorded in the application evidence.

This is an HTTP tracing implementation, not completion of the upstream
observability specifications. gRPC request spans, database spans, request metrics,
controller spans, browser-to-API evidence, OTLP/HTTP transport, collector client
authentication, and production capacity evidence remain open. Domain-specific
span fields must have a separate reviewed data policy.

A local instrumentation measurement used Go 1.26.8 on Linux amd64 and an Intel
Core Ultra 9 185H on 2026-09-10. Three 200 ms samples used a fixed request and a
handler that only returns 204. Disabled tracing took 1.617–1.752 ns per call,
with no allocation. Recording took 1.096–1.218 microseconds, 1,696 bytes, and
24 allocations per call. This isolates the outer request wrapper and span
recording. It excludes routing, the inner route wrapper, sampling rejection,
queueing, serialization, collector traffic, and real application work. It is
not a throughput or latency claim for a deployed service. Repeat with:

```sh
STEGO_BENCH_TRACING=1 go test -v -count=1 ./internal/generator/oteltracing
```
