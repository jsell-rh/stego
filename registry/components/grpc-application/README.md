This component generates protobuf bindings and a gRPC runtime. The application
registers domain services through its configured factory. The runtime supplies
TLS, verified identity, request and stream limits, and shutdown.

A registration factory can use `transport.OnClose(registrar, cleanup)` to give
the runtime ownership of a client or other resource. Register the callback as
soon as the resource exists. If `OnClose` returns an error, the factory must
close that resource itself. A prepared registrar retains this capability.

Cleanup runs once in reverse registration order when startup fails, the runtime
stops, or `Close` is called. Registration ends when the factory returns. A
runtime accepts at most 128 cleanup callbacks. Callbacks must return promptly
and must not call the parent runtime's `Close` method.
A callback panic is reported without its contents; remaining callbacks still
run. Tests cover normal stop, repeated close, registration error, registration
panic, and listener failure.

[RPC client observability](../../../specs/rpc-client-observability.md) adds
common outbound signals through the active runtime. Generated clients select
method names from compiled contracts and preserve the complete stream lifetime.
