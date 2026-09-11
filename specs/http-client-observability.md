`http-application` 1.5.0, `cli-application` 1.5.0, and `otel-tracing` 1.8.0
add shared outbound HTTP signals. The generated HTTPS client uses the runtime
in its context. It does not create a provider, queue, connection, or global
logger. Request and controller boundaries supply this context. Independent
processes must connect their runtime at entry. The generated
[CLI entry point](cli-observability.md) now supplies this boundary.

Each call produces a CLIENT span, an `http.client.request.completed` log, and
an `http.client.request.duration` histogram measurement in seconds.
`http.client.active_requests` counts calls without labels. Completion includes
the response body and each stream callback. Callback errors retain their
identity. A callback panic or `Goexit` records `aborted` and continues to unwind.
The client does not recover application panics. Invalid requests and capacity
failures also produce signals after client and context validation.

HTTP responses of 400 through 599 have error status. Transport, timeout,
redirect, size, and callback failures use fixed error classes. Explicit context
cancellation has the `canceled` outcome, no `error.type`, and no error span status.
The [HTTP span guidance](https://opentelemetry.io/docs/specs/semconv/http/http-spans/)
provides this cancellation rule. The duration metric and span use the same
start and end time, as required by the
[HTTP metric guidance](https://opentelemetry.io/docs/specs/semconv/http/http-metrics/).
Local JSON logs remain available without a collector. Logs and metrics also
cover unsampled calls. Finish calls are idempotent.

Method names come from a fixed list. Unknown methods and error classes use
`_OTHER`. Status codes are limited to 100 through 599. No URL, address, port,
query, account ID, header, body, or raw error reaches these signals. This privacy
policy omits address and URL fields from the broader semantic conventions.
It does not claim full attribute compliance. Peer aliases remain open.

With tracing enabled, the client copies headers, removes all letter-case forms
of `traceparent`, `tracestate`, and `baggage`, and inserts its child trace context.
Other headers, including credentials, remain intact. The caller's header map
is unchanged. Local-only and unbound calls do not inject trace headers.

The existing TLS 1.3, origin, redirect, body size, deadline, connection, and
admission controls remain in force. Public transport errors are unchanged.
The runtime retains its bounded export queues, 128-series metric limit, and
three-second close budget. The owner must let calls finish before runtime close.
Abrupt process exit can lose queued signals.

Generated tests cover real TLS requests, response failures, redirects, size
limits, timeouts, stream completion and cancellation, callback errors, panic,
`Goexit`, private headers, and sampled and unsampled OTLP export. They also check
local-only calls, closed runtimes, private-data exclusion, and duplicate finish.
The existing generated HTTP and CLI tests passed without a tracing peer.

The Hypershell probe first failed on compiler `d771730`: a real Keycloak account
was created, but no HTTP client signals were present. Its provider entry point
also lacked a telemetry boundary. The application test connects the common
runtime and checks API-to-RPC-to-HTTP correlation across provider restart.

A Go 1.26.8 Linux amd64 benchmark used an Intel Core Ultra 9 185H and three
200 ms samples. The direct helper without a runtime took 2.813–2.959 ns with
no allocations. Local recording took 383.6–400.5 ns, 600–608 bytes, and five
allocations per call. It dropped 66.32%–68.83% of local records under this load.
TLS OTLP recording took 6.538–6.678 microseconds, 7,589–7,605 bytes, and 82–83
allocations. No local records were dropped in those samples. OTLP queue drops
were not measured. The local writer discarded JSON. These costs exclude HTTP
transport and header injection. They are not application latency or delivered
capacity. Repeat with `STEGO_BENCH_CLIENT=1 go test -v -count=1
./internal/generator/oteltracing`.

Database signals, full process lifecycle export, other independent worker
entry points, and production capacity evidence remain open.

The full compiler race suite passed with PostgreSQL 18.6 required on port 32919.
`go vet ./...` passed. These checks include generation with and without a
telemetry peer. The Hypershell application records its own acceptance result.
