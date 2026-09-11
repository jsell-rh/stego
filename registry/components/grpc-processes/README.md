This component generates RPC contracts and separate executables without API
storage. It requires a JWT verifier and telemetry. Its protobuf compiler,
transport, clients, and process runtime are shared with `grpc-application`.

Declare `proto_files` and `processes`. Do not declare a primary API factory or
watch-event storage. See [RPC processes](../../../specs/grpc-processes.md).
