This component generates RPC contracts and separate executables without API
storage. It requires a JWT verifier and telemetry. Its protobuf compiler,
transport, clients, and process runtime are shared with `grpc-application`.

Declare `proto_files` and `processes`. Do not declare a primary API factory or
watch-event storage. See [RPC processes](../../../specs/grpc-processes.md).

The generated transport provides checked required and optional timestamp
conversion. Invalid values return no timestamp and a fixed error. A nil
optional input stays absent; a present zero Go time stays present. See
[the conversion contract](../../../specs/checked-timestamp-conversion.md).
