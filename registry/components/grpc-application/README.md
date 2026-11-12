This component serves the gRPC methods that the declared commands and
queries name (ST-SUP-RC07-OC19). It needs the command and the query
components, and a service with data access. A service with no entity, no
command and no query is refused: use grpc-processes for an RPC process.
It takes the setting proto_files only. A proto file of the command component's proto_files keeps
the Go package of the command contract, so a command takes one request
message over HTTP and gRPC. Each declared method is served by the generated
handler of its command or query, with the same verification, transaction,
permission, audit and idempotency as over HTTP; a method that no declaration
names answers Unimplemented. A get query reads its resource member from a
string field of the request message; a list query reads the fields cursor
and limit, and its response message has the fields items and next. The
service serves gRPC in the api mode only, at STEGO_GRPC_ADDR with the TLS
certificate STEGO_GRPC_TLS_CERT and its key STEGO_GRPC_TLS_KEY.

Version 2.0.0 removes the bridge that bound a hand-written factory to the
GORM store, and its settings factory_package, watch_events, processes and
response_mappings: STEGO no longer generates the GORM store
(ST-SUP-RC20-OC39). The runtime supplies TLS, verified identity, request and
stream limits, and shutdown.
