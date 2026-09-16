# Durable asynchronous deletion

The user selected HTTP 202 for Gateway deletion on 2026-09-16. The API must
commit a deletion request before returning 202. New accounts must then be
rejected. The Gateway remains visible as deleting until its required cleanup
finishes. The controller must continue after request cancellation and process
restart. This changes the earlier 204/503 synchronous cleanup contract.

## Existing evidence

Hypershell test source `711659a` reproduces the former failure with only three
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

STEGO default branch `a08e183` supplies permanent finalization, pending-deletion
list and cursor reads, typed empty HTTP 202 responses, and declared deleting
values for observation fields. Field display, search, counts, ordering, and
cursors use the same stored-state projection. The application supplies the text.
CI run `35094308674` passed all five jobs, including real PostgreSQL tests and
regeneration of both examples. Earlier runs `35092656619` and `35093219283` passed
SQL checks but failed a stale registry-version assertion. Preserve those failures.

Hypershell source `c0ab6e5` passed two bounded recovery tests on jshell. A failed
account did not stop independent accounts. A reconstructed service completed the
last retained account after a first pass saved 100 IDs. The tests took 1.18 and
0.53 seconds. Generation matched before and after the tests. Job
`gateway-api-06e3e8773918` and its private fixtures are absent. An earlier attempt
stopped at regeneration before tests because the local compiler lacked Git build
metadata. The corrected compiler came from a clean clone with a verified revision.

Hypershell `31c8646` passed the generated REST and gRPC process gate in 53 seconds.
It proved request rollback, final-event rollback, denied access, filtered lists,
account rejection after acceptance, pending reads, process replacement, permanent
finalization, and final event delivery through Kafka. Source `8856413` then passed
those tests plus unchanged-observation event checks and phase-search checks.
Both generation passes matched. Its Job `gateway-api-5c335707fdf0`, Pods, and
private fixtures were removed. The wrapper failed while releasing the Lease.
A separate operator check confirmed absence and released the same Lease.
Do not report that wrapper as a clean exit. Test and cleanup records are retained
in `projected-result` under the persistent run directory above.

These transport tests use a test account provider and explicit observations for
identity, workload, and SQL. They do not qualify the real provider workflows.
The source `7836bdc` removes the unused synchronous account-cleanup path and the
extra gRPC provider connection. All five selected cluster tests passed in
`retired-result`, with matching generation and complete resource cleanup.

The console candidate from `5af38b8` passed 166 tests in CI run `35094901951`.
Its source, compiler revision, archive hash, and asset hash were checked before
adoption. It keeps deleting Gateways visible, polls their state, and disables
incompatible actions. An earlier run failed one stale confirmation-text assertion.
The corrected test passed. The rendered browser gate must still pass.

The CLI review found that empty HTTP 202 responses needed an explicit common
contract. STEGO `e01e624` adds `Command.EmptyResponses`; CI runs `35095203438`
and `35095315581` passed all five jobs. The change is on the default branch. Hypershell `6d85104` adopts it and updates earlier synchronous-deletion
tests. CI run `35095289920` is checking the core, browser, console, and service
image. The complete CNPG and external-database cluster workflows remain required.
The Hypershell default branch has not changed to the new deletion contract.

Provider inventory still uses a bounded full scan. Its large-inventory behavior
remains open. Hypershell also retains its fresh-schema gate; these checks do not
claim an in-place application schema upgrade. No production capacity claim is
made from the small recovery tests.

The first complete CNPG attempt, run `35095613420`, stopped in the SQL cleanup
precheck. Reporting cleanup for a live Gateway returned gRPC `Internal` instead
of `Aborted`. Hypershell `ab0b3ef` returns the storage state-conflict error. The
failed run removed its application resources, CNPG runtime, volumes, and private
fixtures; the installation remained. Evidence is in `cnpg-first-result`.
Corrected runs `35096455130` (full CI) and `35096666440` (CNPG) are pending or
running. A successful earlier browser or image job does not replace these gates.

The first full Hypershell core run finished with nine failed tests. Three match
the cleanup state-conflict error above. Five retained the old hidden-row contract;
source `faccbf5` now requires visible `Deleting` state while other owners remain.
The namespace-count controller also kept watches for pending rows in the public
list. Source `62ed4b6` uses private stored deletion state before watch assignment.
Its focused race test passed in 5.034 seconds and rejects display text as deletion
proof. The generated worker regression remains in the cluster gate. The next
full CI run is `35098200160` at `371c230`. The superseded run `35096455130` was
canceled and supplies no qualification result. The failed core log and summary
remain under the persistent run directory.
