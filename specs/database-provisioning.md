# Isolated PostgreSQL databases

The user requires CNPG and external PostgreSQL for Hypershell. On 2026-09-14,
the user removed deployment-backed PostgreSQL from the target scope. External
mode must create a separate logical database, login, permissions, and credentials
for each Gateway. Support for existing connections alone does not satisfy this
requirement.

STEGO must supply reusable database provisioning and verification mechanisms.
Hypershell supplies Gateway identity, database selection, and locality. Gateway
databases belong beside their Gateways; the database reconciler belongs beside
the database it manages. CNPG uses the Gateway's managed cluster. External
PostgreSQL needs an explicit operator-supplied association with that cluster.

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

The Hypershell external provider remains an open application gate. The operator
can create an RDS server with Terraform before cluster or installation creation.
Hypershell must register the server with its managed cluster and manage only the
Gateway logical databases and logins. It must never delete the external server.
Support must include one server shared by an installation's component databases
and separate servers for those components. A server is not shared between
installations. Actual RDS acceptance is still required; local PostgreSQL evidence
does not prove RDS permissions or operation.
