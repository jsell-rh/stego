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
| Full compiler CI | STEGO `d00bdc3` | [Run 34985081259](https://github.com/jsell-rh/stego/actions/runs/34985081259) passed. It includes compiler checks, SQL provisioning, and both example services. Later STEGO commits change documentation only. |
| Core application acceptance | Hypershell `3a50db7`, compiler `0b0c932` | [Run 34985981930](https://github.com/jsell-rh/hypershell-stego/actions/runs/34985981930) passed core acceptance with race detection, ordinary browser, console, and image checks. The overall run failed because of CNPG. Sandbox was skipped. See the [core record](https://github.com/jsell-rh/hypershell-stego/blob/codex/namespace-allocation-20260912/acceptance/gateway-core-ci-20260915.json). |
| Public Gateway checks | Hypershell `cc8e545`, compiler `0b0c932` | [Run 34989405950](https://github.com/jsell-rh/hypershell-stego/actions/runs/34989405950) passed public TLS, RPC access and denial, network recovery, certificate rotation, SQL faults, and namespace recovery. The full workflow failed because the telemetry fixture rejected extra expected worker instances. Cleanup passed. See the [progress record](hypershell-public-gateway-progress-20260915.json). |
| Telemetry fixture correction | Hypershell `842a71c` | Nine focused cases passed. The public profile requires two allocator, two identity, and four workload instances. The internal profile requires two of each. Every instance still needs metrics and correlated logs and traces. Full runtime verification remains required. |
| Supplied CNPG workflow | Hypershell `ccfa4a9`, compiler `5e9c89d` | [Recorded workflow](hypershell-cnpg-complete.json) passed failover, retained data and identities, and cleanup with an operator-installed server. It does not prove unattended CNPG CI. |
| Unattended CNPG | Hypershell `3a50db7` | [Recovery record](hypershell-cnpg-ci-recovery-20260915.json) records a failed application startup, cleanup defects, source corrections, and verified manual cleanup. No complete unattended workflow passed. |
| Allocated namespace policy | STEGO kubernetes-service 1.12.0 | [Policy-set evidence](hypershell-network-policy-set.json) covers generated deny-all behavior and 59 live admission checks. No Pods ran in that admission test. Allowed traffic and CNI enforcement remain unproved. Hypershell has not enabled the policy. |

The current verification handles are:

| Check | Source | Handle |
| --- | --- | --- |
| Public Gateway workflow | Hypershell `842a71c` | [34991226917](https://github.com/jsell-rh/hypershell-stego/actions/runs/34991226917), active when checked |
| Core, ordinary browser, console, and images | Hypershell `842a71c` | [34991229447](https://github.com/jsell-rh/hypershell-stego/actions/runs/34991229447), queued behind the earlier core run when checked |
| Earlier core run | Hypershell `cc8e545` | [34989401887](https://github.com/jsell-rh/hypershell-stego/actions/runs/34989401887), core active; ordinary browser, console, and images passed |

Poll these handles before taking further action. A timeout or incomplete log is
not a terminal result. Inspect the test Job and verify cleanup before a new live
cluster test. Keep one live cluster test at a time under the shared Lease.
The manually dispatched contract workflow skips CNPG and Sandbox; those skipped
jobs are not passes.

The next required work is:

1. Complete the public Gateway workflow on the corrected source. Retain all
   public reports, rendered browser evidence, service-account checks, confirmed
   identity-provider logout, normal deletion, and cleanup. Check current full CI.
2. Run the complete unattended CNPG workflow with sufficient credential lifetime.
   Prove application behavior, automatic cleanup, and volume removal together.
   The earlier manual recovery does not meet this gate.
3. Supply allowed Gateway network paths through STEGO and prove them in the same
   application workflow. Include two Gateways, an unrelated namespace, allowed
   and denied fresh connections, endpoint changes, restart, regeneration, and
   cleanup. The [DNS provider choice](allocated-network-dns.md) remains open.
   The controller's public egress fault test does not prove Gateway isolation.
4. Complete separate Sandbox allocation and its permission and network boundary.
   The current controller rejects Sandbox runtime configuration with the shared
   allocator. The user deferred the live Kata test because no suitable cluster
   is available. This deferral does not establish runtime isolation.
5. Audit C1 through C7 and H1 through H3 against current source and complete
   workflows. Remaining work includes the full Hypershell port, backup and
   restore, supported deployment recovery, complete telemetry coverage, and
   measured capacity. Keep the original requirements active until their full
   evidence exists.

Public TLS uses the operator-selected issuer and router passthrough. Internal
Gateway TLS uses a separate operator-supplied trust file. The
[Route permission record](hypershell-public-route-host.json) retains the bounded
role update and installation identities. Neither these checks nor the current
partial public result establishes full production readiness.
