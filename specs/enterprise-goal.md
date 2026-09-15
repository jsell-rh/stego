The active goal is to make STEGO a reliable compiler for enterprise services
and deliver a fully STEGO-based Hypershell. The user authorized this work on
2026-09-08. The [original repository assessment](repository-assessment.md)
defines the initial defect list. The full scope remains active; a passing build
or one passing workflow does not establish completion.

STEGO supplies common runtime, security, telemetry, controller, reconciliation,
storage, and deployment mechanisms. Hypershell supplies its domain behavior.
Keep common mechanisms out of application fills. Verify each common capability
with an independent service and the complete application workflow. The output
must not depend on rh-trex-ai. Security, correctness, and performance claims
require evidence that covers the claimed behavior.

The reference checkout is `/home/jsell/code/hypershell`; keep it unchanged.
The application test bed is `/home/jsell/code/hypershell-stego`, with remote
`https://github.com/jsell-rh/hypershell-stego`.

The original completion requirements remain unchanged:

| ID | Requirement | Acceptance evidence | State |
| --- | --- | --- | --- |
| C1 | Strict compiler input and one semantic validation stage | Unknown fields, invalid constraints, duplicate keys, unsupported capabilities, and invalid paths fail before output changes | Active |
| C2 | Complete project and fill workflow | Init, apply, fill create, test, build, repeated apply, and drift pass in a fresh directory | Active |
| C3 | Reproducible and recoverable generation | Compiler and input identities, stable output, dependency ownership, state format, interrupted-write recovery, and concurrent apply tests | Active |
| C4 | Secure authentication and authorization | Signature, issuer, audience, expiry, key rotation, scope isolation, and denied request tests | Active |
| C5 | Correct storage and event behavior | PostgreSQL integration, explicit migrations, concurrency, transactional writes, durable event delivery, and failure tests | Active |
| C6 | Production runtime support | Health, readiness, tracing, metrics, bounded requests, deadlines, shutdown, and resource limit tests | Active |
| C7 | Explicit generator contracts | Typed wiring, supported capability checks, typed business extension contracts, and compatibility tests | Active |
| H1 | Hypershell compatibility baseline | Inventory and executable checks for REST, gRPC, RBAC, watch, SDK, CLI, UI, and deployment contracts | Active |
| H2 | STEGO Hypershell implementation | Clean generation and tests without rh-trex-ai; reviewed domain code remains outside generated output | Active |
| H3 | System verification | Service integration and local deployment checks, security tests, race tests, measured performance, and regeneration checks in CI | Active |

The application acceptance gate remains Gateway creation and retrieval with
the required IDs and API shapes, an atomic Gateway and owner grant, filtered
lists and denied requests, generated event delivery, and REST, gRPC, restart,
and regeneration checks. Use failures in complete application workflows to
select further infrastructure work. Do not replace application evidence with
reference contracts or compatibility checks alone.

The user approved the controller-local database release. The active API has no
`database_id`, database catalog, or registration API. It rejects retired request
fields, including empty and null values. Installation supplies co-located
external PostgreSQL or CNPG. Controllers create separate Gateway databases and
logins through the common SQL runtime. Legacy or unknown schemas must fail
before writes. This release requires fresh installation or explicit operator
teardown and recreation; deployment must not perform automatic teardown. See
[the database contract](database-provisioning.md) and
[the application contract](https://github.com/jsell-rh/hypershell-stego/blob/codex/namespace-allocation-20260912/acceptance/controller-local-database.md).

The other user decisions remain in force: typed Go SDK methods, verified
service-database TLS with an explicit loopback test exception, a separate Go
browser backend, confirmed console and identity-provider logout, an operator-set
default release, shared-cluster namespace isolation, and no deployment-backed
databases. Keep these decisions in the acceptance scope. The
[complete historical record](enterprise-history.md) preserves earlier decisions,
changes, failed attempts, and results without changing their original status.

Current evidence was checked on 2026-09-15. Each result applies only to its
recorded source and scope. A failed workflow can retain evidence for completed
checks, but it cannot establish a complete application pass.

| Check | Source | Verified result and limits |
| --- | --- | --- |
| Gateway workflow with address replacement | Hypershell `93f590b`, compiler `b0bd9a4` | The [complete public workflow](hypershell-endpoint-change-live-20260915.json) passed in 536.80 seconds with race detection. All 54 fresh-connection checks passed before address replacement, after replacement, and after namespace recovery. The old address was denied and the replacement was allowed. All 238 generation hashes stayed equal. Ten worker instances supplied metrics and correlated logs and traces. Public TLS, certificate rotation, SQL faults, access rules, encrypted recovery, account deletion, and logout passed. Automatic cleanup and independent absence checks passed. Raw evidence is in persistent storage. External DNS and RDS failover remain unproved. |
| Full compiler CI | STEGO `b0bd9a4` | [Run 35012238080](https://github.com/jsell-rh/stego/actions/runs/35012238080) passed compiler race tests, both generated examples, dependency scans, and SQL provisioning. The patched API and console also passed vulnerability scans. [The API cluster gate](hypershell-api-security-ci-20260915.json) passed all 32 required tests. Ordinary Hypershell CI passed all four application jobs; its CNPG job declined an insufficient credential before execution. |
| Core application acceptance | Hypershell `0c2e7fe`, compiler `fe07b0a` | [Run 34994298003](https://github.com/jsell-rh/hypershell-stego/actions/runs/34994298003) passed core acceptance with race detection in 1330.563 seconds. Rendered browser, 229 UI tests, bundle reproduction, regeneration, and image checks passed. The manual workflow skipped CNPG and Sandbox. The main variant branch adopted the checked candidate through `4d1334b`. |
| Public Gateway workflow | Hypershell `842a71c`, compiler `0b0c932` | [Run 34991226917](https://github.com/jsell-rh/hypershell-stego/actions/runs/34991226917) passed the complete rendered workflow in 457.75 seconds with race detection. Public TLS, RPC access and denial, network recovery, certificate rotation, SQL faults, namespace recovery, service accounts, provider logout, and normal deletion passed. Generation hashes matched before and after the test. Automatic cleanup passed with no allocations left for fallback cleanup. See the [complete record](hypershell-public-gateway-complete-20260915.json). |
| Telemetry fixture correction | Hypershell `842a71c` | Nine focused cases passed. The complete public run verified two allocator, two identity, and four workload instances. Every instance supplied metrics and correlated logs and traces. SQL telemetry included cleanup denial and recovery. The internal profile still requires two instances of each worker. |
| Supplied CNPG workflow | Hypershell `ccfa4a9`, compiler `5e9c89d` | [Recorded workflow](hypershell-cnpg-complete.json) passed failover, retained data and identities, and cleanup with an operator-installed server. It does not prove unattended CNPG CI. |
| Unattended CNPG | Hypershell `8e942ac`, compiler `0b0c932` | [Run 34993409789](https://github.com/jsell-rh/hypershell-stego/actions/runs/34993409789) passed the complete rendered application test in 470.24 seconds with race detection. Primary replacement preserved SQL identities, credentials, keys, and data. Automatic cleanup removed runtime, private fixtures, claims, and volumes and released the Lease. See the [complete record](hypershell-cnpg-ci-complete-20260915.json). Earlier failures remain in the [recovery record](hypershell-cnpg-ci-recovery-20260915.json). |
| Allocated IP endpoint bindings | STEGO `5516e48`, kubernetes-service 1.15.0 | [Endpoint evidence](allocated-network-endpoints-20260915.json) covers checked operator bindings, generated runtime updates, and three Kubernetes expression type checks. The first map type error was corrected. All 168 request admission checks passed. The separate application row records traffic checks; this admission result alone does not prove traffic enforcement. Full compiler CI passed. Hypershell candidate `5043608` uses this change; its ordinary application CI passed. Allocation isolation was off in that source. |
| Allocated namespace policy | STEGO `8aedc54`, kubernetes-service 1.14.0 | [Controlled-update evidence](allocated-network-update-admission-20260915.json) covers the generated runtime and 93 live admission checks: 19 allowed and 74 denied. Approved rule changes, removal of retired rules, stale-version denial, ownership, policy-set protection, and cleanup passed. No Pods ran. Full compiler CI passed. The earlier candidate `101f31d` uses compiler `e394ab5`; its browser, console, image, and regeneration checks passed, and core tests passed. Neither result proves Gateway traffic isolation. |

The latest verification handles are:

| Check | Source | Handle |
| --- | --- | --- |
| Endpoint runtime application candidate | Hypershell `5043608`, compiler `5516e48` | [35001051267](https://github.com/jsell-rh/hypershell-stego/actions/runs/35001051267), ordinary application CI passed, including core acceptance in 1215.371 seconds. Regeneration passed in both modules. Allocation isolation was off in this source. |
| Allocation runtime application candidate | Hypershell `101f31d`, compiler `e394ab5` | [34998079776](https://github.com/jsell-rh/hypershell-stego/actions/runs/34998079776), core tests passed in 1360.499 seconds. The rendered browser workflow passed in 97 seconds. Console tests, bundle reproduction, regeneration, and image checks passed. CNPG and Sandbox skipped. Production isolation remains off. |
| Allocated IP endpoint bindings | STEGO `5516e48` | [35000198875](https://github.com/jsell-rh/stego/actions/runs/35000198875), all compiler checks passed. The separate type check removed its unbound policies and namespace and released the Lease. |
| Controlled allocation network updates | STEGO `8aedc54` | [34998601541](https://github.com/jsell-rh/stego/actions/runs/34998601541), completed successfully; compiler checks, both examples, and SQL provisioning passed. The live admission check passed and released its Lease. |
| Declared allocation network peers | STEGO `e394ab5` | [34997668449](https://github.com/jsell-rh/stego/actions/runs/34997668449), completed successfully; compiler checks, both examples, and SQL provisioning passed. The separate live admission check passed and released its Lease. |
| Browser observable metric correction | STEGO `fe07b0a` | [34993843977](https://github.com/jsell-rh/stego/actions/runs/34993843977), completed successfully |
| Browser runtime adoption | Hypershell `0c2e7fe` | [34994298003](https://github.com/jsell-rh/hypershell-stego/actions/runs/34994298003), completed successfully; the main variant branch adopted the checked candidate through `4d1334b`. See the [rendered record](hypershell-browser-observable-metrics-rendered.json). |
| Public Gateway workflow | Hypershell `842a71c` | [34991226917](https://github.com/jsell-rh/hypershell-stego/actions/runs/34991226917), completed successfully; test resources absent and shared Lease released |
| Core, ordinary browser, console, and images | Hypershell `842a71c` | [34991229447](https://github.com/jsell-rh/hypershell-stego/actions/runs/34991229447), completed successfully |
| Complete unattended CNPG workflow | Hypershell `8e942ac` | [34993409789](https://github.com/jsell-rh/hypershell-stego/actions/runs/34993409789), completed successfully with automatic cleanup and Lease release |

Candidate `56a5998` passed the complete public Gateway workflow with the
generated namespace policies enabled. The test passed in 492.39 seconds under
the race detector. Both Gateway namespaces passed allowed and denied fresh
connections before and after recovery, for 24 checks. Normal deletion removed
all Gateway SQL and state. Generation hashes matched, and wrapper cleanup
removed its namespace and owned resources before releasing the Lease.
See the [workflow record](hypershell-gateway-network-workflow-20260915.json).
Its ordinary CI run
[35003173222](https://github.com/jsell-rh/hypershell-stego/actions/runs/35003173222)
passed core acceptance in 1236.192 seconds, plus browser, console, and image
checks. CNPG and Sandbox skipped.
The earlier setup and fixture failures remain in the record.

Candidate `ee3d41d` passed the full public workflow in 512.91 seconds with race
detection. Both Gateways passed all 28 connection checks before and after
recovery, including denial of a reachable listener in an unrelated namespace.
The listener had no ingress NetworkPolicy, token, or Secret mount. Generation
hashes matched. Both test namespaces and owned resources were removed, the
wrapper exited successfully, and the Lease was released. Ordinary CI
[35005977361](https://github.com/jsell-rh/hypershell-stego/actions/runs/35005977361)
also passed, with core acceptance in 1357.772 seconds. Browser, console, and
image checks passed; CNPG and Sandbox skipped. Restricted CI still
has no unrelated listener and records that the extra check did not run there.

CNPG source `6062269` corrects the frozen-source verifier. It accepts the exact
declared database peer and fixed inspection roles together. Seventeen focused
checks passed, followed by generation and complete frozen-source verification.
The earlier prepared source `7e23873` failed that final source check before any
CNPG run. The [CI installation update](hypershell-network-ci-update-20260915.json)
changed two generated admission policies, six Roles, and the selected CNPG
receiver rule. Admission checks passed before permission changes. All resource
identities and specifications, the new immutable record, and Lease release
were verified. The first restricted run stopped before browser setup because the local launch
omitted the required Gateway CA input. The outer cleanup also depended on files
that the browser had not created. [Manual recovery](hypershell-cnpg-network-recovery-20260915.json)
removed all runtime, claims, volumes, and private fixtures and released the
Lease. Source `38a1d76` checks and copies the CA before cluster access and
prepares an independent allocation client before runtime creation. Twenty-two
focused checks passed. Generation and full source verification passed. The
corrected run reached the application and failed at the telemetry network probe
after 182.77 seconds. The static receiver policy lacked the declared TCP port
19093 for Gateway namespaces. [The failure record](hypershell-cnpg-receiver-failure-20260915.json)
retains the successful checks before that failure. Automatic cleanup removed
four allocations, database runtime, claims, volumes, and the private fixture.
Independent reads confirmed absence, and the Lease was released.

The [receiver correction](hypershell-ci-receiver-update-20260915.json) added only
the missing port to the existing namespace rule. The policy UID stayed the
same. Source `2d1d8ea` checks the installed fixture policies before a test Job
starts. Seven focused CI checks passed. The restricted identity rejected the
saved old receiver policy and accepted the corrected installed policies.
Generation and full source verification passed. The
[complete restricted CNPG workflow](hypershell-cnpg-network-complete-20260915.json)
then passed in 466.65 seconds. All 24 network checks passed, and all 238
generation hashes stayed equal. Database primary replacement, namespace
recovery, access checks, worker telemetry, Gateway deletion, and logout passed.
Automatic cleanup removed runtime, claims, volumes, and the private fixture.
The wrapper exited with status 0 and released the Lease. Later operator reads
confirmed resource absence; the next API CI job then held the Lease.

CI identified GO-2026-6443 in gRPC v1.83.1 during that frozen run.
[Compiler b0bd9a4](grpc-security-update-20260915.md) requires v1.83.2 and checks
rejection of requests with missing host headers. Candidate `1df3d36` uses the
patched compiler in both Go modules. Full compiler CI passed, as did the
consumer API and console vulnerability scans. Full application CI is pending. The earlier
CNPG functional pass does not qualify the old dependency for release.

The [address-change fixture preparation](hypershell-endpoint-change-preparation-20260915.json)
passed generation, drift, and 19 focused checks at Hypershell `a39c81f`.
It adds one test endpoint name to the Gateway allocation profile. The checked
operator plan replaces one address in the generated admission variable. All
other cluster fields remain equal. The listener fixture has two separate Pod
addresses and a third unrelated listener. Source `aac4baf` connects those inputs
to the operator and generated workers. It checks the policy UID, version, and
original specification before the update, confirms admission type checking,
and restarts both workers with the replacement binding. It requires traffic
records and telemetry from each new instance. Fifty Python checks, 25 shell
checks, and selected Go checks passed. The complete live address-change test
[failed after a workstation restart](hypershell-endpoint-change-live-20260915.json).
Its first attempt stopped on a read-only TLS handshake timeout before namespace
creation. The second attempt passed all 18 initial network checks, then failed
because the operator did not confirm the address transition. The policy kept
the original address at generation 1. The restart removed the local temporary
source and journal. Evidence was recovered from the existing Job into persistent
storage. Manual cleanup removed the Job, all six namespaces, and 22 owned
cluster resources, then released the Lease. The full address-change and recovery
stages remain required. Future frozen sources and journals must use persistent
storage. The cause of the workstation restart is not established. The next run at
`93f590b` used persistent source, journal, trust, and result files and the same
public Gateway inputs. It passed the complete workflow in 536.80 seconds. All
54 network checks passed on the first attempt. The policy kept its UID and
advanced to generation 2. All 238 generation hashes stayed equal. Ten worker
instances exported all required signals. The wrapper exited with status 0;
automatic cleanup removed the fixture and allocations. Independent reads
confirmed resource absence and a free Lease.


This result closes the fixed-address replacement gate. External database DNS
behavior remains unproved and keeps the full network gate open. The main variant branch still uses the earlier checked candidate.
Before a new live cluster test, verify that the prior
test resources remain absent and the shared Lease is free. Keep one live cluster
test at a time. A timeout or incomplete log is not a terminal result.
The default manual contract workflow skips CNPG and Sandbox. The explicit
`cnpg_only=true` selection runs CNPG and skips ordinary checks. Skips are not passes.

The next required work is:

1. Supply allowed Gateway network paths through STEGO and prove them in the same
   application workflow. Include two Gateways, an unrelated namespace, allowed
   and denied fresh connections, endpoint changes, restart, regeneration, and
   cleanup. The user approved [supported DNS-aware providers](allocated-network-dns.md).
   Keep declarations independent of the provider, reject unsupported
   configurations, and exclude Technology Preview features.
   Fixed-address Gateway isolation and replacement passed the full workflow.
   Native DNS enforcement and failure behavior still need qualification.
2. Complete separate Sandbox allocation and its permission and network boundary.
   The current controller rejects Sandbox runtime configuration with the shared
   allocator. The user deferred the live Kata test because no suitable cluster
   is available. This deferral does not establish runtime isolation.
3. Audit C1 through C7 and H1 through H3 against current source and complete
   workflows. Remaining work includes the full Hypershell port, the separate per-Gateway console, backup and
   restore, supported deployment recovery, complete telemetry coverage, and
   measured capacity. Keep the original requirements active until their full
   evidence exists.

Public TLS uses the operator-selected issuer and router passthrough. Internal
Gateway TLS uses a separate operator-supplied trust file. The
[Route permission record](hypershell-public-route-host.json) retains the bounded
role update and installation identities. The complete public workflow proves its
recorded application scope. It does not establish full production readiness.

A [source review](https://github.com/jsell-rh/hypershell-stego/blob/codex/approved-network-peers-20260915/acceptance/per-gateway-console-gap-20260915.md) found a separate per-Gateway console gap. The API and UI retain
`console_address`, but the current workload and identity controllers do not
create that console or its identity client. The passing management-console
workflow does not close this H1, H2, and H3 requirement.
