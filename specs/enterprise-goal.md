The active goal is to make STEGO a reliable compiler for enterprise services and
to build a fully STEGO-based Hypershell variant. The user authorized this work on
2026-09-08. The findings in `repository-assessment.md` are the initial defect
list. A passing build alone does not satisfy this goal.

The reference application is `/home/jsell/code/hypershell` at commit
`14256be29bcfe4fff38bcaf4a41511cb394ea8e1`. The test bed is
`/home/jsell/code/hypershell-stego`, cloned from
`https://github.com/jsell-rh/hypershell-stego.git`. The remote was empty at the
start of this work. Keep the reference checkout unchanged.

Hypershell requires REST and gRPC contracts, watch streams, PostgreSQL,
transactional authorization rules, gateway provisioning, service accounts,
observability, SDKs, a CLI, a web console, and deployment support. Preserve its
required behavior. Do not treat a CRUD-only replacement as a complete result.
Remove the need for rh-trex-ai generation and runtime infrastructure in the
variant. Keep application-specific decisions separate from reusable generators.

The following milestones define completion:

| ID | Requirement | Acceptance evidence | State |
| --- | --- | --- | --- |
| C1 | Strict compiler input and one semantic validation stage | Unknown fields, invalid constraints, duplicate keys, unsupported capabilities, and invalid paths fail before output changes | Active |
| C2 | Complete project and fill workflow | Init, apply, fill create, test, build, repeated apply, and drift pass in a fresh directory | Active |
| C3 | Reproducible and recoverable generation | Compiler and input identities, stable output, dependency ownership, state format, interrupted-write recovery, and concurrent apply tests | Active |
| C4 | Secure authentication and authorization | Signature, issuer, audience, expiry, key rotation, scope isolation, and denied request tests | Active |
| C5 | Correct storage and event behavior | PostgreSQL integration, explicit migrations, concurrency, transactional writes, durable event delivery, and failure tests | Active |
| C6 | Production runtime support | Health, readiness, tracing, metrics, bounded requests, deadlines, shutdown, and resource limit tests | Active |
| C7 | Explicit generator contracts | Typed wiring, supported capability checks, typed business extension contracts, and compatibility tests | Active |
| H1 | Hypershell compatibility baseline | Inventory and executable checks for REST, gRPC, RBAC, watch, SDK, CLI, UI, and deployment contracts | Active |
| H2 | STEGO Hypershell implementation | Clean generation and tests without rh-trex-ai; reviewed domain code remains outside generated output | Active |
| H3 | System verification | Service integration and local deployment checks, security tests, race tests, measured performance, and regeneration checks in CI | Active |

Resolve work in small atomic commits. Each behavior change needs a regression
check that can fail when the behavior is wrong. Use runtime tests for runtime
claims. Use explicit compatibility evidence for existing Hypershell behavior.
Do not weaken tests or drop requirements to obtain a passing result.

Use complete application workflows to select and assess infrastructure work.
The first required gate is Gateway creation and retrieval, an atomic owner
grant, access filtering and denial, generated event delivery, and REST, gRPC,
restart, and regeneration checks. The variant records the named tests in
`acceptance/gateway-workflow.md`. Its `scripts/check-gateway.sh` command runs
the gate locally and in CI. It requires PostgreSQL and enables race detection
in both the tests and their generated application processes. Select further
common capabilities from demonstrated application requirements. Keep each
capability general and verify it with a separate small service.

The complete gate passed again on 2026-09-08 with compiler revision
`77290f7697c75f73b200253700aea754437c3c34`, Go 1.26.8, and PostgreSQL 18.6.
Regeneration had no changes or drift. The full variant suite passed with race
detection extended to its generated application processes. No compiler code
change was required for this gate review. Production Kafka, database upgrades,
cluster provisioning, and capacity remain outside this acceptance result.

The user authorized direct pushes to both remote repositories. Push completed
commits after their checks pass. Do not wait for pull request merges.

The user confirmed that STEGO must provide common infrastructure. Keep unique
domain behavior in application modules or fills. Do not add Hypershell-specific
rules to STEGO. Test new common capabilities with a separate small service as
well as Hypershell. Whether the variant must upgrade an existing database in
place remains pending user input. Compiler correctness work can proceed.

Security and performance claims require evidence. Record the environment,
commands, outcomes, and limits of each acceptance run. Ask the user when a choice
changes application behavior, the deployment trust boundary, or migration
compatibility. Routine implementation choices do not require approval.

Completed checks for C1: strict declaration and registry parsing rejects unknown
fields, duplicate keys, multiple documents, anchors, aliases, and merge keys.
Document reads are bounded. The root test suite passes. A 10-second parser fuzz
run completed 108,504 executions without a failure. All three CLI commands now
use the same semantic validation gate and declaration snapshot. Regression tests
check that invalid constraints cannot invoke generators or change existing
output and state. Supplied component settings and defaults now have type and
schema checks. Output namespaces must be canonical and non-overlapping. Invalid
output and saved-state paths fail before writes or deletion. Corrupt state no
longer resets silently. The root suite passes after these changes. Capability
checks and complete schema semantics remain open.

Apply now checks symbolic links and special files before writes. Rooted file
operations restrict output writes to their directory. Each file is written to a
temporary file and renamed after a successful sync and close. This prevents
partial file content. Transaction recovery and process locking are described
below.

The compiler now requires Go 1.26.8. It uses the rooted rename and directory
operations added in [Go 1.25](https://go.dev/doc/go1.25#os), with a patch release
from the supported 1.26 series. See the
[Go release history](https://go.dev/doc/devel/release).

Remote registry resolution now validates full commit SHAs before cache access.
It prepares private checkouts and publishes verified content through a rename.
Cache reuse requires the exact commit and a clean tree, including ignored and
untracked files. Modified cache entries are preserved and rejected. Local
registry content identities and compiler build identities remain C3 work.

The application now owns its root Go module. STEGO preserves application
dependencies and settings. It adds missing component requirements and raises
versions only when a component requires a higher minimum. It does not lower an
application's selected version. Existing module settings supply the CLI defaults.
Invalid requirements and conflicting module names fail before writes. Module
edits no longer count as generated output drift. The full root suite passes.
The separate dependency command is described below.

New fills use canonical generated slot types and include a constructor. An
unfinished method returns an error. Fill creation rejects unsafe names, existing
directories, and symbolic links at the fills directory. The workflow regression
test creates a real service, binds a new fill, resolves dependencies, builds the
service, and executes the fill through its generated interface. A second apply
has no changes or drift. This is a build and contract check, not a production
runtime or security acceptance result. Full protobuf validation remains open.

Component wiring can now declare constructors that return errors. A failed
constructor stops startup and runs cleanup for earlier constructors. A generated
program test checks both the returned error and resource cleanup. This supports
authentication configuration checks at startup. Other lifecycle paths still need
review, including database setup, server errors, and shutdown.

The first startup test exposed an omitted constructor when a route used a
handler as a direct argument. Wiring reference checks now parse Go expressions.
They detect direct arguments and leave string values unchanged. The generated
startup test and the full root suite pass after this correction.

The default JWT component now verifies RS256 signatures with golang-jwt v5.3.1.
It requires an HTTPS issuer, an API audience, a subject, an issue time, and an
expiry. It checks the `JWT` token type and rejects duplicate JSON members and
unsupported key headers. Token and key-file reads have size limits. A missing or
invalid public key stops startup. The generated runtime tests use the race
detector and cover forged signatures, algorithm changes, claim failures,
ambiguous input, invalid headers, and concurrent verification. The rules follow
the [JWT library validation options](https://golang-jwt.github.io/jwt/usage/parse/)
and [RFC 8725](https://www.rfc-editor.org/rfc/rfc8725.html).

The first authentication benchmark used Go 1.26.8 on Linux amd64 with an Intel
Core Ultra 9 185H. The command was
`STEGO_BENCH_AUTH=1 go test -v ./internal/generator/jwtauth -run '^TestGeneratedAuthenticationRuntime$' -count=1`.
For a 2048-bit RSA key, one run measured 37,315 ns/op, 9,344 B/op, and 212
allocations/op. This is a local verification baseline, not service throughput.
Key rotation currently requires a restart. Automatic key discovery, rotation,
authorization, and the separate RH SSO component remain open C4 work.

The Hypershell test bed now validates the pinned reference inputs with complete
OpenAPI and protobuf parsers. Its checks cover 37 REST operations, 41 gRPC
methods, and six watch streams. Field ownership and service-account secret
response checks pass. Reference loading cannot fetch remote schemas. This is H1
evidence only; no Hypershell implementation is claimed.

Both repositories now have CI workflows with pinned action commits, read-only
repository permissions, module verification, and race tests. The equivalent
local checks passed in both repositories. The first hosted CI runs also passed:
[STEGO](https://github.com/jsell-rh/stego/actions/runs/34275863387) and
[Hypershell contracts](https://github.com/jsell-rh/hypershell-stego/actions/runs/34275851933).

Plans now compare actual output contents with desired contents. Apply checks
snapshots of output files, orphaned files, the service declaration, module,
registry configuration, and saved state before its first write. Changed files
require a new plan. Plans are bound to their project and output directory.
Input-only changes can update saved state without changing generated code.
Tracked files have a 64 MiB size limit. Complete input identities remain open.

Apply now holds an operating-system file lock and checks snapshots again after
it acquires the lock. Concurrent apply tests allow one writer. A subprocess test
confirms that a terminated process releases its lock. The compiler tests pass
with the race detector. Windows amd64 and macOS arm64 test binaries also compile;
their lock implementations have not been tested at runtime here. The lock file
stays at `.stego/apply.lock` and must not be removed while STEGO processes run.

Apply now saves a versioned transaction record before output changes. The record
contains the required contents, hashes, and prior file snapshots. Recovery checks
the complete record and all affected files before it writes. It completes the
saved output and writes state last. Plan and drift reject a pending transaction.
Tests cover each failure stage, abrupt subprocess exits, corrupt records,
conflicting files, source edits after interruption, and final output verification.
The full root suite passes with the race detector. Windows and macOS test
binaries compile; runtime and power-loss checks on those systems remain open.
Unix directory metadata is synced along with file contents. Apply is recoverable;
it does not give external readers one atomic view of all files. Complete compiler
and registry identities and broader state migration
support remain C3 work.

The repeated-apply workflow now checks both `go.mod` and `go.sum` after a second
dependency resolution. This exposed an unused database JSON dependency. The
PostgreSQL generator now adds that dependency only for JSON fields. The CLI and
PostgreSQL generator tests pass with the race detector.

`stego deps` now resolves dependencies in temporary module files. It verifies
modules, checks package dependencies without module edits, and checks that
resolution is stable. Component minimum versions must remain satisfied. It holds
the project write lock, checks source and local replacement module snapshots,
and saves both module files through the recovery transaction. Command failures,
cancellation, source additions and deletions, external replacement edits, and
interrupted commits have regression tests. The fresh service workflow uses this
command and builds with `-mod=readonly`. The full root race suite passes. This
does not complete dependency security review or prove a hermetic build.

Validation now rejects active components without a generator in the compiler
build. Missing and nil implementations fail before any generator runs or output
changes. The full root race suite passes. Registered health and tracing stubs
still need real implementations; generator presence alone does not prove that a
capability works.

Generated HTTP services now use network deadlines and a graceful shutdown path.
SIGINT and SIGTERM stop new requests and allow a ten-second drain interval.
An expired drain closes remaining connections and returns an error. Startup,
database setup, and server errors return through deferred cleanup before exit.
Runtime tests check request draining, forced closure, listener failure, signal
handling, header size, slow input, write deadlines, and idle connections. The
full root race suite passes. These defaults cover the current request-response
API. Stream-specific deadlines, handler execution limits, readiness, telemetry,
database pool settings, and further runtime checks remain open.
The generated HTTP tests also compile for Windows amd64 and macOS arm64. Their
runtime behavior has only been tested on Linux here.

Both checked-in user-management examples fail current validation because their
`event-publisher` mixin requires the missing `kafka-producer` generator. Their
generated files have not been refreshed. Preserve the publishing requirement.
Implement durable event delivery before regeneration, then check both examples
with apply, dependency resolution, tests, build, and repeated apply. The RH SSO
example also requires the outstanding authentication review. Do not treat the
old example output as evidence that those features work.

The first durable queue generator now emits PostgreSQL queue code and an explicit
migration. Generated runtime tests use PostgreSQL 18.6 and the race detector.
They check atomic write/notification commit, rollback, retries, delivery order,
concurrent leases, stale acknowledgements, and abrupt process exits. A small-queue
benchmark is recorded in `durable-events.md`. The queue is not yet a registered
service capability. Storage integration, Kafka delivery, callback semantics,
migration management, and production operations remain required C5 work.

The queue generator now includes a worker with bounded concurrency, attempt
deadlines, retry delays, and shutdown handling. PostgreSQL tests check stable
delivery IDs, ordered retries, stale leases, unknown destinations, and shutdown
acknowledgements. Fixed failure codes and counters avoid storing sink error
text. Handlers must honor context cancellation. Kafka and service integration
remain open; this is not yet a complete publishing capability.

The Kafka publisher library now verifies TLS and supports mutual TLS and
SCRAM-SHA-512. It bounds credentials, broker-controlled SCRAM work, memory, and
delivery attempts. Protocol tests check acknowledgement policy, invalid trust,
credentials, broker denial, timeout recovery, and concurrent close. A combined
generated-code test publishes a committed PostgreSQL outbox message through a
TLS Kafka protocol fixture. This includes JSONB payload expansion. The fixture
is not a real Kafka deployment. Compiler registration, service composition,
production broker tests, and deployment checks remain open.

The compiler now accepts explicit background task constructor indexes. Tasks
must implement `Run(context.Context) error`. Services can run tasks with or
without HTTP routes. A task failure cancels the other tasks and starts HTTP
request draining. The service waits for all tasks before resource cleanup.
Generated tests cover cancellation, failure, cleanup order, shared dependencies,
name collisions, HTTP draining, and process signals. The contract and its limits
are recorded in `runtime-lifecycle.md`. The full root race suite passes with
PostgreSQL required. Generated task tests also compile for Windows amd64 and
macOS arm64. Outbox and Kafka service composition remain open.

The PostgreSQL generator now supplies a serializable transaction scope. Its
store operations and staged outbox messages can commit together. Invalid
notifications, callback errors, cancellation, database failures, and panics
roll back the scope. PostgreSQL tests cover prepared statements, copied payloads,
retained and nested scopes, SQL deadlines, mutation rollback, and serialization
failure without callback replay. A local create-plus-notification benchmark is
recorded in `store-transactions.md`. The full root race suite passes with
PostgreSQL required. Shared public contracts, domain rule
injection, handler use, and Kafka service composition remain open.

The compiler now emits a versioned public storage contract once per service.
HTTP and PostgreSQL use the same types and errors. Storage no longer imports
HTTP. A domain package outside generated output can use the public transaction
interface to commit a resource and an outbox message. PostgreSQL tests cover
that rule, HTTP reads, rollback, and notification rejection when no queue is
configured. The integration test also exposed a constructor argument rename
error. Assembly now distinguishes package and value references, and a runtime
test checks shared dependency identity. The full root race suite passes with
PostgreSQL required. The constructor fix also passed its own isolated compiler
and CLI checks. See `shared-storage-contract.md` for
contract limits and remaining service integration work.

The next acceptance gate is the Hypershell Gateway workflow. It must prove
creation and retrieval with the required IDs and API shapes, atomic owner
grants, access filters and denied requests, and event delivery through the
generated runtime. REST, gRPC, restart, and regeneration checks must pass before
this gate is complete. Infrastructure work must support this workflow.

Outbox and Kafka are now registered components. The event mixin composes both.
The generated worker reads deployment settings, checks the queue, and owns
publisher startup and cleanup. Explicit constructor resources work with SQL
and GORM. PostgreSQL and TLS protocol fixture tests cover runtime delivery and
restart. This does not complete the Gateway acceptance gate.

The variant now contains a Gateway domain service and generated common storage,
authentication, and event code. PostgreSQL tests prove Gateway, owner grant, and
event atomicity, including rollback when the grant or event fails. Access tests
cover owners, viewers, removed grants, admin reads without creation rights,
opaque denied reads, and filtering before count and pagination. The shared
relation filter also has Record/Membership tests in STEGO.

A test builds and starts the generated event process. It delivers a Gateway
event through a mutual-TLS Kafka protocol fixture, stops, and delivers another
event after restart. A new database connection retains the resource and owner
grant. Signed-token tests use the generated verifier and configured role claim.
The variant pins the compiler revision and checks repeated generation, module
resolution, and drift in CI. A local filtered-page benchmark is recorded in its
`acceptance/README.md`. Both repositories have passed race checks. The initial
Gateway commit passed remote CI.

The generated process now serves Gateway REST creation, retrieval, and lists.
The application supplies domain mappings through a common HTTP factory.
Signed-token tests cover filtered counts, owner and viewer access, removed
grants, denied requests, malformed input, and grant/event failure rollback.
The same process delivers the created event and retains access after restart.
Responses are checked against the pinned OpenAPI contract. The supported list
parameters are page and size; the maximum page size is 100. Other REST query
features remain open. A separate Record service checks the common HTTP code.

The variant now uses generated protobuf clients and service definitions for
Gateway creation, retrieval, and lists. A common TLS gRPC runtime calls the same
domain service as REST. Tests compare complete wire descriptors with the pinned
reference, read resources across both transports, check atomic failures, deliver
events, and retain owner access after restart. A separate Record service tests
the shared gRPC code. Pinned regeneration and remote CI passed for
[STEGO d04350c](https://github.com/jsell-rh/stego/actions/runs/34286053277) and
[the variant 62cdfaa](https://github.com/jsell-rh/hypershell-stego/actions/runs/34286086055).
The requested Gateway workflow gate is complete. This proves the first
application workflow, not the full enterprise or Hypershell goal.

The workflow exposed three required abstractions: a public transaction and
relation-filter contract, typed domain transport factories, and compiler-owned
protobuf inputs with snapshots. All common code has independent Record tests.
Hypershell owns placement, owner grants, field mappings, and authorization rules.
Its gRPC list benchmark averaged 16.79 ms across 100 requests through a separate
generated process, with TLS and token verification. The data set contained 200
Gateways, of which 100 were visible; each page contained 20 items. This is a local
baseline, not a production capacity claim.

The Gateway list now has REST search and ordering. Application tests check that
search cannot bypass the access filter, including `OR` expressions and count-only
requests. The work exposed incomplete AST validation, shared parser cache races,
and numeric precision loss in the old search code. The replacement binds values,
checks declared fields and types, preserves numeric source text, and bounds
parsing. Independent Record tests run against PostgreSQL with the race detector.
Related-resource search remains open. Field selection also remains open: the
reference returns an array for a `fields` query while its OpenAPI contract says
GatewayList. The user has been asked which response behavior to preserve.

Full application models, role projections,
other placement modes, production Kafka checks, and the other Hypershell
workflows also remain open. Separate versioned API contracts are the current
design assumption; the user has been asked whether to use that approach or
derive API contracts from storage entities.

Gateway patches and deletion now use the same domain service through REST and
gRPC. Each access check, mutation, and event write shares one serializable
transaction. Tests prove rollback on event failure and rejection of a stale
patch after a concurrent sandbox-count change. Owner, viewer, revoked-grant,
admin, protected-field, and control-plane checks pass. The generated runtime
delivers update and delete events in order after restart. Both transports then
exclude the deleted resource.

This workflow required two common changes: typed HTTP responses without content,
and a public error for serialization conflicts and deadlocks. The storage
callback is not replayed. Independent STEGO tests cover both changes. Hypershell
maps transaction conflicts to HTTP 409 and gRPC Aborted. The domain retains its
own patch fields, placement rules, and access policy.

The control-plane identity assumption is an explicit list of token subjects
under the configured verified issuer. No subjects are permitted by default.
The user has been asked to choose this policy or a dedicated token role.
Service-account cleanup must be connected before deletion can cover Gateways
with external service accounts. Watch streams, sandbox-count operations, and
the remaining application workflows are still open. The local gRPC patch
benchmark averaged 3.297 ms for 100 sequential requests to one Gateway. It
includes the transaction and outbox insert, but does not establish capacity.

The Gateway sandbox-count RPCs now run through the generated application.
The count and event commit together. Tests cover control-plane-only access,
zero flooring, unchanged values, overflow rejection, absent namespaces,
rollback, REST and gRPC reads, and recovery after restart. A concurrent test
retains all 32 increments while a regular Gateway patch competes for the row.
The local variant race suite passed with PostgreSQL required.

This workflow added the common `ResourceLocker` contract. It locks one existing
resource in a bounded read-committed transaction. Ordinary domain transactions
remain serializable. Independent Record tests prove complete concurrent updates,
events, rollback, lookup validation, and lock deadlines. The application build
also exposed a missing capability in the generated factory interfaces. HTTP
and gRPC now share the public `Repository` interface. A standalone generation
path needed a separate missing-resource error reference; its generated package
now has a compile check. The full STEGO race suite passed.

A local count benchmark averaged 2.035 ms per sequential gRPC request. Concurrent
requests to one Gateway measured 2.226 ms per operation as throughput. This
includes row locking and event storage, but is not a production capacity claim.
Watch streams, service-account cleanup, the control-plane reconciliation port,
and the other enterprise and application requirements remain open. Relative
count requests still have no deduplication key in the reference contract.

Gateway watch now completes another application path. Tests subscribe before
listing state, create through REST, update through both transports, adjust the
sandbox count, and delete through both transports. Two owner streams receive
the same events. Viewer, admin, control-plane, revoked-grant, hidden-resource,
and forged-delete-notice checks pass. Independent database writes reach the
runtime. Failed outbox writes produce no committed change or event.

The common changes are the public event-source contract, transactional live
notices, bounded subscriptions, separate gRPC stream capacity, verified token
expiry, I/O deadlines, and deleted-root recovery with live related grants.
STEGO contains no Gateway event names or access rules. Separate Record and
Membership tests check the storage boundary. Common source tests use independent
PostgreSQL listener sessions and check rollback, order, overflow, and source loss.
Transport tests check subject and process capacity, unary availability, token
expiry, slow clients, and shutdown.

The generated Hypershell supervisor stops both listeners after its PostgreSQL
source session fails. A new process, subscription, and list recover the changed
state. Normal restart, offline changes, later events, and durable Kafka delivery
also pass. The full local race suite passed with PostgreSQL required. The variant
regenerated from the pinned compiler with no changes or drift. STEGO CI passed
for compiler commit `451f6dac1d1301a8f2feb7d03ab5671cb3bffc3e`.

A local benchmark measured 4.773 ms from gRPC update start through watch-event
receipt across 100 sequential updates to one Gateway. It used Go 1.26.8,
PostgreSQL 18.6, TLS, and a separate generated process. It is not a production
capacity or latency-percentile claim. The default watch lifetime is five minutes,
bounded by token expiry, with a configurable maximum of 30 minutes. This remains
the stated design assumption pending the user's stream-lifetime preference.

The live source requires a dedicated PostgreSQL session. Transaction pooling
is not supported. Watch clients must subscribe, list, and apply events again
after failure; the pinned protocol has no history cursor. Kafka remains the
durable delivery path. Full control-plane reconciliation, service accounts,
other resource workflows, production migrations, key rotation, telemetry,
client ports, and deployment checks remain open. The broad goal is active.

Service-account create, list, get, revoke, and delete now run through the generated
Hypershell REST process and a TLS provisioner boundary. Non-secret reservations,
ready state, pending terminal actions, and audit records use generated storage.
The secret stays in memory and appears only in the creation response. Access is
checked again after provisioning. Failed or canceled creation attempts cleanup
by stable resource IDs. Abandoned reservations remain recoverable after restart.

The workflow added three common capabilities to STEGO: a bounded unary RPC
client, supervised application tasks with cleanup, and typed HTTP responses
with dynamic successful status codes. Independent sample services check these
contracts. Tests cover TLS identity, token-file replacement and permissions,
message and concurrency limits, deadlines, no replay of an executed failed call,
application shutdown, and valid 202 and empty 204 responses. The full STEGO race
suite and CI passed for commit `77290f7697c75f73b200253700aea754437c3c34`.

The variant's full local race suite passed with PostgreSQL required. Additional
checks passed for automatic grant revocation, expiration, unsafe connection
metadata, and the 100-account Gateway quota. The generated-process test checks
the pinned response schemas and protobuf descriptors. It stops after a pending
revocation, then verifies that the generated recovery task disables the account
after restart. Creator-role downgrades persist a degraded state on failure and
recover without raising privilege. Restored grants cannot reactivate accounts.

A Gateway row lock now serializes deletion with account reservations. Gateway
deletion refuses live service-account metadata. Automatic provider cleanup within
that deletion operation remains open. The variant regenerated from its pinned
compiler with no changes or drift. A local 100-cycle benchmark averaged
20.503 ms for create, get, revoke, and delete through the generated process and
TLS provisioner fixture. Process startup was excluded; the first cycle included
connection setup. This is not a production capacity claim.

The actual Keycloak adapter, token-issuance verification, full provider drift
checks, orphan discovery, recovery indexes and telemetry, production revocation
capacity, complete service-account list filters, configurable expiration policy,
Gateway cleanup composition, and client ports remain required work. The proposed
internal transport policy is TLS plus a verified service token and an explicit
caller-subject allowlist. The user has been asked to confirm it or select mutual
TLS. No plaintext provisioner channel is generated. The broad goal remains active.

The service-account workflow now has a real Keycloak adapter and a separate
provisioner process. The application uses STEGO's generated TLS RPC runtime,
bounded HTTPS client, and verification of JWT signatures against trusted key
sets. Hypershell retains ownership metadata, client settings, role mappings,
audience mappings, and lifecycle policy. Independent STEGO tests cover HTTPS
origin restrictions, redirects, TLS identity, size limits, concurrency limits,
deadlines, credential files, key selection, signature failures, and key changes.
The full STEGO race suite and CI passed for compiler commit
`b5263cd17553f497a0f0a3c5efbc08019d1936d1`.

The first real-provider test passed with Keycloak 26.7.3, Go 1.26.8, and
PostgreSQL 18.6. It creates an account through the generated API process and a
separate provisioner process. It obtains and verifies an actual access token,
rejects another Gateway audience and an unlisted caller, repairs injected client
drift, and lowers future token roles after a creator downgrade. It then commits
a revocation while the provider is stopped. Restart recovery stops new token
issuance, and deletion removes the client. The variant records the setup and
limits in `acceptance/keycloak.md`.

This result does not close external-operation ordering, production capacity,
automatic provider drift scans, orphan discovery, Gateway cleanup composition,
or deployment requirements. Already issued tokens can remain valid until their
five-minute expiry. Internal caller signing-key rotation still requires restart.
The common key-set verifier validates supplied trusted keys; it does not add
implicit network discovery or a key cache to the API verifier. The next service-
account correctness work must test stale external operations after database or
network connection loss before making stronger revocation claims.

The full local variant race suite passed with PostgreSQL and Keycloak required.
The acceptance package completed in 96.062 seconds. Variant commit `8dd319a`
regenerated from the pinned compiler without output changes or drift. CI now
requires the real-provider test through the same gate command.

The next real-provider failure test reproduced a revocation defect. A TLS proxy
held an accepted Keycloak enable request. The test terminated the PostgreSQL
connection that held the Gateway lock, then revoked through the generated REST
process. After revocation returned success, the test stopped that process and
released the old request. Keycloak returned HTTP 204 and issued a new token for
the revoked account. A database transaction cannot undo accepted external work.

Terminal revocation now uses the existing Delete RPC and removes the Keycloak
identity. The variant retains account metadata, terminal status, and audit
history until a separate visible-resource deletion. The same delayed update now
returns HTTP 404, and token issuance remains denied. This is a domain lifecycle
correction; no compiler change or new Hypershell-specific STEGO abstraction was
required. The domain provider contract now calls the operation `Revoke`.

The user was asked whether to remove the Keycloak identity or retain a disabled
client. Removal is the current design assumption, selected to satisfy permanent
revocation. It differs from the reference's provider retention behavior. Existing
access tokens can still remain valid until their five-minute expiry. Late client
creation after cleanup, orphan discovery, other external failure orderings,
production capacity, and the remaining enterprise and application scope stay open.

The full local variant race suite passed with PostgreSQL and Keycloak required;
the acceptance package completed in 126.135 seconds. The extended failure test
also passed after owner-grant restoration, restart, repeated revocation, and
visible-record deletion with audit retention. Pinned regeneration had no output
changes or drift. CI requires this test with the other application checks.

The late-creation test reproduced another application defect. The API timed out
while a TLS proxy held the Keycloak client-creation request. Initial cleanup
removed the failed account reservation, and the Gateway was then deleted. The
proxy released the request after the API process stopped. Keycloak created the
client, which survived restart because recovery excluded deleted account rows.

Recovery now revisits deleted records for failed, deleting, and abandoned
accounts. It uses stable resource IDs and does not require a live parent or an
old provider UUID. The real Keycloak test passed after this correction. Normal
API reads still exclude deleted records, and audit history remains available.
STEGO's existing `IncludeDeleted` storage option supplies the required query.
Its independent Record and Membership test also preserves live-grant filtering.
No compiler code change or application-specific generator was required.

A separate application test covers cleanup across 101 deleted records, retry of
a provider failure, and preservation of a live account. Recovery keeps its page,
concurrency, and deadline limits. Large-history capacity, bounded tombstone
retention, external objects with no retained database record, provider drift
scans, Gateway cleanup composition, and the broader goal remain open.

The expanded cleanup test exposed a priority regression: historical records
could consume the page before a current failed account was reached. Current
work and historical checks now have separate cursors. Current work runs first,
and both passes share the four-second scan deadline and eight-worker limit.
Each pass reads at most 100 rows. The expanded test passed, including retry
across pages, current-work priority, and preservation of a live account.

A follow-up access test is also required. Gateway OIDC fields can currently be
supplied through application mutations, and the service-account provider checks
the requested audience without checking a trusted binding between that Keycloak
client and the Gateway ID. Test whether an owner can request another Gateway's
audience before relying on cross-Gateway isolation. This is a review finding,
not yet a reproduced test result.

The final local variant race suite passed with PostgreSQL and Keycloak required.
The acceptance package completed in 172.666 seconds. Pinned regeneration had no
output changes or drift. The hosted gate requires the late-creation and cleanup
priority tests with the other application workflows.

The audience follow-up reproduced an access defect with real Keycloak. A second
Gateway owner could not read the first Gateway through REST, but obtained a
verified admin token for its audience. The service-account adapter now checks
trusted Keycloak attributes that bind the audience client to the immutable
Gateway ID. Missing or incorrect bindings fail before role lookup or client
creation. Both creation and repair use the same check. Hypershell owns this
identity rule; no compiler change was required.

The same application test reproduced a second defect. Invalid Gateway OIDC
settings prevented role reduction after an owner became a viewer. The old admin
credential stayed active. Failed reduction now queues terminal revocation and
its audit before provider cleanup. A separate database test proves that failed
cleanup retains this intent across a new service instance and restored owner
access. The real-provider test covers invalid OIDC settings, loss of the binding,
process restart, and restored configuration.

The user was asked about the trusted binding and terminal action after failed
role reduction. Both are current design assumptions. A temporary provider error
can require a new credential. Database or context failure can delay the state
commit; provider outages can delay cleanup. Existing tokens retain their expiry.
Production recovery latency remains unverified.

The Keycloak test fixture sets the binding through its administrator. The actual
control-plane port must create it from trusted Gateway identity data. Existing
clients need a trusted migration. The service-account provider must not adopt a
client from caller-supplied OIDC fields. This is now an explicit requirement for
the next control-plane workflow, with the broader enterprise goal unchanged.

A further failure test exposed a role increase after interrupted completion.
The provider accepted the lower role, but the completion audit failed. Restored
owner access then let recovery raise the credential back to admin. Recovery now
stores the lower role with pending state before the provider call. It also
handles pending records from the earlier implementation. The test checks both
forms of stored state after completion failure and a new service instance.

The expanded local race suite passed with PostgreSQL and Keycloak required;
the acceptance package completed in 210.318 seconds. The later role-completion
correction passed focused race tests in 11.978 seconds. Pinned regeneration had
no output changes or drift, and dependency verification passed. CI requires the
full test set on the final commit. These results do not establish production
capacity or complete the broader goal.

The hosted variant gate found a GORM schema data race. The first Gateway request
and service-account recovery parsed related model metadata concurrently. The
race detector stopped the generated API process. This was a common runtime
defect, not a failure of the new audience rule.

The PostgreSQL adapter now prepares all generated model metadata before it
returns a store. Preparation is serialized between constructors. It performs no
database reads or writes, so external migrations remain separate. `NewStore`
now returns `(*Store, error)`, and generated startup handles the error before
listeners or tasks start. The component version changes from 2 to 3 for this
constructor API change.

An independent Record/Membership test failed before this correction. It uses a
cold GORM cache, concurrent queries, and an observed naming strategy to detect
metadata work after construction. It requires no database connection and runs
under the race detector. The Hypershell failure remains part of the application
gate. The variant must regenerate and handle constructor errors in its fixtures.

The next compiler CI run passed the schema checks but failed the HTTPS deadline
test. Response completion could return success when cancellation occurred at
the end of the body read. A deterministic test reproduced this by canceling
the request as the response reader returned its final bytes. The client now
checks cancellation and the deadline before it returns a response. A failed
completion returns no response data and still releases the body and request
capacity. The real TLS deadline test remains in the gate.

The next application workflow now creates trusted Gateway identities through a
separate controller. Its first test exposed a missing common runtime part: the
generated gRPC client rejected server streams. STEGO now supplies bounded server
streams with TLS, token-file reads, message limits, a handshake deadline, a
maximum lifetime, and separate stream capacity. The independent Record service
checks these behaviors. The compiler race suite passed before publication.

Hypershell owns the controller, provider binding, and OIDC configuration. A new
control-plane RPC returns current Gateway state, including a retained deletion
row. Only configured control-plane subjects can call it. A normal not-found
response cannot authorize provider deletion. The provider requires the immutable
Gateway client name and trusted binding before it changes or deletes a client.
It configures a new client while disabled, checks the stored settings, and then
enables it. Real Keycloak readback exposed provider-added attributes; the
controller now declares those settings and retains the exact configuration check.

The application test creates one Gateway through REST before controller startup.
After the initial scan completes, it creates another Gateway through gRPC. The
live watch must deliver that change before the next scan. The controller creates
the provider bindings and publishes OIDC settings through generated clients.
A service account then obtains a real token for one Gateway; verification rejects
the other Gateway audience. The test also covers API restart while the controller
stays active, rename, and offline deletion followed by a new controller process.
No fixture administrator creates the Gateway bindings in this workflow.

The client ID uses the immutable Gateway ID. Browser login requires PKCE S256;
device login is enabled. The password grant is disabled. This is the current
assumption after the user was asked about reference compatibility. Existing
reference clients require a separate migration. Browser and device login
completion, user role reconciliation, workload deployment, multiple-controller
coordination, and production scan capacity remain open. The test uses an explicit
workload-health fixture before service-account creation. The identity controller
does not claim that a Gateway workload is running.

The full local variant race suite passed with PostgreSQL and Keycloak required;
the acceptance package completed in 241.274 seconds. The final watch test passed
in 32.054 seconds. Pinned regeneration had no changes or drift, and dependency
verification passed. The implementation and its limits are in the
[Gateway identity workflow](https://github.com/jsell-rh/hypershell-stego/blob/e6961b6343f392ae68cb062a4ff0dd6fbb89fdb4/acceptance/gateway-identity.md).
This is application evidence for the active goal; it does not complete the
enterprise or full Hypershell scope.

Work toward Gateway user login exposed a stored-identity defect. The API used
username to find a user and then applied that user's grants. A new process test
proved that a different signed subject with the owner's username could read,
list, and change the Gateway. A username change also removed access from the
original subject. This defect required correction before user-role synchronization.

STEGO now exposes the verified issuer with the subject. The independent verifier
tests check that this issuer appears only after authentication succeeds.
Hypershell stores the issuer and subject as a composite identity key. Username,
email, and name remain profile fields. The API tests now check REST and gRPC,
service-account access, profile changes, restart, and a new configured issuer.
A second subject cannot reach the credential provider through a reused username.

The domain migration preserves old users and grants but does not infer their
identity. Legacy rows need a trusted mapping before access can be preserved.
Recovery revokes service-account clients whose creator has no bound identity.
The migration test recreates the old User schema, applies the migration twice,
opens new database connections, and checks refusal of adoption and provider
cleanup. No running reference database was changed.

The full local variant race suite passed with PostgreSQL and Keycloak required;
the acceptance package completed in 247.458 seconds. A 100-request benchmark with
10,000 other users measured 0.935 ms per Gateway read, including domain access and
local PostgreSQL calls. It used Go 1.26.8 and PostgreSQL 18.6 on an Intel Core
Ultra 9 185H. It excludes transport and concurrent load. Regeneration and module
verification passed. The variant records the evidence and migration limits in
`acceptance/user-identity.md`.

Compiler CI also exposed a stream test timing assumption. Server handler exit
did not prove that the client had observed connection closure. The test now reads
the stream's terminal result before it checks reconnection. Three local race
runs and the next hosted compiler check passed. Runtime limits did not change.

The user was asked whether API and Gateway users must share one Keycloak issuer
or use an explicit trusted identity mapping. That choice remains open. User-role
synchronization, completed browser or device login, workload deployment, and the
other enterprise requirements remain part of the active goal.

The Gateway grant workflow exposed another missing storage rule. After an owner
removed a viewer grant, a new grant failed because the deleted row still reserved
its unique key. The initial application test reproduced that conflict before
any compiler change.

STEGO commit `65ba629` adds explicit `unique_when_live` keys. Normal unique keys
keep their previous behavior. A live composite key requires the same ordered
member list on each field. Computed fields and upsert keys cannot use this rule.
Generated PostgreSQL indexes use the fixed `deleted_at IS NULL` predicate and
stable names that include the entity. Independent Lease, Reservation, and Alias
tests check new creation after deletion, retained history, concurrent duplicates,
NULL values, and normal unique keys. Full compiler tests passed after the registry
version expectation was updated; hosted CI also passed.

Hypershell now provides REST creation, reads, and removal for Gateway grants.
The domain code locks the Gateway while it checks ownership and changes grants.
It refuses to remove the last owner. Grant changes and their grant and Gateway
events commit together. Event failures roll back the operation. A deleted grant
retains its history, and a new grant receives a new ID.

The application test checks Gateway access through REST and gRPC, filtered lists,
denied changes, duplicate grants, last-owner refusal, watch access after removal,
and event delivery after restart. Separate tests cover concurrent owner removal,
invalid targets, unbound user profiles, and transaction rollback. The index
migration test reproduces the previous conflict, applies the migration twice,
and verifies retained history and live uniqueness. No reference database changed.

The full local variant race suite passed with PostgreSQL and Keycloak required;
the acceptance package took 252.023 seconds. A 100-cycle benchmark with 10,000
unrelated grants measured 5.861 ms per owner-grant creation and removal cycle,
252,203 bytes, and 3,455 allocations. This includes domain checks, PostgreSQL
transactions, and event writes. It excludes transport, delivery, and concurrent
load. The environment used Go 1.26.8, PostgreSQL 18.6, and an Intel Core Ultra 9
185H. The variant records the evidence in `acceptance/gateway-grants.md`.

The active goal remains open. RoleBinding list and watch APIs, complete Users and
Roles APIs, global role synchronization, browser or device login, deployment,
production capacity, and the other enterprise requirements remain outstanding.

The next application check completes a real Keycloak browser login and PKCE S256
code exchange. Variant commits `09b9926` and `63b75b5` add a private generated API
for current user access and connect it to the Gateway identity controller.
Gateway owner and viewer grants now determine the roles in newly issued tokens.
The controller uses the stored issuer and subject, not a profile name. It changes
only the roles for the managed Gateway client and preserves other client roles.

The login check exposed a missing subject mapper in the managed Gateway client.
The existing service-account checks did not prove browser token behavior. STEGO
correctly rejected the browser token. The variant now declares a dedicated
Keycloak subject mapper; signature, issuer, audience, expiry, and subject checks
remain unchanged. This increment required no compiler code change.

The real application test covers API login, Gateway creation, owner and viewer
roles, role union, profile change and reuse, API audience isolation, removal, and
restart of the API and controller. A user with a reused profile name receives no
Gateway access. Removing an owner grant preserves a remaining viewer grant.
Retained grant references let a restarted controller remove missed provider roles.

The private API rejects untrusted readers and preserves errors for missing or
unbound identities. Provider tests check the issuer, trusted Gateway binding,
subject lookup, target client, and failed removal before addition. Controller
fault tests also exposed a retry-position error for the last user in a page.
The controller now retains that user's position when the pass times out.

The full local race suite passed with PostgreSQL and Keycloak required. The
acceptance package took 280.953 seconds. The later timeout correction passed
controller and provider race checks. A final focused race run repeated real
login and current-state checks in 29.032 seconds. Dependency verification and
regeneration passed. The variant records details in
`acceptance/gateway-user-login.md`.

A 100-call local benchmark read current user access with 10,000 unrelated users
and grants. It averaged 0.928 ms, 46,462 bytes, and 612 allocations per call on
Go 1.26.8, PostgreSQL 18.6, and an Intel Core Ultra 9 185H. This measures the domain
transaction and database reads. It excludes gRPC, Keycloak, controller scans,
browser login, and concurrent load.

The test confirms that an already issued token retains its earlier role claims.
The user was asked whether grant removal requires an online Gateway access check.
That decision and enforcement remain open. Provider writes are separate from the
database transaction and can lag it. Immediate revocation, multiple-controller
coordination, device login, workload deployment, full user and role APIs, API key
rotation, production capacity, and the other enterprise requirements remain part
of the active goal.
