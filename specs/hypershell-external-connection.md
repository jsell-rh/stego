# External Gateway connection

The supplied-server browser workflow proves authenticated Gateway RPC inside
the cluster. Its connection panel has no external endpoint. That result does
not prove a user can connect through the public route.

The next application gate must publish an address only after the assigned
controller has observed the workload and route. It must use verified TLS and
retain access checks through the router. The gate must prove the following:

- Create a Gateway through the console and wait for its assigned controller.
- Commit a controller observation with the current resource revision.
- Show a usable connection command only when the Gateway is ready.
- Use the published hostname for a real TLS and authenticated Gateway RPC call.
- Reject an unauthenticated call, a foreign audience, and an ungranted user.
- Lose readiness when exposure fails, then recover without replacing SQL data.
- Retain the endpoint and access behavior after worker or API replacement.
- Remove the route on deletion and preserve other Gateways.
- Repeat generation and verify cleanup under the shared CI Lease.

STEGO supplies Kubernetes resource access, ownership checks, availability,
network-policy rendering, bounded clients, and telemetry. Hypershell supplies
Gateway assignment, hostname policy, API observation mapping, and OIDC rules.
The application must not contain a second common controller implementation.

The workload controller uses the shared availability check from
`kubernetes-client` 1.6.0. Small generated
runtime tests cover stale generations, incomplete rollouts, owner mismatch,
missing status, exact integer handling, and malformed observations. Compiler
CI passed at `5e9c89d`. Hypershell adopted the helper at `3f188b2`, whose API
gate passed all 30 required tests and repeated generation. The complete supplied
PostgreSQL browser run also passed, including namespace recovery and automated
cleanup. It still uses the internal Service endpoint. See the
[verified evidence](hypershell-workload-availability.json).

Two application decisions are pending: whether the first public route uses TLS
passthrough, and whether `route_address` becomes a controller-owned field.
The current API permits owner writes to that address. Do not claim the address
is controller-verified while that behavior remains.

The reference defaults to passthrough. That mode leaves TLS at the Gateway and
requires clients to trust its issuer. A public certificate authority generally
cannot issue the internal Service DNS names used by the current workload.
Separate public and internal certificate handling remains part of the design;
no test may disable certificate verification to bypass it. See the
[OpenShift route contract](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/ingress_and_load_balancing/routes).
