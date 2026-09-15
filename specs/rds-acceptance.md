# RDS acceptance

Actual RDS operation remains unverified. The ordinary PostgreSQL and CNPG
results do not prove RDS permissions, maintenance access, or failover behavior.
Use a disposable installation and the same generated SQL runtime for this gate.
Do not add a cloud-provider branch to the Gateway API or SQL lifecycle.

## Installation requirements

The operator supplies the PostgreSQL server, network access, CA, provisioning
database, and provisioning login. Terraform can create these before the
application installation. The application must not create or delete the server.
Use PostgreSQL 16 or later, verified TLS, and the documented non-superuser
provisioning role. Keep administrator credentials outside Gateway workloads.

RDS can restrict password changes with `rds.restrict_password_commands`. When
that setting is enabled, the provisioning login also needs `rds_password` in
addition to `CREATEROLE`. The installation must supply the required grant;
the runtime must not disable the setting or grant itself more authority. See
[the AWS password management contract](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Appendix.PostgreSQL.CommonDBATasks.RestrictPasswordMgmt.html).

Use the server endpoint as the TLS host and provide its current trusted CA.
TLS enforcement by the server does not replace client certificate and host
verification. See [the AWS TLS contract](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL.Concepts.General.SSL.html).

The generated runtime currently requires denied SQL `CONNECT` privilege to
every other connectable database. It does not accept a database-name exception.
The test must inspect this rule against the actual RDS catalog, including
managed maintenance databases. A server-side connection denial alone does not
prove that the current catalog check will pass. Preserve this uncertainty until
the test records both catalog privileges and actual denied connections.

## Required evidence

- Record the application and compiler revisions, RDS engine version, parameter
  settings, role attributes, and test limits. Exclude passwords and tokens.
- Run the complete generated Gateway workflow: create without a database ID,
  commit the owner grant and events, enforce filtered reads and denied writes,
  and use the resulting Gateway through the generated browser backend.
- Create two Gateways on the supplied server. Verify separate logical
  databases and logins, denied cross-database access, and unchanged access for
  the installation's API and identity-provider databases.
- Prove password setup with restricted password management enabled. A missing
  required permission must produce a private error and no healthy Gateway.
  Restore the installation permission and retry with the same durable identity.
- Replace Gateway namespaces and restart controllers. Keep SQL object
  identities, credentials, encryption keys, and application data unchanged.
- Exercise a supported RDS failover. Reconnect through the configured endpoint
  with verified TLS. Prove recovery through the generated network policy and
  connection runtime, without manual address edits during the test.
- Delete one Gateway through REST and another through gRPC. Verify SQL and
  identity cleanup while the RDS instance and unrelated databases remain.
- Repeat generation and compare all generated files. Verify cleanup with
  independent reads. Keep failed attempts and uncertain results as failures.

The test needs an identified AWS account, region, disposable instance, and
network path from the test cluster. No such target is selected yet. Creation
of a new paid instance requires a concrete resource, lifetime, cleanup, and
cost plan. This gate does not permit changes to unrelated installations.
