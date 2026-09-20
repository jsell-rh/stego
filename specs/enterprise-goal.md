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

The accepted application is Hypershell main `82f71a26`, with runtime source
`caffd04` and compiler `ad71bc71`. The main commit adds acceptance records to
the tested runtime source. The common workload builder supplies Deployment and
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

## Change under qualification

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
[compiler evidence](runtime-configuration-evidence.json). Consumer acceptance
remains open.

Hypershell candidate `72cc1786` declares four configuration groups and uses them
in the namespace allocator and Sandbox count worker. It is committed and pushed,
but has no generated configuration package yet. Do not treat the compiler pin
or the worker source as a complete application change. Next actions are:

1. Regenerate the frozen consumer source in CI. Review the output, compiler
   identity, input records, and unchanged module files before committing it.
2. Run the existing application checks, including startup privacy, provider
   setup ordering, REST/gRPC, restart, and repeated generation.
3. Verify the complete bounded service and browser workflows and their cleanup
   before promoting the consumer. Keep one live cluster test at a time.

An explicitly empty numeric setting will fail in this candidate. Omission or
the declared zero value selects the existing default. This behavior is documented
in the consumer and requires application validation. Connection assembly in
other workers and repeated REST/gRPC conversion remain separate work.

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
| H2: common mechanisms | Finish the current configuration adoption, then remove repeated mechanisms from proved workflows. Keep ownership, grants, placement, release selection, OpenShell configuration, and UI policy in Hypershell. Retain clean generation without rh-trex-ai. |
| H3: system behavior | Qualify supported deployment recovery, restore, concurrent and degraded-provider behavior, retained-history costs, and measured capacity. Preserve exact-source records and cleanup proof. |

The initial capacity targets are 100 Gateways per instance, 100 service accounts
per Gateway, and complete Gateway cleanup within 30 seconds after HTTP 202 with
healthy dependencies. These are targets, not admission or compiler limits.
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
