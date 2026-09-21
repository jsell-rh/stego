The active goal is to make STEGO a reliable compiler for enterprise services
and deliver a fully STEGO-based Hypershell. The user authorized this work on
2026-09-08. The [original assessment](repository-assessment.md) defines the
initial defect list. The full scope remains active. A build or one passing
workflow does not establish completion.

STEGO supplies common runtime, security, telemetry, controller, reconciliation,
storage, and deployment mechanisms. Hypershell supplies domain behavior. Common
components must not depend on Hypershell entity names or rules. Verify common
capabilities with an independent service and the complete application workflow.
The variant must not require rh-trex-ai. Security, correctness, and performance
claims require evidence that covers the claimed behavior.

The reference checkout `/home/jsell/code/hypershell` must remain unchanged.
The test bed is [hypershell-stego](https://github.com/jsell-rh/hypershell-stego).
Active work uses isolated worktrees. Keep the working branches and verified
default branches on remote. User approval is not required to merge qualified
work. A failed or incomplete check cannot qualify a revision.

## Completion requirements

| ID | Requirement | Required evidence | State |
| --- | --- | --- | --- |
| C1 | Strict input and one semantic validation stage | Invalid fields, constraints, duplicates, capabilities, and paths fail before writes | [Verified at b0bd9a4](compiler-input-workflow-audit-20260915.md); retain regression coverage |
| C2 | Complete project and fill workflow | Fresh init, apply, fill creation, test, build, repeated apply, and drift | [Verified at b0bd9a4](compiler-input-workflow-audit-20260915.md); retain regression coverage |
| C3 | Reproducible and recoverable generation | Compiler and input identity, dependencies, state format, interrupted writes, concurrent apply | Active; [recovery evidence and remaining gaps](compiler-recovery-audit-20260917.md) |
| C4 | Secure authentication and authorization | Signature, issuer, audience, expiry, rotation, isolation, denied requests | Active |
| C5 | Correct storage and events | Migrations, concurrent transactions, durable events, recovery, failure tests | Active |
| C6 | Production runtime | Health, readiness, logs, metrics, traces, deadlines, shutdown, resource limits | Active |
| C7 | Explicit generator contracts | Typed wiring and extension contracts, capability checks, compatibility | Active |
| H1 | Complete Hypershell contract baseline | REST, gRPC, RBAC, watches, SDK, CLI, UI, and deployment workflows | Active |
| H2 | Fully STEGO-based implementation | Clean generation without rh-trex-ai; reviewed domain code outside output | Active |
| H3 | System verification | Complete workflows, deployment, security, races, measured capacity, regeneration | Active |

The Gateway acceptance gate requires creation and retrieval with the correct
IDs and API shapes; atomic creation of the Gateway and owner grant; filtered
lists and denied access; generated event delivery; and REST, gRPC, restart, and
regeneration checks. Use failures in real application workflows to select the
next infrastructure change. Reference contracts alone are insufficient.

## Current accepted application

The accepted application is Hypershell main `6aa1a658`, with runtime source
`a1ad728` and compiler `5c4afa11`. STEGO constructs the Gateway workload and the
separate browser deployment. Hypershell supplies OpenShell settings, verified
dependencies, placement, and domain rules. The console configuration digest is
also supplied by STEGO.

The full application check passed 1,192 core cases across 368 top-level tests.
The five recorded exclusions and the deferred Sandbox test remain explicit.
The complete browser workflow passed all 11 required tests. Review checked
generation, signed images, telemetry, four browser views, and cleanup. See the
[browser evidence](console-configuration-browser-evidence.json).
The exact main API run then passed all 52 required tests. Independent review
checked 1,673 source files, 429 generated hashes, and all five compiler files.
Cleanup removed the test resources, released the Lease, and preserved all 32
standing resources. See the
[main API evidence](https://github.com/jsell-rh/hypershell-stego/blob/6aa1a658b5ea8155600694f9ebe58aecbc194518/acceptance/console-rollout-main-api-evidence.json).

Normal cleanup with 100 accounts took an observed 32.69 seconds. The 30-second
target remains open. The common dependency converter is released in STEGO, but
its Hypershell adoption still requires its own complete workflow. These results
do not close the remaining enterprise requirements.

## Earlier accepted worker and workload changes

The earlier accepted application was Hypershell main `1f93f1f8`, with runtime source
`e0f3d7b` and compiler `8eeb1169`. It uses common typed connection settings in
five worker entry points. Its hosted check passed 1,176 core cases. The corrected
count workflow passed in 93.08 seconds. The complete browser workflow passed
all 11 required tests in 547.23 seconds. Review checked 1,657 source files, 429
generation hashes, all 430 generated archive files, seven signed images, and
four screenshots. Cleanup removed the test resources, released the Lease, and
preserved all 32 standing resources. See the
[browser evidence](runtime-control-browser-evidence.json) and
[count evidence](runtime-control-count-evidence.json).

The browser repeat crossed the earlier failed API discovery request without a
source or limit change. The first timeout remains unexplained and recorded.
Normal cleanup of a Gateway with 100 live accounts took 32.96 seconds. The
30-second target remains unmet. These results do not prove production capacity,
RDS failover, live Kata isolation, or the remaining enterprise requirements.

The earlier workload construction acceptance used main `82f71a26`, runtime
`caffd04`, and compiler `ad71bc71`. Its records remain the baseline below. The common workload builder supplies Deployment and
Service objects, security settings, probes, read-only Secret mounts, resource
limits, and configuration digests. Hypershell declares OpenShell images,
configuration, dependencies, placement, and domain policy.

The [full application check](https://github.com/jsell-rh/hypershell-stego/actions/runs/35533192186)
passed 1,162 core cases across 363 top-level tests. The browser, UI, and image
jobs passed. Five recorded core exclusions remain; they are not passing tests.
The separate live checks supply their stated coverage.

The service Deployment test passed in 120.13 seconds. It checked HTTPS and gRPC,
atomic Gateway and owner writes, rollback, filtered access, event delivery,
identity reconciliation, and API and worker Pod replacement. Seven signed image
identities and 428 generated hashes matched. The
[browser workflow](https://github.com/jsell-rh/hypershell-stego/actions/runs/35535544885)
passed all 11 required tests. It checked 1,647 source files, 428 generated hashes,
real login, the dashboard, denied requests, restart recovery, deletion barriers,
and correlated logs, metrics, and traces. Four retained images were reviewed.

Both admitted Gateway Pods had the declared image, security settings, probes,
mounts, limits, ownership chain, and configuration digest. Cleanup removed both
test fixtures and their allocated resources, released the shared Lease, and left
all 32 standing resources unchanged at the recorded inspection. See the
[workload construction record](https://github.com/jsell-rh/hypershell-stego/blob/82f71a26e411840648c331ac32f60a2db888210b/acceptance/workload-construction.md),
[service evidence](https://github.com/jsell-rh/hypershell-stego/blob/82f71a26e411840648c331ac32f60a2db888210b/acceptance/workload-construction-service-evidence.json),
and [browser evidence](https://github.com/jsell-rh/hypershell-stego/blob/82f71a26e411840648c331ac32f60a2db888210b/acceptance/workload-construction-browser-evidence.json).
These results accept that change. They do not close C3 through C7 or H1 through H3.

One normal deletion started with 100 live service accounts. Complete cleanup was
observed after 32.565 seconds. Allocation and finalization were still pending
at 30.36 seconds. The 30-second target is not proved. Namespace observations are
read-return times, not exact transition times. The allocator already requests
both retained state namespace removals in one pass after their dependencies.
This record does not justify removing a dependency barrier or a finalizer.

## Configuration history and remaining extraction

Compiler source `8eeb1169` adds common typed environment configuration. Explicit
declarations select fields, environment names, defaults, and limits. The runtime
rejects invalid values without returning partial settings or including supplied
values in errors. The compiler rejects unknown fields, alternate key case,
invalid UTF-8, invalid bounds, and conflicting generated names before output.
Provider constructors retain certificate, token, URL, and access checks.

Its [branch compiler check](https://github.com/jsell-rh/stego/actions/runs/35537375898)
passed all six jobs, 37 race-tested packages, and both complete generated
examples. The focused check passed 38 outer cases and 48 generated cases across
two package layouts. The source is on STEGO main. Its
[main compiler check](https://github.com/jsell-rh/stego/actions/runs/35537985325)
passed all six jobs and the same 37 packages. Independent review checked the
focused results, complete example output, source identity, signed artifact, and
negative artifact-verification cases. The authenticated immutable
[compiler release](https://github.com/jsell-rh/stego/releases/tag/compiler-8eeb11695416799cbc53fa42bba992a1ceeab810)
is published. A fresh installation matched all five verified package files
without running the compiler locally. See the
[compiler evidence](runtime-configuration-evidence.json). Consumer acceptance is recorded above.

Hypershell candidate `7ed1a7b7` declares four configuration groups and uses them
in the namespace allocator and Sandbox count worker. Its generated package is
committed and pushed. [Regeneration](https://github.com/jsell-rh/hypershell-stego/actions/runs/35538564130)
passed for seed `72cc1786`. Review checked all 428 output and module files, all
419 output hashes, and all 41 captured input hashes. The
[full application check](https://github.com/jsell-rh/hypershell-stego/actions/runs/35538751914)
passed 1,165 core cases across 364 top-level tests. All 1,162 prior cases and the
five recorded exclusions are retained. Adapter, browser, UI, and service-image
checks passed. Seven application images and seven browser fixture images passed
the separate source, content, registry, and signature reviews.

The service Deployment workflow passed in 119.43 seconds. Review checked seven
signed images, ready API and identity worker Pods before and after replacement,
and all 429 live generation hashes. The manifest contains the 428 output and
module files plus the pinned Gateway console compiler revision. All ready Pod
restart counts were zero. Cleanup removed the test namespace and owned resources,
released the shared Lease, and left all 32 standing resources unchanged.

The [browser workflow](https://github.com/jsell-rh/hypershell-stego/actions/runs/35540651284)
passed all 11 required tests. Its main scenario took 551.47 seconds. Review checked
1,653 source files, 429 generation hashes, seven signed images, correlated
telemetry, restart behavior, allocation finalization, and four screenshots.
Cleanup removed the test resources, released the shared Lease, and left all 32
standing resources unchanged. Normal deletion of a Gateway with 100 accounts
was observed complete after 34.19 seconds. Allocation and Gateway finalization
were still pending at 32.70 seconds. The 30-second target remains unmet.

The bounded live count test then stopped before its application test: generation
tried to create state under the read-only container home. Cleanup passed. A
separate committed runner correction selects writable generation storage,
verifies the complete compiler transfer before execution, and retains generation
records. The second run completed generation, then failed because its workload
render command omitted the required database endpoint. A corrected render
fixture passed its hosted check. The third run reached allocator construction
and failed because the test process did not receive the rendered network
endpoints. Independent cleanup passed after each run. None of these runs proves
live count behavior. The application source remained `e0f3d7b`.

The corrected fixture supplies the generated allocator settings and selects the
count role binding by role and subject identity. Its hosted check passed all
three repeated worker renders and 15 identity cases. The live test then passed
in 93.08 seconds. It covered ordinary Pods, namespace permissions, denied reads,
permission recovery, namespace UID replacement, events, REST/gRPC, and API and
worker restarts. Review matched all 1,659 source files and 299 generated hashes
in the count test scope. Cleanup removed owned resources, released the shared
Lease, and left all 32 standing resources unchanged.

The test source is `d98b673`; only five test and CI files differ from runtime
source `e0f3d7b`. See the [count evidence](runtime-control-count-evidence.json).
The first reviewer stopped on a missing newline between the compiler's final
message and the Go test start marker. A corrected reviewer recognized that exact
boundary and preserved the raw log. This result does not establish Kata isolation,
actual OpenShell Sandbox execution, or production capacity. The exact-source
browser repeat later passed as recorded above. Keep one live test at a time.

Separate follow-up candidate `e0f3d7b` uses the same generated connection settings
in the Gateway identity worker, Gateway workload worker, and account provisioner.
It adds checks for private configuration failures before provider setup. Its
first application checks stopped because three recorded worker input hashes
were stale; those tests did not run. Hosted
[regeneration](https://github.com/jsell-rh/hypershell-stego/actions/runs/35539887087)
refreshed the state. Review confirmed that only those three hashes and their
combined digest changed, with all 427 other output and module files unchanged.
The corrected source is committed and pushed. Its
[focused recovery check](https://github.com/jsell-rh/hypershell-stego/actions/runs/35540007357)
passed 172 cases across 44 top-level tests and seven packages. Artifact events
matched the job log. Seven application images passed source, content, registry,
and signature review. The
[full application check](https://github.com/jsell-rh/hypershell-stego/actions/runs/35540006064)
passed 1,176 core cases across 366 top-level tests. All 1,165 prior cases, test
definitions, and five exclusions remain. Browser, UI, and service-image jobs
passed; the Sandbox job remains deferred. Live count qualification passed with the recorded test-only changes. Corrected-source
browser qualification passed in run 35544262375. The earlier failed runs cannot qualify this source.

The exact-source [browser run](https://github.com/jsell-rh/hypershell-stego/actions/runs/35543304070)
failed after 377.37 seconds in its main workflow. The other ten required tests
passed. After API restart, Kubernetes API discovery timed out during the TLS
handshake for the generated management console deployment apply. The cause is
not established. Cleanup removed all test resources, released the Lease, and
left all 32 standing resources unchanged. Later checks found all five nodes
ready without pressure and the API and network operators healthy. These later
checks do not explain the failed request. See the
[failure evidence](runtime-control-browser-failure-evidence.json).
The controlled repeat used the same source, images, security controls, and
limits. It passed complete review and is recorded in the current accepted
application above. The failed result remains part of the acceptance record;
it cannot qualify the candidate or establish the timeout cause.

An explicitly empty numeric setting will fail in this candidate. Omission or
the declared zero value selects the existing default. This behavior is documented
in the consumer. Complete live checks passed for this change. Optional provider
connections, other settings, and repeated REST/gRPC conversion remain separate
work. Neither candidate completes the enterprise requirements.

The [transport mapping audit](transport-mapping-audit-20260920.md) identifies
the remaining conversion code and the application rules that it must preserve.
It also records an unchecked catalog timestamp conversion for a focused failure
test. This source review does not establish transport parity or runtime failure.

Common compiler installation diagnostics now identify release metadata, asset
download, and signature-verification failures with fixed public messages. Private
command output remains withheld. All four hosted workflows passed for `7f6d38ba`,
including 37 compiler packages, both generated examples, real immutable release
installation, and local package verification. The consumer compiler and installer
pins remain unchanged. See the [diagnostic evidence](compiler-install-diagnostics-evidence.json).
This change does not prove the cause of the earlier provider setup failure.
The [main checks](compiler-install-diagnostics-main-evidence.json) also passed
for `93104fb5`. Independent review matched both complete examples, all 37
compiler packages, and the authenticated artifact source and bytes.

The [workload boundary review](workload-boundary-review-20260920.md) separates
the accepted common builder from remaining dependency conversion and console
annotation assembly. Keep the upstream Sandbox setup and application policy
outside the restricted workload profile.

The common console rollout is accepted on Hypershell main `7e6d3f1`, from tested
source `a1ad728` and compiler `5c4afa11`. It removes application annotation
assembly and duplicate digest-format validation. All 11 live tests passed in
553.70 seconds. Complete generation, images, telemetry, four browser views, and
cleanup passed review. The Deployment, ReplicaSet, and ready Pod shared the
common digest. Normal cleanup with 100 accounts took an observed 32.69 seconds;
the 30-second target remains open. See the
[live evidence](console-configuration-browser-evidence.json) and
[compiler release evidence](deployment-configuration-main-evidence.json).

Compiler `d8c37d20` also provides bounded dependency data conversion and common
map access. All 145 generated cases, 37 compiler packages, and both examples
passed. Its signed immutable release and fresh installation passed verification.
Accepted Hypershell main still uses its local conversion helper. Candidate
`c3e88f1` removes it after complete regeneration review and adds two invalid-data
cases. Its application and image checks are active. A complete workflow remains
required before acceptance. See the
[dependency release evidence](workload-dependency-main-evidence.json).

## Remaining requirements

The completion table above remains authoritative. The following work is open;
a narrower passing check cannot close a broader requirement.

| Area | Required next evidence or implementation |
| --- | --- |
| C3: generation and delivery | Complete application build input identity, controlled offline inputs, supported installation targets, and durable release qualification. Retain interruption, conflicting-edit, and concurrent-apply tests. Signed Linux amd64 artifacts and repeated output hashes alone do not close C3. |
| C4: authentication and isolation | Complete the current-source audit of signature, issuer, audience, expiry, rotation, browser sessions, grants, and denied cross-tenant operations. Qualify supported DNS-aware enforcement and endpoint failure behavior. |
| C5: storage and events | Complete transaction, migration, durable delivery, recovery, backup, and restore coverage. Preserve the one-writer requirement until cross-process fencing is implemented and verified. Whole-database rollback detection remains open. |
| C6: runtime | Complete health, readiness, timeout, shutdown, resource-limit, and all-signal telemetry coverage. Direct finite controller helpers need explicit operation and parent contracts without duplicate telemetry owners. The earlier recovered browser initialization failure still lacks a proved cause. |
| C7: compiler contracts | Audit typed wiring, capability validation, extension points, and compatibility against the original assessment. Generated Go compilation does not by itself prove a complete component contract. |
| H1: application contracts | Complete parity evidence for REST, gRPC, RBAC, watches, SDK, CLI, UI, deployment, and the upstream dashboard terminal contract. |
| H2: common mechanisms | Complete the dependency conversion adoption and the remaining common transport mapping from proved workflows. Keep ownership, grants, placement, release selection, OpenShell configuration, and UI policy in Hypershell. Retain clean generation without rh-trex-ai. |
| H3: system behavior | Qualify supported deployment recovery, restore, concurrent and degraded-provider behavior, retained-history costs, and measured capacity. Preserve exact-source records and cleanup proof. |

The initial capacity targets are 100 Gateways per instance, 100 service accounts
per Gateway, and complete Gateway cleanup within 30 seconds after HTTP 202 with
healthy dependencies. These are targets, not admission or compiler limits.
The [cleanup timing review](cleanup-latency-review-20260921.md) records two
complete 100-account samples and the additional observations needed before a
latency change.
Larger installations can have thousands of Gateways. Account-only fixtures do
not establish whole-Gateway behavior or production capacity.

The jshell cluster has no additional capacity for the full 100-Gateway workload
at the current resource requests. The user instructed work to continue there.
Use bounded tests and preserve capacity for other workloads. Do not reduce
requests only to make the target fit. Measure any proposed resource change.

Live Kata isolation is deferred because no suitable cluster is available.
Native packet tests do not prove VM isolation or live OpenShell Sandbox execution.
Keep the unchanged upstream Agent Sandbox controller as trusted, operator-managed
cluster infrastructure, as selected by the user. Do not introduce a controller
fork or admission mutation service for the current upstream setup.

PostgreSQL process restart tests do not prove RDS failover. Server creation is
outside Hypershell scope and belongs to external Terraform. The current tests
can use bounded PostgreSQL fixtures; no RDS resource creation is authorized by
this goal record.

Use the [recovery audit](compiler-recovery-audit-20260917.md),
[controller entry point audit](controller-entrypoint-audit.md),
[retained-history results](retained-history-costs.md), and
[original assessment](repository-assessment.md) with their stated source limits.
The [saved goal record](enterprise-goal-history-20260920.md) preserves all prior
results, failures, requirements, and references. The earlier
[history](enterprise-history.md) remains available. Historical pending statements
do not supersede later exact-source evidence or the current next action.

## User decisions that remain in force


- Use the typed Go SDK; preserve HTTP contracts and behavior.
- Require verified service-database TLS, with an explicit literal-loopback test
  exception. Keep OAuth tokens out of browser JavaScript.
- Generate a separate Go browser backend. Confirm logout before ending both
  console and identity-provider sessions.
- Use an operator-set default release. Make `route_address` controller-owned.
- Support shared clusters with strict namespace isolation. Allow only declared
  Gateway destinations. Supported DNS-aware providers are allowed; reject
  unsupported configurations and Technology Preview features.
- Remove the database catalog and `database_id`. Reject retired request fields.
  Use only operator-supplied, co-located PostgreSQL, with a separate logical
  database and restricted login per Gateway. The user adopted
  [Hypershell PR #300](https://github.com/openshift-online/hypershell/pull/300)
  on 2026-09-17. This replaces the earlier CNPG requirement. Hypershell must not
  create database servers or select providers. RDS creation belongs to external
  Terraform; bounded PostgreSQL fixtures are authorized for the current tests.
  Keep reusable SQL provisioning, access checks, deletion, and telemetry in
  STEGO. Preserve earlier CNPG evidence as historical results.
- Use public TLS passthrough and an operator-selected issuer. Internal trust is
  a separate explicit input.
- Keep the upstream per-Gateway dashboard. Generate its common authentication,
  deployment, and lifecycle in STEGO; retain its terminal contract.
- Keep the current Hypershell and OpenShell Sandbox setup. Do not change the
  workspace helper user or socket volume through a mutation service or a fork.
  Complete the common namespace and permission controls separately. See
  [the recorded Sandbox decision](sandbox-upstream-settings.md).
- Allow people and authorized API automation for Gateway grants.
- Use restricted jshell CI, one live test at a time. Do not use Playwright,
  privileged containers, or workstation performance tests. Verify cleanup before
  another live run. Keep frozen sources and evidence in persistent storage.

The database model uses a fresh-schema gate. Old or unknown schemas fail before
writes. Existing installations require explicit operator teardown and recreation;
the application must never perform automatic teardown.
