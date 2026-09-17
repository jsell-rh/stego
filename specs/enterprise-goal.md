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
| C3 | Reproducible and recoverable generation | Compiler and input identity, dependencies, state format, interrupted writes, concurrent apply | Active |
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

## Current work and evidence

The user selected durable asynchronous Gateway deletion. The implementation
returns HTTP 202 after durable acceptance, blocks new accounts, and retains
visible deleting state until final cleanup. Retained account and journal scans
have saved progress. A common database scope guard prevents completion during
concurrent journal registration. The new provider discovery path uses bounded
pages, per-client closure preparation, saved failure, and another full scan
before completion. See [the deletion contract](asynchronous-deletion.md),
[the scope guard](resource-state-scopes.md), and
[the provider discovery record](keycloak-inventory.md).

Each result below applies only to its named source. Later source changes need
their own qualification. Persistent raw evidence is under
`/home/jsell/.local/state/stego/runs/gateway-cleanup-20260916`.

| Check | Source | Result and limit |
| --- | --- | --- |
| Common scope guard | STEGO `cc6b906` | All six checks in [35103461720](https://github.com/jsell-rh/stego/actions/runs/35103461720) passed, including SQL conflicts, rollback, schema checks, and full compiler races. |
| Common closure preparation | STEGO `dfc9a1e` | All six checks in [35104344440](https://github.com/jsell-rh/stego/actions/runs/35104344440) passed. Default branch `fad0642` contains this code. |
| Common discovery cursor | STEGO `59bd24e` | All six checks in [35105032964](https://github.com/jsell-rh/stego/actions/runs/35105032964) passed. Real Keycloak also checked prepared closure and late-create cleanup in 57.56 seconds. The later source-identity and failed-window checks are listed below. |
| Common source and window recovery | STEGO `af67e7b` | All six checks in [35106013382](https://github.com/jsell-rh/stego/actions/runs/35106013382) passed. Real Keycloak passed in 54.43 seconds. Compiler races, SQL storage, dependency checks, and both examples passed. |
| Durable deletion API gate | Hypershell `b58d9a2` | All 49 required checks in [35103602751](https://github.com/jsell-rh/hypershell-stego/actions/runs/35103602751) passed. All generation snapshots matched. Test cleanup was independently verified. |
| Composed discovery recovery | Hypershell `5753c33` | Ten checks in [35105755539](https://github.com/jsell-rh/hypershell-stego/actions/runs/35105755539) passed. SQL and HTTPS fixtures prove independent progress, reconstruction, page shifts, foreign-client preservation, and final scope closure. |
| Failed-window SQL recovery | Hypershell `85706c6` | All twelve required checks in [35106173055](https://github.com/jsell-rh/hypershell-stego/actions/runs/35106173055) passed, with no skips. The limit remained a failure until another complete scan. Full deployed qualification remains separate. |
| Earlier full application gate | Hypershell `d1ec34c` | [35102064260](https://github.com/jsell-rh/hypershell-stego/actions/runs/35102064260) passed core, rendered browser, image, and console checks. This source predates scope closure and bounded discovery. CNPG and Sandbox were not selected. |
| Public closure workflow | Hypershell `550b2b7` | [35104667835](https://github.com/jsell-rh/hypershell-stego/actions/runs/35104667835) passed in 501.24 seconds. All 266 generation hashes matched and cleanup was independently checked. It predates bounded discovery. |
| CNPG application workflow | Hypershell `bceea63` | [35100459235](https://github.com/jsell-rh/hypershell-stego/actions/runs/35100459235) passed in 464.05 seconds with primary replacement, retained SQL identities and data, regeneration, and verified cleanup. It predates journal enumeration. |

The common query identity and failed scan-window changes passed full STEGO CI.
The corresponding Hypershell application runtime at `85706c6`, with compiler
`af67e7b`, has now passed all required current workflow gates:

| Check | Result |
| --- | --- |
| [Full application 35106305055](https://github.com/jsell-rh/hypershell-stego/actions/runs/35106305055) | Core, rendered browser, image, and console jobs passed; 483 test passes and four named exclusions. CNPG and Sandbox were not selected. |
| [API 35106819091](https://github.com/jsell-rh/hypershell-stego/actions/runs/35106819091) | All 51 required checks and regeneration passed. Independent cleanup passed. |
| [TLS provisioner 35107104565](https://github.com/jsell-rh/hypershell-stego/actions/runs/35107104565) | All twelve SQL and boundary checks passed. |
| [Public Gateway 35107987220](https://github.com/jsell-rh/hypershell-stego/actions/runs/35107987220) | Complete workflow passed in 498.58 seconds with 269 stable generation hashes. Independent operator cleanup passed. |
| [CNPG Gateway 35114192951](https://github.com/jsell-rh/hypershell-stego/actions/runs/35114192951) | All ten required tests passed at `e8ace19`. The complete browser workflow took 470.85 seconds. Primary replacement preserved SQL identities and data. Regeneration and independent resource and volume cleanup passed. |

Later application sources add tests and evidence without changes to that
runtime. Hypershell default branch `d6b1fe3` contains the qualified changes.
The source-specific records remain in its `acceptance/` directory. These
results close the current deletion and provider discovery qualification gate.
They do not close the remaining enterprise and application requirements below.

STEGO also qualified the common deployment Go API at `ea30c88` and confidential
browser client management at `1db04e6`. See the [deployment evidence](deployment-library-evidence.json)
and [browser provider evidence](browser-client-evidence.json). The separate
upstream dashboard must now use these common parts in its complete workflow.

The separate dashboard now uses the common pinned registry, generated browser
backend, browser telemetry, and generated Monaco adapter. The latest completed
live run, [35170428686](https://github.com/jsell-rh/hypershell-stego/actions/runs/35170428686)
at Hypershell `a29edb5`, passed editor behavior, SQL and namespace recovery,
viewer membership, filtered lists, denied writes, and both access-removal paths.
Native dashboard and Keycloak sign-out passed, as did authenticated correlation
of dashboard telemetry, worker telemetry, and rendered service-account use.
Three automation identities used the actual Gateway and were closed by durable
deletion. Main Gateway deletion removed its namespace, SQL state, roles, and
keys. Remaining cleanup failed after the identity fixture's 600-second deadline.
Hypershell now gives that fixture the full browser test's 900-second window.
The earlier HTTP 409 did not recur; its cause remains unknown. Full CI passed
302 tests with four declared live skips. The generated application must still
pass the complete cluster workflow. See the [current integration record](upstream-dashboard-integration.md)
and [native sign-out evidence](browser-logout-origin.md). These results do not
close H1, H2, H3, or the separate dashboard gate.

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
  Support co-located external PostgreSQL or CNPG, with a separate logical
  database and restricted login per Gateway. Deployment-backed databases are
  removed. RDS creation belongs to external Terraform; PostgreSQL fixtures are
  authorized for the current tests.
- Use public TLS passthrough and an operator-selected issuer. Internal trust is
  a separate explicit input.
- Keep the upstream per-Gateway dashboard. Generate its common authentication,
  deployment, and lifecycle in STEGO; retain its terminal contract.
- Allow people and authorized API automation for Gateway grants.
- Use restricted jshell CI, one live test at a time. Do not use Playwright,
  privileged containers, or workstation performance tests. Verify cleanup before
  another live run. Keep frozen sources and evidence in persistent storage.

The database model uses a fresh-schema gate. Old or unknown schemas fail before
writes. Existing installations require explicit operator teardown and recreation;
the application must never perform automatic teardown.

## Remaining application and enterprise work

1. Check large retained-history costs in CI. The complete current deletion and
   provider discovery workflows passed, but functional checks do not prove
   production capacity.
2. Complete the separate upstream per-Gateway dashboard workflow. Its common
   local transport and browser-session foundation have passed their recorded
   checks, but that does not prove application deployment or terminal behavior.
   See [the integration record](upstream-dashboard-integration.md).
3. Qualify native external DNS enforcement and failure behavior. Fixed-address
   isolation and address replacement have complete workflow evidence, retained
   in the history; they do not establish DNS failover behavior.
4. Complete the separate Sandbox allocation and permission boundary. The user
   deferred the live Kata test because no suitable cluster is available. Record
   this as a deferral, not evidence of runtime isolation.
5. Audit C1 through C7 and H1 through H3 against current source and complete
   workflows. Backup and restore, supported deployment recovery, complete
   telemetry coverage, full application parity, and measured capacity remain
   in scope. Preserve the one-writer requirement until cross-process fencing
   is implemented and verified. Whole-database rollback detection is open.

The [historical record](enterprise-history.md) preserves earlier decisions,
failed attempts, network and public workflow results, and source-specific
limits. It does not replace the completion requirements above. The goal remains
active until the full requested state is implemented and verified.
