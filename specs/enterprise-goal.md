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
| Full compiler CI | STEGO `8aedc54` | [Run 34998601541](https://github.com/jsell-rh/stego/actions/runs/34998601541) passed controlled network updates, compiler checks with race detection, SQL provisioning, and both example services. The Kubernetes service package passed in 15.869 seconds. Browser runtime adoption remains recorded in the [runtime record](browser-telemetry.md). |
| Core application acceptance | Hypershell `0c2e7fe`, compiler `fe07b0a` | [Run 34994298003](https://github.com/jsell-rh/hypershell-stego/actions/runs/34994298003) passed core acceptance with race detection in 1330.563 seconds. Rendered browser, 229 UI tests, bundle reproduction, regeneration, and image checks passed. The manual workflow skipped CNPG and Sandbox. The main variant branch adopted the checked candidate through `4d1334b`. |
| Public Gateway workflow | Hypershell `842a71c`, compiler `0b0c932` | [Run 34991226917](https://github.com/jsell-rh/hypershell-stego/actions/runs/34991226917) passed the complete rendered workflow in 457.75 seconds with race detection. Public TLS, RPC access and denial, network recovery, certificate rotation, SQL faults, namespace recovery, service accounts, provider logout, and normal deletion passed. Generation hashes matched before and after the test. Automatic cleanup passed with no allocations left for fallback cleanup. See the [complete record](hypershell-public-gateway-complete-20260915.json). |
| Telemetry fixture correction | Hypershell `842a71c` | Nine focused cases passed. The complete public run verified two allocator, two identity, and four workload instances. Every instance supplied metrics and correlated logs and traces. SQL telemetry included cleanup denial and recovery. The internal profile still requires two instances of each worker. |
| Supplied CNPG workflow | Hypershell `ccfa4a9`, compiler `5e9c89d` | [Recorded workflow](hypershell-cnpg-complete.json) passed failover, retained data and identities, and cleanup with an operator-installed server. It does not prove unattended CNPG CI. |
| Unattended CNPG | Hypershell `8e942ac`, compiler `0b0c932` | [Run 34993409789](https://github.com/jsell-rh/hypershell-stego/actions/runs/34993409789) passed the complete rendered application test in 470.24 seconds with race detection. Primary replacement preserved SQL identities, credentials, keys, and data. Automatic cleanup removed runtime, private fixtures, claims, and volumes and released the Lease. See the [complete record](hypershell-cnpg-ci-complete-20260915.json). Earlier failures remain in the [recovery record](hypershell-cnpg-ci-recovery-20260915.json). |
| Allocated IP endpoint bindings | STEGO `5516e48`, kubernetes-service 1.15.0 | [Endpoint evidence](allocated-network-endpoints-20260915.json) covers checked operator bindings, generated runtime updates, and three Kubernetes expression type checks. The first map type error was corrected. Full request admission and traffic enforcement remain unproved. Compiler CI is running. No Hypershell workflow uses this change yet. |
| Allocated namespace policy | STEGO `8aedc54`, kubernetes-service 1.14.0 | [Controlled-update evidence](allocated-network-update-admission-20260915.json) covers the generated runtime and 93 live admission checks: 19 allowed and 74 denied. Approved rule changes, removal of retired rules, stale-version denial, ownership, policy-set protection, and cleanup passed. No Pods ran. Full compiler CI passed. The earlier candidate `101f31d` uses compiler `e394ab5`; its browser, console, image, and regeneration checks passed, and core tests remain active. Neither result proves Gateway traffic isolation. |

The latest verification handles are:

| Check | Source | Handle |
| --- | --- | --- |
| Allocation runtime application candidate | Hypershell `101f31d`, compiler `e394ab5` | [34998079776](https://github.com/jsell-rh/hypershell-stego/actions/runs/34998079776), core tests passed in 1360.499 seconds. The rendered browser workflow passed in 97 seconds. Console tests, bundle reproduction, regeneration, and image checks passed. CNPG and Sandbox skipped. Production isolation remains off. |
| Allocated IP endpoint bindings | STEGO `5516e48` | [35000198875](https://github.com/jsell-rh/stego/actions/runs/35000198875), all compiler checks passed. The separate type check removed its unbound policies and namespace and released the Lease. |
| Controlled allocation network updates | STEGO `8aedc54` | [34998601541](https://github.com/jsell-rh/stego/actions/runs/34998601541), completed successfully; compiler checks, both examples, and SQL provisioning passed. The live admission check passed and released its Lease. |
| Declared allocation network peers | STEGO `e394ab5` | [34997668449](https://github.com/jsell-rh/stego/actions/runs/34997668449), completed successfully; compiler checks, both examples, and SQL provisioning passed. The separate live admission check passed and released its Lease. |
| Browser observable metric correction | STEGO `fe07b0a` | [34993843977](https://github.com/jsell-rh/stego/actions/runs/34993843977), completed successfully |
| Browser runtime adoption | Hypershell `0c2e7fe` | [34994298003](https://github.com/jsell-rh/hypershell-stego/actions/runs/34994298003), completed successfully; the main variant branch adopted the checked candidate through `4d1334b`. See the [rendered record](hypershell-browser-observable-metrics-rendered.json). |
| Public Gateway workflow | Hypershell `842a71c` | [34991226917](https://github.com/jsell-rh/hypershell-stego/actions/runs/34991226917), completed successfully; test resources absent and shared Lease released |
| Core, ordinary browser, console, and images | Hypershell `842a71c` | [34991229447](https://github.com/jsell-rh/hypershell-stego/actions/runs/34991229447), completed successfully |
| Complete unattended CNPG workflow | Hypershell `8e942ac` | [34993409789](https://github.com/jsell-rh/hypershell-stego/actions/runs/34993409789), completed successfully with automatic cleanup and Lease release |

The allocation runtime candidate has active core application CI. The IP endpoint
change has active compiler CI. The earlier network compiler and application
runs are complete.
Before a new live cluster test, verify that the prior
test resources remain absent and the shared Lease is free. Keep one live cluster
test at a time. A timeout or incomplete log is not a terminal result.
The default manual contract workflow skips CNPG and Sandbox. The explicit
`cnpg_only=true` selection runs CNPG and skips ordinary checks. Skips are not passes.

The next required work is:

1. Supply allowed Gateway network paths through STEGO and prove them in the same
   application workflow. Include two Gateways, an unrelated namespace, allowed
   and denied fresh connections, endpoint changes, restart, regeneration, and
   cleanup. The [DNS provider choice](allocated-network-dns.md) remains open.
   The controller's public egress fault test does not prove Gateway isolation.
2. Complete separate Sandbox allocation and its permission and network boundary.
   The current controller rejects Sandbox runtime configuration with the shared
   allocator. The user deferred the live Kata test because no suitable cluster
   is available. This deferral does not establish runtime isolation.
3. Audit C1 through C7 and H1 through H3 against current source and complete
   workflows. Remaining work includes the full Hypershell port, backup and
   restore, supported deployment recovery, complete telemetry coverage, and
   measured capacity. Keep the original requirements active until their full
   evidence exists.

Public TLS uses the operator-selected issuer and router passthrough. Internal
Gateway TLS uses a separate operator-supplied trust file. The
[Route permission record](hypershell-public-route-host.json) retains the bounded
role update and installation identities. The complete public workflow proves its
recorded application scope. It does not establish full production readiness.
