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
64 streams per connection, 128 connections, and 128 active unary calls. Unary
calls have a ten-second context deadline. Streams have separate capacity: 32
per process and four per verified subject. Handlers must honor cancellation.

A stream stops at verified token expiry or its maximum lifetime. The default
lifetime is five minutes. `STEGO_GRPC_STREAM_TIMEOUT` accepts one second through
30 minutes. `STEGO_GRPC_STREAM_IO_TIMEOUT` bounds each stream I/O operation to
one through ten seconds, with a ten-second default. A blocked I/O operation
closes its TCP connection. Other calls on that connection must reconnect.
Application capacity remains reserved until its handler exits.

Set `watch_events: true` to supply the public `events.Source` as the third
argument to `Register`. This requires the outbox component. The generated
supervisor owns the event source. The application subscribes before it sends
response headers, checks current access for each event, and maps resource data
to its wire contract. Clients wait for the headers, list current state, then
apply events. They must repeat this sequence after any stream failure. The
source does not retain history. See [live events](durable-events.md).

The service supervisor runs gRPC with HTTP and event tasks. Cancellation allows
ten seconds for gRPC calls to drain before forced stop. Unknown application
errors and panics become fixed internal errors. Domain handlers must return
only safe public text in explicit gRPC status errors. Reflection is not enabled.
Production capacity, certificate rotation, health, and
telemetry remain separate runtime work.

A separate Record service tests generated client and server code, optional
fields, authentication on unary and stream methods, TLS trust, input limits,
error isolation, deadlines, concurrent calls, and shutdown. Hypershell tests
compare generated wire descriptors with pinned reference contracts. They then
execute REST and gRPC against one generated process and one database, including
event delivery and restart. These checks do not establish full Hypershell scope
or production readiness.

The generated `grpcapi/client` package supplies unary and server-streaming RPCs. Application
configuration supplies a host and port, trusted CA file, and private bearer-token
file. The client requires TLS 1.3 and checks the server identity. It reads the
token for each call to support replacement of expiring credentials. Token files
must be regular files with no group or other permissions. File sizes are bounded.

Each client permits 32 calls, with a five-second deadline and 64 KiB request and
response limits. Call options cannot raise these limits. Each client also permits
four server streams, independent of unary calls. Each stream has a five-minute
lifetime and the same message limits. The caller must call `Header` or `RecvMsg`
within five seconds to complete the handshake. An idle stream can then remain
open until its lifetime ends. Cancellation, completion, and client closure release
stream capacity. Client-streaming and bidirectional calls fail.
The application must reconnect and list current state after a watch ends.
The application owns `Close`; a managed HTTP application can connect it to the
service supervisor. Application-level retries and resolver service configuration
are disabled. Go gRPC can still retry calls that the server application has not
processed. See the [gRPC retry model](https://grpc.io/docs/guides/retry/) and the
pinned Go implementation. A failed mutation can still have an uncertain outcome;
applications need stable operation IDs and recovery rules.

`transport.PrepareRegistrar` wraps a service registrar with one request
preparation callback. Use the returned registrar for the services that need
preparation. In the generated runtime, the callback runs after verification and
admission. Unary preparation runs after protobuf decoding. Stream preparation
runs once before the stream handler, not once per message. It has at most the
normal ten-second request deadline, even when the stream has a longer lifetime.
The callback must honor cancellation and return safe application errors. An
error stops the handler. Registration copies descriptors and does not change
the shared generated descriptors. Preparation does not supply authentication
when used outside the generated runtime. Its commits are separate from later
domain operations.
