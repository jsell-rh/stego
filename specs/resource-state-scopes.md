# Resource-state scope closure

This is the next required storage change. It is not implemented or qualified.

Hypershell test `TestGatewayJournalRegistrationDuringScanPreventsCompletion`
failed in run `35102352727`. A key inserted before a saved cursor was missed.
The application reported cleanup complete after visiting 101 of 102 saved keys.
This requires a common storage guard, not an application count query or another
unbounded scan.

## Required contract

Keep an independent revision for each exact entity and scope. Increment it when
a new resource-state key is accepted. Updates to an existing record must not
change this revision. Key pages still expose only IDs and record versions.
The scope revision must have an indexed read with bounded time and size.

Add a transaction-only operation that closes registration of new keys for a
scope. It must compare the revision read before the scan. A different revision
must fail the transaction. Closure and the caller's cleanup observation must
commit or roll back together. Closure is permanent and safe to repeat. It must
also work for a scope with no state records.

A concurrent new-key insert must have only two possible results: it commits
first and makes closure fail, or closure commits first and rejects the insert.
Enforce this in the database, including direct SQL writes. Existing records must
remain available for load, update, and repeated cleanup. Retain scope revisions
and closure after restart. Prevent revision overflow, key changes, and deletion
of scope history. Verify the required schema and guards before storage opens.
Keep existing numbered migrations unchanged.

The application uses the scope revision in its durable cycle input version.
A changed key set starts a new cycle from its beginning. Only a successful full
cycle and provider inventory check can attempt conditional scope closure. The
application selects the exact scope and cleanup owner. Common storage must not
contain Gateway names or access policy.

## Required evidence

Test both orders of a new-key insert and scope closure. Prove that a rollback
leaves registration open and that a committed closure blocks new keys while
existing records remain writable. Check empty scopes, exact entity and scope
isolation, stale revisions, repeat closure, restart, prepared statements, schema
guards, and invalid requests before database access.

Repeat the failed Hypershell page test with the common guard. Add a new-key
attempt between the final page and the cleanup commit. Require cleanup, scope
closure, and final event to share rollback behavior. Preserve earlier tests for
failed-item progress and retained IDs with no account row.

Scope closure covers saved obligations. It does not prove that an external
provider list is complete or discover an unknown external resource. Provider
ownership checks and the legacy inventory recovery requirements still apply.
