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
| C3 | Reproducible and recoverable generation | Compiler and input identity, dependencies, state format, interrupted writes, concurrent apply | Active; offline module inputs closed at `d43e696d` (PR #2, run `35950507614`; see [application-delivery-evidence.json](application-delivery-evidence.json)); [remaining recovery gaps](compiler-recovery-audit-20260917.md) |
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

Hypershell main `7f0bd9f` contains accepted runtime source `b5ee359` and
compiler `52306a6b`. STEGO now generates the Role response model and the
bounded JSON object conversion for the permissions field. Hypershell keeps
authentication, Role lookup, filtering, paging, projection, and grant policy.
The conversion preserves JSON numbers without floating-point conversion and
returns private errors for malformed objects, invalid Unicode, duplicate
decoded keys, and bound violations.

All 14 hosted candidate groups passed. API run `35884290611` passed all 59
required roots with 93 cases and no failures. Browser run `35894616139`
passed its complete scenario in 571.58 seconds using fixture `eb7be18e` and
image run `35592854743`. Repeated generation matched all 436 hashes and the
suite left them unchanged. All seven signed images passed the common checks
and the public Gateway connection was verified. Both test fixtures and
allocations were absent after cleanup, no live Job remained, and all 17
standing access checks passed. See the
[workflow evidence](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/role-workflow-evidence.json).

Four dispatch attempts failed before the accepted browser run: two expired
one-hour CI credentials, one registry policy that omitted the operator blob
origin, and one credential that expired while queued behind the shared
live-test group. The operator refreshed the credential and supplied the
operator registry policy. The failed records are retained in the workflow
evidence.

The measured Gateway had 100 accounts with verified token issuance. The
observed cleanup upper bound was 36.01 seconds against the 30-second target.
The target and 100-Gateway capacity remain unproved. RDS failover, live Kata,
and upstream OpenShell Sandbox execution remain outside this proof. C3
through C7 and H1 through H3 remain open.

## Accepted gRPC grant baseline

Hypershell main `c42b2ed` contains accepted runtime source `62adcf0` and compiler
`3666fed3`. STEGO now generates the Gateway and grant REST and protobuf response
mappings. Hypershell keeps domain selection, access rules, grant policy, watch
replay, and public error policy.

All 14 hosted candidate groups passed. The complete suite passed 1,422 cases in
397 roots and retained all 1,404 prior cases. The focused suite passed 214 cases
in 27 roots. API run `35579620505` passed all 56 required roots, including the
stored grant fault across restart. Browser run `35580560335` passed all 11
required roots; its full scenario took 561.88 seconds. Review verified 1,731
source files, 435 generation hashes, seven signed images, all four rendered
images, correlated logs, metrics, and traces, and complete cleanup. Both test
fixtures and allocations were absent. The test lock was free and all 32
standing resources were unchanged. See the
[workflow evidence](grpc-grant-workflow-evidence.json).

A trace evidence reader initially used the previous full CI run path. A
separate corrected reader verified the current source and all unchanged trace
requirements against the same collected result. No cluster test was repeated
for this reader error. The initial failed reader record is retained.

The measured Gateway had 100 service accounts. The observed cleanup upper
bound was approximately 31.65 seconds. The 30-second target and 100-Gateway
capacity remain unproved. This result does not cover RDS failover, live Kata,
or upstream OpenShell Sandbox execution. C3 through C7 and H1 through H3 remain
open. The exact main checks at `c42b2ed` also passed: 13 automatic hosted groups,
1,422 core cases, 214 focused cases, 56 API roots, repeated generation, and
seven signed images. API cleanup preserved all 32 standing resources. Only
four acceptance documents differ from the accepted browser and event source;
no separate main browser or event run is claimed. See the
[main evidence](grpc-grant-main-evidence.json).

## Accepted Gateway protobuf baseline

Hypershell main `c583681d` accepts runtime source `047f550f` and compiler
`82439300`. STEGO now generates every Gateway protobuf response field, including
bounded conversion of stored JSON string lists. Hypershell selects the current
domain view first. Access, observation, placement, release, and public error
rules remain in the application.

The candidate passed 1,335 core cases across 380 roots, including all 1,314 prior
cases, 141 focused cases, five restart cases, 11 provider roots, regeneration,
and seven image reviews. The complete live workflow passed all 11 required tests
in 562.45 seconds. Review checked 1,713 source files, 432 generation hashes,
exact compiler bytes, REST and gRPC access, events, restart, correlated telemetry,
four browser images, and cleanup. All seven evidence readers completed without
error. Both test fixtures were absent, the Lease was free, and all 32 standing
resources were unchanged. See the [consumer evidence](gateway-mapping-consumer-evidence.json).

Exact main checks at `c583681d` also passed independent review. All ten CI runs
passed. They retained all 1,335 core cases across 380 roots, 52 required live API
roots, and 141 focused cases. Repeated generation matched all 431 generated and
module files. Image review authenticated seven candidate images and checked
registry fixture bytes. This image evidence does not establish production
publication or production CA adoption. API cleanup released the Lease and
preserved all 32 standing resources. See the
[main evidence](gateway-mapping-main-consumer-evidence.json).

Normal cleanup with 100 accounts took an observed 33.04 seconds. The 30-second
target remains unmet. REST conversion, grant mapping, worker configuration,
capacity, C3 through C7, and H1 through H3 remain open. This result does not
explain the historical event timeout.

## Accepted catalog mapping baseline

Hypershell main `ccf350b1` accepts runtime source `b145ee5b` and compiler
`6d68417d`. STEGO now generates the catalog protobuf response mappings.
Hypershell declares the public fields and retains access checks, domain
operations, observation selection, and public error policy.

The candidate passed 1,314 core cases, all 120 focused response cases, five
restart cases, 11 provider test roots, repeated generation, and seven image
reviews. Its complete live workflow passed all 11 required tests in 561.52
seconds. Review checked 1,707 source files, 431 generation hashes, exact compiler
bytes, REST and gRPC access, events, restart, telemetry, four browser images,
and cleanup. All seven evidence readers completed without error. The test
fixtures were absent, the Lease was free, and all 32 standing resources were
unchanged. See the [consumer evidence](catalog-mapping-consumer-evidence.json).

Exact main checks at `ccf350b1` also passed independent review. They retained all
1,314 core cases across 376 test roots and passed all 52 required live API roots,
120 focused cases, repeated generation, and seven image reviews. API cleanup
removed the test resources, released the Lease, and preserved all 32 standing
resources. See the [main evidence](catalog-mapping-main-consumer-evidence.json).
These checks identify the catalog baseline. The Gateway result above has its
own source and live workflow proof.

Normal cleanup with 100 accounts took an observed 32.70 seconds; the 30-second
target remains unmet. Gateway and grant
mapping, REST conversion, remaining worker configuration, and C3 through C7 and
H1 through H3 remain open. The historical event timeout is not explained by
this result.

## Accepted queue claim baseline

Hypershell main `dc6cd0e6` accepts runtime source `06cc36b2` and compiler
`65b18de8`. The common queue now returns no partial delivery batch after a row
iteration error. A database write can still have committed, so existing leases
remain subject to normal expiry and receipt checks. Worker behavior, lease and
attempt limits, and Hypershell policy are unchanged.

The candidate passed 1,294 core cases, five restart cases, 11 provider test roots,
and seven image reviews. The complete live workflow passed all 11 required tests
in 533.05 seconds. Independent review checked 1,694 source files, 430 generated
hashes, live compiler bytes, REST and gRPC access, event delivery, restart,
telemetry, four browser views, and cleanup. All seven evidence readers completed
without error. Cleanup removed test resources, released the Lease, and preserved
all 32 standing resources. See the [consumer evidence](queue-claim-consumer-evidence.json).

Normal cleanup with 100 accounts took an observed 32.26 seconds. The 30-second
target remains open. This change does not explain the historical event timeout.
The exact main checks at `dc6cd0e6` also passed. Independent review checked
1,294 core cases, all 52 required live API tests, regeneration, and seven signed
images. Cleanup preserved the 32 standing resources and released the Lease.
Hypershell evidence commit `caa0dfb3` records those results; it has no runtime
change. The accepted workload construction, dependency
conversion, and checked timestamps remain in the generated runtime. Complete
response mapping, remaining worker configuration, and the enterprise requirements
are still open.

## Accepted checked timestamp baseline

Hypershell main `52001546` accepts runtime source `967bc1d8` and compiler
`f18286e8`. It retains the accepted common workload construction and dependency
conversion. Gateway, grant, catalog, and cleanup summary responses now use the
same generated checked timestamp conversion. Hypershell retains field selection,
observation policy, access checks, and public error contracts.

The full application check passed 1,294 core cases, including all 1,194 prior
cases. All 100 focused timestamp cases and seven independent image checks passed.
The complete live workflow passed all 11 required tests in 547.86 seconds.
Review checked source identity, 430 generation hashes, telemetry, four browser
views, and cleanup. Test resources were absent, the shared Lease was free, and
all 32 standing resources were unchanged. Three evidence reader failures remain
recorded with their replacement or saved-record checks. No live test was repeated.
See the [consumer evidence](checked-timestamp-consumer-evidence.json).

Normal cleanup with 100 accounts took an observed 30.84 seconds. The 30-second
target remains open. The test-only restart change uses the existing queue lease
recovery bound before the unchanged event read. It does not change production
timeouts or explain the historical event failure. Exact main checks at
`52001546` also passed: 1,294 core cases, 100 focused timestamp cases, five
restart cases, 11 provider test roots, seven image checks, regeneration, and all
52 live API tests. API cleanup released the Lease and preserved all 32 standing
resources. Hypershell main `8968fa09` adds the result records. See the
[main evidence](checked-timestamp-main-consumer-evidence.json).

## Accepted workload dependency baseline

The workload dependency baseline is Hypershell main `ab9d6cb1`, with runtime source
`c3e88f16` and compiler `d8c37d20`. STEGO constructs Gateway and browser
Deployments, security settings, probes, mounts, resource limits, and configuration
digests. It also validates and converts workload dependency data. Hypershell
supplies dependency selection, ownership, placement, OpenShell settings, and
domain rules. The application no longer has a separate dependency converter.

The full candidate check passed 1,194 core cases across 368 top-level tests.
The five recorded exclusions and the deferred Sandbox test remain explicit.
All seven images passed independent source, content, and signature checks.
The complete browser workflow passed all 11 required tests in 539.60 seconds.
Review checked 1,674 source files, 429 generation hashes, telemetry, four browser
views, and cleanup. The ready console had the common digest on its Deployment,
ReplicaSet, and Pod. See the
[browser evidence](workload-dependency-browser-evidence.json) and the
[application boundary](https://github.com/jsell-rh/hypershell-stego/blob/a1cdd3e621e97cc40df03206a7b687fd45eb97fe/acceptance/workload-dependency-conversion.md).

A separate regeneration check matched all 428 generated and module files,
419 output hashes, and 41 input hashes. The patch was empty. Live cleanup
removed all test resources, released the Lease, and preserved all 32 standing
resources. Normal cleanup with 100 accounts took an observed 32.34 seconds.
The 30-second target remains open. Exact main checks at `a1cdd3e6` also passed:
the full suite retained all 1,194 core cases, and the live API check passed all
52 required tests. Its cleanup preserved the 32 standing resources and released
the Lease. The later commits add only result records. See the
[main full result](https://github.com/jsell-rh/hypershell-stego/blob/ab9d6cb1cca1f622b5fc54c4e1df6449707fb71e/acceptance/workload-dependency-main-full-evidence.json)
and [main API result](https://github.com/jsell-rh/hypershell-stego/blob/6c5fc1a760a78c86c12aab6e2290eb2e418f2546/acceptance/workload-dependency-main-api-evidence.json).

An earlier main check at `7e6d3f1` failed to receive one event within ten seconds
after API restart. Two queued rows still had leases when the test failed.
The later candidate check passed, but it does not explain that failure. Keep
this recovery issue open. See the
[failure evidence](event-restart-failure-evidence-20260921.json).
These results do not close the remaining enterprise requirements.

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

The [workload boundary review](workload-boundary-review-20260920.md) records
the common builder, dependency conversion, and console annotation assembly.
These mechanisms are accepted in the generated runtime. Keep the upstream
Sandbox setup and application policy outside the restricted workload profile.

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
Hypershell source `c3e88f1` removes its local conversion helper and adds two
invalid-data cases. It is accepted after complete regeneration, application,
image, and live workflow review. See the
[dependency release evidence](workload-dependency-main-evidence.json) and
[consumer workflow evidence](workload-dependency-browser-evidence.json).

## Remaining requirements

The completion table above remains authoritative. The following work is open;
a narrower passing check cannot close a broader requirement.

| Area | Required next evidence or implementation |
| --- | --- |
| C3: generation and delivery | Controlled offline inputs are implemented and checked: `stego build download` produces a zips-only module cache, and `stego build --module-cache` builds offline under `go.sum` enforcement with `dependency_proxy: "off"` records ([delivery evidence](application-delivery-evidence.json)). Remaining: supported installation targets, production CA profile, and durable release qualification. Retain interruption, conflicting-edit, and concurrent-apply tests. |
| C4: authentication and isolation | Current-source audit complete at Hypershell `7f0bd9f` and STEGO `4d0ce01a`; see [auth-isolation-audit-20260923.md](auth-isolation-audit-20260923.md). Signature, issuer, audience, expiry, rotation, browser sessions, grants, and denied cross-tenant operations audited with focused passing checks. DNS-aware enforcement and endpoint failure behavior qualified at the supported public Gateway TLS path. New live denial evidence from the current source can extend this; no open defect found. |
| C5: storage and events | Complete transaction, migration, durable delivery, recovery, backup, and restore coverage. Preserve the one-writer requirement until cross-process fencing is implemented and verified. Whole-database rollback detection is implemented (STEGO PR #3, merged `2f080620`): generated stores track `database_id` plus a `nextval` write epoch and reject restored or replaced databases with `ErrDatabaseRollback`, covered by four passing generated tests and full CI. |
| C6: runtime | Complete health, readiness, timeout, shutdown, resource-limit, and all-signal telemetry coverage. Direct finite controller helpers need explicit operation and parent contracts without duplicate telemetry owners. The earlier recovered browser initialization failure still lacks a proved cause. |
| C7: compiler contracts | Audit typed wiring, capability validation, extension points, and compatibility against the original assessment. Generated Go compilation does not by itself prove a complete component contract. |
| H1: application contracts | Complete parity evidence for REST, gRPC, RBAC, watches, SDK, CLI, UI, deployment, and the upstream dashboard terminal contract. |
| H2: common mechanisms | Complete the remaining common transport mapping from proved workflows. Keep ownership, grants, placement, release selection, OpenShell configuration, and UI policy in Hypershell. Retain the accepted workload and dependency mechanisms and clean generation without rh-trex-ai. |
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


## Declared response mapping release

Compiler `6d68417d` now provides typed protobuf response mappings with complete
field coverage checks, owned output data, optional value checks, and checked
numeric, UTF-8, and timestamp conversions. The mechanism uses provider model
contracts and protobuf descriptors. It contains no Gateway type or policy.

The [release evidence](response-mapping-release-evidence.json) records the
compiler, generated runtime, example, database, signature, and installation
checks. The catalog consumer and its complete candidate workflow are now
accepted above. This release does not close H1 or H2. Gateway response mapping,
REST mapping, stored JSON adoption, and domain view inputs remain open.


## Bounded stored JSON conversion release

Compiler `82439300` adds bounded JSON string-list conversion to the common
protobuf mapper. Declarations require limits for encoded bytes, item count,
and decoded item bytes. The compiler checks the combined mapping budget.
The runtime rejects malformed input, invalid Unicode, and limit violations
without a partial response or supplied error data. It contains no DNS policy.

The independent Shipment fixture passed 104 runtime cases in two namespaces
with the provider-declared dependencies. All 12 compiler test groups, 37 compiler
packages, both generated examples, 34 timestamp cases, and database access
checks passed. Signatures and a separate immutable release installation were
verified. See the [release evidence](json-string-mapping-release-evidence.json).
Hypershell Gateway generation and its full application checks remain required.
This release does not close C3 through C7 or H1 through H3.

## Released scalar REST response mapping

Compiler `ca25ab06` provides generated public OpenAPI types and checked scalar
REST conversions. It uses actual backend field declarations, requires complete
property coverage, and rejects invalid mappings before output writes. Generated
conversions preserve declared presence, own their pointers, and return a fixed
error without a partial response on failure.

All 38 tested compiler packages, both examples, focused backend and generated
runtime checks, and database access checks passed. The main artifact signature
and separate installation of the immutable release were verified. See the
[release evidence](rest-response-mapping-release-evidence.json).

Hypershell catalog source preparation at `6b35ea25` still requires generation
and full application acceptance. Array conversion, three-state nullable inputs,
prepared domain views, and Gateway and grant REST mapping remain open. This
release does not close C3 through C7 or H1 through H3.


## Released protobuf application inputs

Compiler `3666fed3` adds declared, typed application inputs to protobuf response
mappings. REST and protobuf use the same bounded input declaration parser.
Applications retain domain queries and access checks. The generated mapper
checks and copies declared values, preserves optional presence, and returns no
partial response on conversion failure. Input declarations cannot supply Go
expressions, imports, callbacks, or field paths.

All six candidate and main check groups passed. The result covers 39 compiler
packages, both generated examples, 63 protobuf runtime cases in each of two
package layouts, 34 timestamp cases, and existing REST, SDK, and database
checks. The candidate and main compiler bytes match. Release signatures, four
immutable assets, and a separate five-file installation were verified. See the
[release evidence](protobuf-prepared-input-release-evidence.json).

The Hypershell Gateway and grant REST workflow at `f1366d1` uses compiler
`f972410b`. Its complete workflow is accepted below. The gRPC grant mapper at `62adcf0` uses compiler `3666fed3`. Its complete
application and live workflow are accepted in the current baseline above. Domain grant selection, access checks, replay,
and response-size policy stay in Hypershell. This compiler release does not
close C3 through C7 or H1 through H3.

## Accepted Gateway and grant REST workflow

The service declares all 24 Gateway and nine grant response fields. STEGO owns
checked conversion, output copies, optional presence, enum checks, and bounded
stored JSON conversion. Hypershell retains observation selection, creator
resolution, access rules, list behavior, and public error policy.

All 14 hosted application check groups passed for `f1366d1`. The complete suite
passed 1,404 cases across 393 top-level tests and retained all 1,370 prior cases.
The response suite passed 199 cases. Regeneration matched all 434 generated and
module files, 425 output hashes, and 41 inputs. The API workflow passed all 55
required roots, including grant behavior across REST, gRPC, events, and restart.

[Browser run 35575801502](https://github.com/jsell-rh/hypershell-stego/actions/runs/35575801502)
passed all 11 required roots. Its main scenario took 550.42 seconds. Independent
review checked 1,728 source files, 435 live generation hashes, seven signed
images, all four screenshots, and correlated logs, metrics, and traces. It
covered account lifecycle, provider outage, restart, and durable deletion with
namespace finalization. Cleanup removed the test fixtures and owned allocations,
released the shared test lock, and preserved all 32 standing resources.

The measured Gateway had 100 service accounts. Complete cleanup had an observed
upper bound of 32.203 seconds. The 30-second target and 100-Gateway capacity
remain unproved. This check does not cover RDS failover, live Kata, or upstream
OpenShell Sandbox execution. See the
[workflow evidence](rest-gateway-grant-workflow-evidence.json). C3 through C7 and
H1 through H3 remain open.

## Released nullable scalar response policy

Compiler `9788915c` requires an explicit `on_absent` policy when a pointer source
supplies a nullable scalar response field. `emit_null` preserves JSON null;
`omit` is permitted only for an optional field that can be omitted. Present
empty strings, false values, numeric zero, and zero time remain present.
Generated values own their storage. Invalid strings, enums, numbers, and
timestamps return a private error with no partial response.

All four candidate and main check groups passed independent review. These
checks cover 39 compiler packages, both generated examples, HTTP generation and
preflight checks, two SDK modes, and database access. The generated nullable
runtime checks cover presence, owned values, invalid values, and numeric bounds.
Candidate and main compiler bytes match. Signatures, four immutable release
assets, and a separate five-file installation were verified. See the
[release evidence](nullable-response-release-evidence.json).

This mechanism maps two source states to an explicit subset of JSON states.
Full three-state inputs and nullable enum, object, and array mappings remain
unsupported. Hypershell service-account adoption passed the complete Gateway
workflow described above. This release does not close the enterprise goal.

## Released bounded JSON object response conversion

Compiler `52306a6b` adds declared conversion from stored JSON to a free-form
REST response object. The declaration requires byte, node, depth, and scalar
bounds and an explicit absent-value policy. The generated converter preserves
number text and present empty objects. It rejects invalid encoding, duplicate
decoded names, unsupported root values, and exceeded bounds. A failure returns
no partial response and no supplied data in the error. Domain selection and
access checks remain in the application.

All five candidate and main check groups passed. The checks cover 39 compiler
packages, both generated examples, OpenAPI mapping and preflight checks, both
Go SDK modes, retained protobuf mappings, and database access. Candidate and
main compiler bytes match. Main signatures, four immutable release assets,
and a separate five-file installation passed verification. See the
[release evidence](json-object-response-release-evidence.json).

Hypershell candidate `b5ee359` uses this compiler for Role responses. Its complete
application and live workflow checks are still pending. Compiler acceptance
does not establish application acceptance. Nullable and schema-constrained
object conversions remain unsupported. C3 through C7 and H1 through H3 remain
open.
