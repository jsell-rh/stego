This component generates the tenancy of a service: the roles of the service and
the row-level security of its tables. Its role migration, in the folder
internal/tenancy/migrations, makes the owner, runtime, worker and relay roles,
the purge role of the idempotency keys and the outbox relay role. The owner
role is a member of the purge and outbox relay roles WITH SET TRUE, INHERIT
FALSE and no ADMIN, so the migration job can make their functions as those
roles.

At start, the service checks that the purge and outbox relay roles have no
other member (ST-SUP-RP15-LC01). On PostgreSQL 16 and later, a login with
CREATEROLE that makes a role becomes a member of it WITH ADMIN, granted by the
bootstrap superuser. The login cannot revoke that membership itself, so the
service refuses to start while it holds it. To start the service, do one of
these:

- run the role migration as a superuser, so no login holds such a
  membership; or
- as a superuser, run REVOKE <role> FROM <login> GRANTED BY <superuser> for
  the purge role and for the outbox relay role.
