# Resource-state scope closure

The adapter 4.9.0 candidate implements this guard. It is not yet qualified.

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

## Candidate implementation

Migration 011 adds the scope table and database guards. A new state key takes
its scope row lock before it increments the key-set revision. Conditional
closure uses that same lock. All commands run in the caller's transaction.
Backfill counts existing keys under a bounded table lock. Migrations 009 and
010 are unchanged. Scope reads use the exact primary key and do not count
records during recovery.

The generated code compiles. Invalid read requests pass their local unit check.
The local generated check took 6.92 seconds. Database cases were skipped locally;
this is not SQL qualification. A separate bounded CI job now runs the state key
and scope tests. Hypershell adoption and its failed race test remain required.

## First SQL result and CI correction

The stored log from run `35103083212` is not a pass. Closure, restart, rollback,
invalid-input, schema-guard, backfill, overflow, and prepared-statement checks
passed. Both concurrent-order cases rejected the losing transaction with the
existing `ErrSerialization` contract and SQLSTATE 40001. Their assertions
incorrectly required only `ErrResourceStateConflict`.

The revised test accepts either defined conflict at that boundary. It then
starts a fresh transaction, requires the committed scope to reject the stale
operation, and checks that the losing transaction added no key. The generated
runtime did not change for this test correction.

The focused job had also hidden the failing `go test` exit code behind its log
pipe. Its success status was false evidence. The stored log exposed the failure.
The job now selects Bash with `pipefail`, rejects failure markers, and requires
both named wrapper tests to pass. A local negative check confirmed exit code 1
for a failed command before a successful log sink. A new CI run must prove the
complete check. Cancellation of the superseded run was requested.

## Corrected focused SQL evidence

Run `35103461720`, source `cc6b906`, passed the corrected focused storage job.
The stored log has no failed or skipped cases. Both concurrent commit orders
passed in 0.49 seconds together. Each order also checks a fresh rejection and
confirms that the losing transaction added no key. The scope wrapper passed in
17.94 seconds; the key wrapper passed in 5.93 seconds. Evidence is retained at
`/home/jsell/.local/state/stego/runs/gateway-cleanup-20260916/resource-state-scopes-corrected-ci`.
Full compiler CI remains pending. The superseded run `35103083212` is canceled.

The earlier adapter 4.8.1 and controller scan sequence passed all five checks in
run `35102404039`. STEGO main was advanced to its tested source `60ebe0a`. The
scope closure candidate is still on the feature branch pending full checks.

All six jobs in run `35103461720` passed at `cc6b906`. This includes the full
compiler race suite, both examples, real Keycloak, PostgreSQL provisioning,
and the focused scope and state-key checks with the corrected CI wrapper.
The default branch now includes this code and its evidence at `8db1fa5`.
The result record is `scope-full-ci-success.json` in the Gateway cleanup run
directory. The later provider closure operation is a separate candidate.
