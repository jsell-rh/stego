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

The later complete browser run at `10a0827` also passed viewer recovery and live
account cleanup through the actual Gateway. Its access checks still use the
internal Service endpoint. The rendered connection panel still has no usable
public command. See the [viewer and account evidence](hypershell-viewer-recovery.json).

On 2026-09-15, the user selected TLS passthrough with an operator-selected
certificate issuer. The user also made `route_address` controller-owned.
The assigned controller must publish a verified address. Gateway owner writes
must be rejected, including writes that clear the address. The field remains
in responses and controller observations.

Hypershell `52d37e9` removes owner address inputs from REST, the generated REST
SDKs, and CLI apply. The gRPC field remains for controller observations. Writes
require an exact `observe.endpoint` grant for the stored cluster and the current
revision. The generated `endpoint` observation group hides stale addresses after
a desired generation change. It uses the existing compiler observation runtime.

Focused request-decoder and policy checks passed. The full acceptance package
compiled, and repeated generation produced no changes. New transport checks
cover owner rejection, mixed fields, stale writes, clearing, placement changes,
and grant removal after restart. Their CI results remain required. The public
route controller and complete connection check remain incomplete. Do not claim
a verified public endpoint from this API change alone.

The reference defaults to passthrough. That mode leaves TLS at the Gateway and
requires clients to trust its issuer. A public certificate authority generally
cannot issue the internal Service DNS names used by the current workload.
Separate public and internal certificate handling remains part of the design;
no test may disable certificate verification to bypass it. See the
[OpenShift route contract](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/ingress_and_load_balancing/routes).
