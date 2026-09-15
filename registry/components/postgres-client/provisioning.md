# Logical PostgreSQL databases

`postgres-client` supplies `EnsureDatabase` and `DeleteDatabase`. These methods
manage a logical database, a non-login owner role, and a separate application
login. They do not create or delete a PostgreSQL server. They have no Kubernetes,
Gateway, or cloud-provider dependency.

The operator supplies verified TLS options for a dedicated provisioning
database. Use PostgreSQL 16 or later and an account with `CREATEDB` and
`CREATEROLE`. The runtime rejects superuser, replication, and bypass-RLS
attributes on that account. It uses SCRAM authentication. IAM authentication
is not implemented. RDS has no PostgreSQL superuser or host-file access; see
[the RDS role contract](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Appendix.PostgreSQL.CommonDBATasks.Roles.rds_superuser.html).
Actual RDS acceptance is still required.

`DatabaseKey.Scope` identifies the application installation on the server.
`Resource` identifies one durable application resource. Both are opaque strings
with a 256-byte limit. The runtime derives SQL names from their SHA-256 hash.
Callers cannot select system databases or supply SQL identifiers. The application
must retain the selected server and these keys for the resource's whole life.

Call `NewDatabasePassword`, then store its result durably before calling
`EnsureDatabase`. The password contains 32 random bytes encoded as 64 hexadecimal
characters. The runtime records only its hash. It sends a SCRAM verifier in
password DDL, never the plaintext password. A changed or missing stored password
must not be treated as permission to reset existing data. Password rotation needs
an explicit future protocol and is not implemented by this API.

## Ownership and recovery

The runtime creates `stego_provisioning.resources` in the provisioning database.
Its schema and table must belong only to the provisioning account. The record
holds resource keys, SQL names, object OIDs, the credential hash, and lifecycle
state. It contains no plaintext password. Do not purge or edit this ledger during
normal operation. Operator backup and recovery must keep it with the managed
databases. Loss of the ledger is not permission to adopt existing SQL objects.

The role reservation and ownership record commit in one transaction. Database
creation uses a separate statement because PostgreSQL does not permit it inside
a transaction. The database starts with connections disabled. After a lost
create result, the reserved owner role proves which database the call can resume.
See [CREATE DATABASE](https://www.postgresql.org/docs/18/sql-createdatabase.html).

A session advisory lock excludes another operation for the same resource. The
same session performs the nontransactional DDL. No connection pool can move that
work onto an unlocked connection. Separate resource keys use separate locks.
A busy resource returns `ErrDatabaseBusy`; the caller's reconciler decides when
to retry. There are no automatic mutation retries. Each call has a 30-second
limit, or the caller's shorter limit. Statements and lock waits also have limits.
After failed activation, login cleanup can use up to three additional seconds,
even if the caller has canceled the operation.

Deletion records its intent first. It disables login and new database
connections, drops the owned database, then drops its two roles. The retained
deleted record blocks a late `EnsureDatabase` and deletion-before-create races.
Changed object OIDs cause an ownership error. Unrecorded objects are never
adopted or dropped. Dependencies in another database cause deletion to stop;
the runtime does not use `DROP OWNED` or remove unrelated grants to force success.
`DROP DATABASE ... FORCE` can terminate only sessions for which the provisioning
account has permission. See [DROP DATABASE](https://www.postgresql.org/docs/18/sql-dropdatabase.html).

All provisioners for this server must use the same provisioning database and
ledger. The application must fence changes to desired state before it calls the
runtime. Operator DDL and restores must stop the affected reconcilers or use the
same coordination. An external administrator can bypass database permissions
and advisory locks; this API is not a boundary against that administrator.

A failed final check attempts to disable the login on the locked connection.
A lost connection can leave an uncertain DDL result. The caller must retain the
resource and credential, report the error, and retry reconciliation. It must not
report readiness from a missing result. Disabling LOGIN alone prevents new
sessions; it does not close sessions that were already open. An isolation error
also invokes the session quarantine described below.

## Access boundary

Before enabling an application login, the runtime requires denied CONNECT access
to every other connectable database. The operator must revoke PUBLIC CONNECT
and grant access to the correct component roles on shared servers. The runtime
will not change unrelated databases to establish this prerequisite.

The database owner cannot log in. The application role has no administrative
attributes or role memberships. It gets CONNECT and TEMPORARY on its database,
and USAGE and CREATE on its public schema, without grant options. It cannot grant
database access to another login. Extra grants on its owned database or schema
are removed during repair. Normal user tables and their data are retained.

The operator must preserve the server-wide connection policy when it adds a new
database. A later PUBLIC CONNECT grant can violate isolation. A subsequent
reconciliation detects that condition and attempts to quarantine both owned
roles. This is not continuous interception of external administrator changes.
Connection limits are bounded from 1 to 1000. PostgreSQL enforces them
approximately; this API does not claim a strict process or storage quota.

`postgres-client` 1.2.1 disables login on each recorded role separately after an
isolation error. An enabled owner login is also an isolation error. Quarantine
disables that login before a later reconciliation can recover. It then selects at most 129 session records by the exact role
OIDs. It can stop up to 128 sessions in one call. The selection includes sessions
in other databases, but excludes other roles. Each signal rechecks the role,
process ID, and process start time. A positive one-second timeout requires
PostgreSQL to confirm termination. A final fresh read requires both roles to
remain non-login roles and no owned session to remain.

The normal 30-second operation limit also applies to quarantine. An exceeded
session bound, denied signal, unknown session identity, cancellation, or failed
final check returns an error. A later reconciliation can continue bounded work.
The original isolation error remains even after all observed sessions stop;
the operator must repair unsafe grants before readiness can recover. No new
credentials are created and no unrelated grants or sessions are removed.
The runtime uses membership in its owned roles and does not require global
`pg_signal_backend` rights. See the PostgreSQL
[session signal rules](https://www.postgresql.org/docs/18/functions-admin.html#FUNCTIONS-ADMIN-SIGNAL)
and [activity visibility and snapshot rules](https://www.postgresql.org/docs/18/monitoring-stats.html#MONITORING-STATS-VIEWS).
Quarantine emits its own bounded span, duration metric, and fixed log event.

A healthy repeat call checks permissions and verifies login without changing
catalog rows or passwords. SQL reads and lifecycle operations emit bounded OTEL
spans, duration metrics, and structured log events. Labels contain only operation
and outcome. They exclude keys, database names, hosts, SQL text, arguments,
passwords, and verifiers. The application uses STEGO's normal telemetry runtime
to export these signals.

The generated SQL acceptance test uses a separate disposable server. Its
provisioning account has no superuser rights. The general compiler test server
must not run that test: the fixture changes CONNECT grants on its own template
and maintenance databases. The dedicated CI job requires this test explicitly.
The Hypershell external provider and actual RDS workflow remain separate work.

## Durable server binding

Call `DatabaseServerIdentity` during initial setup. Save its result with the
resource credentials before `EnsureDatabase`. Set `Options.ServerIdentity` to
that saved value on each later ensure or delete operation. A missing or different
server marker returns `ErrDatabaseServer` before resource changes. This detects
an empty replacement server even when its host name is unchanged.

Keep the private server marker, provisioning ledger, and application databases
in the same backup and restore plan. Do not generate a new marker to bypass a
failed check. Administrator credentials and TLS trust can change while the
server marker remains unchanged. An empty option permits initial setup and is
only suitable before a durable resource binding exists.
