The `otel-tracing` component version 1.1.0 adds gRPC server spans. The
`grpc-application` component version 1.8.0 uses the same tracing runtime as HTTP
when both components are present. It has no application-specific exporter or
provider. Without the tracing component, the gRPC bridge keeps its existing
constructor contract. Tracing uses `NewTracingRuntime` in compiler assembly to
avoid the `NewRuntime` name used by event delivery. `NewRuntime` remains available
in the generated tracing package.

A transport observation starts before authentication and capacity checks. It
ends after the final transport status is known. A watch has one span for its
whole transport lifetime, including expiry or cancellation. Panic recovery and
error sanitization run before span completion. The observation does not change
authentication, capacity reservations, stream identity expiry, I/O deadlines,
or shutdown. Application code that fails to stop after cancellation can still
run after the transport span ends; it retains its capacity reservation.

The span records three fields: `rpc.system.name`, `rpc.method`, and
`rpc.response.status_code`. Names use the registered protobuf service and method.
Only bounded names with protobuf name characters are retained. The fallback is
`_OTHER`; the original invalid value is not retained. The status field uses one
of the 17 gRPC status names. `UNKNOWN`, `DEADLINE_EXCEEDED`, `UNIMPLEMENTED`,
`INTERNAL`, `UNAVAILABLE`, and `DATA_LOSS` mark a server span as failed and add
`error.type` with that fixed status name. Status messages are excluded. These
fields follow the server rules in the
[OpenTelemetry gRPC conventions](https://opentelemetry.io/docs/specs/semconv/rpc/grpc/)
version 1.44.0, whose RPC fields are release candidates. The instrumentation
scope is `stego/grpc`.

The HTTP tracing [export and ingress limits](http-tracing.md) also apply to gRPC.
Only one valid 55-character `traceparent` value is read from incoming metadata.
The local sampling ratio controls recording. Request and response messages,
resource IDs, user identity, tokens, baggage, tracestate, peer addresses, and
error text are excluded. The same bounded queue and verified TLS exporter serve
both protocols. Collector failure does not block application calls.

This observation covers registered application methods that reach the server
interceptors. Transport failures before interception, invalid message decoding,
and unknown methods have no span from this observation. It does not add client,
database, controller, or per-message spans. It does not claim complete upstream
observability compliance or production capacity.

Generated runtime tests check span lifetime, all gRPC status classes, private
data exclusion, valid and invalid parent contexts, disabled operation, and
method fallback. The transport tests check observation before authentication
and final unary status after denial, handler failure, or panic. Generated
application assembly builds with tracing in a custom namespace and without
tracing. The Hypershell acceptance gate adds Gateway creation, watch delivery
and expiry, denied reads, restart, and collector failure. Its old-compiler
baseline created the Gateway and delivered the event, but failed because no
gRPC trace was exported.

The local application baseline failed after 7.78 seconds. With the new generator,
the Gateway workflow passed in 8.48 seconds under race detection. The full
compiler race suite passed with PostgreSQL required on port 32908. Static checks
also passed. Pinned application regeneration and remote CI are separate checks;
the local result does not replace them.
