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

The following current results were independently checked. Each applies only to
its recorded source and test scope:

| Result | Source | Evidence and limits |
| --- | --- | --- |
| Complete Gateway API gate, 31 required tests | Hypershell `752d92e`, compiler `a355306` | [API evidence](hypershell-shared-jwt-api.json): 911 source files, 231 generated files, four matching generation records, and complete cleanup. Includes all four worker startup failures with local and TLS OTLP records, pool metrics, cancellation, API restart, and collector failure and recovery. |
| Complete generated browser workflow and private SQL telemetry | Hypershell `752d92e`, compiler `a355306` | [Browser evidence](hypershell-shared-jwt-browser.json): correlated SQL logs, traces, and metrics across worker restart, cleanup denial, and recovery; all three console pool checkpoints; SQL isolation; access rules; namespace recovery; account cleanup; encryption; session checks; and automatic cleanup. |
| Complete supplied CNPG workflow | Hypershell `ccfa4a9`, compiler `5e9c89d` | [CNPG evidence](hypershell-cnpg-complete.json): failover, retained data and identities, and automatic cleanup. The operator installed the server; this is not unattended CI. |
| Shared pool factory | Compiler `5c5e7c9` | [Full CI](https://github.com/jsell-rh/stego/actions/runs/34961472255) passed. The earlier `e5b9931` run failed a stale registry version assertion; its failure remains recorded. |
| Private PostgreSQL client telemetry | Compiler `f2b09c0` | [Full CI](https://github.com/jsell-rh/stego/actions/runs/34961995199) passed, including generated runtime race checks and real SQL provisioning. Complete application evidence remains required. |
| Worker setup and cleanup telemetry | Compiler `f97b315` | [Full CI](https://github.com/jsell-rh/stego/actions/runs/34964671704) passed, including generated controller race checks and real SQL provisioning. Four generated Hypershell workers passed the TLS OTLP startup check within the complete API gate; the complete browser gate also passed. |
| Full Hypershell CI at the browser source | Hypershell `677973f` | [Run 34964891409](https://github.com/jsell-rh/hypershell-stego/actions/runs/34964891409) passed core acceptance, ordinary browser tests, console, and service image. Overall result is failure because CNPG and Sandbox jobs lack installation fixtures and restricted runners. |

The following results were checked on 2026-09-15. Confirm the active run
state before taking further action. Do not restart a run because observation
timed out or a log is incomplete.

| Required check | Source | Run |
| --- | --- | --- |
| Full CI with the shared JWT runtime | Hypershell `752d92e`, compiler `a355306` | [34967271365](https://github.com/jsell-rh/hypershell-stego/actions/runs/34967271365), failed only the absent CNPG and Sandbox fixtures; core, ordinary browser, console, and image jobs passed |
| Complete restricted CNPG workflow and full CI | Hypershell `b6e0434`, compiler `a355306` | [34969545259](https://github.com/jsell-rh/hypershell-stego/actions/runs/34969545259), CNPG job failed before application execution; Pod readiness timed out, then cleanup used a forbidden namespace list. Manual recovery is complete; [failure and cleanup evidence](hypershell-cnpg-ci-first-run.json) is recorded. |

Hypershell `59a6d32` includes controller-owned address observations and a corrected
CNPG cleanup check through the existing allocator client. Its API run
[34972072027](https://github.com/jsell-rh/hypershell-stego/actions/runs/34972072027)
refused to start while recovery held the live-test Lease. It is not an application
result. Its second attempt was canceled while pending to add the required
endpoint ownership check. API run `34972191691` uses Hypershell `4977b62` and
requires 32 checks. Browser run `34972072109` is active, and full CI `34972073256`
was canceled while pending when newer source was pushed. The failed CNPG run has no remaining runtime or volumes, and its
Lease was released. Verify credentials before another live attempt.

Full CI `34969545259` is now complete. Core acceptance, ordinary browser checks,
console, and service image passed. CNPG and the old Sandbox job failed. The
Sandbox source precedes the explicit live-test deferral. The overall result
remains failure. Do not treat manual CNPG recovery as an automatic cleanup pass.

Hypershell `ace5823` uses compiler `74d9a70` for shared public TLS Secret
verification. Focused tests and repeated generation passed. Full compiler run
`34973403349` and application runs `34973489877`, `34973489579`, and
`34973489648` remain required. Public certificate support is still incomplete
application evidence: Route creation, verified public RPC, and address
publication must be tested together. See the
[public connection contract](hypershell-external-connection.md).

The next action is to collect and verify these results, then fix failures without
weakening the gate. The [pool metric contract](database-pool-metrics.md) and
[SQL client signal contract](postgres-client-observability.md) define the new
requirements. A queued test, successful compilation, or partial workflow is not
a passing application result.

The original module coverage audit also found stale example output and no CI
jobs for either nested example module. The [example project checks](example-project-checks.md)
restore current generation and add CI gates for both projects. Their build and
test results remain required; the root compiler suite does not cover them.
The first example gates failed on generated dependency vulnerabilities. The
corrected modules in `6e2b514` passed both example jobs in
[CI 34966021202](https://github.com/jsell-rh/stego/actions/runs/34966021202). The
overall run failed a stale REST registry version assertion, now corrected.

The [SSO authentication audit](sso-auth-audit.md) reproduced acceptance of tokens
with the wrong issuer, wrong audience, and no expiry. Compiler `94f9fa0` replaces
that separate verifier with the shared JWT runtime and a bounded key source.
Expanded generated race tests pass. Both examples were regenerated in `a355306`.
[Full CI 34966748920](https://github.com/jsell-rh/stego/actions/runs/34966748920)
passed, including both examples, generated authentication race checks, and real
SQL provisioning. Hypershell `752d92e` adopts this verified compiler; its complete API and browser gates passed;
full CI failed its old CNPG and Sandbox fixture jobs as recorded above. Key-source telemetry and performance
evidence remain separate open requirements. These checks do not close all of
C4 or C6.

The restricted CNPG CI path is committed in Hypershell `a0402c8`, with its
lifetime policy correction in `b6e0434`. The
[initial static evidence](hypershell-cnpg-ci-installation.json) verifies 45
objects and ready webhook trust. The
[policy repair evidence](hypershell-cnpg-ci-lifetime-admission.json) preserves
four unwanted requests accepted by the old policy, the applied repair, and all
18 passing Job admission probes. The current installation adds one default deny
network policy and a stricter lifetime Job policy. Independent reads found no
runtime resources and a free Lease. The complete restricted workflow above
remains required; these dry-run checks are not a CNPG application pass.

The first queued CNPG workflow was canceled before execution so that the
corrected source could run. Duplicate ordinary API and browser jobs from these
CI-only pushes were also canceled before execution. They are not passes. The
[CI contract](https://github.com/jsell-rh/hypershell-stego/blob/codex/namespace-allocation-20260912/acceptance/cnpg-ci.md)
records all run IDs and the remaining acceptance checks.

Known release gaps include unattended CNPG CI, public Gateway connectivity,
external PostgreSQL contract coverage, backup and restore evidence, and measured capacity.
On 2026-09-15, the user selected TLS passthrough with an operator-selected issuer
and controller ownership of `route_address`. These accepted choices still need
implementation and a complete [public connection check](hypershell-external-connection.md).
The user approved PostgreSQL containers for the external database gate. No RDS
test instance exists. Terraform creates RDS outside Hypershell. AWS-specific
operation remains unverified; it is not a required live test for this gate.

On the same date, the user deferred the live Kata Sandbox test because no
suitable cluster is available. CI must show this test as skipped, not passed.
Keep ordinary code, protocol, authorization, and count-controller checks active.
Current VM isolation and runtime capacity remain unverified. The earlier kind
fixture is withdrawn and must not be run. Resume the live test when a suitable
cluster and restricted identity are available. These gaps do not replace the original
C1–C7 and H1–H3 requirements. Completion also requires a requirement-by-requirement
audit against the original assessment, component contracts, and current output.

The [admission attempt record](pinned-resource-admission.md) preserves four
failed probes. That renderer is withdrawn from generated output. Do not restore
it until its complete live gate passes. Do not restart the cluster API server
to make a probe pass. The
[external PostgreSQL gate](rds-acceptance.md) uses a supplied container server
to test permissions, isolation, verified TLS, recovery, and cleanup. The
[shared observability requirement](shared-observability.md) still includes
complete process and controller coverage, failure isolation, and measured cost.
Service startup and other unbound entry points retain separate open checks.

Keep both repos up to date on remote with atomic commits. The user authorized
direct pushes; do not wait for pull request merges. Follow [AGENTS.md](../AGENTS.md):
no Playwright, no local performance or stress tests, small ordinary local checks,
and bounded CI or jshell tests with one live test at a time. Use the saved jshell
context explicitly and preserve unrelated workloads. Inspect interrupted runs
before starting another run. Keep credentials out of output. The restricted CI
credential was last renewed through 2026-09-15 13:37:23 UTC; check its remaining
lifetime before a queued run starts.

Keep this file limited to current requirements and result links. Put detailed
measurements and failed attempts in their feature evidence files. Preserve the
full historical record. Ask the user about critical application or trust-boundary
choices; resolve routine implementation choices within the authorized scope.
