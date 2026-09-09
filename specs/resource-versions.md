# Resource revisions

Set `versioned: true` on an entity to enable database-managed revisions. This
contract requires `postgres-adapter` 3.5.0. The compiler rejects other storage
configurations and domain fields that conflict with the revision metadata.
`versioned` accepts only the YAML values `true` and `false`.

```yaml
entities:
  - name: Task
    versioned: true
    fields:
      - {name: state, type: string}
```

The generated model has `ResourceVersion int64`. The field is omitted from JSON
and ordinary writable fields. PostgreSQL sets it to 1 on insertion. Every update,
including raw SQL, ordinary replacement, and soft deletion, adds 1. A caller
cannot supply the next revision. Overflow fails the write. A transaction rollback
also rolls back its revision change.

The database trigger rejects changes to the immutable ID, reversal of deletion,
and physical deletion. Retained rows prevent an old ID and revision from becoming
valid for a different resource. This first contract has no history-purge or
restore protocol. Do not enable it where physical deletion is required until
that protocol exists.

Apply the generated migration before application startup. `Migrate` runs each
revision statement in one transaction and supports prepared statements. The SQL
artifact `migrations/000002_resource_versions.sql` is available for external
migration tools. Existing revision values remain intact. Startup checks the
required column, trigger, and function body and fails if they differ. Removing
`versioned` from the declaration does not remove an installed trigger.

Use separate migration and application roles. The application role must not own
the table or function. It must not have DDL, TRUNCATE, replication, or other
privileges that can bypass triggers. Database administrators remain trusted.
Startup checks cannot prevent a later administrator change, database restore,
or replacement of the database with older state.

## Conditional writes

Read the revision before external work. Use the optional generated
`storage.VersionedWriter` interface to call `ReplaceIfVersion` with that revision.
The implementation checks the exact ID, live state, and revision in the UPDATE.
It returns `ErrVersionConflict` when the row is absent, deleted, or changed.
Only one writer can commit against a given revision. The method rejects entities
that do not enable revisions.

Access checks, field ownership, the conditional write, and event insertion must
share the application transaction. Return each error from the callback so that
failed writes cannot commit events or other changes. After a conflict, read
current state and repeat the external observation. Do not retry an old result
with a new revision.

`grpc-application` 1.3.0 supplies `client.WithResourceVersion` and
`transport.ResourceVersion`. They use the `if-resource-version` metadata key and
preserve the protobuf message shape. The server parser rejects duplicate values,
non-positive values, non-canonical numbers, and overflow. The metadata is a
precondition, not authorization. The application must require it on controller
writes and enforce it inside the transaction.

## Evidence and limits

Generated PostgreSQL tests cover ordinary and raw SQL writes, stale observations,
concurrent writers, rollback, prepared statements, migration repeatability,
immutable identity, retained deletion, startup refusal, and overflow. Generated
gRPC tests cover strict metadata parsing and preservation of the parent context.

A revision identifies all writes. It does not distinguish desired-state changes
from observations. Desired generation, observed generation, compiler-enforced
field ownership, durable finalizers, and cross-process fencing remain separate
requirements. A revision cannot undo external work that completed after its
observation became stale. Periodic repair remains necessary.
