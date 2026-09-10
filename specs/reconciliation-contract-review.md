This review applies the guidance in [Hypershell PR 200](https://github.com/openshift-online/hypershell/pull/200)
to STEGO and its application test bed. The reviewed PR head is
`a5dbb5c427d461b3988d371d20a02cf5f46088d0`. The review includes all seven changed
specifications and the review discussion. The PR defines intended behavior; it
does not provide runtime proof.

The inspected STEGO revision is `ed4d411e6cbf33e7e9e4613e5947c212b3b3dc30`.
The inspected variant is `baacc3ccc242d38eb21ba6bfba0e312e908a21b5`, with compiler
pin `639b95bb49bc9020b849f5f9ee6180a7b1a1ee09`. The table and probe below describe that baseline. The progress section records
later changes. The reconciliation contract is not complete.

| Required property | Current evidence | Remaining gap |
| --- | --- | --- |
| Current state, initial discovery, and periodic repair | Gateway actions read privileged current state. Generated controllers open a watch before scanning. Retained Gateway IDs and database rows recover missed deletion events. Restart and offline-deletion workflows passed locally. | Database deletion actions still use retained event data directly. They need a common authoritative deletion-state contract. |
| Repeat-safe actions and repair after drift | Generated Kubernetes writes check ownership and object identity. Repeated stable database reconciliations avoid changes. Gateway workload tests cover Pod and database restart, namespace replacement, and recovery. Healthy phase does not bypass `Ensure`. | There is no complete failure-injection matrix for stopping after every external write. External providers need explicit adoption and repeat rules. |
| Per-resource serialization and bounded retry | `Run` serializes a whole controller. `RunKeyed` combines repeated keys, preserves changes during actions, and applies capped exponential delays. `RunSweep` bounds worker count and rotates recovery groups. | Gateway and database controllers still use FIFO scheduling and scan-based retries. One slow action delays unrelated keys until its timeout. There is no cross-process leader or fencing contract. |
| Immutable identity, versions, and conditional commits | Resource IDs are assigned at creation. Kubernetes mutations use observed UID and resource-version preconditions. Database transactions prevent conflicting writes inside one transaction. | API resources lack a common revision, desired generation, and observed-generation contract. An earlier observation can publish status after a later desired-state change. Identity must also remain safe across deletion, restore, and any future reuse policy. |
| Durable cleanup completion | Soft-deleted rows retain cleanup data. Recovery APIs require a configured control-plane subject. Cleanup failures survive restart and later scans. | Retention is not finalization. There is no durable set of pending cleanup owners, conditional completion, or common pending-deletion status. Public reads return 404 before all external cleanup ends. |
| Destructive operations require positive evidence | Gateway cleanup requires explicit deleted state. Generated Kubernetes deletion checks owner labels, observed UID, and resource version. Denied reads and foreign namespaces have tests. | All destructive adapters need the same durable intent and identity checks. Labels require a trusted permission boundary; a matching name alone must never permit adoption. A stale action can still be in flight when deletion starts. |
| Status describes current observations and has distinct owners | Domain controllers check provider readiness and avoid some unchanged status writes. Count updates protect current cluster assignment. Patches preserve unrelated stored fields inside their transaction. | Public and controller Gateway writes share mutable phase/status fields. Status ownership is not compiler-enforced. Stale success, independent controller updates, and generation-specific failure conditions need tests. |
| Bounded calls and useful diagnostics | Generated clients bound requests and response sizes. Runtime cancellation joins workers. Protocol failure summaries omit private remote messages. | Controller notices do not supply the full queue-depth, duration, retry, and pending-cleanup metrics. Operators cannot yet identify every unconverged resource from durable status. |

The main local evidence is in the generated controller tests under
[`internal/generator/controller/testdata`](../internal/generator/controller/testdata),
the generated Kubernetes client, and the variant's Gateway, database, identity,
count, and service-account acceptance workflows. A passing queue test does not
prove correct application status or finalization.

The existing `TestConcurrentChangeCannotBeOverwrittenByGatewayPatch` checks a
write that races another write inside a serializable transaction. It does not
check the interval between a controller's external observation and its later
status request. A temporary PostgreSQL probe tested that interval on 2026-09-09:

1. Create a Gateway and read its privileged identity state.
2. Change its desired external DNS through the owner's normal update path.
3. Submit `Healthy` and `Running` through `UpdateControlPlane`, using the earlier
   observation.
4. Require a conflict instead of accepting success for the changed resource.

The probe failed in 0.24 seconds: the stored resource had the new DNS and the
old pass's `Healthy` status. The status API carries no expected revision or
generation. Starting a serializable transaction for that later request cannot
establish which resource version the controller observed. The temporary test was
removed after execution; it is not a passing acceptance test. The next
implementation must add this case as a permanent regression and make it pass.

Use the following implementation order. It replaces the prior plan to optimize
unused recovery counts first.

1. Define a generated resource-lifecycle storage contract. Separate immutable
   identity, per-write revision, desired generation, and deletion intent. Define
   which declared fields affect generation. Apply revision checks and status
   ownership in the same transaction as the write and event. Reject stale
   commits and return a conflict that requires a new observation. Do not use a
   timestamp or a client-supplied higher generation as proof of freshness.
2. Connect Gateway and database observations to conditional status operations.
   Test a desired-state change during provider work, independent status writers,
   deletion during an active pass, and restart. Required checks must cross REST,
   gRPC, storage, and generated runtime boundaries. An older successful pass must
   not mark a newer desired state healthy.
3. Add durable cleanup ownership and conditional completion. Keep cleanup data
   until required effects are confirmed absent. Do not make early public
   disappearance the proof of cleanup. Preserve repair data for late external
   effects; the existing late Keycloak creation tests must continue to pass.
4. Extend shared keyed scheduling for independent resource progress and bounded
   concurrency. Preserve one active action for each controller/resource identity,
   keep changes that arrive during work, and give deletion priority over obsolete
   desired work. Add cross-process ownership with fencing before permitting
   multiple active writers. A replica count of one is not a lease.
5. Generate bounded metrics and durable failure conditions. Cover queue depth,
   action time, retry counts, and age of pending cleanup. Avoid credentials,
   remote error bodies, and per-resource IDs in unbounded metric labels. Keep
   resource-specific diagnostic detail in access-controlled status or logs.
6. Complete common storage cursor adapters and remove unused counts. Preserve
   filters, deletion visibility, access checks, and database ordering. Measure
   queries and backlogs after the correctness contract is in place.

Common lifecycle storage, conditional writes, scheduling, metrics, and fencing
belong in STEGO. Hypershell supplies desired fields, owned conditions, provider
definitions, access policy, and cleanup dependencies. Do not add another local
controller framework or a Hypershell-specific primitive to the compiler.

The PR discussion identifies differences with other upstream proposals. This
variant will continue periodic observation and drift repair after a resource
reports healthy. Generation equality alone must not disable observation. Events
can carry snapshots, but snapshots do not replace current authoritative state.
Use one version vocabulary in STEGO rather than copying incompatible field names
from several proposals. These are local design choices, not claims that upstream
has resolved its open discussions.

One client-visible decision is pending: retain an authorized public read with a
Deleting state until completion, or retain immediate 404 with a separate operator
view. The recommended choice is visible pending deletion. Either choice still
requires durable cleanup tracking and must not permit resurrection. The user was
asked for this preference. Resource-version work does not depend on that answer.

Keep the existing no-exactly-once and no-cross-system-transaction limits explicit.
Eventual repair depends on stable desired state and responsive dependencies.
Local success does not establish a production recovery time or capacity limit.

## Progress after the baseline review

The generated [resource revision contract](resource-versions.md) now provides
opt-in per-write revisions and conditional PostgreSQL writes. The trigger also
protects immutable IDs and retained deletion. Generated gRPC helpers carry a
strict revision precondition without changing public protobuf message shapes.

The Gateway variant uses this contract for workload and identity observations.
A permanent application regression now passes: a REST desired-state change makes
an earlier controller observation fail through gRPC. The test also covers missing
and malformed preconditions, denied callers, event rollback, successful delivery,
and restart. The original failing probe remains useful baseline evidence.

This closes the tested stale Gateway status write. It does not complete desired
or observed generations, field ownership, database status writes, durable cleanup,
cross-process fencing, or production capacity evidence. Continue the work order
above from those remaining requirements.

The optional [generation and observation contract](resource-generations.md) now
tracks declared inputs and each observation group. Gateway phase and status use
a workload group. The application rejects owner writes and presents an older
observation as pending after an input change. This does not yet cover database
status, referenced-resource changes, automatic CRUD APIs, or durable finalization.

The database variant now uses generated revision storage and gRPC response
metadata. Its controller reads a revision before provider work. A later write
must match that revision. The API requires this precondition for configured
controller subjects and commits the write with its event. The application tests
cover stale observations, denied requests, rollback, restart, and deletion.
Controller tests require another provider observation after a conflict. Live
provider selection uses the current record. Database generations and observation
field ownership remain open; a revision check alone does not make stored status
current after an input change. See the variant's
[database evidence](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/database-observations.md).

The generated retained-read contract now provides one exact versioned resource
with its deletion state. The database controller uses it before provider work,
including work caused by deletion events. Missing records, failed reads, and
missing deletion metadata do not permit cleanup. A false deletion-event test
failed before this change because the event alone reached the provider. The
application regression also changes event fields and recovers after API restart.
Durable cleanup owners, conditional completion, and cross-process fencing remain
open. A fresh deletion read does not establish completion of those requirements.

The optional [cleanup contract](resource-cleanup.md) now records declared owners
and requires a deleted resource at an exact revision for each observation. The
storage transaction includes the event. Owner removal cannot silently discard
stored obligations. Hypershell applies it to the database provider and continues
periodic checks after a recorded success. A later pending or failed provider
check clears that confirmation. This supplies durable observations, but it does
not fence other processes or establish safe purge after late external effects.

Gateway login identity now uses the same cleanup contract. The private state API
uses generated retained reads. The Keycloak adapter confirms absence after a
delete response. The controller continues checks after completion, and a stale
completion requires new provider work. Application checks cover event rollback,
denied writes, restart, and a late client created after recorded completion.
See the variant's [identity cleanup evidence](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/gateway-identity-cleanup.md).

The Gateway cluster-move workflow exposed the need for cleanup by target.
A global workload owner cannot prove cleanup in every cluster that held the
resource. The generated target contract now records locations from a declared
field before provider work and preserves earlier targets after a move. Conditional
observations update one target and derive the owner's aggregate from all targets.

Hypershell now uses this contract for workload cleanup. The real Gateway workflow
confirms removal in the former cluster, restarts its controller, creates a late
namespace, and confirms that the old target becomes pending again. After another
restart and removal, that target completes while the new cluster stays pending.
The API check also covers independent identity cleanup, denied and stale writes,
event rollback, delivery, and restart. See the variant's
[target cleanup evidence](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/gateway-target-cleanup.md).

Enabling target history or changing its field mapping on existing rows requires
an explicit history migration. The compiler refuses to infer earlier locations
from the current field. It has no history import or target retirement protocol.
Database placement history, cleanup of former locations while a resource is live,
provider identity binding, permissions for other controller operations, cross-process
fencing, and parent finalization remain open.

The generated [grant policy](authorization-grants.md) now matches the verified
issuer, subject, resource type, operation, and target. Hypershell applies it to
cleanup observations. Its API tests use separate subjects for cluster targets
and identity cleanup. They reject writes across targets, owners, and resource
types, and deny configured subjects that lack a grant. Removing a grant and
restarting the API revokes that scope while preserving another subject's access.
A granted target still requires recorded resource history and the current
revision. See the variant's [cleanup permissions](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/cleanup-permissions.md).

Policies are immutable within each process. All API instances must be updated
for a grant change to apply throughout a deployment. The policy does not reload
or revoke grants across running processes. Provider credentials remain a
separate boundary, and other controller reads and writes still need distinct
permissions. The tested cleanup observation gap is closed. The complete controller
permission model remains open.

The Gateway workload controller now uses generated keyed scheduling with four
workers. A new application test first failed with FIFO scheduling: a blocked
provider call prevented a second deleted Gateway from completing cleanup after
API restart. With the shared keyed watch runtime, the second Gateway records
its observation and delivers its event while the first call remains blocked.
Releasing the first call then permits its completion. The check crosses REST,
TLS gRPC, retained storage, and the generated event runtime.

The generated scheduler combines duplicate keys and preserves changes during an
action. It permits one active action for each key within a runtime call. A
failed watch cancels and joins that session before reconnect and discovery.
Identity and database controller scheduling, admission beyond queue capacity,
cross-process ownership, and fencing remain open. See the
[keyed controller contract](controller-keyed.md).

A larger Gateway test exposed a queue-admission defect. With 1280 retained
Gateways and a 1024-key queue, repeated scan resets completed only 31 of 1279
healthy cleanups in 30 seconds. One provider remained deliberately unavailable.
The generated watch and scanner now wait for queue capacity. They keep their
current key, and the scan keeps its cursor. The same application check then
completed every healthy cleanup within its 30-second check. The failing Gateway
remained pending. The test also verifies gRPC state and delivery of the last
Gateway's new cleanup event after API restart.

A scan may wait longer than its resync interval. Producers must release locks
needed by actions before emitting. Capacity still includes pending, active, and
delayed keys. The generated producers retain one additional waiting key each.
No admitted key or retry delay is discarded. A full queue of persistent failures
can still block admission. A generated test verifies that admission resumes when
those providers recover. Durable retry storage and complete saturation handling
remain open; the user was asked to choose between persistent retry storage and
scan-based overflow recovery.

Gateway conditional patches now use the same generated exact grant policy as
cleanup observations. Hypershell maps phase/status, OIDC settings, and console
address to three separate operations. Workload and console grants require the
current stored cluster. The check and write share the mutation transaction.
Moving a Gateway removes the former cluster controller's write authority.
Mixed field groups and other controller patch fields are rejected.

The new application test failed against the previous handler: an identity
controller without a workload grant could publish workload status. With the
permission check, it passes separate subject and target checks, unchanged state
and no Gateway event on denial, successful event delivery, REST bypass denial,
placement changes, and grant removal after API restart with the same token.
See the variant's [controller write contract](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/controller-write-permissions.md).

This change needs no additional compiler mechanism. Exact matching and strict
grant parsing remain generated by STEGO; the application supplies its field
mapping. OIDC remains a workload input with existing public-owner access. Other
controller methods, database field permissions, provider credentials, and
cross-process fencing still need work. This is not a complete permission model.

The later variant CI run `34416352961` failed the backlog's 30-second cutoff:
1009 of 1279 healthy cleanups completed. Discovery completed, but that result
does not prove why the remaining work was slower. The functional gate now allows
90 seconds and records progress and total elapsed time. It retains the same
backlog, provider failure, worker count, and complete-cleanup requirement.
The initial local 30-second result remains historical evidence, not a portable
performance target. Production recovery and throughput targets remain open.

The final local PostgreSQL/Keycloak race suite passed in the variant, with a
614.098-second acceptance package run. The backlog completed every healthy
cleanup in 17.22 seconds after controller startup. The actual Gateway Kubernetes
workflow passed in 229.855 seconds. The separate database workflow passed in
88.948 seconds after its restart test was changed to wait for a successful TLS
read through the Service. A prior CI connection refusal is recorded in the
variant's database evidence; the later pass does not prove its cause. These
checks add application evidence without completing the enterprise goal.

Gateway identity now uses the generated keyed watch runtime with four workers.
The application cleanup check first failed with its FIFO worker: a blocked
identity provider prevented another Gateway from completing after API restart.
With the shared scheduler, the second Gateway commits cleanup through TLS gRPC
and delivers its event while the first stays blocked. Releasing the first call
then permits its completion. The same test helper checks workload cleanup.

The identity controller's existing user-scan cursors now have a short lock.
No API or provider call holds that lock. A race test starts 64 Gateway user scans,
interrupts each after its first user, and verifies that each resumes at its own
second user and removes its completed cursor. The real Keycloak identity and
stored-grant user-login workflows also pass with the shared scheduler.
The application adds no scheduler, retry map, or worker pool. The generated
runtime remains the owner of those mechanisms.

Database scheduling, durable retry storage, complete queue saturation handling,
cursor memory bounds, cross-process ownership, and fencing remain open. The
identity cursor data is process-local; a queue bound alone does not prove a bound
on all application memory. See the variant's
[identity scheduling evidence](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/gateway-scheduling.md).

The identity migration passed the full local PostgreSQL/Keycloak race suite;
its acceptance package took 652.983 seconds. The actual Gateway Kubernetes
workflow passed in 219.678 seconds. The prior variant revision `08b399f` also
passed all four CI jobs in run `34418365029`, including the revised backlog and
database restart checks. The identity migration requires its own new CI run.
The enterprise goal remains active.

The database controller now uses the same generated keyed watch runtime as the
Gateway controllers. A new application check first failed with FIFO scheduling:
after API restart, one blocked provider call prevented another deleted database
from completing cleanup. With four generated workers, the second database
records cleanup through TLS gRPC and delivers its event while the first remains
blocked. The same test helper checks database, Gateway workload, and Gateway
identity cleanup.

The database adapter validates event types and matching IDs before admission.
The queue retains only IDs. Each action reads current retained state and checks
the ID, revision, deletion metadata, and cleanup owner. Current live state selects
provisioning; current deleted state selects cleanup. A delete-type hint cannot
authorize deletion of a live database. Missing state remains an error for every
event type and permits no provider work. This removes event-type dependence from
the action while preserving current-state authority and one queue key per ID.

Invalid recovery records, list pages, and missing replay capabilities stop the
runtime. Tests check that these invalid inputs cannot reach resource reads or
provider work. Live list calls have a 20-second limit. Replay has a scoped
context, but still needs a separate idle deadline. Durable retries, complete
queue saturation handling, count-free storage cursors, field ownership,
cross-process fencing, and production capacity remain open. See the variant's
[database scheduling evidence](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/database-scheduling.md).

The database migration passed the full local PostgreSQL/Keycloak race suite;
its acceptance package took 647.391 seconds. The real database Kubernetes
workflow passed in 76.921 seconds, and the complete Gateway workflow passed in
215.211 seconds. The preceding identity revision `6074a76` passed all four CI
jobs in run `34419484600`. The database migration requires its own new CI run.
The complete enterprise goal remains active.

The database migration at variant revision `765985d` passed all four CI jobs in
run `34420720619`. Its finite replay still had no receive limit. A new application
check opened a real TLS gRPC replay after API restart, confirmed its capability,
and held its first receive call until cancellation. The old controller did not
cancel that call within 25 seconds, so no later recovery scan could complete.

Controller component 1.6.0 now supplies `ScanStream`. STEGO owns separate setup
and receive limits, item bounds, callback completion, and stream cancellation.
No receive timer runs while queue admission waits for capacity. Hypershell
retains protocol validation and sets 20-second limits. With this scanner, the
same application check passed: a later replay completed cleanup and delivered
its committed event. See the [finite stream contract](controller-stream.md).

The compiler race suite and static checks passed. Compiler revision `9cf5a48`
also passed CI in run `34421072103`. On 2026-09-10, the complete application
PostgreSQL/Keycloak race suite passed with a 656.062-second acceptance package
run. The real database Kubernetes gate passed in 86.616 seconds. It covered TLS
access, persistence, foreign namespace denial, offline deletion, late effects,
and replay under C and ICU ordering. Earlier interrupted attempts had no final
results and are not counted as passes.

The variant change is `2dcd5ce`. Pinned regeneration passed after its commit;
all 71 generated and dependency file hashes remained unchanged. Module
verification and application static checks also passed.

Durable retries, complete queue saturation handling, cursor bounds, field
ownership, cross-process fencing, and production capacity remain open. The
enterprise goal is not complete.

Database conditional observations now use the generated exact grant policy.
The application permission test first failed in 3.21 seconds: a configured
identity controller without a database grant could publish database status.
Hypershell now maps status and the connection-secret reference to
`ManagedDatabase` / `observe.provider`, with the stored provider name as target.
The field and grant checks run in the mutation transaction before any patch.

The focused check passed in 5.48 seconds. Wrong subjects, providers, and
operations, missing grants, and mixed field groups cannot change database state
or commit a database event. Valid observations advance the revision and deliver
the event.
Removing a grant and restarting the API denies the same token. Existing stale
observation, transaction rollback, catalog, and Gateway grant tests also pass.
See the variant's [database write contract](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/database-write-permissions.md).

STEGO already supplies strict grant parsing and exact matching, so this change
needs no new compiler mechanism. Hypershell supplies its field mapping and
provider scope. Public administrator access to observation fields remains in
the current API. The connection-secret ownership choice, database generations,
other controller operations, stable provider identity, and fencing remain open.

The full PostgreSQL/Keycloak race suite passed on 2026-09-10 with a
664.210-second acceptance package run. The real database Kubernetes gate passed
in 77.547 seconds. The complete Gateway Kubernetes gate passed in 213.285
seconds, including database and identity setup, access denial, provider
persistence, restart, offline deletion, and late-effect cleanup. These results
verify the application grant change; they do not complete the enterprise goal.

The variant commit is `87966df`. Pinned generation passed after commit, with
all 71 generated and dependency file hashes unchanged. The compiler pin remains
`9cf5a48d2b7bbf7d32b576a23d87a18b6390977b`. Application static checks and module
verification passed. The preceding variant CI run had passed its database,
Gateway, and sandbox jobs; its full acceptance job was still running when these
local checks finished. That run is not yet counted as a complete CI pass.
Gateway ID recovery, database deletion replay, and service-account recovery now
use the generated CursorReader storage contract. An application test first found
one unused count and two total reads per Gateway or database recovery page. With
the new reader, the page uses one read and no count. Denied callers perform no
read. STEGO owns the bound ID predicate, database order, shared filters, row
limits, and continuation. Hypershell retains access, canonical IDs, and state
policy. Deleted service-account streams now select deleted rows directly.

The [storage cursor contract](storage-cursors.md) records C and ICU ordering,
current-observation filtering, related access, transaction reads, cancellation,
and measured query cost. The application passed the full PostgreSQL/Keycloak
race suite in 673.043 seconds, the real database gate in 66.974 seconds, and the
complete Gateway gate in 206.104 seconds. Variant cc36ff2 is on remote main;
regeneration preserved all 73 generated and dependency file hashes.

This removes common query construction and unused counts from three recovery
paths. Other discovery queries, cursor memory bounds, durable retries, complete
queue saturation handling, cross-process fencing, field ownership, and production
capacity remain open. The enterprise goal is not complete.

Controller 1.7.0 now generates an observation time reserve. Application tests
first showed that a provider timeout could leave a previous Healthy Gateway or
ready database in place because its failure write used an expired context.
`RunObservation` bounds provider work and leaves time for a conditional write
under the same parent. It preserves work and commit errors, waits for callbacks,
and stops writes on parent cancellation. Hypershell retains status values,
revision preconditions, exact grants, and cleanup meaning.

The [observation budget contract](observation-budgets.md) records five application
workflows: Gateway and database status, plus workload, database, and identity
cleanup. All check timeout failure, event delivery, API restart, and recovery.
The workflow also exposed an initial gRPC error hidden by a capability check;
the application now preserves that status for the generated retry policy.

Compiler `46b5f4e` passed its race suite, static checks, and CI. Application
`354715a` passed the complete PostgreSQL/Keycloak race suite in 917.518 seconds,
the real database gate in 81.938 seconds, and the complete Gateway gate in
231.422 seconds. Both repositories have these changes on remote main. Pinned
regeneration after the application commit preserved all 74 generated and
dependency hashes.

A reserved write budget does not guarantee a successful observation. API failure,
permission loss, a concurrent revision, or parent cancellation can still prevent
it. Durable conditions, freshness after controller loss, retry persistence,
complete queue saturation handling, and cross-process fencing remain open.
The enterprise goal is not complete.

Database recovery now uses existing generated mechanisms for all retained IDs.
The application requests one finite stream of live and deleted databases. Its
server uses `Scan` with storage `CursorReader`; its controller uses `ScanStream`
with `RunKeyedWatch`. This removes the separate live offset-list loop and its
unused totals. Hypershell retains recovery authorization, canonical IDs, replay
scope validation, and event shapes. No new compiler API is required.

The retained replay test first failed because the server did not support that
scope. The focused checks then passed in 58.136 seconds. They covered both the
legacy deleted-only mode and retained mode, more than one page, C and ICU order,
access denial, empty results, restart, an idle stream, and current-state cleanup.
Another check deleted an earlier row between pages and still recovered all 21
IDs exactly once. A query recorder found one read and no count per retained
page, with no reads for a denied caller.

The server continues to support the old deletion replay. The new controller
requires an exact retained-scope confirmation and stops if that protocol is
unsupported. Deploy the API before that controller. Page snapshots, watches,
and repeated scans still define recovery; this is not a durable queue or a
cross-process ownership mechanism.

The Kubernetes database gate passed in 94.465 seconds, including both replay
modes. The complete Kubernetes Gateway gate passed in 238.447 seconds with the
new database recovery path. It covered access, service accounts, provider
persistence, restart, namespace replacement, offline deletion, and former-cluster
cleanup. These runs do not establish production recovery capacity.

The full PostgreSQL/Keycloak race suite passed with 114 acceptance tests and a
941.540-second acceptance package run. Static checks and module verification
passed. Application commit `ea79d5310f598522989fca3a85a69b886267d8f8` is on remote
main. Regeneration after commit used compiler
`46b5f4e5327dfd056cafb65fe3a39ff0cde74500` and preserved all 74 generated and
dependency file hashes. The application [database recovery contract](https://github.com/jsell-rh/hypershell-stego/blob/ea79d5310f598522989fca3a85a69b886267d8f8/acceptance/database-recovery.md)
records the protocol, deployment order, test coverage, and limits.

The preceding observation revision `354715a` passed all four CI jobs in run
`34483963655`. The new recovery revision still requires its own CI result.
Other discovery paths, durable retry storage, complete queue saturation
handling, cross-process fencing, field ownership, and production capacity remain
open. The enterprise goal is not complete.

Identity reconciliation now uses generated bounded reads for exact user and
role lookups and grant existence. An owner-state read uses four queries instead
of six; viewer and removed-role reads use six instead of nine. These paths run
no count query. Access denial, retained user identity, role precedence, and
missing-state errors have PostgreSQL checks. The full query-change race suite
passed 116 acceptance tests. See [identity reads](identity-query-reads.md) for
allocation cost and the remaining inventory and progress limits.

The real Gateway gate then found a shared client defect: copying nil gRPC headers
changed the signal used to receive a terminal status. The raw-client controller
test had missed the generated wrapper. STEGO now preserves nil headers, and both
the compiler and application test the generated client. The fixed-compiler
Gateway gate passed, including the failed deletion-before-startup case. See
[stream header evidence](grpc-stream-headers.md). This is another case where the
application workflow determined the common infrastructure correction.

Identity inventory recovery now uses common resumable cursor scans. The old
controller stopped after 10,000 retained grant references. The application
regression exposed that limit; controller 1.8.0 and the variant's private cursor
RPC now continue across bounded passes. A PostgreSQL/gRPC runtime check resumes
a partial page after API restart and reads 10,106 retained references. Current
identity reads, real login from stored grants, and independent cleanup after
restart passed. See [resumable scans](resumable-scans.md).

The saved progress is still process-local. A controller process restart repeats
the full inventory. Frequent restarts can delay later users. Durable progress,
retry storage, cursor-map bounds, and cross-process fencing remain unresolved.
The new cursor contract does not establish an acknowledgment or a state snapshot.

The keyed runtime now supplies [bounded controller metrics](controller-metrics.md).
Queue pressure, active work, retry counts, action outcomes, and action duration
can be observed without resource IDs or private error labels. Hypershell's
cleanup workflow checks these values while one provider action waits and another
resource completes. Durable conditions and cleanup-age metrics remain open.

[Cleanup summaries](cleanup-summaries.md) now provide the pending count and oldest
original deletion time for a selected owner and retained target. The generated
sampler runs independently of recovery scans and reports unavailable data after
a failed read. This adds cleanup-age data without resource labels. It does not
complete durable conditions, per-resource diagnosis, or safe history retirement.

The [CNPG provider gate](cnpg-provider-evidence.md) extends the same generated
runtime to a second database provider. It covers real encrypted SQL, restart,
Cluster specification repair, and cleanup after a denied deletion. Common API
discovery validation now belongs to the generated Kubernetes client. CNPG
resources and readiness policy remain in Hypershell. CNPG's readiness condition
lacks an observed generation, so this evidence does not certify every external
setting at a specific generation. The later Gateway work is recorded below.

The shared CNPG Gateway workflow exposed two more common requirements. A
computed role list must use the exact Kubernetes version from which it was
built; attaching a newer version to an old list can remove another Gateway's
role. STEGO now supplies `PatchOwned` for this case. A resource write must also
reject unknown fields. An incorrect CNPG policy field was silently pruned,
which retained the SQL database after its resource was deleted. The generated
Kubernetes client now requests strict field validation. Both contracts have
independent generated Widget tests.

The provider still needs current external evidence. CNPG's role status did not
repair direct SQL drift within the gate's limit. Hypershell now checks SQL role
permissions and authentication through verified TLS and requests operator repair
when needed. Cleanup requires actual SQL role and database absence before it
removes credentials and keys. These provider rules run inside STEGO's existing
controller, retry, recovery, and conditional-observation contracts. They do not
add an application controller framework. See the
[CNPG evidence record](cnpg-provider-evidence.md) for the test result and limits.

The full CNPG Gateway gate passed under race detection in 335.777 seconds at
application commit `0cf98a51900e58ac9b3cccdcad525d93dff481ff`. It verifies separate
Gateway data and keys, privilege and password repair, actual SQL deletion,
retained cleanup after denied SQL access, and recovery from a late SQL role
after restart. The deployment regression passed in 215.319 seconds. Pinned
regeneration preserved 83 generated and dependency file hashes. Cross-process
fencing, durable progress and conditions, safe history retirement, and measured
production recovery capacity remain open.

The two CNPG SQL checks now use the common generated
[PostgreSQL read client](postgres-reads.md). Verified TLS, environment isolation,
connection lifetime, time and message limits, and safe SQL error metadata no
longer reside in the provider. SQL queries and decisions about repair remain
in Hypershell. The complete CNPG Gateway workflow passed with this boundary
in 362.168 seconds under race detection.


## Durable identity scan progress

The Gateway identity controller now uses generated `ScanCheckpointed` and
`CheckpointStore` contracts. This replaces the process-local cursor map described
above. A page-budget regression with 10,100 references first failed after
controller replacement: the new controller repeated the first 10,000 references.
The saved PostgreSQL cursor now permits the next controller to reach the tail.

The generated runtime reserves a commit budget after work. Parent cancellation
prevents the commit. A failed save can repeat the unsaved prefix. A complete scan
resets the cursor and retains its version to reject stale writers. PostgreSQL
stores one row per permitted resource and fixed scope. Hypershell permits one
identity-user scan scope for each Gateway. It owns access checks, source mapping,
and provider actions; it has no cursor cache or checkpoint SQL.

Application tests read 10,106 retained references across API restart. Another
uses the actual identity controller with a recording provider, replaces both the
controller and API, and checks current grant state before resumed provider work.
The public Gateway revision does not change for a checkpoint save. See
[scan checkpoints](scan-checkpoints.md) for the contract and limits.

This closes the identity cursor's restart and process-memory gaps. Distributed
provider ownership, durable retry scheduling and conditions, safe history
retirement, and production capacity evidence remain open. A checkpoint version
is not a provider lease and does not fence external actions.
