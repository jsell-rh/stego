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
The pinned Gateway image supports separate public and internal certificates,
selected by SNI. Hypershell `25927c2` adds the public certificate policy and a
separate Secret mount. Internal clients retain their Service name and private
CA. Source inspection and focused tests establish this configuration path;
the live public connection and rotation checks remain required. No test may
disable certificate verification to bypass them. See the
[OpenShift route contract](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/ingress_and_load_balancing/routes).

Compiler `8f86550` supplies strict passthrough Route admission checks in
`kubernetes-client` 1.7.0. [Full CI](https://github.com/jsell-rh/stego/actions/runs/34973001056)
passed, including both independent example services and SQL provisioning.
The earlier run at `21ae108` failed a stale test that assumed a fixed generated
file count. The corrected test still checks all output for the withdrawn
admission prototype. Route admission alone does not prove live connectivity.

Compiler `74d9a70` adds shared TLS Secret verification in version 1.8.0.
Hypershell `ace5823` calls this runtime and removes its separate verifier.
The application keeps only certificate creation and installation policy.
Focused generated and application checks pass; repeated generation passes.
[Full compiler CI](https://github.com/jsell-rh/stego/actions/runs/34973403349)
passed. A later regression check found that the standard TLS parser can skip a
private-key block in `tls.crt`. The helper could then return that private block
with the certificate data. Compiler `0953fcc`, version 1.8.1, rejects private
blocks, extra text, and malformed block prefixes. The regression failed before
the correction and passes after it. Hypershell `0bfce85` adopts the correction
and tests it through the Kubernetes client fixture. [Full compiler run
34973613718](https://github.com/jsell-rh/stego/actions/runs/34973613718) passed,
including both independent examples and SQL provisioning. The
[TLS Secret contract](../registry/components/kubernetes-client/tls-secrets.md)
defines trust, ownership, data limits, and error privacy.

The complete browser gate at `59a6d32` passed in 385.91 seconds. Its
[evidence](hypershell-controller-endpoint-browser.json) includes 924 checked
source files, 232 generated files, three matching generation records, access
rules, SQL isolation and repair, restart, namespace replacement, encryption,
account deletion, telemetry, and automatic cleanup. Independent reads found no
test Jobs, Pods, Deployments, or selected fixtures; the shared Lease was free.
This run used an external PostgreSQL container and the internal Service address.
The reviewed connection panel still shows loading placeholders.

The console gate at `25927c2` failed because a fresh asset build differed from
the committed archive. Its frontend inputs match `59a6d32`. The browser pass
therefore applies to the committed archive, not a reproduced asset build. A CI
candidate build and a new application run are required after the archive changes.
The 32-test API attempts at `25927c2` and `0bfce85` did not start application
Jobs because the CI credential had too little time left. They are failures,
not missing passes or evidence of API behavior.

Hypershell `01fa021` fixes the asset drift from a verified CI candidate. Only the
owner patch schema changes in the application bundle: it no longer accepts
`route_address`. Asset names and manifest references change with the bundle.
Both services regenerate without drift. Full CI `34974186524`, API
`34974186030`, and browser `34974186012` remain required for this source.
The [archive evidence](hypershell-console-endpoint-assets.json) records the exact
source and output identities. Commit `17e5d5e` also records the queried PostgreSQL
version and provisioning role in the next browser run; compilation is not live
evidence for that new check.

The next workflow change must join Route creation, public TLS and RPC checks,
and current endpoint publication. Status and address must describe the same
observation. A failed or stale check must not publish a healthy public endpoint.
The existing generated transaction and observation contracts must be considered
before a new common abstraction is added.
