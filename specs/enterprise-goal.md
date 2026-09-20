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

## Current work and evidence

The common controller trace change is qualified and released in compiler
`f6ebd0ba93983bfdcaefa15d8c3344b299af245e`. Reconciliation, scans, cleanup
samples, and watch sessions have separate traces. Provider calls remain children
of their operation. All six compiler jobs, focused trace and cleanup cases,
authentication checks, and signed release verification passed. Hypershell has a
regenerated candidate; its complete consumer checks and live trace proof remain
required. See the [trace contract and evidence](controller-trace-boundaries.md).
This result does not close C6, H3, or the full goal.

The consumer trace check found an incorrect expected allocator instance count
after a deliberate cleanup restart. The corrected focused check passed. Its
next live run stopped at the database restart Pod guard before the final trace
check. The cause of that guard failure remains unknown. The new guard reports
fixed failure categories and HTTP status, with the same checks and deadline.

The same consumer source failed its full Keycloak late-creation recovery test.
A deterministic hosted test then proved that the inventory failed when cleanup
removed a client between the list and its current-state read. Hypershell fix
`a5bf571` skips only that typed not-found result. STEGO already supplies the
common error contract; current ownership checks and retained cleanup proof stay
in place. Fresh full, provider, journal, allocation, and adapter checks passed
at that source. The full check passed 1,093 core cases across 347 top-level
tests. The bounded live workflow has started after a fresh cluster preflight;
its result and the separate API gate remain required. No failed or pending
consumer replaces main. See the [consumer verification record](controller-trace-boundaries.md#consumer-verification).

The pending-result consumer at `7bc21b7` passed its complete workflow with the
signed compiler `7a674e6`. Full checks passed 982 core cases across 341 top-level
tests, retaining all 966 prior cases. The focused allocation and adapter checks
passed. Live browser run `35490262586` passed all 11 required tests. It retained
the deleting Gateway in REST and gRPC while the allocator was stopped, then
completed namespace removal before finalization after recovery. Four browser
views passed visual review. See the [browser evidence](https://github.com/jsell-rh/hypershell-stego/blob/06f9de2f36a5553e627efe1a1ab017d773633bf7/acceptance/allocation-pending-live-evidence.json).

The separate API run `35491294055` passed all 52 required tests at that source.
Independent checks matched all 1,574 source files, 421 generated hashes, repeated
generation, and the signed compiler bytes in the actual bounded test Pods.
The joint cleanup audit found both fixtures absent, no allocated namespaces or
grants, a free shared lease, and all 32 standing installation resources unchanged.
See the [API and cleanup evidence](https://github.com/jsell-rh/hypershell-stego/blob/06f9de2f36a5553e627efe1a1ab017d773633bf7/acceptance/allocation-pending-api-evidence.json).

One Gateway with 100 REST-created accounts completed full cleanup with an
observed upper bound of 40.7967 seconds. The prior rescan workflow observed
57.0464 seconds. Both exceed the 30-second target. All 15 cleanup stages passed
with no observed return to pending. Sequential observations and bounded logs
do not establish the cause of the delay or production capacity. The later trace
compiler requires its own full application proof before another cleanup change.
Live Kata isolation remains deferred.

The prior Hypershell main test source `0aa8f0d` contains the qualified compiler retry,
cleanup ownership, and Sandbox integration changes. Main live run `35479995475`
passed all 11 required tests. Independent checks matched 1,548 source files,
421 generation hashes, the signed compiler, six browser startup instances,
and four inspected views. All 11 main gates now have independently verified
results. The API run passed all 52 required tests. Full main run `35479995448`
passed 950 core cases across 334 top-level tests, retaining all 816 baseline
cases. Browser, web console, and service image jobs passed; Kata remains
deferred. Independent reads confirmed removal of both live fixtures and release
of the shared lease. All 32 standing installation objects matched the adopted
configuration. See the [complete main record](https://github.com/jsell-rh/hypershell-stego/blob/f89e796/acceptance/main-qualification.md).

One Gateway with 100 REST-created accounts completed cleanup with an observed
upper bound of 53.9497 seconds. The prior candidate observed 53.5477 seconds.
These functional results do not prove the 30-second target or production
capacity. See the
[main browser review](https://github.com/jsell-rh/hypershell-stego/blob/3df3f64/acceptance/main-browser-review.md).

The later phase test at `80b0e00`, run `35481347942`, found a correctness defect.
The API finalized a Gateway at 29.73 seconds, but its retained state namespace
was still present at 51.69 seconds. Complete cleanup proof took 54.34 seconds.
The application did not include namespace allocation in its durable cleanup
owners. The earlier passing checks did not cover this ordering requirement.
See the [phase review](https://github.com/jsell-rh/hypershell-stego/blob/9b658d5/acceptance/cleanup-phase-review.md).

The allocation fix at Hypershell `6061469` passed the complete browser workflow,
the separate API gate, and ten hosted checks. It uses STEGO's existing targeted
cleanup and observation contracts. Namespace removal order remains Hypershell
policy. Main adopts this runtime with result records at `e9b9bf9`.

Browser run `35485013231` passed all 11 required tests. With the allocator
stopped, the other cleanup owners completed while two state namespaces remained.
REST and gRPC retained the deleting Gateway. A replacement allocator completed
removal before finalization. The normal deletion record contains all 15 required
phase observations and no namespace observed present after finalization.
API run `35486021140` passed all 52 required tests. Independent checks matched
1,568 source files, 421 generation hashes, the signed compiler and its bytes in
both bounded test Pods, 24 ready-Pod account observations, and four inspected
UI images. Both fixtures were absent, the shared lease was free, and all 32
standing installation objects were unchanged. See the
[qualified application result](https://github.com/jsell-rh/hypershell-stego/blob/e9b9bf9/acceptance/allocation-finalization.md).

The full suite passed 963 cases across 337 top-level tests. The earlier full
run at `ecb3160` failed three catalog and CLI fixtures that omitted allocation
cleanup. Source `6061469` corrected those fixtures; all three now pass. The
placement test also requires HTTP 409 after SQL and workload cleanup, before
allocation completion. The failed result remains recorded.

Complete cleanup proof for one Gateway with 100 accounts took 58.1439 seconds.
The 30-second whole-Gateway target remains unproved. The bounded account-only
capacity check took 21.9520 seconds with 10,000 background clients. It does not
include workload cleanup or prove production capacity.

The account cleanup flag returned to pending 14 times. A separate regression
at `bf6b4e5`, run `35486074519`, reproduced one cause: a clean partial rescan
removed an earlier cleanup proof while its scope stayed sealed and its inputs
stayed unchanged. It was the only failed test; all 169 earlier passing journal
cases remained passing. Candidate `7fe2f3d` preserves the observation in that
case and requires failures and changed inputs to invalidate it. It uses the
existing common scan and storage contracts. Journal run `35486427733` passed
all 172 cases, including the three new checks and all 169 earlier cases.
Real-provider run `35486753166` passed its six tests and 15 snapshot cases.
It closed all 61 protected identities after interruption and preserved the
unrelated client. The full suite and complete workflow remain pending.
The candidate is not part of the qualified main runtime.
See the [rescan candidate](https://github.com/jsell-rh/hypershell-stego/blob/7fe2f3d/acceptance/account-rescan-proof.md).

The automatic main browser check at `e9b9bf9`, run `35486475436`, also passed
all 11 required tests. Its source, generated output, compiler, account identity,
cleanup order, four images, and browser resource removal passed independent
checks. All 32 standing resources were unchanged. Normal whole-Gateway cleanup
took 58.2276 seconds. The API test and full suites are still separate checks.
Existing allocator logs show short HTTP requests and retry intervals that
follow the configured exponential delay. The source treats normal waiting as
a failed action. The next common controller review must distinguish expected
progress from failure while retaining failure backoff and cleanup correctness.
See the [evidence and required behavior](controller-pending-review.md).

These results do not prove the 30-second target. Schema upgrades,
distributed fencing, restore, live Kata, full application parity, and the other
open completion requirements remain unproved.

STEGO now combines a pinned common Git registry with distinct local application
archetypes. Hypershell no longer copies common component declarations. Both
consoles use the common browser archetype and telemetry runtime. The compiler
and common registry have matching full revision pins. Generated code remains
committed and checked for repeatable generation. Registry vendoring is explicit;
it does not supply all inputs for an offline build. See
[registry composition](registry-composition.md).

Compiler `b8fdfd6` adds common database credential preparation. It checks server
identity, stored ownership, and SQL names under the provisioning lock. The
application selects and retains one candidate in protected state before SQL
creation. All six branch and main compiler jobs passed, including the real SQL
cases. The signed main package, exact source, two build outputs, and rejection
cases were checked independently. Its immutable release and common installer
also passed verification. Hypershell has adopted the generated API on its test
branch. Dedicated API/SQL, core, provider, journal, and browser checks passed.
See [the application record](https://github.com/jsell-rh/hypershell-stego/blob/53ddb6744e698ec4b207d8ad044edb465b69ac4d/acceptance/database-credential-preparation.md).

The complete CNPG Gateway workflow passed at Hypershell `d9cd7e3` with compiler
`b8fdfd6` in run `35224348179`. Independent checks matched 1,432 source files,
416 generated file hashes before and after tests, the published compiler
package, the console image, all 11 required tests, and cleanup. The main
workflow took 728.81 seconds. It included login, owner grants, REST and gRPC,
filtered lists, denied requests, events, service accounts, durable deletion,
and recovery after namespace, database Pod, and provisioner replacement.
Credential preparation exported the required correlated SQL signals. Six
browser runtime instances supplied complete startup telemetry. Three dashboard
images were reviewed. See [the CNPG record](https://github.com/jsell-rh/hypershell-stego/blob/53ddb6744e698ec4b207d8ad044edb465b69ac4d/acceptance/postgres-signal-cnpg.md).
The final frozen CNPG run also passed at `53ddb67` in `35229065005`. All 11
required tests passed; the main workflow took 700.42 seconds. Independent checks
matched 1,438 source files, 416 generation hashes, compiler and image identity,
six browser startup instances, and complete runtime and volume cleanup. The
three dashboard images were reviewed. This run verified the corrected fixture
deadlines. It is a historical result: the user has removed CNPG from the product
scope. See [the final record](https://github.com/jsell-rh/hypershell-stego/blob/9cb10239c83d9b76e31f9403cfa35e34c70345ea/acceptance/retired-cnpg-final.md).

The external-only Hypershell change removed the CNPG installer, credential
projection, network additions, and CI jobs. Its hosted browser, 231 UI tests,
and seven image builds passed. CI generated the replacement test policy;
independent checks found one changed network validation rule among 19 cluster
resources. The operator applied that exact rule after verified cleanup.
The complete external PostgreSQL workflow passed in `35232271576` at
`62e82d5`. Its core checks also passed. The main branch repeat at `d0d5250`
passed all eleven required live tests in `35235034044`. Independent checks
matched 1,434 source files, 416 generated hashes, compiler and image identity,
and live recovery records. Independent cleanup found no test resources or held
lease. The separate API and journal gates passed 52 and 28 required tests.
The hosted browser and core repeat passed. The core log confirms 311 top-level
tests, 670 test cases, three generation checks, and container cleanup.
See [the database contract and evidence](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/external-gateway-databases.md).

The earlier hosted core run at `4897fc1` failed a cleanup recovery event deadline
and an unrelated Keycloak client response comparison. Later checks follow the
provider's unordered scope-name contract and retain all other value checks.
The full core suite at `c55b2e2` passed 309 top-level tests, including both
earlier failure cases. The cause of the earlier scope-array difference and
event timeout remains unknown. The first complete b8 CNPG run also failed its
telemetry contract because it did not accept the common `prepare` operation.
The later complete pass above requires that operation and its correlated
signals. Preserve all failed results; a later pass does not establish their
full cause.

Common allocation account identity passed twenty live admission and RBAC
probes, twelve policy type checks, and independent cleanup. Compiler `09efc7`
adds account readiness checks and is published as a verified immutable release.
Full compiler and renderer checks passed. Hypershell has adopted it on a test
branch, with matching generated output and an explicit console module pin.
The complete external PostgreSQL consumer workflow passed at Hypershell
`c253c04`: all 11 browser checks and all 52 API checks passed. Independent checks
matched 1,447 source files, 416 generated hashes, compiler and image identity,
24 ready-Pod account observations, and empty cluster cleanup. The core suite
passed 738 cases. The prior console Pod-readiness failure remains recorded;
the test now waits for current readiness while account errors still fail.
See the [application record](https://github.com/jsell-rh/hypershell-stego/blob/f258441/acceptance/pod-readiness-evidence.md)
and [allocation evidence](allocation-service-accounts-evidence.md).

Related-profile grants passed all 12 live permission probes with independent
cleanup. The control-account audit then confirmed retained grant reuse after
namespace replacement. The common generator now reserves the allocator and
declared control-worker account names for a separate trusted installer.
At `931f712`, the live regression denied both names and their generated-name
prefixes, verified installer permissions, and restored both accounts through
the trusted installer. All 14 policies passed type checking. Independent
cleanup verified 57 resource paths absent. The full race step passed 34
packages, and all six CI jobs passed. The qualified source is on STEGO main.
Compiler `931f712` is published as a verified immutable release. Hypershell
adopted it on main at `39b8f40`. All 11 required live browser tests and all 52
required API tests passed. Independent checks matched the source, 416 generated
hashes, compiler signatures, and the actual console image. The workflow recorded
24 ready-Pod account observations across two Gateways and three namespace
incarnations. Both live tests have empty independent cleanup records and a free
shared lease. The hosted core suite passed 738 cases. Account retirement and
arbitrary external grants remain outside this result. See the
[policy evidence and limits](allocation-control-account-policy.md). Sandbox setup
and admission remain separate from the deferred VM test; see the
[upstream settings review](sandbox-upstream-settings.md).

These results do not close production capacity, restore, fencing, Sandbox
isolation, or complete application parity. The goal remains active.

The following source-specific records describe earlier steps.


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
live run, [35173393321](https://github.com/jsell-rh/hypershell-stego/actions/runs/35173393321)
at Hypershell `e8bb965`, passed the complete public workflow in 670.99 seconds.
It passed editor behavior, SQL and namespace recovery,
viewer membership, filtered lists, denied writes, and both access-removal paths.
Native dashboard and Keycloak sign-out passed, as did authenticated correlation
of dashboard telemetry, worker telemetry, and rendered service-account use.
Three automation identities used the actual Gateway and were closed by durable
deletion. Main Gateway deletion removed its namespace, SQL state, roles, and
keys. Cleanup of all Gateways and final managed-cluster deletion passed. The
supplied PostgreSQL server and installation data remained. Final PostgreSQL
telemetry, including schema operations, and browser log privacy passed. All 412
generation hashes matched before and after the test and in the saved archive.
Independent cluster cleanup passed at `2026-09-17T02:31:24Z`.
The earlier HTTP 409 did not recur; its cause remains unknown. Full CI passed
302 tests with four declared live skips.

The expanded CNPG workflow at `aed33a9` passed in 768.33 seconds in
[35176586562](https://github.com/jsell-rh/hypershell-stego/actions/runs/35176586562).
It includes primary replacement, retained SQL identities and data, complete
dashboard behavior, access recovery, durable deletion, and linked telemetry.
All 412 generation hashes and the three viewed screenshots match the final
archive. Independent cleanup passed at `2026-09-17T03:29:00Z`. Hypershell remote
`main` is now `b21604b`. The run needed one manual secondary Pod replacement for
scheduling. One console recovered after three startup failures; their cause is
not established. See the [current integration record](upstream-dashboard-integration.md)
and [native sign-out evidence](browser-logout-origin.md). This closes the
expanded CNPG workflow gate, but not H1, H2, H3, or the full enterprise goal.

Common browser startup and relay telemetry now have complete public workflow
evidence at Hypershell `bf83eef` with compiler `83592be`. Run
[35180785681](https://github.com/jsell-rh/hypershell-stego/actions/runs/35180785681)
passed in 667.64 seconds. Six observed browser runtime instances supplied 48
matching startup log/span pairs and complete metrics. All 415 generation hashes
matched, and independent cleanup passed. See [browser startup](browser-startup.md).
The API runner recovery passed all 51 required checks at `8c37bb2` in
[35182200900](https://github.com/jsell-rh/hypershell-stego/actions/runs/35182200900).
All 415 generated-file hashes matched, the original Job reached `Complete`,
and independent cleanup passed. The full application check at `bf83eef` also
passed 307 top-level core tests, rendered management, console, and image checks.
The CNPG run at `c0c2d23` failed because the account test did not wait for
Gateway controller recovery after its deliberate provisioner restart. Generated
RPC logs show the dependency outage and recovery; the API retained its readiness
rule. Independent cleanup passed. The revised test must prove denial during
the outage and recovery before further account creation. See the
[browser startup record](browser-startup.md). These checks do not close C6 or H3.

The revised CNPG workflow passed all 11 required tests at `cf232b0` in
[35186648964](https://github.com/jsell-rh/hypershell-stego/actions/runs/35186648964).
It verified provisioner outage denial and recovery, 48 startup log/span pairs
from six browser runtime instances, all 415 generation hashes, CNPG replacement,
and complete deletion. Independent cleanup passed at `2026-09-17T06:04:12Z`.
No scheduling intervention was required. Hypershell remote `main` is `0175b0b`.
The empty archive in the prior run remains unexplained and remains a failure.
See [the startup evidence](browser-startup.md). C6, H3, and the full goal remain
open.

The [compiler artifact build check](compiler-artifact-builds.md) passed at
`7b75de6`. Two isolated source trees and caches produced identical Linux amd64
compiler bytes. Independent inspection verified the saved binary, all 1,181
source records, 21 module records, and embedded build settings. All six normal
compiler jobs also passed. The check is on remote `main`. Authenticated artifact
publication, toolchain provenance, and complete application build inputs remain
open; C3 is not complete.

The main Hypershell repeats at `0175b0b` are complete. The public and CNPG
workflows both verified 1,360 source hashes, 415 generation hashes, and 48
matching browser startup log/span pairs. The API gate passed all 51 required
checks; the core suite passed 307 top-level tests. The CNPG repeat needed one
owned secondary-Pod replacement for scheduling, then completed its recovery,
deletion, and independent cleanup checks. Hypershell `9dabb6c` retains this
evidence and its limits. See [browser startup](browser-startup.md).

The [compiler signature verifier](compiler-provenance.md) is implemented. Its
first main CI verification failed because of mutually exclusive CLI options.
The corrected command passed independent checks with the retained real
signatures, including rejection cases. The corrected source `8417750` passed
all six compiler jobs and the controlled artifact build, then reached remote
`main`. The corrected main signature gate passed in
[35192910718](https://github.com/jsell-rh/stego/actions/runs/35192910718).
Independent verification accepted both signatures and confirmed that the
compiler and build record match the checked branch build. All four rejection
cases passed in CI, followed by another successful check of valid input.
Permanent release delivery and complete offline builds remain open. No failed
or incomplete run is treated as a pass.

The compiler now checks the official Go archive and extracted SDK inventory
before it executes Go. Source `403a7eb` passed the two-build artifact gate,
independent source and artifact inspection, and all six compiler jobs. It is
on remote `main`. The original executable bits now match the official archive;
the earlier cache permission difference is no longer accepted. See the
[SDK preparation evidence](compiler-artifact-builds.md).

A further composition review found that Hypershell's local Gateway browser
archetype adds only common browser telemetry. The common browser archetype now
includes that runtime and resolves one client/relay identity. Conflicting
identities fail before generation. Source `0057370` passed all six compiler
jobs, the native browser logout check, and independent artifact inspection.
Its main full suite and independent signature checks also passed. Hypershell
now uses this common archetype for both consoles. Only its API application
archetype remains local. The full suite, public workflow, 52-test API gate, and
11-test CNPG workflow passed at their recorded sources. The CNPG run at `854bbb1`
matched all 1,368 source files and 415 generated hashes. Six browser instances
supplied 48 startup log/span pairs with complete metrics and no failed pairs.
Database replacement, access, recovery, and durable deletion passed. Independent
cleanup passed at `2026-09-17T08:46:58Z`. One owned secondary Pod replacement
was needed for scheduling. Hypershell `8813797` is on remote `main` with the
qualified changes and records. See [browser telemetry](browser-telemetry.md)
and [registry composition](registry-composition.md). This completes that
composition change; C3 through C7 and H1 through H3 remain open.

The external PostgreSQL-only Hypershell workflow passed at `62e82d5` in
[run 35232271576](https://github.com/jsell-rh/hypershell-stego/actions/runs/35232271576).
All eleven tests passed; the main workflow took 669.65 seconds. It checked
Gateway creation, grants, filtered and denied access, REST and gRPC, events,
accounts, dashboard behavior, PostgreSQL process restart, namespace and
provisioner recovery, encrypted state, and durable deletion. All 1,427 source
files and 416 generated hashes matched. Six browser instances supplied all
eight startup stages with matching logs, traces, and metrics. The deployed
compiler and console bytes matched their qualified records. Independent
cleanup passed. The hosted core run at `29a238a` also passed 311 top-level tests,
670 test cases, three regeneration checks, and container cleanup.

The nine unused CNPG test role bindings were then removed with object identity
and version checks. Independent RBAC checks confirmed five denied old
permissions and two retained current test permissions. Shared CRDs and
unrelated workloads were unchanged. See the
[external database evidence](hypershell-external-database-workflow.json).
This result qualifies the selected database path. It does not prove RDS
failover, the production capacity target, live Kata isolation, or the complete
enterprise goal. The common account allocation gate and consumer adoption
remain separate work.

The common service-account allocation gate also passed all twenty live probes,
twelve policy type checks, and five namespace incarnations. Independent checks
confirmed runner and manifest identity, all responses, and removal of 49
resource paths. See the [issuer evidence](allocation-service-account-issuer-evidence.json).
The common component was then published in compiler `09efc7c`. Hypershell
adoption passed the complete workflow at `0ec0180`: 11 required tests,
416 generated-file checks, and 24 ready-Pod account observations across two
Gateways and three namespace instances. Names survived namespace replacement;
account and Pod UIDs changed. Worker account writes were denied. Independent
cleanup passed. The change is on Hypershell main at `110d9c4`.
See the [account adoption evidence](allocation-service-accounts.md).
Separate Sandbox allocation and permissions, live Kata isolation, production
capacity, and the other completion requirements remain open.

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

## Remaining application and enterprise work

1. Extend the [measured retained-history baselines](retained-history-costs.md)
   to real provider capacity and concurrent load. The SQL grant scan passed at
   10,000 and 100,000 synthetic grants. Known account and protected-journal
   cleanup also passed with 1,000 account rows and 2,000 provider clients over
   verified HTTPS. Median cleanup was 4.3863 seconds in a bounded protocol
   fixture. The real unknown-client discovery and API/provisioner restart case
   now has the bounded 61-client result above. Real Keycloak capacity, concurrent
   discovery, state-service and database recovery, other Gateway controllers,
   and production SLOs remain open. On 2026-09-17, the user set the first
   application target at 100 Gateways per instance, 100 API service accounts
   per Gateway, and Gateway cleanup within 30 seconds. These are performance
   targets, not compiler validation limits or runtime admission limits. Larger
   installations can have thousands of Gateways. Measure cleanup from acceptance
   of the deletion request to confirmed cleanup with healthy dependencies. Keep
   larger-scale, degraded-dependency, and concurrent-load results separate.
   These targets have not yet been proved against real providers at that scale.
   Five real-provider runs now separate setup defects, failed cleanup, and
   complete cleanup above the target. The fifth run, `35463991341`, used
   Hypershell `032d56b` and compiler `58a3bcc`. It completed account cleanup in
   93.2722 seconds after HTTP 202, so the 30-second target failed. All 100
   selected clients and users were absent, all 100 journals and account rows
   were closed, and all 100 success audits were present. The 9,900 background
   account rows and 10,007 other clients retained their saved state. Source and
   binary checks matched, and test resources were removed. See the
   [five capacity results and limits](https://github.com/jsell-rh/hypershell-stego/blob/2904ec62f3530285b33c2702d0cf8ab167490c87/acceptance/provider-capacity.md).

   The common action reserve removed the saved failure caused by starting work
   too near the deadline. All sampled saved cycles in the fifth run had clean
   failure flags. The unchanged six-account restart fixture also passed in
   journal run `35463688587`, with all 33 required tests. Its earlier failure
   remains recorded. Full hosted run `35463688329` passed 325 top-level tests
   and 816 cases at the same source. These branch results do not promote the
   pending Sandbox changes or qualify total Gateway cleanup.

   The first five cleanup passes closed 18, 38, 59, 80, and 99 account rows,
   about 12 seconds apart. The next change uses the common keyed parallel scan
   runtime. Account rows and journals must use the same resource key. Keep the
   current work and commit budgets, page sizes, sweep cadence, and capacity
   fixture for the comparison. Compiler `d3ccd11` passed full branch and main qualification and was
   published as an authenticated immutable package. See the
   [common runtime evidence](scan-parallel-evidence.json). Hypershell adopted
   the generated runtime at `6c54674`. Its full check `35467115168` passed 327
   top-level tests and 830 cases, with all 816 earlier cases retained. Both
   restart tests and the browser, UI, and image jobs passed. Five conditional
   tests and the Kata job remained excluded. See the
   [application result](https://github.com/jsell-rh/hypershell-stego/blob/ecaee43/acceptance/scan-parallel-full-evidence.json).
   The sixth capacity run `35468703943` used that exact source and the
   unchanged fixture. It completed account cleanup in 92.3174 seconds and
   failed the 30-second target. All 100 selected identities and journals were
   closed or removed. The 9,900 background rows and 10,007 other clients were
   preserved. Source, binary, resource-limit, and test cleanup checks passed.
   Parallel work did not establish a material timing improvement. Failed saved
   cycles required another scan, and saved passes remained about 12 seconds
   apart. Source review found that the common sweep waits one interval after
   each group, including empty groups. Ten application groups add ten seconds
   per rotation before callback time. The next common scheduling change must
   preserve bounded work, group fairness, and existing caller defaults. The
   new option is now qualified in compiler `3220812`: callers can wait after
   a complete round while the default still waits after each group. Its signed
   immutable release passed full branch and main checks. See the
   [interval qualification](sweep-interval-evidence.json). Application candidate
   `6a8a882` passed full run `35471009094`: all 329 top-level tests and 844
   cases remain, with the same five conditional exclusions. Browser, UI, image,
   and both restart checks passed. The separate journal run passed all 36
   required cases. See the [application qualification](https://github.com/jsell-rh/hypershell-stego/blob/df85c40/acceptance/sweep-round-validation.md).
   The seventh capacity run `35472389423` completed with the unchanged fixture.
   Account cleanup decreased from 92.3174 to 61.5320 seconds, but the 30-second
   target still failed. All 100 selected accounts, journals, provider identities,
   and success audits were checked. The 9,900 background rows and 10,007 other
   clients retained their state. Resource limits and test cleanup passed. See
   the [seventh result](https://github.com/jsell-rh/hypershell-stego/blob/ad84170/acceptance/sweep-round-capacity-evidence.json).
   The scheduling candidate is not yet on application main. A separate
   diagnostic correction at `c1b3580` retains canonical RPC statuses. All 34
   canonical status checks passed in eighth run `35472990779`. Account cleanup
   took 44.9706 seconds and still failed the target. All selected accounts,
   journals, provider identities, and audits were checked; background data stayed
   unchanged. The first saved count of 100 closed rows occurs at 34.6770 seconds.
   The scope closes at 44.6510 seconds. Its changed collector must be identified
   in timing comparisons. See the [eighth result](https://github.com/jsell-rh/hypershell-stego/blob/65fa253/acceptance/capacity-eighth-evidence.json).
   Three early delete deadlines and two internal errors precede a later scan
   that closes the remaining rows. Review the common failed-work retry path
   without removing durable progress or late-effect checks. The record does
   not prove the cause of each server delay. Earlier evidence does not identify
   the cause of every failed provider call. See the
   [sixth result](https://github.com/jsell-rh/hypershell-stego/blob/fb7377d/acceptance/provider-capacity-evidence.json).

   Common candidate `d205073` adds explicit bounded action retries. Default
   behavior and checkpoint encoding are unchanged. It retains saved failures,
   complete action time budgets, key order, and cancellation. Fifty generated
   cases passed in each telemetry mode in focused run `35473708792`. All six
   full jobs and 34 race packages passed in run `35473708806`. The qualified
   candidate is merged. Exact main `ee348b8` passed all six full jobs and 34
   race packages in run `35474193099`. Focused checks passed in both telemetry
   modes. The signed immutable compiler release is published; independent source,
   dependency, build-policy, signature, and installer checks passed. See the
   [retry evidence](controller-cycle-retry-evidence.json).
   Hypershell candidate `7333332` retains safe retry classes without changing
   public error messages. Journal run `35473973149` passed all 38 required tests,
   including 33 status cases and both restart fixtures. Consumer candidate
   `51b2bb2` now uses compiler `ee348b8` and enables two attempts with a
   25-millisecond delay for selected temporary failures. Its full run
   `35474805484` passed 332 top-level tests and 937 cases, including all 816
   baseline cases, 22 retry cases, and both restart fixtures. The five
   conditional exclusions are unchanged. Browser, console, and image checks
   passed; the Kata job remains deferred. Candidate generation and all 420
   generated file hashes were checked. Application main still uses the earlier
   qualified compiler.

   Ninth capacity run `35475425102` at `b0952c4` used the same fixture as the
   eighth run. Account cleanup took 61.5487 seconds and failed the unchanged
   30-second target. All 100 selected rows, journals, provider clients, and
   users closed or were removed. The 9,900 background rows and 10,007 other
   clients stayed unchanged. Source, binary, compiler, resource-limit, and test
   cleanup checks passed. This comparison does not show a timing improvement
   and does not establish the cause of the difference. See the
   [ninth result](https://github.com/jsell-rh/hypershell-stego/blob/69c5214/acceptance/capacity-ninth-evidence.json).

   Source review found repeated provider deletion in the deleted-account
   stream and the retained Gateway scan. Application candidate `018ce9c`
   assigns that work to Gateway recovery when the exact retained parent is
   deleted. Live or absent parents still use account recovery. STEGO keeps
   ownership of scheduling, retry, saved progress, and provider lifecycle.
   The candidate retains complete row and journal scans, provider inventory,
   and late-effect checks. Journal run `35476194174` passed all 41 required
   tests, including 11 owner selection cases, 33 RPC status cases, 22 retry
   cases, and both restart fixtures.

   Tenth capacity run `35476517343` at `4ea3e1e` passed account cleanup in
   21.2756 seconds. The scope sealed at 21.0061 seconds. All 100 selected rows
   and journals closed; their provider clients and users were absent. The
   9,900 background rows and 10,007 other clients stayed unchanged. The six
   fixture files match the ninth run. Source, compiler, binary, resource limits,
   and test cleanup were verified. This is one passing account cleanup result.
   It does not prove repeatable latency, concurrent or larger cleanup, or
   complete Gateway workload and database cleanup. See the
   [tenth run](https://github.com/jsell-rh/hypershell-stego/actions/runs/35476517343).
   The full application gate at the same source passed in run `35476514171`:
   334 top-level tests and 950 cases, including all 816 baseline cases, 22 retry
   cases, 11 owner selection cases, and both restart fixtures. The five
   conditional exclusions are unchanged. Browser, console, and image jobs
   passed. The Kata job remains deferred.

   The normal live workflow now records complete Gateway cleanup observations
   separately from the deliberate SQL-denial test. It checks namespaces, SQL
   roles and databases, allocation bindings, final state, and owner HTTP 404.
   Sequential observations give upper bounds. Candidate `f924e96` now creates
   100 accounts through REST on each measured Gateway and checks token issuance
   before deletion. Completion requires removal of all provider clients and
   users, authenticated closed journals, closed rows, and one success audit per
   account. Hosted adapter run `35476824851` passed all 63 required tests,
   six allocation cleanup tests, and 28 collection cases. The live test compiled.
   Live run `35477799297` stopped when the scheduler preempted the test Pod
   for the OpenShift image registry. There is no complete workflow result.
   Independent checks confirmed that test resources were removed, all 32
   standing resources were restored, and the shared lease is free. Candidate
   `4c6744b` retains terminal Pod status when the completion record is absent.
   Hosted run `35478679579` passed five status cases, eight completion cases,
   all 63 required adapter tests, six allocation cleanup tests, and 28 collection
   cases. The live test compiled. Run `35478851396` passed all 11 live tests
   at that fixed source. The browser workflow took 737.07 seconds. The normal
   cleanup sample had 100 REST-created accounts with verified token issuance.
   All rows, journals, provider clients, provider users, and success audits
   passed the complete cleanup checks. The observed upper bound was 53.5477
   seconds. The 30-second target remains unproved. Sequential checks do not
   identify each resource's removal time or the cause of the delay. This
   two-Gateway workflow does not prove the whole production capacity target.
   Live attempt `35476176959` stopped before workload
   creation because the saved test installation lacks three named reads in
   two inspection roles. Cleanup confirmed no test resources and all 32
   standing resources unchanged. The corrected hosted plan at `4ea3e1e`
   passed source and compiler checks and adds only those test reads. Production
   roles, admission rules, network policy, and RuntimeClasses are unchanged.
   The live timing result is now recorded. The next timing review must use
   the complete workflow evidence and preserve the verified cleanup behavior.
   Application diagnostic candidate `80b0e00` records separate observations
   for namespace removal, durable cleanup flags, and final proof checks. A
   later pending read clears an earlier completion and records a regression.
   Existing conditions, polling, population, and deadlines remain unchanged.
   Hosted run `35480246600` passed all three observation cases and compiled the
   live test. The existing adapter and collection checks passed. No live phase
   timing result is yet available. Main regeneration and adapter checks were independently verified
   at `0aa8f0d`; all 420 archived generation files match committed source.
   Automatic capacity run `35479995472` at the same main source passed account
   cleanup in 16.8819 seconds; the scope sealed at 16.4927 seconds. All selected
   accounts, protected journals, provider identities, and audits passed. The
   9,900 background rows and 10,007 other clients were preserved. Source, binary,
   compiler, resource limits, and test cleanup were independently checked. All
   six fixture files match the tenth run. Two account-only passes do not prove
   the complete Gateway target or identify the cause of its 53.55-second upper
   bound. The complete phase result remains required.
   Main journal, provider, console, and dashboard checks at `0aa8f0d` also passed
   independent review. All 41 journal tests, both restart cases, six provider
   tests, 15 snapshot cases, and 13 dashboard router tests passed. Console
   generation and repeated asset archives match source. The saved compiler
   public records match the verified release. See the
   [hosted result review](https://github.com/jsell-rh/hypershell-stego/blob/2c55966/acceptance/main-hosted-review.md).
   Main browser run `35479995475` then passed all 11 required tests. Source,
   generation, compiler, telemetry, four views, and browser cleanup passed
   independent checks. Its normal 100-account Gateway cleanup upper bound was
   53.9497 seconds. Full main run `35479995448` then passed 950 core cases
   across 334 top-level tests, with all 816 baseline cases retained. Both
   restart cases, 22 retry cases, and 11 cleanup owner cases passed. Browser,
   web console, and service image jobs passed. The five core exclusions are
   unchanged, and Kata remains deferred. See the
   [full main result](https://github.com/jsell-rh/hypershell-stego/blob/9ba52eb/acceptance/main-full-evidence.json).
   API run `35479995493` passed all 52 required tests. Its 1,548 source files,
   421 generation hashes, actual compiler bytes, Job limits, and private fixture
   cleanup passed independent checks. Joint cleanup found no test resources,
   a free lease, and all 32 standing objects unchanged. All 11 main gates passed
   exact source and attempt checks. Phase run `35481347942` is now active at
   `80b0e00`. It uses the standing installation and changes only test observations.
   It has no complete result yet. Require all 14 phase observations before a
   performance change is selected.
   A [source review of pending work](controller-pending-review.md) found that
   expected incomplete namespace work shares error retry delays and failure
   telemetry. This is a common runtime distinction to review after the phase
   data arrives. It does not establish the cause of the observed duration.
   No queue, retry, or telemetry behavior has changed from this review.

   Setup first exposed fixed application quotas. Main `034b46b` now has operator
   quota settings with the original defaults. Its journal gate passed all 32
   required tests; its API gate passed all 52. Full main CI passed 315 top-level
   tests and 754 cases. The separate live browser attempt stopped before Job
   creation because the installed Sandbox candidate policy differs from main.
   That earlier attempt remains a failed result. The later qualified Sandbox
   workflow and promotion below close this setup mismatch. The user has
   resolved the controller trust decision as recorded below.

   The whole-realm Gateway identity inventory has a separate candidate fix at
   `739835e`. It uses the generated name-query cursor and scan for native and
   console client prefixes, then verifies current ownership. Its 36 required
   journal tests and real-provider recovery checks passed. The combined full
   check at `674c6e5` and live check at `7f81556` now passed. Main `dbd8363`
   contains this qualified change. This removes unrelated account
   clients from the normal query; it does not remove the per-query window or
   the controller's discovery deadline. The account-only capacity sample does
   not run that controller, create all background accounts through the API, or
   qualify total Gateway workload cleanup. Concurrent deletions, larger
   installations, and degraded dependencies remain separate checks.
2. Identify the earlier recovered browser initialization failure. Common
   startup diagnostics now have complete public and CNPG workflow evidence.
   An earlier CNPG run needed one secondary Pod replacement for scheduling;
   run `35217578495` did not. Each result retains its limits. Live terminal behavior still requires evidence; the live Kata
   test is deferred. See [the integration record](upstream-dashboard-integration.md).
3. Qualify native external DNS enforcement and failure behavior. Fixed-address
   isolation and address replacement have complete workflow evidence, retained
   in the history; they do not establish DNS failover behavior.
4. Complete live OpenShell Sandbox execution and the deferred Kata checks.
   Hypershell main `dbd8363` now contains the qualified configured Sandbox
   setup. Full run `35467762040` at `674c6e5` passed 329 top-level tests and
   844 cases. Source `7f81556` adds only named read permissions to the test
   inspector and checks those permissions; production source is unchanged.
   Live run `35470884946` passed all 11 required Gateway workflow tests at
   `7f81556`, with compiler `d3ccd11`. The promoted commit adds only acceptance
   records to that live source.

   The main application repeat at `dbd8363` also passed 329 top-level tests and
   844 cases in run `35472230170`. Browser, UI, image, and both restart modes
   passed. Five conditional exclusions and the deferred Kata job remain in
   the [source-specific result](https://github.com/jsell-rh/hypershell-stego/blob/876d193/acceptance/sandbox-activation-main-full-evidence.json).

   The live test checked REST and gRPC, filtered lists, denied writes, events,
   restart, deletion, telemetry, and worker Sandbox setup before and after
   recovery. All 1,502 source hashes and 417 generated file hashes matched.
   It checked 34 permissions, four denied Sandbox writes, six admission
   denials, 24 Gateway network paths, and 40 native Sandbox packet paths.
   Of the Sandbox paths, eight were allowed and 32 were denied. Test resources
   and the temporary native RuntimeClass were removed. All 32 standing
   installation resources were restored unchanged, and the shared lease is
   free. See the [complete live result](https://github.com/jsell-rh/hypershell-stego/blob/dbd8363ed2aeb77483fe2cfb98a31f84e808ac18/acceptance/sandbox-activation-live-evidence.json).

   The test used a PostgreSQL process restart within the fixture Pod; it does
   not prove RDS failover. Native packet probes do not prove VM isolation or
   OpenShell Sandbox execution. The user deferred the live Kata test because
   no suitable cluster is available. Production capacity remains unproved.

   The pinned upstream Agent Sandbox controller has no namespace-only entry
   point. Its default permissions permit workload changes across namespaces.
   On 2026-09-19, the user selected the unchanged upstream controller as
   trusted cluster infrastructure. Keep its cluster-wide permissions with the
   operator-managed controller. Do not build a namespace-scoped entry point.
   Gateway workers and Sandbox accounts retain their existing permission
   limits. Review the upstream installation when its pin changes. See the
   [selected boundary](sandbox-upstream-settings.md).
   Keep the current upstream workspace-copy user and ordinary socket volume.
   Add no mutation service or OpenShell fork for those fields.
5. Qualify the common key-refresh telemetry and its generated SSO startup.
   Candidate `2cd2237` emits fixed source and outcome values through the existing
   bounded logs, metrics, and trace runtime. It records no tokens, key IDs, key
   documents, paths, addresses, or provider error text. Focused runs `35472590761`
   and `35472880350` passed at their recorded sources. The latter includes both
   SSO peer modes, startup cancellation, deadline expiry, and runtime ownership.
   Corrected example output is committed from verified hosted archives.
   Intermediate source `a49bd5d` passed all six full jobs and 34 race-tested
   packages. Startup source `ea38962` passed the same compiler packages but
   failed the stale SSO example check. Candidate `2cd2237` corrects that output;
   all six jobs and 34 race-tested packages passed in run `35473062128`.
   The qualified implementation is merged with the current goal records.
   Exact main source `67c78e5` passed all six jobs and 34 race packages in
   run `35473552043`. Its signed immutable release is published. Independent
   source, module, build-policy, signature, and release-installation checks
   passed. See the [complete evidence](jwt-key-telemetry-evidence.json).
   The regenerated Hypershell candidate includes this common implementation
   through compiler `ee348b8`; its full checks passed at `51b2bb2`. Promotion
   to application main is complete at `0aa8f0d`. Hypershell uses its static
   public-key verifier. These rotating-key results
   do not prove a change to that authentication path or close C6.
6. Audit C1 through C7 and H1 through H3 against current source and complete
   workflows. Backup and restore, supported deployment recovery, complete
   telemetry coverage, full application parity, and measured capacity remain
   in scope. Preserve the one-writer requirement until cross-process fencing
   is implemented and verified. Whole-database rollback detection is open.

The [historical record](enterprise-history.md) preserves earlier decisions,
failed attempts, network and public workflow results, and source-specific
limits. It does not replace the completion requirements above. The goal remains
active until the full requested state is implemented and verified.


## Common compiler installation

STEGO now has a common installer for exact, authenticated Linux amd64 compiler
packages. The existing Hypershell compiler `0057370` is published in an immutable
release after its full compiler and signature gates passed. Installer source
`5fdc97c` passed the real release and local package installation check. Both paths
produced identical verified bytes without compiler execution. See
[installation evidence and limits](compiler-installation.md).

This supplies a durable package for one qualified compiler. Hypershell now uses the common installer in its regeneration scripts.
Automatic release qualification, complete offline inputs, and other supported
installation targets remain open. C3 is not complete.
