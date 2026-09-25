# Fresh schema generations

`postgres-adapter` 4.3 adds the optional `schema_generation` setting. It is a
release boundary for applications that require a fresh database and do not
support an in-place schema transition. Existing applications that omit the
setting keep their migration behavior.

```yaml
postgres-adapter:
  migrations: external
  schema_generation: fresh-v1
```

The value is a lowercase name of 1 through 64 characters. Later characters can
also be digits, periods, or hyphens. The generated marker includes this name and
a SHA-256 hash of the declared entities and collections. A changed declaration
with the same name is also rejected. This is not a general upgrade framework.

## Writer lease

`postgres-adapter` adds the optional `writer_lease` setting. It requires the
`schema_generation` setting and accepts `single` or `off`; the default is
`single`. In `single` mode every write takes or re-asserts a database-wide
single-writer lease, and a store that lost the lease to a newer process fails
closed with `ErrWriterFenced`. Use `off` when several live processes write the
same database on purpose, for example a runtime plus independent tooling or a
multi-replica deployment. Epoch and identity rollback detection stay active in
both modes; `off` removes only the lease table, its fence code, and its grants.

```yaml
postgres-adapter:
  schema_generation: fresh-v1
  writer_lease: off
```

## Initialization and startup

`Migrate` calls `BootstrapSchema` before its registered migrations. The latter
also accepts a trusted application callback. It owns the transaction; do not
call it from another transaction. The callback must use only its supplied
`*gorm.DB`. It must not issue transaction control, use another connection, or
perform external effects. Register additional application migrations before
calling `Migrate` if they must commit with the generated schema. A marker covers
only work in that transaction; unrelated setup calls remain the application's
responsibility.

Before any schema write, the runtime takes a database-wide transaction advisory
lock and checks for a completed marker. A matching marker returns success
without repeating the callback. A missing marker permits initialization only
when the database has no application schemas, public relations, public types,
or public routines. An empty old table is still an old schema and is rejected.
Preinstalled extensions with public objects also require a different explicit
installation policy; this strict mode does not adopt them.

The schema, its data, and the completed marker commit together. Callback errors
or connection loss before commit roll them back. After a lost commit response,
retry the same call and inspect its result. A matching marker prevents repeated
initialization. Calls have a 30-second context limit, a 25-second statement
limit, and a five-second lock-wait limit. A shorter caller deadline wins. There
is no automatic retry of a failed mutation.

PostgreSQL releases the transaction advisory lock when the transaction ends.
Other installation tools must use the same protocol or stop before this call.
The lock is cooperative; it does not stop arbitrary operator DDL. See the
[PostgreSQL lock contract](https://www.postgresql.org/docs/18/explicit-locking.html#ADVISORY-LOCKS).

`NewStore` calls `VerifySchema` before model preparation and storage checks.
The check has a five-second context limit and uses a read-only transaction.
It accepts exactly one matching marker row. It rejects a view, row-security
policy, inherited or partitioned relation, altered column types, and write
permissions granted to another role. A relation lock keeps the inspected marker
stable until the check ends. Unknown relations cannot supply executable marker
values through a view. See the [relation catalog](https://www.postgresql.org/docs/18/catalog-pg-class.html).

Use a separate installation role for DDL. The application role needs USAGE on
`stego_schema` and SELECT on `stego_schema.generation`, plus its normal data
permissions. Do not give it marker writes or DDL rights. The marker is not a
boundary against the database owner, who can replace schema objects. It does
not audit all later operator changes to application tables.

An old or unknown generation returns `ErrSchemaGeneration`. Inspection failure
also rejects startup. Neither failure authorizes migration, adoption, or
teardown. The operator must inspect the installation and perform any approved
teardown separately. Do not change a marker to force a new release to start.

## Application acceptance

The generated checks cover fresh initialization, repeat startup, empty and
populated legacy schemas, changed generations and declarations, concurrent
starts, callback failure, connection loss, and an executable view used as a false
marker, marker permissions, and a short deadline during lock wait. All eight
generated tests passed under race detection in the bounded jshell fixture. See
[the evidence record](../../../specs/schema-generation-evidence.json).
Hypershell must still adopt this mechanism with its database catalog removal,
include its application setup in the transaction, and prove the complete new
workflow. A common component test does not establish application acceptance.
