# Absent gRPC stream headers

The real Hypershell Gateway gate exposed a generated-client defect. The database
controller stopped at startup because the watch did not confirm deletion
support. The test then failed while it waited for the database to become ready.
The original RPC status was not recorded, so that run does not identify which
server error preceded the controller's capability error.

Source inspection found that the generated client copied every header map.
The gRPC client returns nil headers when a stream ends before headers. Its
caller receives the terminal status through Recv. However, metadata.MD.Copy
changes nil into an empty, non-nil map. The controller checks for nil before it
receives that status. The copy therefore changed protocol behavior and could
turn a retryable RPC error into a terminal capability error.

The existing application test used a raw gRPC client. It proved the controller
check in isolation but did not test STEGO's client wrapper. A new compiler
regression uses both clients against the generated TLS server. For clean
completion and Aborted, Unavailable, PermissionDenied, Unauthenticated,
InvalidArgument, and Unimplemented, it checks repeated Header calls followed
by Recv. The raw client preserved nil. The generated client returned an empty
map in every case. The baseline failed in 24.596 seconds.

The first draft of that test supplied authorization metadata to the generated
client in addition to its token file. The server correctly rejected duplicate
authorization. The final baseline uses the raw client's authorized context and
the generated client's configured token file separately. Its failures are the
header-copy defect, not that invalid test setup.

The grpc-application component is now version 1.6.1. The generated Header method
returns nil before it attempts a copy. Present headers still use a deep copy.
The regression changes a returned header value and adds a map entry, then checks
that another Header call returns the original values. It also checks that Header
does not consume the response message.

This change preserves the underlying gRPC contract. It does not accept a missing
or incorrect application capability, retry permission failures, or change retry
delays. Hypershell still owns its required capability names and error policy.
The shared client behavior belongs in STEGO.

The full STEGO race suite and `go vet ./...` passed after the fix. Both generated
server variants ran the regression. The original Kubernetes run completed with
a failed deletion-before-startup test and a passing complete Gateway workflow
(181.07 seconds). The gate as a whole failed in 339.638 seconds; it is not
recorded as a pass.
