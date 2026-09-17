# Isolated PostgreSQL databases

On 2026-09-17, the user selected externally supplied PostgreSQL as the only
Hypershell database path, following
[Hypershell PR 300](https://github.com/openshift-online/hypershell/pull/300).
This replaces the earlier CNPG requirement. Hypershell must not create database
servers, install database operators, or select database providers. Each Gateway
gets a separate logical database, login, permissions, and credentials on the
supplied server. Support for existing connections alone is insufficient.

STEGO must supply reusable database provisioning and verification mechanisms.
Hypershell supplies Gateway identity, assigned-controller ownership, and locality.
On 2026-09-14, the user selected the controller-local database model in
[Hypershell PR 272](https://github.com/openshift-online/hypershell/pull/272),
reviewed at `d61f1dffe639f1de6e67cd82d89282fedb0d15e6`.
This supersedes the API database registration and selection model.
The Gateway API must have no `database_id`, including an empty placeholder,
and no `ManagedDatabase` entity. The installation supplies each execution
controller with its local PostgreSQL connection and administrative credential
reference. Gateway databases belong beside their Gateways and controller.
The operator owns the supplied PostgreSQL server infrastructure. The application
controller owns only Gateway SQL resources and credentials.

Required evidence includes creation, current ownership checks, concurrent
reconciliation, restart after partial creation, credential loss, cleanup, verified
TLS, bounded execution, and private diagnostics. Actual connection tests must
prove that each login can access its database and cannot access other databases.

PostgreSQL grants `CONNECT` and `TEMPORARY` to `PUBLIC` on databases by default.
A separate login alone does not prove database isolation. The provisioning
contract must account for existing server access rules and must not silently
change access for unrelated workloads. See the PostgreSQL
[privilege rules](https://www.postgresql.org/docs/18/ddl-priv.html) and
[connection authentication rules](https://www.postgresql.org/docs/18/auth-pg-hba-conf.html).

`CREATE DATABASE` cannot run inside a transaction block. Recovery must therefore
handle partial provisioning and verify ownership before further effects. See
[CREATE DATABASE](https://www.postgresql.org/docs/18/sql-createdatabase.html).

`postgres-client` 1.1.0 adds the common SQL lifecycle API. Its
[contract](../registry/components/postgres-client/provisioning.md) specifies
ownership records, separate owner and login roles, verified TLS, connection
isolation, bounded calls, recovery, and deletion. It has no Gateway or cluster
selection logic. Logs, metrics, and traces exclude private SQL inputs.

The complete Hypershell controller workflow passed with an operator-supplied
PostgreSQL fixture. See the
[application evidence](hypershell-external-database-workflow.json).
The operator can create an RDS server with Terraform before installation.
No database registration API call is required.
The controller must never delete the supplied server or its storage. It must
preserve durable Gateway cleanup intent and reject a changed database destination
before it creates replacement data, changes credentials, or deletes objects.
The user authorized a bounded PostgreSQL container for this acceptance gate.
RDS creation is outside Hypershell scope. Container evidence does not prove
RDS permissions or operation; those remain explicit compatibility limits.

The transition requires matching API, controller, SDK, CLI, and console releases.
Retired protobuf field numbers and names remain reserved. Fresh schemas must
contain no database catalog or Gateway database reference. Old or unknown
schemas must be rejected before writes. Shared schema checks and serialized
bootstrap mechanisms belong in STEGO; Hypershell declares its schema generation
and application compatibility policy. Teardown is a separate operator action.
The new runtime must not migrate or remove existing installations implicitly.
The evidence below applies only to each recorded source. Earlier CNPG
requirements and pending checks are historical. The external-only contract
above controls current work.

## Earlier qualification records

The final focused jshell check passed on 2026-09-14 with a non-superuser
provisioning account. Seven generated tests passed with race detection. The
SQL lifecycle test took 2.07 seconds; the generated package took 3.300 seconds.
Two generation calls produced identical paths and bytes. All six generator and
test source files match the fixed source copy. The Job completed, its resources
were removed, and the shared live-test Lease was released. The
[evidence record](postgres-provisioning-evidence.json) includes failed attempts
and exact source hashes. These durations are test results, not benchmarks.

The lifecycle check covers separate database access, stable repeat calls,
credential repair, failed activation, new foreign grants, an interrupted create,
concurrent-call exclusion, deletion with an active login, retained foreign
dependencies, resumed deletion, replaced role OIDs, and deletion tombstones.
The signal test checks span parentage, a duration metric, a structured log event,
and exclusion of private values. Full compiler CI and its separate PostgreSQL
job passed in [run 34885672242](https://github.com/jsell-rh/stego/actions/runs/34885672242).


`postgres-adapter` 4.3 now supplies the optional
[fresh schema generation gate](../registry/components/postgres-adapter/schema-generation.md).
Eight generated PostgreSQL tests passed in jshell. They cover rejection before
initialization, restart, concurrent calls, interrupted connections, marker
permissions, and bounded lock waits.

Hypershell adopted this gate in
[`6e5116e`](https://github.com/jsell-rh/hypershell-stego/commit/6e5116e718bb602388f6f3945e5afa4351ace246).
The API has no database catalog or Gateway database field. REST and gRPC reject
the retired field, including an empty value. The application declares
`controller-local-v1`; its initial schema, role data, and event outbox commit
together. Legacy schema checks run before writes.

The application check at
[`425871f`](https://github.com/jsell-rh/hypershell-stego/commit/425871f64846cff7ac24d0d9b2a7db0c609e9f4f)
has passing results for 81 selected application tests. These include both SQL
and workload cleanup records, parent deletion, restart, and access rules.
The [application evidence](https://github.com/jsell-rh/hypershell-stego/blob/425871f64846cff7ac24d0d9b2a7db0c609e9f4f/acceptance/controller-local-extended-evidence.json)
also records the failed attempts and test limits. Old live fixtures still
prevent the full acceptance package from building. These results do not prove
the complete Gateway Pod, browser, or supplied SQL server workflow.


`postgres-client` 1.2.1 adds bounded session quarantine to `EnsureDatabase`.
An isolation error disables both recorded roles and stops their current sessions
across databases. It preserves other roles and their sessions. An enabled owner
login is also an isolation error. The runtime checks the stored OIDs, uses a
positive termination wait, and checks fresh session state before quarantine can
succeed. The original isolation error still prevents readiness until the unsafe
state is repaired. No global signal role is added.

The final generated suite passed in jshell with a non-superuser provisioner.
The lifecycle test passed in 4.57 seconds; the generator check passed in 86.75
seconds. It covers live login and owner sessions, sessions in another database,
retained unrelated connections, denied signals, cancellation, bounded progress,
recovery, data retention, and telemetry privacy. The source matches the frozen
copy. The Job and private resources are absent, and the shared Lease is free.
The [evidence](postgres-quarantine-evidence.json) retains all five attempts.
Earlier attempts used the wrong database for the function-permission fault.

Hypershell still needs to adopt this runtime and prove active-session termination
through its real Gateway workflow. The earlier supplied-server browser and
27-check API results remain valid for compiler `16e09a2`; they do not prove this
new quarantine behavior. Actual CNPG and RDS operation remain required.

The full compiler CI and its dedicated PostgreSQL job passed at `868ff1f` in
[run 34925662236](https://github.com/jsell-rh/stego/actions/runs/34925662236).
This verifies the common runtime. The new Hypershell application check remains
required before adoption can be reported as verified.

Hypershell adopted `postgres-client` 1.2.1 in `acfdc83`. The complete browser
Gateway workflow passed in 336.85 seconds with race detection. Both unsafe
privileges and role membership caused the generated runtime to stop an already
open Gateway SQL session. An already open session for the other Gateway stayed
usable. Recovery retained keys, credentials, and provider data. PostgreSQL
restart, process replacement, and both Gateway deletions also passed. The
[application evidence](hypershell-sql-quarantine.json) confirms matching source,
all 229 generated files, and resource removal. This closes the application
session-termination check. Installation CNPG and RDS operation remain open.
