The `otel-tracing` component version 1.2.0 generates request metrics and
OpenTelemetry request logs as well as HTTP and gRPC server spans. One runtime
owns all three providers and their shared verified TLS 1.3 export connection.
Application handlers do not construct exporters or record these common signals.
This is progress toward the [full observability requirement](shared-observability.md),
which remains open.

The existing HTTPS collector, trust, service-name, and local trace-sampling
settings apply. With a collector endpoint, metrics and request logs are enabled
by default. `OTEL_METRICS_EXPORTER` and `OTEL_LOGS_EXPORTER` each accept `otlp`,
`none`, or an absent value. Other values fail startup. The metric export interval
defaults to ten seconds. `OTEL_METRIC_EXPORT_INTERVAL` accepts a canonical integer
from 1,000 through 60,000 milliseconds. Other nonempty, undeclared `OTEL_` settings
still fail startup. Without an endpoint, these providers and request wrappers
remain disabled. Existing process logging remains in use. Public health and discovery routes
remain outside the application request wrappers.

The generated instruments are:

| Name | Type and unit | Fields |
| --- | --- | --- |
| `http.server.request.duration` | Histogram, seconds | Method, registered route when known, response status when known, fixed error class for server failure |
| `http.server.active_requests` | Up-down counter, requests | No labels |
| `rpc.server.call.duration` | Histogram, seconds | Protocol, registered method, gRPC status, fixed error class for server failure |
| `rpc.server.active_requests` | Up-down counter, requests | No labels |

Histograms use fixed boundaries through 1,800 seconds and cumulative temporality.
Each instrument has a 128-series limit, including the SDK overflow series.
Overflow retains total counts without retaining excess label combinations.
Active counts have no labels, so series overflow cannot move a decrement to a
different series. Request metrics cover all calls, including calls whose spans
are not sampled. Sampled calls can attach trace and span IDs as exemplars.
IDs are not metric labels. Span and histogram duration use the same timestamps.

The RPC duration name and seconds unit follow the current
[OpenTelemetry RPC metric conventions](https://opentelemetry.io/docs/specs/semconv/rpc/rpc-metrics/).
They differ from the older `rpc.server.duration` millisecond name in Hypershell's
upstream observability specification. Dashboards must use the declared name and
unit. The RPC convention fields remain release candidates.

Request logs use fixed event names and messages: `http.server.request.completed`
and `rpc.server.call.completed`. They carry the same request fields as the span,
a duration, severity, and the SDK trace and span IDs. A normal completion has
INFO severity; a denied or unsuccessful client request has WARN severity; a
server failure has ERROR severity. A watch emits one completion log when the
stream ends. It does not log each watch message. An uncaught HTTP panic balances
the active count and records `error.type=panic`, without inventing a response
status or copying panic text. Panic propagation remains unchanged.

Raw paths, IDs, bodies, credentials, user details, arbitrary metadata, SQL,
baggage, tracestate, and remote error messages are not recorded. Registered route
patterns remain available when authentication copies the request, including
when trace sampling is zero. Metrics and logs do not depend on recorded span
attributes to obtain their fields.

Trace and log queues each hold at most 256 records, with batches of at most 32.
Metric state has a separate fixed series bound. Each exporter has a two-second
deadline, no retry, a one-MiB request bound, and the shared 64-KiB response bound.
Export failures have fixed safe text and separate counters. These counters do
not count SDK queue drops. A blocked collector cannot block request recording.
All providers flush concurrently under one three-second shutdown deadline.
Export remains best effort; there is no durable queue or loss-free guarantee.

Dependencies are OpenTelemetry traces and metrics 1.46.0, and logs 0.22.0.
The logs API and SDK are still beta at this pinned version. STEGO uses their
[official OTLP/gRPC exporter](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc@v0.22.0).
The generated application pins these modules; runtime deployment does not select
a different SDK version.

The independent service tests use a TLS collector for all three signals. They
check correlation, unsampled requests, route preservation after authentication
copies, status classes, active streams, overflow across collection cycles,
privacy, invalid settings, signal disabling, blocked export, panics, and the
shared shutdown deadline. The Gateway gate creates over gRPC, receives the watch
event, reads through REST, denies missing credentials, observes an active watch,
then cancels it. It checks correlated logs and spans, cumulative request counts,
API restart, and progress after collector loss. The old compiler served this
workflow but did not export the required logs.

This change does not complete service-log capture, JSON process logging,
controller or database instrumentation, outbound propagation, runtime metrics,
Prometheus integration, or production capacity. Request logs currently export
through OTLP; they do not replace the application's existing process logs.
Those remaining requirements must use the shared runtime and application gates.

A local measurement used Go 1.26.8, Linux amd64, and an Intel Core Ultra 9 185H.
Three 200 ms samples with a local TLS collector measured 9.539–9.823 microseconds
per request with all three signals, 8,680–8,948 bytes, and 107–110 allocations.
This includes a new HTTP response recorder per call and concurrent SDK export
work. The disabled baseline with that recorder took 145.3–152.1 ns, 208 bytes,
and four allocations. The separate direct-handler disabled check took
2.180–2.721 ns with no allocation. These are recording-cost measurements under
load. They do not prove complete delivery under queue pressure, concurrent
application capacity, or production latency. Repeat with:

```sh
STEGO_BENCH_TRACING=1 go test -v -count=1 ./internal/generator/oteltracing
```

The final full compiler race suite passed with PostgreSQL required on port
32910. Static checks passed. The generated three-signal tests and the local
recording measurement also passed. The first full run found a variable-name
error in the new log-collection test; that test was corrected before the final
full run. Pinned application checks and remote CI are separate evidence.
