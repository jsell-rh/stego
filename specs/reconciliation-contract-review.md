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
