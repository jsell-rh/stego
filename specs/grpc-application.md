The `grpc-application` component compiles protobuf contracts and connects domain
operations to the generated service runtime. It does not infer wire fields from
database entities.

`proto_files` lists each project-relative `path` and its protobuf `import_path`.
The compiler reads these files once and checks their snapshots before apply.
Inputs must be regular files outside generated output. File count and byte limits
apply. Imports resolve from this list or the protobuf standard descriptors. They
cannot read other files or fetch remote schemas. Invalid contracts fail before
output changes. Recovery completes saved output and preserves later source edits.

The current compiler accepts proto3 contracts. It uses protocompile v0.14.1,
the protobuf Go generator v1.36.11, and the gRPC Go generator v1.6.1. The gRPC
generator source is included with its license and source revision. These versions
are fixed in STEGO. Generation requires no external `protoc` binary or plugins.
Generated Go packages follow protobuf import directories under `grpcapi/pb`.
Only Go import options change; wire descriptors remain the application contract.
See the [protobuf generator](https://protobuf.dev/reference/go/go-generated/)
and [gRPC generator](https://grpc.io/docs/languages/go/generated-code/) contracts.

`factory_package` identifies a domain package outside generated output. Its
`Register(grpc.ServiceRegistrar, Repository) error` function registers domain
handlers. The repository supplies the public storage and transaction interfaces.
The generated bridge owns runtime assembly. The domain package owns field
mapping, business rules, and public error mapping.

The runtime requires `STEGO_GRPC_TLS_CERT` and `STEGO_GRPC_TLS_KEY`. TLS 1.3 is
the minimum. `STEGO_GRPC_ADDR` defaults to `127.0.0.1:9090`. All registered unary
and stream methods use the JWT verifier. Duplicate bearer values fail. Limits
include 64 KiB request messages, 8 MiB response messages, 32 KiB header lists,
64 streams per connection, and 128 active application calls. Each call has a
ten-second context deadline. Handlers must honor cancellation. Stream lifetimes
also have this bound; long-lived watch support remains required work.

The service supervisor runs gRPC with HTTP and event tasks. Cancellation allows
ten seconds for gRPC calls to drain before forced stop. Unknown application
errors and panics become fixed internal errors. Domain handlers must return
only safe public text in explicit gRPC status errors. Reflection is not enabled.
Total connections, production capacity, certificate rotation, health, and
telemetry remain separate runtime work.

A separate Record service tests generated client and server code, optional
fields, authentication on unary and stream methods, TLS trust, input limits,
error isolation, deadlines, concurrent calls, and shutdown. Hypershell tests
compare generated wire descriptors with pinned reference contracts. They then
execute REST and gRPC against one generated process and one database, including
event delivery and restart. These checks do not establish full Hypershell scope
or production readiness.
