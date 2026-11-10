This component generates the commands of a service on the command runtime
(stego.dev/runtime/command). It generates only what changes with the
declaration. For each declared command it generates:

- the command slot contract in the package commands, which the fill imports:
  the fill type, which takes the decoded request message and one command
  handle only, the command handle, the typed refusals, and the repository
  declarations of the entities that the command writes;
- the Go code of the request and the response message of the command, from
  the proto files of the setting proto_files, with the names
  <Command>Request and <Command>Response, such as ReserveAndChargeRequest;
- the command declaration, with the entities it writes, its effect
  destinations, its relations, its retry on conflict, its lock timeout, its
  typed probe, its permission and its resource;
- one HTTP handler at the command's route and one gRPC handler, which call
  the one fill of the command through Run, and the binding of each command to
  its fill folder.

The handlers verify the identity, take the tenant from the verified claim,
require the Idempotency-Key header, and answer a deny with 403, a declared
refusal with its status and code, a conflict with 409 and the code conflict
(ABORTED over gRPC), and any other error with 500 and the code internal. They
write the audit row of a denied or a failed command in its own short
transaction.

The stage is alpha. The service needs pooled tenancy with the tenant column
tenant_id and authorization with subject_from_identity. The main wiring of the
command runtime and of the gRPC methods (ST-SUP-RC07-OC19) is not generated
yet.
