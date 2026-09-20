# Workload construction boundary

The main Gateway workload extraction is complete in accepted Hypershell source
`caffd04`, recorded on main `82f71a26`. STEGO constructs Deployments and Services
from typed declarations. Hypershell selects OpenShell settings and dependencies.
The later source `e0f3d7bb9ae93666e9a1381137a57df57a0416f8` retains that boundary.
Its worker configuration changes remain under qualification.

This review checks the remaining assembly code at `e0f3d7b`. A source review
does not establish complete application parity or production capacity.

| Mechanism | Current owner | Remaining work |
| --- | --- | --- |
| Gateway Deployment and Service construction | STEGO `kubernetes-workload` component | Retain validation and tests for field removal, security repair, API serialization, and convergence. |
| Security settings, probes, mounts, limits, and content digest | STEGO `workload.Build` | Keep the fixed security profile. Hypershell declares health paths, resource amounts, image, files, and verified dependencies. |
| ConfigMap and Secret content conversion | Hypershell `gatewayworkload.workloadDependency` | The bounded map conversion and strict base64 decoding can become a common typed adapter. Dependency selection and verification must remain explicit. |
| Gateway console Deployment and Service construction | Separately generated STEGO browser module | Hypershell still edits a Pod annotation to supply the configuration digest. A typed generation option can remove this map traversal. |
| Console credential content digest | STEGO `kubernetes.OpaqueSecretSetDigest` | Preserve its namespace and owner checks. Do not add another application hash implementation. |
| Console Service separation, placement, and dependency checks | Hypershell | Retain the checks that prevent console Pods from matching the Gateway Service and prevent use of another Gateway's credentials. |
| OpenShell Sandbox Pod setup | Hypershell upstream integration | Preserve the user's selected upstream behavior. The restricted Deployment profile does not support its init containers, user settings, or capabilities. |
| Environment parsing and connection setup | Generated configuration and provider components, with remaining application setup | Complete the current live checks. Optional console connection settings still use manual environment reads. |
| REST and gRPC mapping | Generated transport helpers plus application adapters | Follow the separate transport audit. Authorization and protocol-specific behavior remain explicit application rules. |

The source scan found direct security-context construction in
`internal/gatewayworkload/sandbox.go`. The ordinary Gateway already calls the
common builder in `internal/gatewayworkload/resources.go`. The console calls
generated `deployment.Resources` in `internal/gatewayworkload/console.go`.
These three paths have different contracts. A raw Pod escape hatch in the
restricted builder would weaken its guarantees.

## Next extraction boundary

Complete the current configuration adoption before another runtime change.
Then remove the remaining dependency conversion and console annotation assembly
through typed common interfaces. The caller supplies authorized dependencies;
the common implementation checks their representation and constructs the output.
It must not infer Gateway ownership, grant rules, placement, release selection,
OpenShell configuration, or management UI behavior.

Require bounded inputs, stable content hashes, private errors, no partial output,
and no Secret contents in a Deployment or Service. Keep certificate and owner
verification before construction. Test changed contents, metadata-only changes,
missing and extra dependencies, invalid encoding, and repeated reconciliation.
Use a second application without Gateway types to test the common interface.
The full Hypershell workflow must still pass through REST, gRPC, event delivery,
restart, and regeneration before the consumer change is accepted.

## Evidence limits

The accepted workload source passed 1,162 core cases, the live service workflow,
and all 11 required live browser tests. The admitted Gateway Pods had the
declared image, probes, mounts, limits, security settings, and content digest.
The immutable [consumer record](https://github.com/jsell-rh/hypershell-stego/blob/82f71a26e411840648c331ac32f60a2db888210b/acceptance/workload-construction.md)
contains the source and run identities. This accepts the extraction; it does
not prove a 100-Gateway installation or close the enterprise requirements.

The newer configuration candidate has separate passing hosted and ancestor
workflow results. Its current live count test failed at allocator construction
because the fixture omitted required network endpoints. No count behavior
passed in that run. Independent cleanup found the test resources absent, the
shared Lease free, and all 32 standing resources unchanged. The fixture also
used a positional role-binding name that no longer selected the count role.
The corrected setup passed its hosted checks and then the live count workflow
in 93.08 seconds. Review checked the unchanged runtime files, generated hashes,
and complete cleanup. See the [count evidence](runtime-control-count-evidence.json).
The generated allocator retains its endpoint and identity checks.

The later browser run failed during a Kubernetes API TLS handshake after API
restart. Independent cleanup passed. Its cause remains unknown; current-source
browser acceptance is still open. See the
[failure evidence](runtime-control-browser-failure-evidence.json).

The common renderer candidate `c22cb19` adds a typed configuration digest option.
It validates the digest and sets the Pod template annotation. Focused checks
passed 69 generated cases, including independent API, worker, and RPC examples.
The full compiler check passed 37 packages and both generated examples. The
change is merged into STEGO. Its exact main source `5c4afa11` passed the same
focused and full checks. Its signed compiler and build record passed independent
verification. The immutable release is published; a fresh installation matched
all five verified files. Hypershell regeneration is queued with this release.
Consumer adoption and complete workflow checks remain required. See the
[candidate evidence](deployment-configuration-evidence.json) and
[main release evidence](deployment-configuration-main-evidence.json).
The change does not move Gateway policy into STEGO.

See the [enterprise goal](enterprise-goal.md) and the
[transport mapping audit](transport-mapping-audit-20260920.md) for the remaining
requirements and their acceptance conditions.
