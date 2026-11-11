On the runtime data path this component serves the gRPC methods that the
declared commands and queries name (ST-SUP-RC07-OC19). It takes the setting
proto_files only. A proto file of the command component's proto_files keeps
the Go package of the command contract, so a command takes one request
message over HTTP and gRPC. Each declared method is served by the generated
handler of its command or query, with the same verification, transaction,
permission, audit and idempotency as over HTTP; a method that no declaration
names answers Unimplemented. A get query reads its resource member from a
string field of the request message; a list query reads the fields cursor
and limit, and its response message has the fields items and next. The
service serves gRPC in the api mode only, at STEGO_GRPC_ADDR with the TLS
certificate STEGO_GRPC_TLS_CERT and its key STEGO_GRPC_TLS_KEY.

On the GORM store path the description below applies.

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

RPC client observability (a retired record, in git history) adds
common outbound signals through the active runtime. Generated clients select
method names from compiled contracts and preserve the complete stream lifetime.

The optional `processes` declarations generate separate RPC entry points with
common authentication, telemetry, signals, and cleanup. Domain factories supply
only their services and dependencies. See RPC processes (a retired record, in git history)
for the source contract and process bounds.

Terminal `Header` and `RecvMsg` results complete the client span before they
return. The gRPC completion callback remains active for abandoned or remotely
completed streams. Both paths use the same once-only telemetry function.
This prevents a caller from closing the telemetry runtime before a returned
stream deadline has been recorded. Successful headers and response frames do
not end the stream span; EOF records successful completion.

The generated transport provides checked required and optional timestamp
conversion. Invalid values return no timestamp and a fixed error. A nil
optional input stays absent; a present zero Go time stays present. See
the conversion contract (a retired record, in git history).
