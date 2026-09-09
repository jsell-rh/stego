# Cleanup owners and observations

`postgres-adapter` 3.8.0 adds durable cleanup owners to versioned resources.
The application declares the owners and supplies their provider actions.

```yaml
entities:
  - name: Task
    versioned: true
    cleanup_owners: [worker, identity]
    fields:
      - {name: command, type: string}
```

Declare a YAML list of at most 32 distinct string owners. Boolean and numeric
values are not converted to names. Each name starts with a lowercase ASCII
letter and contains at most 63 lowercase letters, digits, or underscores.
Cleanup owners require `versioned: true`. Field names cannot collide with the
cleanup metadata or generated methods. Cleanup does not require desired
input generations.

PostgreSQL stores every declared owner in `stego_cleanup`. The generated model
exposes this read-only metadata as `CleanupState` and omits it from JSON.
Creation records each owner as false. A live resource has no pending deletion.
Soft deletion records every owner as pending in the same row update. A caller
cannot supply a completed cleanup state on creation or deletion.

`CleanupObservations` returns the declared owners and their recorded booleans.
It rejects invalid state. `PendingCleanup` returns incomplete owners in stable
order for a deleted resource. `CleanupComplete(owner)` requires a deleted
resource and a valid true observation for that owner. Unknown owners and invalid
state cannot prove completion. Sparse generated lists retain the private
metadata needed by these methods.

## Conditional observations

Read current retained state before provider work. Use the generated optional
`storage.CleanupWriter.ObserveCleanupIfVersion` to record one owner's result.
The UPDATE requires the exact resource ID, deleted state, and observed revision.
Unknown owners and unsupported entities fail. A missing, live, or changed
resource returns `ErrVersionConflict`. One owner's observation preserves the
other owners. Every accepted observation advances the revision.

Authorize the owner before this call. Use the same transaction for the
observation and its durable event. Return errors from that transaction. After
a conflict, read again and repeat provider work; do not attach a newer revision
to the earlier result. The storage interface does not grant controller rights.

Record true only after the provider confirms the required effects are absent.
Record false when a later attempt is pending or fails. Continue periodic provider
checks after a true observation. An external action can finish late, and a
second process can still have work in progress. A recorded success is an
observation, not a fence or a permanent absence guarantee. Avoid unchanged writes
when the latest provider result matches the stored observation; otherwise, a
write and watch loop can create unnecessary events.

A change to retained resource fields resets all cleanup observations to false.
Changes only to the revision, generation, observation metadata, cleanup metadata,
or update timestamp do not reset unrelated cleanup owners. The trigger rejects
invalid cleanup objects, unknown or missing owners, and non-boolean values.
Application database roles and configured controllers remain trusted writers;
this validation is not per-owner database authorization.

## Transport

`grpc-application` 1.6.0 adds `transport.SetCleanupObservations` and
`client.ObservedCleanupObservations`. Use them with the retained-read revision
and deletion-state helpers. All metadata must describe the same authorized read.
The `resource-cleanup` header contains canonical JSON, at most 4096 bytes, with
at most 32 valid owner names and boolean values. The client rejects missing,
duplicate, non-canonical, or invalid metadata. Require the expected owner before
provider work. Old servers that omit this metadata cannot satisfy that check.

The application supplies a private cleanup operation and authorizes its owner.
Hypershell uses a generated private protobuf service. Its request carries the
resource ID, owner, and result, with the existing revision precondition metadata.
Its public resource messages and deletion visibility remain unchanged.

## Migration and operation

Apply the complete generated migration in one transaction. The
`000004_resource_cleanup.sql` artifact includes the current revision, generation,
and cleanup contracts. The earlier version and generation artifacts also carry
the complete current contract when generated together.

The migration takes an exclusive table lock. It temporarily disables the old
revision trigger within that transaction and restores the current trigger before
commit. A failure rolls back the trigger and data changes. Repeating the same
contract preserves revisions and confirmations. A changed contract resets
observations and advances revisions; it can update all retained rows. Plan that
cost for production data volumes. New owners become pending. Existing owner
names cannot be removed while stored rows still carry them, including live rows.
An older generated application rejects a changed trigger contract at startup.

This version has no owner retirement or rename protocol. It also has no history
purge protocol. Completing all cleanup owners does not permit physical deletion,
ID reuse, or reversal of deletion. Keep the existing migration/application role
separation. Database restore and administrator bypass are outside this contract.

## Evidence and remaining requirements

Generated PostgreSQL tests cover live-write refusal, atomic deletion ownership,
independent and concurrent owners, stale revisions, event rollback, invalid
metadata, retained-input changes, repeated migration, new owners, rejected owner
removal, and startup refusal for an old contract. Generated gRPC tests cover
bounded canonical metadata and error propagation. Hypershell supplies REST,
gRPC, restart, and provider workflow evidence.

Cross-process fencing, owner-specific controller credentials, completion history,
cleanup metrics, public deletion presentation, retention, and purge remain open.
Do not use the confirmed flag to remove a resource from all recovery scans until
late external effects and in-flight work have a separate safe completion rule.
