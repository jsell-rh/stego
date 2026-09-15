# Externally supplied PostgreSQL acceptance

On 2026-09-15, the user approved a PostgreSQL container as the test server for
an externally supplied database. No RDS test instance exists. RDS creation is
outside Hypershell scope. Terraform manages that infrastructure before the
application installation. This gate does not require an AWS test instance.

The operator supplies the PostgreSQL server, network access, CA, provisioning
database, and provisioning login. The application creates and deletes only its
Gateway logical databases and roles. It must preserve the server, storage, and
unrelated databases. Use the same generated SQL runtime for external servers
and CNPG. Do not add a cloud-provider branch to the Gateway API or SQL lifecycle.

## Required container evidence

Use a dedicated PostgreSQL container with CPU, memory, and time limits. Docker
or Podman is permitted for small local checks. Run the complete application
workflow in CI or jshell. Keep one live cluster test at a time. No privileged
container, local performance test, or change to an unrelated server is permitted.

- Record application and compiler revisions, the pinned PostgreSQL image,
  server version, role attributes, test limits, and cleanup. Exclude credentials.
- Supply verified TLS and a non-superuser provisioning role. Keep administrator
  credentials outside Gateway workloads. Reject invalid trust and hostnames.
- Create two Gateways without database IDs. Commit ownership and events, enforce
  filtered reads and denied writes, and use the generated browser backend.
- Verify separate logical databases and logins. Deny cross-database access and
  access to installation databases. Preserve installation data and access.
- Remove a required provisioning permission. Require a private error and no
  healthy Gateway. Restore the permission and retry with the same stored identity.
- Replace Gateway namespaces and restart controllers. Preserve SQL identities,
  credentials, encryption keys, application data, and the server binding.
- Delete through REST and gRPC. Verify SQL and identity cleanup while the supplied
  server and unrelated databases remain. Repeat generation and compare output.
- Verify cleanup with independent reads. An incomplete test is not a pass.

The compiler's generated SQL lifecycle test uses a non-superuser account with
`CREATEDB` and `CREATEROLE`. The complete Hypershell browser fixture also supplies
a separate non-superuser provisioning account. Existing
[API evidence](hypershell-shared-jwt-api.json) and
[browser evidence](hypershell-shared-jwt-browser.json) cover their recorded source
revisions. They include SQL isolation, permission failure and recovery, retained
credentials, and deletion. Public Gateway connectivity remains a separate gate.

The later [browser run at `59a6d32`](hypershell-controller-endpoint-browser.json)
also passed. Its evidence records the pinned PostgreSQL container and resource
limits, source and generation checks, SQL and account cleanup, and independent
cluster cleanup reads. It does not record a queried PostgreSQL server version.
The console archive needs a fresh build after a later drift failure. Keep both
limits visible; the run does not close every required item in this document.

## Limits of the evidence

Container tests prove the external PostgreSQL contract. They do not prove
AWS-specific operation. RDS maintenance databases, managed password permissions,
parameter settings, and managed failover remain unverified on RDS. CNPG failover
results apply to CNPG only. Do not report either result as an RDS test pass.

The current generated runtime requires denied SQL `CONNECT` privilege on every
other connectable database. It has no database-name exception. An RDS installation
must meet this requirement, including its maintenance catalog. Keep this
compatibility limit explicit until an actual server is qualified.

An operator who qualifies RDS must supply its required password-management
grants and TLS trust. The runtime must not disable server controls or increase
its own authority. See the [AWS password management contract](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Appendix.PostgreSQL.CommonDBATasks.RestrictPasswordMgmt.html)
and [AWS TLS contract](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL.Concepts.General.SSL.html).
