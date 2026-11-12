This component generates RPC contracts and separate executables without API
storage. It requires a JWT verifier and telemetry. Its protobuf compiler,
transport, clients, and process runtime are shared with `grpc-application`.

Declare `proto_files` and `processes`, and optional `response_mappings`. Do
not declare a primary API factory or watch-event storage. See RPC processes (a
retired record, in git history).

Version 1.5.0 accepts the `response_mappings` that its configuration declares,
with prepared `inputs`. A mapping reads a model of a model provider and fills
a protobuf message; it keeps the wire contract, owns its output and refuses an
invalid value with no partial message (ST-SUP-RC07-OC12, ST-SUP-RC07-OC15).
grpc-application 2.0.0 no longer takes response mappings.

The generated transport provides checked required and optional timestamp
conversion. Invalid values return no timestamp and a fixed error. A nil
optional input stays absent; a present zero Go time stays present. See
the conversion contract (a retired record, in git history).
