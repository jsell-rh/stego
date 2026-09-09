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

`grpc-application` 1.4.0 also supplies `transport.SetResourceVersion` and
`client.ObservedResourceVersion`. A single-resource unary handler calls the
server helper once, after an authorized read. It must return the data from that
same read. The revision travels in the `resource-version` response header.
Capture the header with `grpc.Header`, check the RPC error, then decode it with
the client helper. Missing, duplicate, invalid, or overflowing revisions fail
closed. Keep the decoded revision with its response through external work.

A response header does not represent each item in a list or each event in a
stream. Do not use this helper for those cases. Neither helper grants access or
adds a conditional write automatically. Hypershell's database API uses them to
preserve its public protobuf message shapes while requiring controller revisions.

Apply the schema change, replace all API instances that can accept controller
writes, and then start the updated controllers. An old API instance can ignore a
new request header. Requiring a header in new client code cannot enforce a check
inside an old server. Do not treat a mixed rollout as proof of the new contract.

## Retained reads

`postgres-adapter` 3.7.0 supplies the optional `storage.RetainedReader` interface.
`GetRetained` returns one versioned resource, whether live or soft-deleted. The
query matches the exact ID and does not run a list count. It returns raw stored
fields, the revision, and deletion metadata from the same query. It supports a
transaction and bounds the query by the storage operation timeout. Unknown and
unversioned entities fail. Missing IDs return `ErrNotFound`.

Authorize recovery access before this read. The storage method does not grant
access or convert raw observation fields to their current public presentation.
An absent record is not proof of deletion. Do not delete external resources when
the retained read fails, is denied, or returns a different ID.

`grpc-application` 1.5.0 adds `client.WithRetainedResourceRead` and
`transport.RetainedResourceRead`. They use the explicit request mode
`resource-read-mode: retained-v1`. A handler must authorize this mode before it
uses `GetRetained`. Missing mode keeps the ordinary live-only read. Unknown,
empty, and duplicate values fail. The helper does not make other handlers accept
retained reads.

After an authorized single-resource read, use `transport.SetResourceState`
instead of `SetResourceVersion`. It sends the revision and explicit
`resource-deleted: true` or `false` from that read. Capture the response header
and use `client.ObservedResourceState`. Require a successful RPC, matching
resource ID, valid revision, and explicit deletion state before cleanup. The
decoder rejects missing, invalid, and duplicate state. An old server that omits
this evidence cannot authorize cleanup through the new client contract.

Events supply identities that need another check. Read current retained state
before each provider action and after each failed attempt. A delayed live event
can refer to a resource that is now deleted. A deletion event cannot override a
current live record. Keep periodic recovery after a successful provider action;
these helpers do not record durable cleanup completion or prevent late external
effects. They also do not fence actions across controller processes.

Replace the API before the updated controllers. During a mixed rollout, old
controllers can still use event data directly. Complete their replacement before
claiming this contract for all cleanup actions. Public deletion visibility and
retention policy remain application decisions.

## Evidence and limits

Generated PostgreSQL tests cover ordinary and raw SQL writes, stale observations,
concurrent writers, rollback, prepared statements, migration repeatability,
immutable identity, retained deletion, startup refusal, and overflow. Generated
gRPC tests cover strict metadata parsing and preservation of the parent context.

A revision identifies all writes. Optional [desired generations and observation
groups](resource-generations.md) distinguish declared inputs from observations.
They require an explicit application policy. Durable finalizers, cross-process
fencing, and automatic CRUD observation support remain open. A revision cannot undo external work that completed after its
observation became stale. Periodic repair remains necessary.
