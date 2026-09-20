# Database access installation

The compiler collects explicit database object requirements from storage,
outbox, and browser sessions. It emits `contracts/databaseaccess/access.go`
and a stable `access.json` record. A service without declared database objects
has no installer. Duplicate ownership, system schemas, unsafe names, and
unsupported permissions stop generation.

The installer grants CONNECT to the current database, USAGE to the declared
schemas, and only the listed table and sequence privileges. It does not use
ALL TABLES, ALL SEQUENCES, or default privileges. The schema marker is
read-only. Checkpoint, effect-binding, and resource-state tables do not receive
DELETE permission. Their existing structure and state checks remain required.
Application entity tables receive the permissions needed by generated storage.
Application request authorization stays in the application.

An empty sequence privilege set means inspection without direct runtime access.
The outbox uses this form: inserts use its identity column, and the runtime
cannot call sequence functions or read sequence state directly. The database
test requires a successful outbox insert and denied direct sequence access.

`GrantRuntime(ctx, ownerConnection, runtimeRole)` operates on schemas and roles
that already exist. The current connection role must own the database and
declared relations. The runtime must be a separate login with NOINHERIT, no
role memberships, no administrative flags, and no database or schema DDL rights.
The operator must remove default PUBLIC temporary-object privileges before
installation. The installer rejects unsafe existing access; it does not revoke
operator grants, change passwords, or create or change roles.

Inspection and grants use one transaction. All object checks precede grants.
The transaction has a 30-second context limit, a 25-second statement limit,
and a five-second lock limit. A shorter caller deadline wins. It uses the same
advisory lock as schema bootstrap. Other installation tools must use the same
protocol or be stopped. It cannot fence arbitrary concurrent operator DDL.
Failure rolls back grants. There is no automatic retry after an uncertain commit.
A caller can repeat the same operation and inspect its result.

`VerifyRuntime(ctx, database)` uses a read-only transaction with a five-second
context limit. It checks the current login and the same object permissions.
It makes no changes. Component schema and generation checks remain required.
Unknown views, foreign tables, row security, inherited relations, unsafe column
grants, and grant options are rejected. Unrelated objects and grants are outside
this contract. The connection must use the common verified database opener.
Errors use fixed text and retain context cancellation without exposing SQL errors.

Generated services with external storage migrations and generated browser
backends call this read-only check before they create handlers or start tasks.
The service process does not call the installer. The existing startup-migration
profile keeps its current behavior; it does not qualify the separate-role
production profile. An unused component does not cause a startup database check.

PostgreSQL combines direct, inherited, and PUBLIC privileges. Ownership also
permits object changes. The checks account for these paths within the declared
objects. See the [PostgreSQL grant contract](https://www.postgresql.org/docs/18/sql-grant.html)
and [role membership catalog](https://www.postgresql.org/docs/18/catalog-pg-auth-members.html).

The generated database tests use a separate non-login owner and runtime login
in a disposable database. They check real table and outbox writes, denied marker
and DDL writes, repeat installation, missing access, unsafe role and object
states, preservation of operator grants, no access to future tables, and a
short lock deadline. A database-local test event trigger rejects a grant after
it changes a schema ACL. The test requires rollback of that grant and the
preceding CONNECT grant. See the [event trigger contract](https://www.postgresql.org/docs/18/event-trigger-definition.html).

This change is a candidate. Source `468ea86a` passed all 44 bounded PostgreSQL
checks in [hosted run 35521273847](https://github.com/jsell-rh/stego/actions/runs/35521273847).
The saved source archive and result artifact hashes match. The generated REST
process reached HTTP configuration with complete access. Missing marker access
and excess table access stopped startup at `database.access`, before handlers
or tasks started. The output contained none of the selected private database
values. Normal outbox inserts passed with direct sequence access denied.

The first complete compiler check found an old registry version assertion and
stale generated examples. The assertion and examples are now updated. Complete
hosted checks, compiler release, browser installation adoption, and the complete
Hypershell workflow remain required. No production or application result is
claimed here. PostgreSQL 18 has test evidence; versions 16 and 17 still need
separate checks before a support claim.
