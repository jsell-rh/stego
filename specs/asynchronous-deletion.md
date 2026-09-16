# Durable asynchronous deletion

The user selected HTTP 202 for Gateway deletion on 2026-09-16. The API must
commit a deletion request before returning 202. New accounts must then be
rejected. The Gateway remains visible as deleting until its required cleanup
finishes. The controller must continue after request cancellation and process
restart. This changes the earlier 204/503 synchronous cleanup contract.

## Existing evidence

Hypershell test source `711659a` reproduces the current failure with only three
retained accounts. Each request confirms one account, then cancels during the
next provider call. Three requests reconstruct the application services from
the same PostgreSQL database. All three repeat the first account; two accounts
remain unreached. Normal cleanup succeeds when the interruption is removed.
The test failed its progress requirement in 0.73 seconds on jshell. This is a
correctness test, not a capacity measurement. Its branch is
`codex/gateway-cleanup-20260916`; it is not qualified for the default branch.

The bounded Job was `stego-ci/gateway-api-aa241d162013`, UID
`46e8a544-fe98-4759-82a4-c66c1cd4f964`. The saved cleanup record confirms that the
Job, Pods, and private fixtures are absent. The frozen source, selected test,
results, and wrapper are retained in
`/home/jsell/.local/state/stego/runs/gateway-cleanup-20260916`.
An earlier preparation attempt stopped before Job creation because the source
archive had no Git metadata. The second attempt used a frozen local clone.

## Common boundary

STEGO already supplies soft deletion, exact resource versions, declared cleanup
owners and targets, retained reads, conditional cleanup observations, durable
checkpoints, bounded scans, and the controller scheduler. Reuse these contracts.
Hypershell must not add a second queue, checkpoint table, or worker scheduler.

Soft deletion records the accepted request and blocks ordinary live-resource
writes and account reservations. Cleanup observations are evidence about current
external state. They can return to incomplete if a late provider effect appears.
They are not a permanent API visibility marker.

Add common finalization state and an exact-version finalization operation for
resources with declared cleanup owners. Finalization requires a deleted resource
and completion of every declared owner and retained target. It must commit with
the application's final event. Finalization is permanent: later cleanup retries
must not restore the resource to public lists or reads. Cleanup still uses
retained state after finalization to remove late provider effects.

Common storage must provide explicit reads for live resources plus deletion
requests that are not finalized. Apply this predicate before related access
filters, counts, and pagination. Ordinary reads and mutation locks keep their
live-only behavior. Do not make every retained resource public. Unsupported
entities or conflicting read options must fail closed.

The generated schema must enforce the finalization transition and prevent its
removal. Validate the schema at startup. Define and test migration behavior
before enabling the new application contract. A change of visibility must not
make previously removed Gateways appear again. A completed cleanup observation
alone must not be mistaken for a committed finalization event.

## Hypershell application work

Declare account cleanup as a required Gateway cleanup owner. Supply account and
provider policy to the common controller and scan runtime. Persist progress in
bounded passes and reserve time for its commit. A failed or cancelled item stays
eligible. Completed prefixes survive controller replacement. Independent items
must not be starved by one failed account. Do not remove historical account
checks to make the test pass.

Provider inventory and retained account rows are different sources. Inventory
finds orphans. Retained IDs find uncertain and late creations. Both need bounded
work and later full scans. Account journals must record terminal intent before
provider deletion. Account metadata and its successful cleanup audit must commit
together. A provider response alone must not acknowledge database progress.

Return HTTP 202 only after the authorized request commits. Repeated requests
while deletion is pending must be safe. Unauthorized requests must have no
provider effects. Expose the deleting state through the existing API shapes and
disable incompatible console actions. Preserve generated event delivery. The
final deletion notice follows committed finalization, not the first cleanup
owner's success. Update REST, gRPC, SDK, CLI, browser, and watch expectations.

## Required qualification

1. Prove common finalization with a second entity and different cleanup owners.
   Reject early, stale, live-resource, and reversed finalization. Keep finalized
   rows hidden when late effects reopen a cleanup observation.
2. Prove filtered reads, counts, pages, and denied access while deletion is pending.
   Keep ordinary account creation and mutation blocked after the request commit.
3. Replace the API and controller after request acceptance and after partial
   progress. Confirm the tail is reached, failed items retry, and audits are not
   duplicated. Test a failed final event commit and stale completion writes.
4. Re-run real Keycloak orphan and late-result cleanup, the rendered Gateway
   workflow, REST and gRPC, and the full CNPG workflow. Verify regeneration and
   test-resource cleanup. The existing green workflows do not qualify this change.

The implementation and these gates remain open. No production capacity claim is
made from the three-account interruption test.

## Implementation and qualification in progress

STEGO candidate `cd08dc6` supplies permanent finalization, pending-deletion list
and cursor reads, and typed empty HTTP 202 responses. PostgreSQL tests passed in
CI runs `35092656619` and `35093219283`. Both full runs failed because the registry
version test still expected adapter 4.5. That assertion is corrected in
`eb0e543`, which is included in the candidate. A full green run is still required.

Hypershell source `c0ab6e5` passed two bounded recovery tests on jshell. A failed
account did not stop independent accounts. A reconstructed service completed the
last retained account after a first pass saved 100 IDs. The tests took 1.18 and
0.53 seconds. Generation matched before and after the tests. Job
`gateway-api-06e3e8773918`, UID `7d8a6c18-f910-4168-bd1d-5267d1220b54`, and its
private fixtures are absent. The results are in `recovery-result-2` under the
persistent run directory above. An earlier recovery attempt stopped at the
regeneration check before tests: its local compiler lacked Git build metadata.
The compiler was rebuilt from a clean clone with a verified revision.

Hypershell `31c8646` connects request acceptance, account recovery, cleanup owners,
finalization, and events. Its selected REST and gRPC process gate is running.
`9386f88` also prevents unchanged cleanup observations from publishing events.
These changes are on `codex/gateway-cleanup-20260916`; the default branch has not
changed to the new deletion contract.

The next checks must cover the new transport workflow and event commit failures,
then align the console and earlier synchronous-deletion tests. The current public
view supplies phase `Deleting` after the storage query. Search and ordering on
that field must use the same value before qualification. Provider inventory still
uses a bounded full scan; its large-inventory behavior remains open. These limits
must not be hidden by the small recovery tests.
