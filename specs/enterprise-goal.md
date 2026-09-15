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
| Complete Gateway API gate, 31 required tests | Hypershell `916f3a7`, compiler `e5b9931` | [API evidence](hypershell-pool-api.json): 899 source files, 230 generated files, four matching generation records, and complete cleanup. Includes pool metrics, cancellation, API restart, and collector failure and recovery. |
| Complete generated browser workflow | Hypershell `8551ad3`, compiler `1edd407` | [Browser evidence](hypershell-route-browser.json): SQL isolation, access rules, namespace recovery, account cleanup, encryption, session checks, and automatic cleanup. Does not verify the later pool or SQL client telemetry changes. |
| Complete supplied CNPG workflow | Hypershell `ccfa4a9`, compiler `5e9c89d` | [CNPG evidence](hypershell-cnpg-complete.json): failover, retained data and identities, and automatic cleanup. The operator installed the server; this is not unattended CI. |
| Shared pool factory | Compiler `5c5e7c9` | [Full CI](https://github.com/jsell-rh/stego/actions/runs/34961472255) passed. The earlier `e5b9931` run failed a stale registry version assertion; its failure remains recorded. |
| Private PostgreSQL client telemetry | Compiler `f2b09c0` | [Full CI](https://github.com/jsell-rh/stego/actions/runs/34961995199) passed, including generated runtime race checks and real SQL provisioning. Complete application evidence remains required. |
| Full Hypershell CI at the browser source | Hypershell `8551ad3` | [Run 34960101322](https://github.com/jsell-rh/hypershell-stego/actions/runs/34960101322) passed core acceptance, ordinary browser tests, console, and service image. Overall result is failure because CNPG and Sandbox jobs failed. |

The following runs are active or queued as of 2026-09-15. Confirm their current
state before taking further action. Do not restart a run because observation
timed out or a log is incomplete.

| Required check | Source | Run |
| --- | --- | --- |
| Complete browser pool metrics across restart and key rotation | Hypershell `916f3a7` | [34961014607](https://github.com/jsell-rh/hypershell-stego/actions/runs/34961014607), active; Job `stego-service-ci/service-check`, UID `1133fe9a-b8de-4f81-adf2-9b1f5a9440a7` |
| Full CI for the pool changes | Hypershell `916f3a7` | [34961014648](https://github.com/jsell-rh/hypershell-stego/actions/runs/34961014648), active |
| API gate with private SQL client telemetry | Hypershell `2af1b46`, compiler `f2b09c0` | [34962176232](https://github.com/jsell-rh/hypershell-stego/actions/runs/34962176232), queued |
| Complete browser SQL creation, cleanup denial, and recovery signals across worker restart | Hypershell `2af1b46`, compiler `f2b09c0` | [34962176339](https://github.com/jsell-rh/hypershell-stego/actions/runs/34962176339), queued |
| Full CI with private SQL client telemetry | Hypershell `2af1b46`, compiler `f2b09c0` | [34962176260](https://github.com/jsell-rh/hypershell-stego/actions/runs/34962176260), queued |

The next action is to collect and verify these results, then fix failures without
weakening the gate. The [pool metric contract](database-pool-metrics.md) and
[SQL client signal contract](postgres-client-observability.md) define the new
requirements. A queued test, successful compilation, or partial workflow is not
a passing application result.

Known release gaps include unattended CNPG CI, public Gateway connectivity,
actual RDS operation, Sandbox execution on a Kata-capable cluster, backup and
restore evidence, and measured capacity. The public route trust and ownership
choices, an identified disposable RDS target, and a suitable Kata cluster still
need user input or an external resource. These gaps do not replace the original
C1–C7 and H1–H3 requirements. Completion also requires a requirement-by-requirement
audit against the original assessment, component contracts, and current output.

The [admission attempt record](pinned-resource-admission.md) preserves four
failed probes. That renderer is withdrawn from generated output. Do not restore
it until its complete live gate passes. Do not restart the cluster API server
to make a probe pass. The
[RDS gate](rds-acceptance.md) requires a real supplied server and evidence for
its permissions, isolation, TLS, and failover. The
[shared observability requirement](shared-observability.md) still includes
complete process and controller coverage, failure isolation, and measured cost.
Service startup and other unbound entry points retain separate open checks.

Keep both repos up to date on remote with atomic commits. The user authorized
direct pushes; do not wait for pull request merges. Follow [AGENTS.md](../AGENTS.md):
no Playwright, no local performance or stress tests, small ordinary local checks,
and bounded CI or jshell tests with one live test at a time. Use the saved jshell
context explicitly and preserve unrelated workloads. Inspect interrupted runs
before starting another run. Keep credentials out of output. The restricted CI
credential was last renewed through 2026-09-15 12:19:07 UTC; check its remaining
lifetime before a queued run starts.

Keep this file limited to current requirements and result links. Put detailed
measurements and failed attempts in their feature evidence files. Preserve the
full historical record. Ask the user about critical application or trust-boundary
choices; resolve routine implementation choices within the authorized scope.
