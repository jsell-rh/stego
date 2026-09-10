`grpc-application` 1.9.0 and `otel-tracing` 1.7.0 add common outbound gRPC logs,
metrics, spans, and trace propagation. Generated clients use the runtime in
their call context. They do not create providers or export connections.

Generated HTTP and gRPC request boundaries and controller work attach their
runtime to the context. `Runtime.Context` is also available to code that owns a
runtime and starts other work. This method does not transfer resource ownership.
Clients called without a runtime keep their existing transport behavior. A
closed runtime records nothing. No global OpenTelemetry provider is changed.
CLI startup and other independent entry points still need runtime integration.

Each unary invocation and server-response stream produces one client span,
one completion log, and one `rpc.client.call.duration` histogram measurement.
`rpc.client.active_requests` counts current calls without labels. A stream span
ends at transport completion, not at its first header or message. Its final code
includes cancellation, EOF success, and the generated handshake deadline.
Unsupported stream shapes and capacity errors also produce completion signals.
The existing call limits, TLS checks, credentials, deadlines, and retry policy
remain in force. Finish calls are idempotent.

These span lifetimes follow the [RPC span guidance](https://opentelemetry.io/docs/specs/semconv/rpc/rpc-spans/).
The duration metric uses seconds, as specified by the
[RPC metric guidance](https://opentelemetry.io/docs/specs/semconv/rpc/rpc-metrics/).
Every non-OK client code has error status and a fixed `error.type`, in accordance
with the [gRPC client error guidance](https://opentelemetry.io/docs/specs/semconv/rpc/grpc/).
Local and exported completion logs use `rpc.client.call.completed`. Success has
INFO severity; other codes have ERROR severity. Logs and metrics also cover
unsampled calls. Local logs remain available when collector export is disabled.

The method allowlist comes from the same compiled protobuf descriptors used for
code generation. Its order is deterministic. Unknown methods use `_OTHER`.
Addresses, ports, original unknown method names, request and response values,
metadata, and error messages are omitted. This privacy policy deliberately omits
address and original-method attributes requested by the broader conventions.
Deployment peer aliases are not yet supported. The low-level trace method must
receive an allowlisted method; generated clients enforce that boundary.

With tracing enabled, clients copy outgoing metadata and replace `traceparent`
with their child span context. They remove `tracestate` and `baggage` from that
copy. Other call metadata and per-RPC credentials remain intact. The caller's
metadata is unchanged. When export is disabled, no trace header is injected.
Client spans and signals use their parent's runtime instance. This change does
not create a second runtime for each client.

The existing 128-series metric bound, 256-record log and span queues, TLS export
connection, and three-second runtime close budget still apply. Local output
uses the existing bounded queue. Client code does not wait for export or local
writes. The runtime owner must let work finish before it closes providers.
The new signals do not guarantee delivery on abrupt process exit.

Generated tests cover sampled and unsampled signals, context ownership,
private-data exclusion, duplicate finish calls, metadata copies, unknown methods,
unary deadlines, stream EOF, stream cancellation, remote failure, unsupported
streams, and the five-second handshake deadline. A real TLS gRPC server checks
the propagated trace header and retained credentials. The full compiler race
suite passed with PostgreSQL required on port 32918. Static checks passed. The
Hypershell probe first found no child client span after service-account
revocation failed on compiler `facefeb`.

Recording measurements used Go 1.26.8, Linux amd64, and an Intel Core Ultra 9
185H, with three 200 ms samples. The direct helper without a runtime measured
3.010–3.209 ns and no allocations. Local recording measured 385.6–426.1 ns,
415–422 bytes, and four allocations per call. It dropped 59.65%–62.23% of local
records under load. TLS OTLP recording measured 7.953–8.291 microseconds,
9,431–9,678 bytes, and 100–103 allocations. No local records were dropped in
those samples; OTLP queue drops were not measured. The local writer discarded
JSON. These are recording costs, not application latency or delivered capacity.

Attaching the runtime also changes HTTP behavior when export is disabled. The
HTTP microbenchmark measured 121.2–124.2 ns, 368 bytes, and two allocations per
request in that mode. Its recording mode measured 1.528–1.592 microseconds,
1,896 bytes, and 31 allocations. Repeat with `STEGO_BENCH_CLIENT=1 go test -v
-count=1 ./internal/generator/oteltracing`.

Outbound HTTP, database signals, independent CLI and worker entry points,
process resource metrics, and the wider observability requirements remain open.
Application adoption and production capacity are separate gates.
