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

The [component preflight review](compiler-preflight-gap.md) records shared input
checks before rendering. The [Go package-name contract](go-package-names.md)
resolves the later namespace mismatch. Keep C1 active until the full command
consistency audit passes.

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

The grant discovery workflow now uses REST lists and the reference gRPC
RoleBinding list and watch service. Variant commits `e4d7227` and `277b590`
contain the generated contracts and the application implementation. The compiler
pin is `0839f9da65cbfff1a0aa328da663c145fc1682a5`.

The application requires a union of two access paths: the caller's own grants
and grants on Gateways that the caller owns. Both require a live Gateway. The
previous storage contract could not express that union before count and paging.
STEGO now supplies bounded `RowFilter` trees and related filters through declared
keys. Parent references and references to the same parent are supported. All
identifiers come from the schema; values remain SQL parameters. Empty or
ambiguous conditions, invalid joins, and excessive trees fail. Record and
Membership tests prove these rules without Hypershell entities. Full compiler
checks and hosted CI passed.

The first application watch check exposed a second gap. Gateway creation
committed its owner grant but emitted only the Gateway event. It now commits
both creation events with the Gateway and owner grant. Failure of either event
write rolls back all of them. The generated runtime delivers the owner-grant
event through the outbox and the grant watch stream.

Grant lists apply current access before search, count, and paging. Role and
profile details are read in batches in the same transaction. Missing roles fail
the complete response. The watch subscribes before its active replay and checks
current access before each send. It checks stored deletion state before sending
a delete event. Restart, false notices, failed writes, removed grants, filtered
pages, and denied reads are covered through the generated runtime.

The unpaged gRPC list and initial replay have a 10,000-grant limit. Tests prove
that 10,000 grants return in full and 10,001 fail without partial output. A
separate gRPC test rejects a response above 3 MiB and then returns a smaller
filtered response. REST paging can read the tail of a larger inventory. These
are resource bounds, not production capacity claims. Replay restores active
state; it does not recover missed deletions. The identity controller still uses
retained grant references for deletion recovery.

The full local variant race suite passed with PostgreSQL and Keycloak required.
The acceptance package took 286.550 seconds. Final descriptor, schema, and row
boundary checks passed in 13.703 seconds. A separate byte-limit check passed in
5.655 seconds. Dependency verification and pinned regeneration passed. The
variant records the evidence in `acceptance/grant-discovery.md`.

A separate 100-call benchmark read two visible grants among 10,000 unrelated
grants. It averaged 4.271 ms, 92,691 bytes, and 1,129 allocations per call. It used
Go 1.26.8, PostgreSQL 18.6, and an Intel Core Ultra 9 185H. It includes access,
filtered count, database reads, and role and user details. It excludes transport,
concurrent load, and watch replay.

The active goal remains open. Full Users and Roles APIs, global grants, sparse
fields, larger REST pages, device login, workload deployment, immediate token
revocation, distributed coordination, key rotation, production capacity, and the
remaining enterprise requirements still need work. The current grant inventory
uses the existing grantee, Gateway-owner, and configured control-plane rules.
A platform-admin token alone does not grant access to it.

Hosted variant CI exposed a timeout in the new 10,000-grant boundary check.
Variant commit `71e2950` removes repeated root scans from that snapshot. It now
counts and reads the bounded grant set once, caches role and user details within
the transaction, and selects only the required display fields. The transaction
deadline is unchanged.

Database inspection also found stale statistics after the fixture's bulk load.
PostgreSQL estimated three live users instead of 10,001 and chose the deletion
index for repeated detail reads. The bulk fixture now runs `ANALYZE` before
response-limit checks. A diagnostic run measured user-detail reads at 3.94
seconds before the statistics update and 0.17 seconds after it. Application
requests do not run `ANALYZE`.

With unchanged stale statistics, one local snapshot sample fell from 6.48 seconds
to 4.08 seconds after the code change. With current statistics, ten final
snapshots of 10,000 grants averaged 316.48 ms, 35,755,411 bytes, and 377,492
allocations per call. This used `GOMAXPROCS=2` and the race detector on the same
local environment. It excludes transport and concurrent load. The final small
page averaged 4.095 ms, 91,536 bytes, and 1,091 allocations across 100 calls
without the race detector. These are test measurements, not capacity guarantees.

The final focused race checks passed with two Go processors in 10.301 seconds.
They retain the 10,000-row success boundary, 10,001-row refusal, gRPC byte limit,
current access, contract shapes, outbox delivery, and restart checks. The failed
hosted run was not restarted. The fix starts a new full CI run on its own commit.
The broader goal and its remaining scope are unchanged.

The grant-snapshot correction passed hosted CI on variant commit `71e2950`
(run `34306724409`). STEGO commit `f7da5b1` also passed hosted CI
(run `34306737057`). This closes the failed boundary check from the previous
increment.

Variant commit `29e83ea` adds authenticated role discovery to the Gateway grant
workflow. The real browser test now retrieves owner and viewer role IDs through
REST before it creates grants. Generated storage, search, authentication, and
transport support this change without a compiler extension. Built-in role names
and permission metadata remain in the Hypershell variant.

The catalog exposes the reference read routes and role fields. Tests check the
pinned response schema, search, paging, count-only requests, authentication,
invalid requests, denied mutations, and restart. A catalog reader cannot create
a Gateway. A Gateway owner cannot assign discovered platform roles through a
Gateway grant. Permission metadata does not replace domain access checks.

The explicit role migration preserves existing IDs and grants. It adds metadata
and seeds the four built-in roles. Repeated execution leaves current metadata
and its timestamp unchanged, but repairs changed built-in metadata. A deleted
built-in role stops the whole transaction. Custom roles remain unchanged. The
application does not apply this migration at startup.

The focused role and real-login race checks passed in 33.152 seconds. The full
local race suite passed with PostgreSQL and Keycloak required; the acceptance
package took 295.363 seconds. Dependency verification and committed regeneration
passed. Details are in the variant's `acceptance/role-catalog.md`.

Recipient user ID discovery remains incomplete. The browser test still reads
that ID from its private database. The reference has no public user directory.
The user was asked whether recipients must supply their ID or owners can search
registered users. No directory has been added while that policy is open. Sparse
fields, REST page sizes above 100, global grant projection, and the broader
enterprise and Hypershell requirements remain part of the active goal.

The role-catalog increment passed hosted checks on variant commit `29e83ea`
(run `34307480461`) and STEGO commit `2608199` (run `34307492187`).

The next application increment removes the remaining recipient-ID database
lookup from the real browser sharing test. An authenticated recipient calls
`GET /api/hypershell/v1/users/me` and supplies the returned stored ID to the owner.
The owner gets a role ID from the catalog and creates the grant through REST.
The test then verifies the resulting Gateway roles through real Keycloak login,
removal, and restart. The API extension has its own OpenAPI contract; it is not
represented as a route in the pinned reference.

The new route selects only the verified caller. It rejects target selectors and
request bodies. Existing issuer-and-subject rules preserve identity through
profile changes and prevent profile reuse from transferring grants. The generated
transaction creates or updates the user record and reads its stored timestamps
before commit. A failed write returns an error. Concurrent creation is protected
by the unique identity key; clients can retry conflicts. A deleted identity is
not restored by login. No compiler or schema change is required.

A public user directory remains a separate policy decision. The self-identity
route does not create one. Global role projections, full user administration,
instant token revocation, distributed coordination, workload deployment, and the
other enterprise requirements remain open. Source inspection confirms that the
reference specification requires global role records to follow verified claims,
including removal when the claims are empty. That behavior still needs a full
application workflow in the variant.

Variant commit `771ed28` contains this self-identity workflow. The focused race
checks passed in 34.467 seconds. The full local race suite passed with PostgreSQL
and Keycloak required; the acceptance package took 297.088 seconds. Real responses
pass the extension's schema. Tests cover token rejection, target selectors,
profile changes, reused names, failed writes, concurrent registration, deleted
identities, issuer isolation, and restart. Dependency verification and pinned
regeneration passed with no generated changes or drift.

A 100-call lookup benchmark with 10,000 unrelated users averaged 0.356 ms,
26,644 bytes, and 354 allocations per call. It used Go 1.26.8, PostgreSQL 18.6,
and an Intel Core Ultra 9 185H. It includes the domain transaction and database
reads. It excludes HTTP, token verification, initial registration, profile writes,
and concurrent load. The variant records these limits in
`acceptance/current-user.md`.

The self-identity increment passed hosted checks on variant commit `771ed28`
(run `34308570500`) and STEGO commit `c935686` (run `34308578790`).

The global-role workflow exposed a transport boundary requirement. Role claims
must be projected before an application operation, but not once per event on an
existing stream. STEGO commit `42fe61a` adds an optional HTTP preparation callback
and a gRPC registrar wrapper. They run with verified identity and a bounded
context. A preparation error stops the operation and retains an application
error status. Shared protobuf descriptors remain unchanged. The compiler race
suite and hosted checks passed (run `34308915530`). A later independent-registration
check also passed with the generated Record service in 26.493 seconds.

The variant now projects the two managed global roles before its REST and gRPC
operations. An empty role set removes both records. Preparation commits its own
transaction before the domain operation; a denied request therefore cannot undo
a role removal. The transaction includes user profile changes, global records,
and their outbox events. Gateway creation retains its separate atomic Gateway,
owner-grant, and event boundary. Authorization still uses current verified claims
and stored Gateway grants; global records are not a replacement for the token.

Global RoleBindings have no Gateway reference. The migration preserves Gateway
grants, enforces scope/reference consistency, and adds a separate live global
key. Lists apply access before count and paging. Callers can read their own
global records, while existing Gateway inventory rules remain in effect. A
Gateway-owner role, platform-admin token, or controller status does not itself
expose another user's global records. Public grant mutation routes cannot assign
or remove the managed global roles.

The generated runtime delivers global creation and deletion events and replays
active global records with Gateway grants. Existing streams do not project their
old claims again when they send an event. The workflow also proves removal before
a denied request, retained ownership and sharing rights, re-grant with a new ID,
failed writes, REST and gRPC errors, and restart. The real browser test adds
Keycloak role removal and re-grant with fresh API tokens.

A new request with an older valid token can still project its old claims. This
is not immediate revocation or monotonic token tracking. That policy remains
open. Full user administration, distributed coordination, workload deployment,
sparse fields, larger REST pages, production capacity, and the other enterprise
requirements remain part of the active goal.

Variant commit `78d08f7` contains the global-role workflow and pins compiler
`42fe61abc0ada2232f38bc8b4811d9eeb314fd54`. The focused global-role, grant-discovery,
and role-catalog race checks passed in 18.747 seconds. Migration and concurrent
projection checks passed in 1.703 seconds. The sandbox-count check now prepares
its caller roles before it measures count events. Role preparation can commit
its own event even when a later count operation is denied. The corrected count
workflow and migration/concurrency checks passed in 6.950 seconds.

The full local race suite passed with PostgreSQL and Keycloak required. The
acceptance package took 306.128 seconds. A final reference check corrected valid
public global-role assignment requests to return HTTP 403. The correction passed
focused race checks in 11.156 seconds. These repeated global-role changes,
migration, concurrency, invalid targets, and Gateway sharing through both
transports and restart. Dependency verification and committed regeneration
passed with no generated changes or drift.

A 100-call benchmark prepared unchanged claims with 10,000 unrelated users and
global grants. It averaged 0.922 ms, 60,914 bytes, and 759 allocations per call on
Go 1.26.8, PostgreSQL 18.6, and an Intel Core Ultra 9 185H. It includes the
transaction, identity and role lookups, and comparison of current records. It
excludes transport, token verification, Keycloak, initial registration, role
changes, and concurrent load. This is not a production capacity claim. The
variant records the workflow and its limits in `acceptance/global-roles.md`.

### Placement records through the generated application

The global-role workflow on variant commit `78d08f7` passed hosted run
`34309947994`. STEGO journal commit `aa29d12` passed hosted run `34309968117`.

The next workflow removes direct placement inserts from the application test.
The variant creates clusters, releases, and managed databases through REST and
gRPC. It then creates a Gateway from the returned cluster and release IDs. The
server selects the sole CNPG database. Retrieval, owner grants, events, watches,
and restart use the generated process.

The first test found that the database namespace needs 29 characters. The schema
was corrected. The workflow also exposed a common numeric constraint gap:
PostgreSQL models ignored declared `min` and `max`. STEGO commit `eae36e0` adds
inclusive database checks, preserves optional NULL values, rejects non-finite
bounds, and rejects non-finite values in bounded floating fields. Its independent
Measurement test covers integer, fractional, optional, and direct SQL writes.
The full local race suite passed every package except a registry expectation for
the previous component version. That expectation was corrected. The registry
race check then passed in 4.568 seconds. PostgreSQL was required for these checks.
The variant pins the full compiler revision
`eae36e055bfcf26e564f40930ca52c7b8c0aeb06` and postgres-adapter 3.3.0.

Catalog writes use platform admins and configured controller subjects. Gateway
creators can read and select records. This restricts the reference fallback,
which lets Gateway creators change placement records. The user was asked to
review the rule. The more restrictive rule applies unless that decision changes.
Each catalog mutation and its event use one serializable storage transaction.
Deletion refuses live Gateway references. The concurrent test forces a Gateway
creation between the deletion reference check and commit. PostgreSQL rejects
the deletion for all three resource types.

The focused workflow, migration, concurrency, and role discovery race checks
passed in 10.485 seconds. The new migration requires explicit placement data for
old name-only records. It does not invent providers, images, or secret references.
It preserves IDs and timestamps and can run again. An offline change test requires
the exact retained event ID to reach Kafka after process restart.

A 100-call local catalog page benchmark over 10,001 matching cluster records
averaged 4.212 ms, 222,506 bytes, and 2,865 allocations. It includes domain access,
transaction, count, and page queries. It excludes transport, token verification,
role preparation, and concurrent load. It is not a production capacity result.

Per-Gateway deployment database placement and workload deployment remain open.
Catalog metadata does not prove cluster access, secret resolution, image integrity,
rollout behavior, or database provisioning. These need the next control-plane
workflow. The enterprise goal remains active.

Hosted STEGO run `34310608422` passed on compiler commit `eae36e0`.
The reference `DATABASE_PROVIDER` setting defaults to `deployment`. Its adapter
creates a database named `gw-<gateway-name>-db`. The variant does not yet implement
that default. The next placement workflow must create the dedicated database,
Gateway, owner grant, and their events in one transaction. This work does not
require broad catalog write access for a Gateway creator.

Variant commit `f4b6842` contains the catalog workflow. Its full local race suite
passed with PostgreSQL and Keycloak required; the acceptance package took
312.660 seconds. Dependency verification and regeneration from the committed
files passed. No generated changes or drift remained. The workflow and migration
steps are recorded in the variant's `acceptance/placement-catalog.md`.

### Default deployment database placement

The prior STEGO journal passed hosted run `34311049664`. The next increment adds
the reference deployment placement path. An unset or empty `DATABASE_PROVIDER`
selects `deployment`; `cnpg` selects the explicit shared-database path. Invalid
values stop generated application startup. A Gateway creator can create a Gateway
without catalog write access. One transaction creates a private ManagedDatabase,
the Gateway, its owner grant, and three resource events. A caller cannot select
another Gateway's database. Existing shared-database tests now select CNPG.

The workflow exposed a REST compatibility error. The reference requires a
`database_id` property but permits an empty string. The variant previously
rejected empty strings. STEGO commit `35349fe` adds `stego:"required"` to the common
JSON reader. It distinguishes absent or null members from valid zero values during
the existing shape check. It does not reread the body. Independent Record tests
cover empty strings, zero, false, empty arrays, nested objects, untagged fields,
and invalid declarations. The full local STEGO race suite passed with PostgreSQL
required. Hosted compiler run `34311390396` also passed. The variant pins full
revision `35349fea6a2b112ac53a59f7cf649550b12369a3` and http-application 1.2.0.

Database names now permit the 255-byte Gateway name plus `gw-` and `-db`.
Migration 000007 widens the name column and preserves existing data and timestamps.
The workflow exercises a 255-byte Gateway name. Namespace allocation remains
based on the database KSUID and is shared with catalog creation.

The fault tests reject database insertion, Gateway insertion, owner insertion,
and each event kind. Each failure must leave no partial creation. A concurrent
test creates eight Gateways for one user and requires eight distinct deployment
databases. The generated process test covers both transports, filtered access,
owner grants, watches, and all three Kafka events. Its offline creation check
requires the exact retained message IDs after restart. The real Keycloak browser
login and sharing test also runs with default deployment placement.

The first focused run found an incomplete grant-list request in the new test.
The request now includes its required user ID. The corrected deployment workflow
passed in 5.125 seconds. The fault, concurrency, migration, startup, catalog, and
real Keycloak checks passed in the earlier focused run; that run took 39.556 seconds
and failed only the incomplete grant-list request. Dependency verification and
pinned generation passed before the full variant race suite.

A local 100-call creation benchmark averaged 3.088 ms, 116,992 bytes, and 1,651
allocations per call. It includes the database, Gateway, owner grant, and three
queued events in one transaction for an existing caller. It excludes transport,
token checks, role preparation, Kafka delivery, Kubernetes work, and concurrent
load. It is not a production capacity claim.

This increment creates placement records. It does not deploy database pods or
Gateway workloads. The next control-plane workflow also needs the reference
ManagedDatabase deletion replay, capability handshake, and durable cleanup retries.
The enterprise goal remains active.

Hosted variant run `34311045030` passed on the prior catalog commit `f4b6842`.
The reference ManagedDatabase watch uses the capability header
`hypershell-managed-database-delete-tombstones: v1`. A separate watch request with
`hypershell-managed-database-replay: deleted-v1` streams deletion history in pages
of 500. The reference controller starts and drains its live watch before replay.
The variant currently has live delete records but lacks this replay and handshake.
The next workflow must prove cleanup after disconnected deletion and restart,
with replay access limited to configured controller subjects.

Variant commit `4fabc6c` contains default deployment placement. Its full local race
suite passed with PostgreSQL and Keycloak required; the acceptance package took
318.581 seconds. Regeneration from the committed files passed with no changes or
drift. The variant records configuration, migration, evidence, and remaining work
in `acceptance/deployment-placement.md`.

Local Kubernetes test tools are available: kind 0.31.0 and kubectl 1.35.7. The
reference control-plane module pins Kubernetes libraries at 0.36.3. The next
application gate can use an isolated kind cluster to prove actual database
provisioning and cleanup after disconnected deletion and controller restart.

The next increment connects Gateway creation to an actual PostgreSQL workload.
STEGO compiler commit `8b3da389821bdcb2d10e48319f50daadc9c2484b` adds
`ListOptions.OnlyDeleted` and postgres-adapter 3.4.0. It selects deleted root rows
before counts and pages. Related access rows must remain live. The independent
Record test covers page boundaries, counts, live-row exclusion, combined filters,
and precedence over IncludeDeleted. The first local full run found a registry
version expectation that still named 3.3.0. The corrected registry test passed.
Hosted compiler run `34312268198` passed on the committed change.

The variant adds the reference database tombstone capability and dedicated
historical replay. It limits replay to configured controller subjects, pages by
canonical ID, and rejects invalid modes. A generated gRPC test crosses a replay
page, excludes live records, and denies ordinary users, creators, and admins.
The database controller starts and drains a live watch before replay and live
list scans. Failed cleanup remains in stored replay data. Generated clients
supply bounded TLS transport, credential file reads, request limits, and stream
limits. The Kubernetes objects and Hypershell ownership rules are domain code.

The controller provisions a namespace, Certificate, Secrets, ConfigMap, PVC,
Service, and Deployment in one configured Kubernetes cluster. Namespace and
resource ownership checks prevent adoption of foreign resources. Namespace
DELETE includes the observed UID and resource version. Missing namespaces are
already cleaned up; denied or conflicting requests must be retried. Credentials
are preserved. Missing credentials for an existing volume produce an error.
A stale create notice must fetch current API state before it can create anything.

The database image is PostgreSQL 18.6 Alpine, pinned by digest. The Pod runs as
UID/GID 70 under the restricted Pod security policy. It has a read-only root
filesystem, no added capabilities, no privilege escalation, no service-account
token, seccomp, and explicit CPU and memory limits. Database connections require
TLS 1.3, a verified certificate, and SCRAM credentials. The application account
owns its database but has no superuser, role creation, database creation,
replication, or row policy bypass privileges. Bootstrap credentials are separate
and must not be copied to Gateway workloads. Readiness performs a verified SQL
query; failed reconciliation clears a stored ready state through the API.

The TLS issuer choice was sent to the user for review. The current assumption
uses a configured cert-manager ClusterIssuer. The user has not confirmed that
choice. Certificate issuance is covered by the cluster test. Scheduled renewal,
CA rotation, network policy enforcement, backups, restore, OpenShift UID handling,
CNPG provisioning, multiple controller coordination, and production capacity
remain open. Large-history replay also needs a completion test against the
five-minute generated stream limit. These limits prevent a production-readiness
claim.

The application test creates a Gateway through REST and obtains database
readiness through generated gRPC. It writes data through the database Service
with TLS and hostname verification, rejects an unencrypted connection, checks
the limited application role, and preserves data and credentials after restart.
It rejects adoption and deletion of a foreign namespace. It then deletes the
Gateway and database while the controller is stopped, restarts the API, forces
a Kubernetes delete denial, restores access, and requires successful cleanup.
This is actual workload and recovery evidence, not only a contract check.

The local Kubernetes environment uses kind 0.33.0, Kubernetes 1.35.8, and
cert-manager 1.21.1. Tool downloads, the cert-manager manifest, and runtime images
are pinned. The isolated-cluster script creates and removes its own cluster.
The first complete workload test passed in 59.66 seconds. After separation of
the database accounts and the forced delete denial, it passed in 60.20 seconds.
Five stable reconciliations took 75.7 ms and left the Deployment unchanged. A
second run through the new cluster setup script passed the workload in 70.93
seconds and replay in 3.47 seconds. That run took 75.443 seconds for the acceptance
package. These measurements do not establish production latency or capacity.

The next application gate is an actual Gateway workload that uses this database
and the existing identity configuration. It must prove startup, authenticated
use, stored application data, and recovery. Let failures in that workflow select
further reusable compiler and runtime work. The enterprise goal remains active.

The focused workflow with verified SQL readiness passed in 60.02 seconds; the
acceptance package took 61.073 seconds. The controller failure tests cover namespace
identity conflicts, denied deletion, absent namespaces, foreign ownership,
missing volume credentials, token file rotation and permissions, unsupported
watch capabilities, stale create events, and failed status updates. The controller
race tests and `go vet` passed. The prior variant run `34312014752` on commit
`4fabc6c` also completed successfully.

The pre-publication dependency scan found reachable
[GO-2026-5970](https://pkg.go.dev/vuln/GO-2026-5970) in golang.org/x/text 0.36.0.
Invalid UTF-8 input could cause an infinite loop in normalization. Compiler commit
`ef842d5` adds a direct runtime minimum of 0.40.0 for the relevant dependencies.
The variant now selects that version. The review also raised golang.org/x/net to
0.56.0, golang.org/x/sys to 0.48.0, and filippo.io/edwards25519 to 1.1.1. Those
updates remove the additional package and module findings from the scan.
The updates also select golang.org/x/sync 0.22.0 through module resolution.

The compiler supplies these minimums when the corresponding runtime dependencies
are present. It preserves higher selected versions and does not add unrelated
modules to independent services. The tests cover those rules. Both CI workflows
now run govulncheck 1.4.0 with Go 1.26.8. The local scan of the updated variant
reports no known vulnerabilities at symbol, package, or module level. The
compiler scan also reports no known vulnerabilities. These results are bounded
by the vulnerability database and static analysis; they do not prove the absence
of unknown defects.

The variant full race suite before these dependency updates passed; its acceptance
package took 326.992 seconds. A new full race run and a new real-cluster workflow
run check the final dependency set before publication.

Compiler commit `e8374ca995c6ea3964b316250645f29bc57d7d32` supplies the remaining
transitive dependency minimums. The full local compiler race suite passed with
PostgreSQL required. The variant pins this compiler and regenerated with no
changes or drift. Its real-cluster workflow with the updated dependencies passed
in 60.65 seconds; the acceptance package took 61.696 seconds. The controller also
rejects unsupported engine, version, region, instance class, and connection Secret
settings before Kubernetes writes. Invalid mutable settings do not block cleanup.

Hosted compiler run `34313105365` passed on the final compiler pin `e8374ca`.
The complete Kubernetes workflow after the unsupported-setting checks passed in
60.18 seconds; the acceptance package took 61.217 seconds. Both the script-created
cluster and the separate development test cluster were removed after their checks.

The final full variant race suite passed with PostgreSQL and Keycloak required;
its acceptance package took 325.383 seconds. The separate real-cluster run and
focused controller tests cover the final provisioning checks. Module verification,
formatting, and shell syntax checks passed. This result closes the database
workload gate. The actual Gateway workload and the complete enterprise goal
remain open.

Variant commit `c3f4618` contains the dependency updates. Commit
`a622a177bd63b1014f68504ba422d5d84e70e336` contains the database workflow and its
required CI job. Both are on remote main. Regeneration from the committed files
passed with no changes or drift. Hosted variant run `34313469490` is queued;
its final result must be checked in the next goal turn. The test bed records
configuration, measured results, security rules, and limits in
`acceptance/database-workflow.md`.

The user reaffirmed the five-part Gateway acceptance gate. The current variant
already has generated-process tests for creation, atomic owner grants, access
filtering and denial, event delivery, and REST/gRPC restart behavior. Keep this
gate as the basis for application claims. Its broker is a mutual-TLS Kafka
protocol fixture. It does not prove a production Kafka deployment or an actual
OpenShell Gateway workload. Hosted variant run `34313469490` passed both the
acceptance and database workflow jobs. Compiler journal run `34313489049` passed.

Compiler commit `e6b4d6ceb198c89c9ad4eaedb486a1b2e81e7037` completes the pending
Kubernetes client extraction from the database workflow. It is optional and has
no Hypershell types. The generated HTTP transport supplies TLS, deadlines, body
limits, and connection limits. The resource client supplies ownership checks,
UID and resource-version preconditions, and completion checks for deletion.
Hypershell retains placement, resource definitions, and readiness rules.

The review found that float64 JSON decoding could make different int64 values
compare equal. The client now preserves exact JSON numbers. Its independent
Widget test checks integer precision, caller input preservation, creation,
unchanged resources, updates, deletion, denied requests, ownership failures,
conflicts, invalid responses, and token rotation. The complete compiler race
suite passed with PostgreSQL required. The compiler commit is on remote main.

Variant commit `cbfce8e025145900ea00bdf47ecaeef8f707116b` pins this compiler and
uses the generated client. The real Kubernetes database workflow passed in
69.77 seconds; deletion replay passed in 2.93 seconds. Five stable reconciliations
took 75.8 ms without a Deployment change. Both repository vulnerability scans
reported no known vulnerabilities. Hosted compiler run `34314041752` passed.

The complete local `scripts/check-gateway.sh` gate passed on this variant commit.
It required PostgreSQL and Keycloak and enabled race detection. Pinned generation
had no changes or drift; the acceptance package took 321.869 seconds. All other
packages passed or had no tests. This verifies the requested five-part Gateway
API gate on the current implementation. The compiler and variant code commits
are on remote main. The actual Gateway workload and the full enterprise goal
remain open. Hosted variant run `34314163495` passed both its full acceptance
job and its database workflow job. These results close the current gate review.
The acceptance report records the exact compiler and variant code revisions,
commands, results, and limits. Further infrastructure work must follow failures
in complete application workflows.


The actual OpenShell Gateway now runs with the STEGO Hypershell API, provisioned
TLS database, and Keycloak identity configuration. Variant commit `cd56815`
adds the Gateway workload controller and a required `gateway-workload` CI job.
The compiler pin remains `e6b4d6ceb198c89c9ad4eaedb486a1b2e81e7037`.
The test uses the Gateway image at digest
`sha256:a80b79e514826e8d57ea137749cf18a6e7f3d92e26bfefe005f3a9c4a55b8bdd`.
Its original gRPC contracts come from image source revision
`681c9b2d8b9887f230cee4871bdbdbc9a362dfc8` and have recorded file hashes.

An owner signs in through real Keycloak browser login with PKCE, creates a
Gateway through REST, and creates and retrieves a provider through the actual
Gateway gRPC service. Missing, forged, and wrong-audience tokens are rejected.
An ungranted user is denied. Provider data survives Gateway and database Pod
restart, controller restart, and complete Gateway namespace replacement.
Signing and encryption keys remain identical. Provider retrieval returns a
redacted record; this does not prove external provider credential use.

The application test found that database cleanup could lose its retry path
after the Gateway namespace was removed. A link on the database namespace now
keeps that work visible. The test forces the database deletion transaction to
fail, removes the failure, and requires automatic cleanup. It also changes the
cluster assignment before deletion while the controller is stopped. The former
cluster still removes resources that it owns. This does not prove migration.

The workload uses the generated Kubernetes client and API runtime. Placement,
resource definitions, readiness, and identity policy remain in Hypershell.
Keys have an immutable primary Secret in the database namespace and a recorded
fingerprint. The controller rejects missing, replaced, corrupt, and foreign
keys. The Gateway uses a limited database account with verified TLS. The public
trust ConfigMap contains only parsed certificates. Private-key blocks are
rejected. The Gateway Pod uses restricted security settings and resource limits.

The final local actual-image test passed in 163.20 seconds; its race-enabled
acceptance package took 164.243 seconds. The complete variant race suite passed
with PostgreSQL and Keycloak required; its acceptance package took 331.637
seconds. The database workflow and deletion replay regression package passed in
74.076 seconds. Focused race tests and vet cover the final ownership and trust
checks. The Go vulnerability scan found no known vulnerabilities. External
container images were not included in that scan. All temporary test clusters
were removed. Prior journal CI runs `34314772170` and `34314774032` passed.

The next recovery test must cover Gateway deletion before the workload controller
first observes it. Live watches and owned-resource scans cannot recover that ID
when neither a Gateway resource nor a database namespace link exists. Retained
Gateway IDs need a replay path. Large-list progress across watch reconnects also
needs evidence. Common watch and retry behavior remains a candidate for STEGO
extraction after these application tests define its requirements.

The pinned Gateway image requires workspace membership as well as the standard
user role. The user was asked whether a Hypershell viewer grant must also create
default-workspace membership. No answer has arrived. Owner access and ungranted
user denial are proved; full viewer workspace access remains open. Sandbox
execution is also open: the reference supervisor requests capabilities that the
restricted Gateway namespace rejects. Do not weaken that policy without a clear
isolation design. The provider-management gate does not prove sandbox execution.
Backup, restore, key rotation, certificate renewal, CNPG, public routes, network
policy enforcement, capacity, and complete CLI and console behavior remain open.
The complete enterprise goal remains active.

Variant commit `cd56815850f29498a7907924de259b87ce375ec0` is on remote main.
Regeneration from that commit passed with no changes or drift. Hosted CI for
this commit must pass all three jobs: acceptance, database workflow, and Gateway
workload. Check that result before the next implementation step.


Hosted variant run `34316927657` passed all three jobs on `cd56815`:
acceptance, database workflow, and the actual Gateway workload. Compiler journal
run `34316943845` also passed.

A new real-cluster test reproduced the next recovery defect. A Gateway was
deleted before its workload controller first started. Its database already
existed, but no Gateway namespace or database link existed. After API restart,
the controller completed repeated empty scans and left the database running.
The test failed in 85.61 seconds against the previous implementation.

The variant now uses a private controller-only RPC to scan retained Gateway IDs.
Each page has at most 100 IDs and uses the last ID as its cursor. STEGO already
supplies field projection, retained-row reads, validated search, transactions,
and gRPC generation. No compiler change is needed. Cleanup still requires a
fresh privileged state read. A missing or denied row cannot authorize deletion.
Scans can exceed the ten-second resync interval. Each request has a deadline,
and the next scan starts after the previous scan finishes.

The generated-runtime test passed with 205 live and deleted IDs across three
pages, a concurrent deletion, invalid cursors, and denied callers. The focused
race tests include a scan that exceeds the resync interval. Vet passed. The Go
vulnerability scan found no known vulnerabilities. The strongest real-cluster
recovery check drains the event queue before API restart. It passed in 45.50
seconds; its package took 46.553 seconds. Retained state supplied the deleted ID,
and both the database API row and its Kubernetes namespace were removed.

Very large retained histories and progress under sustained watch overflow still
need measurement. The generated list adapter counts matching rows even when a
recovery caller does not use the count. Further common infrastructure changes
must follow that application evidence. This recovery result does not close
viewer workspace access, sandbox execution, cluster migration, or the complete
enterprise goal.

The complete local race suite passed with PostgreSQL and Keycloak required;
its acceptance package took 334.339 seconds. The actual-image regression passed
in 167.88 seconds. The combined real-cluster recovery and workload package took
224.754 seconds. All temporary Kubernetes test clusters were removed.

Variant commit `e705e233e4c07a6c2a44254f4382d9a757d52fd9` is on remote main.
Regeneration from the committed files passed with no changes or drift. Hosted
variant run `34317681420` is queued. Check all three jobs before the next
implementation step. The full enterprise goal remains active.


The next application check covers viewer access through the actual Gateway.
The reference resolves the earlier workspace question: its
`tests/e2e/e2e-openshell.sh` grants workspace membership separately from the
Hypershell viewer role. The variant preserves this rule. Automatic membership
would be a product change, not a requirement for reference compatibility.

The test grants viewer access through Hypershell REST using the recipient's
`/users/me` ID. A real browser login supplies a token to the actual Gateway.
The Gateway must retain the provider subject and standard user role. The role
alone must not grant workspace access. The owner then grants default-workspace
membership through Gateway gRPC. The viewer can read the provider, sees only
its permitted workspace, and cannot change providers, create workspaces, grant
administrator membership, or read Gateway administrator information. The test
repeats access after namespace and database restart. Its initial run passed in
165.77 seconds; the recovery and workload package took 222.344 seconds.

The test also preserves the two access-removal paths. Hypershell grant removal
denies API access immediately and removes the Gateway role from new tokens after
reconciliation. An already issued role-bearing token can still work until
expiry when membership remains. Workspace removal denies that same viewer token
immediately. This is viewer behavior; it does not prove immediate revocation
of an administrator token. Global token revocation remains open.

The user was asked whether production sandboxes may require a runtime with a
separate virtual machine, such as Kata Containers, or must support standard
container runtimes on dedicated nodes. No answer has arrived. The pinned
supervisor requests SYS_ADMIN, NET_ADMIN, SYS_PTRACE, and SYSLOG. The restricted
namespace rejects that configuration. The Gateway also accepts a requested
runtime class, so a default class alone cannot enforce isolation. A selected
boundary needs admission checks and actual execution tests. A local KVM device
is available for a possible compatibility test. The sandbox runtime choice remains open. Compiler journal run `34317691085`
passed. The complete enterprise goal remains active.

Hosted variant run `34317681420` passed all three jobs on `e705e23`, including
the retained-ID recovery test. The current viewer checks extend the actual
Gateway gate; they do not require a compiler or production runtime change.

The final viewer run also requires redacted provider credentials, no credential
handles, and Hypershell list changes after grant creation and removal. It passed
in 165.74 seconds. Recovery before workload startup passed in 55.43 seconds;
the combined race-enabled package took 222.204 seconds. Vet and pinned
regeneration passed. All temporary test clusters were removed.

Variant commit `02202f40c5ef2849cae99adab059c2926750e382` is on remote main.
Regeneration from the committed files passed with no changes or drift. Hosted
variant run `34318420211` is queued. Check its result in the next goal turn.
The next complete application workflow is sandbox creation and execution with
an enforced isolation boundary. The full enterprise goal remains active.


Hosted variant run `34318420211` passed all three jobs on `02202f4`.
Compiler journal run `34318430356` also passed.

The next application test creates and executes a real sandbox through the
Gateway. The first complete run passed in 254.27 seconds; its race-enabled
package took 255.320 seconds. The owner command ran as UID 1000 under guest
kernel 6.18.35, with a different boot ID from the host. It could not read either
client-key path. Ungranted users could not create or execute the sandbox.
The workload could not raise its hard process limit. A bounded fork test reached
504 children, then received a resource-limit error and removed all children.
Execution and a stored file survived Gateway and database restart, including
Gateway namespace replacement. Sandbox deletion and final Gateway cleanup also
passed. Admission objects remained until the sandbox namespace was absent.

The variant adds an experimental operator-selected runtime class, a separate
sandbox namespace, and admission rules. The Gateway namespace remains restricted.
The pinned OpenShell sidecar keeps process and binary network checks enabled.
Only pinned helpers receive extra capabilities. The client identity is verified
before copy; database and Gateway signing keys remain outside the sandbox
namespace. Workspace setup and the workload run without root or capabilities.
These rules stay in Hypershell. STEGO's existing generated clients and runtime
support the workflow; no compiler change or compiler-pin change is required.

The tests exposed runtime integration faults. Docker's 64 MiB shared-memory
mount was too small for QEMU. The fixture now expands that mount inside its
node before VM startup. A disk-backed sidecar socket could not connect under
Kata. A namespace-scoped mutation policy puts the socket in guest memory.
The same policy changes the pinned driver's root workspace setup to the
workload user. Kubernetes 1.35 must enable its beta mutation API. The controller
verifies admission before it permits sandbox capabilities.

Neither the kubelet PID setting nor the OCI cgroup PID fields set a limit in
the guest. The pinned runtime removes the dedicated PID field; its agent
protocol does not carry the cgroup v2 unified map. The test runtime instead
sets a hard `RLIMIT_NPROC` of 512 and verifies enforcement through Gateway exec.
This is a per-user guest limit. It does not limit the trusted root helpers.

The production runtime question remains open. The fixture does not prove
production isolation. The pinned Kata memory-volume path does not pass its
16 MiB size request to the guest mount. Its stock configuration disables OCI
guest seccomp. NetworkPolicy enforcement, image review, hostile-image tests,
quotas, capacity, and OpenShift support still need evidence. The full enterprise
goal remains active.

The standard Gateway regression passed in 184.90 seconds, including viewer
access and deletion recovery. Its combined package took 244.198 seconds.
Focused race tests, vet, shell syntax, and pinned regeneration passed.

The fresh-cluster sandbox script passed without manual cluster changes. The
Gateway and sandbox test took 292.75 seconds. The combined recovery and workload
package took 350.790 seconds with the race detector. Cold sandbox startup took
65.32 seconds. It repeated all admission, execution, process-limit, key,
restart, retained-file, and cleanup checks. The new CI job runs the same script.

Variant commit `7db4475537b40d8ea8350a3ce844eecf4a8ac1bc` is on remote main.
Hosted run `34357695481` has started. Check all four jobs, including the new
sandbox gate. All local test clusters and the task PostgreSQL container were
removed. The full enterprise goal remains active.

Hosted variant run `34357695481` passed all four jobs. Its real sandbox workflow
passed in 282.00 seconds. Compiler journal run `34357729264` also passed.

The next application check exposed a missing count controller. A test of variant
`7db4475` created a real sandbox and executed a command, but REST still had no
active sandbox count. That negative test completed the other sandbox checks
before it failed on the missing count. The application failure now drives a
common list and watch client in STEGO and a domain count controller in Hypershell.

The generated HTTP client now supports bounded, ordered streams and cancellation
on close. The generated Kubernetes client builds a complete list baseline,
resumes watches from the last resource version, and relists after expired
history. It reads rotated tokens on reconnect, rejects access loss, and bounds
frames, total bytes, list pages, and object count. Retry delays include random
variation and honor bounded server delay metadata. The independent Widget tests
cover this protocol without Hypershell types. Full compiler race tests passed.
Additional stream and snapshot bound tests also passed after the final changes.
The full enterprise goal remains active.

The new Pod count controller passed the real sandbox workflow. REST and gRPC
reported zero before creation and one after the sandbox became active. The
controller repaired a forced count of nine from its cache, then restored the
count after process restart with the same Pod. Sandbox execution, retained data,
and count survived Gateway and database restart and Gateway namespace
replacement. Sandbox deletion returned the count to zero. Final Gateway,
namespace, database, and policy cleanup also passed.

The fresh-cluster Gateway workflow took 299.48 seconds. Recovery and workload
tests together took 357.304 seconds with the race detector. The full variant
race suite also passed; its acceptance package took 404.162 seconds. The count
transport test proved atomic event failure, event delivery, denied access,
and rejection of writes from a former cluster. It passed again after pinned
regeneration. Static checks and compiler vulnerability checks passed.

Compiler `9abcea993bfb0eb1c8d3dcd7f38b0ead0a2b32f5` is on remote main.
Its hosted run `34360166149` passed. The variant now uses that compiler and has
pushed the count workflow in `64b9685`. Post-commit regeneration passed with
no changes or drift. Check the new hosted variant run before the next change.

The count is advisory. One active count controller is required per cluster.
Kubernetes access uses a separate Pod read account. The API still uses the
existing control-plane subject policy. The production runtime and identity
policy decisions remain open. The full enterprise goal remains active.

Hosted variant run `34361031910` passed all four jobs. The count workflow ran
against the real Gateway in 272.22 seconds. Compiler journal run `34361073456`
also passed.

The next workflow deletes a Gateway that has automation accounts. The baseline
created three accounts but returned HTTP 409 on Gateway deletion. The reference
requires provider cleanup before the Gateway disappears, and HTTP 503 when that
cleanup cannot be verified. The new transport tests cover REST and gRPC,
provider failure, atomic metadata and audit rollback after event failure,
restart and retry, and retained cleanup records for late provider results.
A real Keycloak test removed two stored clients and one orphan after an outage
and restart. A credential for another Gateway still worked.

The gRPC registration factory needs to own its provider client. STEGO now
supplies bounded resource registration and cleanup through `OnClose`. Startup
failure, registration panic, normal stop, repeated close, callback panic, and
registration limits have independent generated-runtime tests. Full compiler
race tests and focused runtime tests passed. No Hypershell types entered STEGO.

The actual Gateway test then exposed a separate contract mismatch. The workload
controller reported `ready`, while service-account creation requires the
reference `Running` phase and `Healthy` status. The controller now reports both
fields together and removes healthy state when the workload is unavailable.
The complete Gateway workflow is being repeated with that correction.
The full enterprise goal remains active.

The corrected Gateway workflow passed. Three automation accounts each obtained
an actual Keycloak token and read a stored Gateway provider. Gateway deletion
then removed all three clients before workload cleanup. The Gateway test took
177.50 seconds. Recovery and workload tests together took 235.178 seconds with
the race detector. The full variant race suite passed; its acceptance package
took 392.445 seconds. Focused tests passed again with the final compiler pin.

REST and gRPC checks cover denied deletion, provider outage, event failure,
transaction rollback, restart, retry, and concurrent account creation. The
provider ownership check now requires the exact immutable client name as well
as ownership attributes. A partial provider failure test verifies that all
owned clients are disabled before any client is removed. Another Gateway's
client remains active. The real provider outage and orphan cleanup test passed
in 29.70 seconds. One three-client cleanup took 412.8 milliseconds after restart,
including connection setup. This measurement does not establish capacity.

Compiler commit `95ff57f6a02bd0d0a2808bbe7da8e0f72da7861c` is on remote main.
Its hosted run `34363399585` passed. Variant commit
`d2ae4c1b4e2040ce6e7bac7fe0c9d86f1a5c9cf8` uses that compiler and is on remote
main. Post-commit regeneration passed with no changes or drift. Hosted variant
run `34363972330` is in progress. All task test containers and the retained test
cluster were removed.

Provider cleanup has a five-second limit and a 10,000-client inventory limit.
Large-realm capacity still needs tests. Provider removal cannot roll back with
the database transaction; retry verifies cleanup again. Previously issued JWTs
remain subject to expiry and workload removal. These limits and the production
runtime and identity decisions remain open. The previous count turn made
progress. This deletion workflow adds application evidence. The full enterprise
goal remains active.

Hosted variant run `34364219258` passed all four jobs at commit
`296600efab46fbbbb5e64677169501427468116a`. This documentation commit replaced
run `34363972330`, which CI cancelled. The actual Gateway test passed in
190.28 seconds. The sandbox workflow passed in 292.36 seconds. Both logs confirm
that Gateway deletion removed all three automation clients before workload
teardown. The full acceptance package passed in 505.083 seconds, including
regeneration. Compiler journal run `34364188267` also passed. Both repositories
match remote main. The full enterprise goal remains active.

The previous turn made progress. Compiler journal run `34365395664` passed.
The next workflow starts from the reference CLI account list. That command
always sends `sort` and `order`. The variant accepted only `page` and `size`,
so the normal client request returned HTTP 400. A new generated-process test
reproduced that failure in 3.37 seconds.

STEGO now supplies a bounded literal text match in the shared storage contract.
The adapter accepts declared string fields, quotes columns, and binds values.
SQL-like text and wildcard characters remain literal. Scopes and access rules
apply before counts and paging. Independent generated tests cover these rules,
NULL values, invalid types, duplicate fields, and input and structure limits.
The full compiler race suite passed with PostgreSQL required.

Hypershell now selects its account search fields and supplies status and sort
rules. The reference CLI at `14256be29bcfe4fff38bcaf4a41511cb394ea8e1` passed the
new discovery test against the generated API. It covered all five sort fields,
filtered totals, owner and viewer access, revocation, deletion, and restart.
The test took 5.782 seconds. The reference CLI is a compatibility probe; the
complete STEGO client port remains open. History measurements and the full
variant checks are the next verification steps. The full enterprise goal remains
active.

Compiler commit `bfc113b72015bc441583363e66138856d4e03a4a` is on remote main.
Its hosted run `34366140046` passed. The variant uses that compiler. The full
local variant race suite passed with PostgreSQL and Keycloak required, including
the reference CLI probe. Its acceptance package took 437.363 seconds.

A contract check then found that OpenAPI permits the degraded status filter,
while the reference handler omits it. The new contract-driven test reproduced
HTTP 400. The variant now accepts every OpenAPI status. The final discovery
workflow passed in 5.41 seconds through HTTP and the reference CLI. Static
checks and post-commit regeneration passed with no changes or drift.

A local benchmark queried 5,000 revoked account records, including 1,000 in the
selected Gateway. The status and literal search matched 100 rows in that
Gateway. A 20-row page averaged 7.19 milliseconds across 100 calls, with 111,362
bytes and 1,875 allocations per call. This includes domain access checks and
PostgreSQL count and paging. It excludes transport and concurrent load and does
not establish production capacity or large-history retention policy.

Variant commit `f2ad276990702db535312493d9c28505106b2f4c` is on remote main.
Hosted run `34367135235` is queued. The task PostgreSQL container was removed.
The next verification step is the final remote run. The complete STEGO client
port and wider enterprise goal remain open.

The previous discovery turn made progress. Hosted variant run `34367135235`
passed all four jobs. Its full acceptance package took 511.419 seconds.
Compiler journal run `34367179831` also passed.

The next workflow uses a generated CLI to load a private token file, create
and retrieve a Gateway, check filtered access, and delete it. The baseline
variant had no CLI entry point. STEGO now has a separate CLI component with
application command declarations. The HTTP and CLI components share one HTTPS
renderer. The CLI owns command parsing, private configuration, bounded files,
JSON checks, token-file rotation, and response handling. The application owns
its paths and fields. No Hypershell types enter the component.

The CLI uses verified TLS, optional explicit CA roots, and a 15-second request
deadline. Common client timeouts remain five seconds by default and accept a
bounded override. Configuration stores file references and uses atomic writes
through an open directory handle. Generated Record tests cover parsing,
malformed JSON, private files, rotation, redirects, and failure handling. Full
compiler race tests passed; focused generator tests passed after the final
command-prefix changes. The live variant workflow is the next acceptance check.
The complete CLI and enterprise goals remain active.

Compiler run `34369953695` passed at `ca5c2ce`. The CLI workflow then passed
against the generated variant in 5.49 seconds. It checked TLS, atomic Gateway
and owner-grant creation, gRPC reads, token rotation, filtered access, event
rollback, restart, deletion events, and logout. Its binary dependency graph
contains only standard-library and generated or application command packages.

A reuse check exposed an overly strict route validator. It rejected dotted API
groups, numeric version segments, and `.well-known` paths. A generated test
reproduced the failure. The validator now accepts these path segments and still
rejects traversal, encoded separators, alternate origins, queries, and fragments.
An actual HTTPS request verifies a dotted API path. Focused generated CLI race
tests passed. The full variant run is still active on the previous pin; repeat
the CLI gate after the final pin. The full enterprise goal remains active.

The complete local variant race suite passed with PostgreSQL and Keycloak
required. Its acceptance package took 456.425 seconds. Compiler run
`34370817903` passed at `1810c7c91a5a29c41c7ad25fad97e1a9a4a06795`.
The variant now uses that compiler. Its final CLI workflow passed in 4.95
seconds, and the request-field check passed. This duration includes setup and
is not a latency or capacity measurement.

The final rollback test first completes identity synchronization, then rejects
only the Gateway creation event. It verifies rollback of the placement database,
Gateway, and owner grant. This targets the resource transaction directly; the
earlier general event rejection could fail during identity preparation.

Variant commit `a75075775c2861e4d4a88496f602717dd997a2e5` is on remote main.
Post-commit regeneration passed without changes or drift. Static checks passed.
Hosted run `34371164637` is in progress. The task PostgreSQL container was
removed. Browser and device login, token refresh, remaining CLI commands,
protected credential output, other client ports, and production checks remain
open. The complete enterprise goal remains active.

The final CLI review found a target-version gap. Its safe file operations use
Go 1.25 APIs, but generation accepted an older Go target. A common optional
generator requirement now lets the compiler reject this mismatch in validation,
plan, and apply before any generator runs. Tests cover older targets, invalid
requirements, invalid targets, and successful output at supported versions.
The generated CLI test now declares its minimum Go version. Compiler, command,
generator, and generated CLI race tests passed. This check does not change
the generated application runtime. The complete goal remains active.

Compiler run `34371661001` passed at
`6a7b973739116fea81fe827c5ca75d3274f1a3ec`. Variant commit
`fd56df62076b38fb9beaa8b6eb6785eedb2e8eb9` uses that compiler and is on
remote main. Its live CLI workflow passed again in 5.06 seconds. The
request-field check passed. Post-commit regeneration found no changes or
drift. The temporary PostgreSQL container was removed. Hosted variant run
`34371717106` is in progress; it replaces the cancelled earlier run. This
turn made progress. The full CLI port and enterprise goal remain active.

The previous CLI turn made progress. Hosted variant run `34371717106` passed
all four jobs. Its acceptance package took 527.693 seconds. Compiler journal
run `34372091311` also passed.

The next application workflow creates and revokes a service account through
the generated CLI. The reference returns its secret once. STEGO now supports
up to eight named route parameters and output files reserved before HTTP work.
Sensitive commands require an explicit output choice. New output files use
exclusive creation and mode 0600. Existing files are not overwritten. Write
failures keep the file for inspection and cannot fall back to stdout. Explicit
`--output-file -` selects stdout. This does not guarantee recovery after a
process crash or an uncertain mutation result.

Generated Record tests cover private output before a request, existing files,
symlinks, FIFOs, directory permissions, invalid routes, request-body separation,
malformed responses, cleanup, exact JSON numbers, and write failures. The full
compiler race suite passed with PostgreSQL required. Focused generated CLI
race tests also passed after the final edit. Hypershell supplies its account
paths and fields; no account types enter the common generator. The real-provider
application workflow is in progress. The complete enterprise goal remains active.

Compiler run `34373555720` passed at
`f678fb295b221fe158659ff5ab37d59ff5455760`. The complete local variant race
suite passed with PostgreSQL and Keycloak required; its acceptance package
took 410.041 seconds. The final service-account CLI workflow passed in 26.60
seconds after the reference list aliases were added. It checks real token
issuance, private output, filtered access, denied role elevation, cross-Gateway
isolation, API and provisioner restart, revocation, deletion, and logout.

Variant commit `2e7e02cf47e83ffd8313de2422661112d10b61f8` is on remote main.
Post-commit regeneration passed without changes or drift. Static checks passed.
The CLI dependency graph contains only standard-library and generated or
application command packages. Hosted run `34374674543` is in progress. The
local PostgreSQL container was removed. A review also corrected the main
README's unsupported general production claim and documented application
factories and the generated CLI. Browser and device login, token refresh,
remaining commands and reference CLI options, other clients, and production
checks remain open. This turn made progress. The full enterprise goal remains
active.

The previous turn made progress. Hosted variant run `34374674543` passed all
four jobs at `2e7e02c`. Compiler run `34374770068` passed at `82527b0`.

The next client workflow uses a real OIDC provider. STEGO now generates browser
and device login, discovery, PKCE, signed ID-token checks, private stored
sessions, process locking, refresh, and token revocation. Hypershell supplies
its default public client ID and domain command definitions. The common
runtime contains no Hypershell names or provider endpoint paths.

The live workflow exposed two missing details. The CLI must request only the
`openid` scope; it does not need email or profile data. It must also support
device PKCE when the provider advertises S256. Generated tests check both
device modes, claim validation, callbacks, polling, concurrent refresh,
cancellation, failed saves, and local logout after provider failure. Output
reservation precedes refresh and API requests. A durable pending record prevents
reuse of an old refresh token after an uncertain exchange.

The full compiler race suite passed with PostgreSQL required. Static checks
passed. The final focused generated CLI race tests passed in 10.371 seconds.
The generated CLI dependency scan found no known vulnerabilities. The real
provider workflow has passed browser login, Gateway creation, API restart, and
concurrent refresh. Its final device and logout checks are still in progress.
The complete client port and enterprise goal remain active.

Compiler run `34377592668` passed at
`dc2af4e283d0da07a17bdefd2acf0b97a6a2dd2f`. The real OIDC CLI workflow
passed in 38.21 seconds. It uses Keycloak 26.7.3, a separate generated CLI,
and the generated API. It checks browser and device login, Gateway creation,
filtered access, denied reads, API restart, concurrent refresh, and provider
refresh-token revocation on logout.

The full local variant race suite then passed with PostgreSQL and Keycloak
required. Its acceptance package took 441.456 seconds. The original five-part
Gateway gate remains in this suite. Static checks passed, and the full variant
dependency scan found no known vulnerabilities. Regeneration from the remote
compiler pin had no changes or drift. These durations include setup and are
not capacity measurements.

The provider test browser required an HTML Accept header to select Keycloak's
form endpoint. The generated CLI does not submit password forms. The test
supplies those browser interactions while the CLI owns PKCE, callbacks, token
exchange, session storage, and refresh. The acceptance timeout is now 12 minutes;
the previous hosted suite took 571 seconds before this workflow was added.
Individual network deadlines are unchanged. The final application commit and
hosted checks follow. This turn made progress. Remaining reference CLI options,
commands, other clients, and production checks remain open. The complete
enterprise goal remains active.

The previous turn made progress. Variant run `34379180034` passed all four
jobs at `fc2aedb958caf91acc1e50114796251f903e9fbf`. Its acceptance package
took 607.695 seconds. Compiler run `34379181501` passed at
`70426bf3481f0fbb5370b20b4e48dbe914f6b435`. Both remote heads were verified.

The next workflow grants and removes Gateway access through the CLI. Hypershell
now supplies role discovery and role-binding command definitions. The existing
STEGO runtime handles these definitions without a compiler change. An added
`get current-user` command returns the API user ID needed for a grant. It does
not replace the reference `whoami` command.

The focused race test passed in 5.66 seconds. It discovers IDs through the CLI,
creates a viewer grant, and checks REST and gRPC access. It checks filtered
lists, denied grant changes, duplicate grants, last-owner protection, event
rollback, event delivery, restart, and grant restoration with a new ID. The
request-field checks passed. A reference CLI port table records the remaining
commands and options. The full application suite is in progress. The complete
enterprise goal remains active.

The complete local variant race suite passed with PostgreSQL and Keycloak
required. Its acceptance package took 470.195 seconds. Static checks and pinned
regeneration passed. This step adds application command definitions and tests.
The compiler pin and generated source are unchanged. The CLI port table also
records that Gateway-network application behavior is still missing. The final
application commit and hosted checks follow. This turn made progress. The full
Hypershell port and enterprise goal remain active.

The previous turn made progress. Variant run `34381693723` passed all four
jobs at `98328a03bedb905179c8142716bfddb1f59f3314`. Its acceptance package
took 597.046 seconds. Compiler run `34381695228` passed at
`4748f695368efa412d079199bacb6d2d8db29d41`. Both remote heads were verified.

The next CLI workflow starts with empty placement catalogs. Hypershell now
declares create, get, list, and delete commands for managed clusters, Gateway
releases, and managed databases. It uses the existing STEGO runtime without a
compiler change. The field checks now recognize pointer integer types as well
as scalar and list types.

The focused race workflow passed in 8.10 seconds. It obtains catalog IDs through
the CLI and creates Gateways under CNPG and default deployment modes. It checks
access rules, response shapes, zero and null values, invalid numeric values,
server-owned namespaces, event rollback, event delivery, REST and gRPC reads,
restart, reference protection, and deletion. The default path proves that a
client database ID cannot override dedicated placement. Field checks passed.
The full acceptance and workload suites will run in CI. The full CLI port,
Gateway-network application behavior, other clients, and production acceptance
remain open. The complete enterprise goal remains active.

Pinned regeneration and static checks passed. No generated source or dependency
change was needed. The application definitions, tests, and port documentation
are ready for remote checks. This turn made progress.

The previous turn made progress. Variant run `34383391563` passed all four
jobs at `6bcdc1400cd74331b51faa0eb5c9b4f30e7bdfd6`. Its acceptance package
took 603.078 seconds. Compiler run `34383391678` passed at
`d0783f38ed8d3c39eabe152d60985c5af237e0cb`. Both remote heads were verified.

The next application workflow covers Gateway-network records. The reference
has CRUD and watch contracts, but its network controller only records events.
The port now uses STEGO-generated storage and protobuf services, with Hypershell
field mapping and access rules. It also supplies CLI commands. The existing
STEGO runtime required no compiler change.

A design question asked whether networks should be shared platform records,
user-owned records, or shared records that all Gateway creators can change.
The current default uses the existing placement policy: administrators and
configured controllers can write; Gateway creators can read. REST and gRPC use
the same policy. The reference fallback rules differ by transport. The open
question can change this application policy without changing STEGO.

The final focused race workflow passed in 6.02 seconds. It checks REST and gRPC
CRUD, CLI commands, response shapes, filtered lists, watch events, denied
requests, Gateway ownership limits, validation, mutation rollback, offline event
delivery, restart, deletion, and schema upgrade. Descriptor and CLI field checks
passed. The full application race suite is running. Tunnel execution, the full
CLI and client ports, and production acceptance remain open. The complete
enterprise goal remains active.

Reference inspection found a concrete CLI risk for the next workflow. The
reference `apply` command declares `--dry-run`, but does not read that flag before
it writes resources. It also continues after resource errors and returns success.
The STEGO port must implement a real dry run, validate inputs, and report failed
application with a nonzero exit status. These checks belong in an executable
apply workflow. No reference source was changed.

Pinned regeneration and static checks passed. The full local race suite found
one stale role-discovery expectation after 465.807 seconds. It omitted the new
network permissions. The role seed and expected permission map now include those
permissions. The affected role, migration, and network checks passed in 11.590
seconds; the network workflow took 5.61 seconds. CI will run the complete suite
on the final commit. The first full local run is not a pass. This turn made
progress. The complete enterprise goal remains active.

The previous turn made progress. Variant run `34385889566` passed all four
jobs at `b3bc451e3b89388636a1ee25285e46ed4309909c`. Its acceptance package
took 621.560 seconds. Compiler run `34385893286` passed at
`3c79f8f487a09c2b1e0e8360e6c910901930b4b2`. Both remote heads were verified.

The next workflow applies resource documents through the generated CLI. STEGO
now provides bounded YAML and JSON input, typed resource definitions, a local
dry run, complete preflight before resource writes, exact ID selection, and
ordered create or patch requests. It rejects ambiguous names and duplicate
targets. Partial results distinguish failed, unknown, and unattempted writes.
Mutations are not retried. Hypershell will supply its own kinds and fields.

The generated Record and Widget tests passed. They check input and response
boundaries, no-contact dry runs, preflight failures, partial writes, integer
precision, search escaping, file protection, stdin, and cancellation. The full
compiler race suite and static checks passed. Name lookup is not an atomic
server upsert, and a batch is not one transaction. These limits are explicit in
`specs/cli-apply.md`. Kustomize rendering and remaining client behavior remain
open. The Hypershell acceptance workflow follows. The full goal remains active.

Compiler run `34388034265` passed at
`cc4c3527e4394f7a1eddc32db8887ec9c4604767`. Hypershell now pins that revision
and declares apply contracts for its five current resource kinds. Its focused
race workflow passed in 8.02 seconds. It starts from empty catalogs, creates a
Gateway and owner grant, patches through owner access, checks both database
modes, rejects ambiguous names, and uses exact IDs. It proves no-contact dry
runs, preflight before resource writes, event rollback, partial failure with a
nonzero exit status, event delivery, gRPC reads, and restart. Field checks passed.
The full application suite and workload gates will run in CI. Role and grant
apply mappings, Kustomize, the remaining client ports, and production capacity
remain open. The full enterprise goal remains active.

Review added two executable compiler regressions. Two kinds could map to the
same collection and create the same target twice. A null list count could also
select creation. Both tests reproduced the problem. Duplicate input identity
now uses the collection path, and null counts are rejected before writes. The
full compiler race suite is running again before the final application pin.

The full compiler race suite passed after both regression fixes. The generated
CLI suite includes the two failing cases. The application will regenerate from
the follow-up compiler commit and rerun its focused workflow before publication.

Compiler run `34388741815` passed at
`ae8364b9525e279ac75257ee963f881a91d6b044`. The final pinned apply workflow
passed in 8.43 seconds, including a name with quotes and search operators.
Field checks, regeneration, and static checks passed. Variant commit
`def701137f7f0a603609118bb7631379104e7271` is on remote main. Its complete
application and workload checks are running in `34389001511`.

The user identified a misplaced boundary: common controller mechanisms remained
in Hypershell. Source inspection confirmed repeated watch, queue, scan, timeout,
reconnect, and shutdown code in three controllers. The database controller lacked
the other controllers' action deadline. The identity controller also canceled
scans at a fixed interval. These differences belong in one tested STEGO runtime.

The new `controller` component provides a typed source contract and one serial
worker. It starts a confirmed live watch before recovery scans. It bounds queue
size, watch setup, and each action. Source failure or overflow cancels and joins
the session before a new watch and scan. Scans finish before their next interval
starts. A required error policy stops on denied access. Domain callbacks retain
placement, identity mapping, workload definitions, authoritative reads, provider
ownership, and cleanup order. Hypershell's identity, workload, and database
controllers now use this component in the local test build.

Generated runtime tests and all three domain race suites passed. The tests use
IDs and typed records; STEGO contains no Hypershell resource rules. Full compiler
checks and pinned application workflows follow. This is the first boundary
correction, not the end of the common-runtime work. The Pod count controller's
observation cache, dirty-key scheduling, retained deletion scans, typed transport
adapters, and process coordination still require a common contract and executable
migration evidence. Do not add more copies of these mechanisms to Hypershell.
The full enterprise goal remains active.

The full compiler race suite and static checks passed with the new controller
component. All three application domain race suites also passed. The final
compiler pin and application workflow checks follow before publication of the
controller migration.

Compiler commit `b7837932b0fc6792cb6859e69a39d7096fff7e60` passed CI run
`34389383086`. Variant commit `add91d3a310560204d9f5b0efccb16a5a1f0b2f6`
is on remote main and pins that compiler. Its focused application race suite
passed in 95.408 seconds: identity reconciliation, stored-grant user login,
Gateway recovery IDs, database deletion replay, and CLI apply. Application static
checks and regeneration after the commit passed. Both remote refs were verified.

Variant run `34389668334` is running all four gates on the controller migration.
It replaced apply-only run `34389001511`; that older run was canceled after its
database job passed. Do not count the older run as a full pass. The task-owned
local PostgreSQL container was removed after all local tests stopped.

The boundary review also found a timer, cursor map, and worker pool in
`internal/serviceaccounts/recovery.go`. Those mechanisms belong in STEGO;
account expiry, creator access, and credential rules belong in Hypershell.
`specs/controller-boundary.md` records the ownership split and next workflow
checks. This turn made progress. The complete enterprise goal remains active.

The previous turn made progress. Compiler run `34389875945` passed at
`8445a0aff7151ab6dad53ec4de81315c283f24d2`. The full controller application
run `34389668334` then exposed a regression: the migration removed specific
failure diagnostics. The database and Gateway workload tests still retried, but
could no longer observe the denied DELETE or failed database cleanup. The
acceptance checks were not weakened. Compiler commit
`66a58a6eb00f24c39a68e187f621128c00ed0a61` adds tested, bounded protocol
summaries. They include the known Kubernetes method and status, or the gRPC code,
and omit remote messages, response bodies, URLs, and credentials. The application
uses these summaries in its controller notices.

The next common mechanism is keyed scheduling. The `controller` component now
provides `RunKeyed`, a baseline gate, duplicate suppression, one serial writer,
operation deadlines, scan scheduling, and capped exponential retries. Capacity
includes queued, delayed, and active keys. A change during a write is retained;
a new event cannot bypass a failed key's delay. Due retries precede newer work.
The Pod count controller now supplies cache changes and domain actions without a
polling timer, retry map, or worker lifecycle. Its changed-namespace set describes
one cache update; STEGO owns pending work and retries.

The generated keyed tests and domain count tests passed. The full compiler race
suite and static checks passed. A final scan-retry reporting test and registry
checks also passed. The local benchmark at 10,000 keys measured 369.1–391.2 ns and
112 allocated bytes per cycle. `specs/controller-keyed.md` states the method and
limits; these values are not production capacity evidence. The real sandbox
workflow is running with the generated queue and corrected failure summaries.
Pinned regeneration and remote application gates follow. The full goal remains
active; worker pools, shared recovery adapters, and distributed fencing remain
open.

The real sandbox gate passed in 317.874 seconds with the keyed controller and
safe diagnostics. Gateway recovery before controller startup took 57.69 seconds;
the complete sandbox workflow took 259.14 seconds. It verified count repair
through REST and gRPC, count-controller restart, access changes, actual sandbox
execution, Gateway and database restart, namespace replacement, and offline
cleanup. The added reset-during-write test passed with the count race suite in
3.137 seconds. It allows the active write to finish but requires a complete
replacement before subsequent writes.

Compiler `50e393410fb9eb77ccfc523155f2b7ccbe60c74d` passed full CI run
`34390663953`. Regeneration from that pin produced the same controller and
client bytes used by the workload gate. The count transport test passed in
7.23 seconds, and application static checks passed. Variant commits
`facdd0bfe5a3e9f8d4b8504158d9721646be48b8` and
`b7108c397bb3294d8d748388972e1d4a3be21779` separately update the generated
runtime with safe diagnostics and migrate Pod count scheduling. Both are on
remote main. Variant run `34391200715` is running the full acceptance, database,
Gateway, and sandbox gates on the final commit.

The earlier variant run `34389668334` is terminal: its general acceptance job
passed, but all three workload jobs failed on the lost cleanup diagnostics.
The exact failure logs were inspected. No earlier failed job is counted as a
pass. The new run must verify the corrections. The local test cluster and its
PostgreSQL container were removed after testing. This turn made progress. The
full enterprise goal remains active.

The previous turn made progress. Compiler run `34391254604` passed at
`18a3151bac6b97f1b983016f3357412cf2db0442`. The final keyed-controller variant
run `34391200715` has passed its database, Gateway, and sandbox workload gates;
the general acceptance job is still running. This verifies the corrected failure
diagnostics in the real workload paths.

The next extraction covers service-account recovery. The application still had
its own timer, cursor map, worker semaphore, and goroutine lifecycle. A new
17-record regression test proved a cursor bug in that code: eight slow provider
calls caused the short page to restart forever, so later records received no
turn. The test failed on the old code after 20.41 seconds. It passed with the
new generated sweep in 13.32 seconds. Multi-page cleanup and role/expiry recovery
also passed with the first generated build.

The `controller` component now supplies `RunSweep`. It validates groups, streams,
and complete pages before effects. It owns a fixed worker pool, a group time
budget, cursor progress through the started prefix, and group rotation. A partial
short page retains its cursor. Failures remain in retained source data for later
cycles. The application supplies state groups, expiry and access filters, and
provider actions. It no longer owns a scheduler or worker pool.

A second generated regression proved that a slow first stream could starve
history. After a pass exhausts its budget, the next pass now begins with the next
stream. The full compiler race suite and static checks passed after this fix.
The real application and Keycloak workflows are running on the final scheduler.
A dispatch benchmark measured 40.2–47.8 microseconds and about 4 KB per 100 empty
actions with eight workers. `specs/controller-sweep.md` records the limits; this
is not an application throughput claim. Cursor progress is not an acknowledgment,
and distributed claims and fencing remain open. The full goal remains active.

All four jobs in variant run `34391200715` passed at
`b7108c397bb3294d8d748388972e1d4a3be21779`. The previous controller diagnostics
and keyed scheduler therefore have complete remote workflow evidence.

The final sweep workflow suite passed in 157.916 seconds. It covers late
Keycloak creation and restart (37.38 seconds), revocation after database loss
(32.78 seconds), the real Keycloak account lifecycle (37.57 seconds), multi-page
cleanup (18.68 seconds), the partial-page regression (13.31 seconds), generated
REST and gRPC transports (6.64 seconds), and role limits and expiry (10.46 seconds).
The suite uses the final stream rotation behavior. These timings include fixture
setup and are not production capacity evidence.

Compiler `025aa22555b84d4e14ff8055f62eb7db9ee6107d` passed CI run
`34392328767`. Variant `9254dec8910a6b6705c190ccb238b8b33f85b3b7` pins that
compiler and is on remote main. Pinned generation produced the same controller
bytes used by the workflow suite. Application static checks and regeneration
after the commit passed. Variant run `34392649734` is running all four remote
gates on the migration. The task PostgreSQL container was removed after tests
stopped. This turn made progress. The full enterprise goal remains active.

The previous turn was a verified wait: it polled the live variant run
`34392649734`. Compiler run `34392732740` passed. The current turn removes
another common mechanism from Hypershell. The Gateway recovery page loop could
emit one valid ID before it found an invalid later ID. A strengthened application
regression failed on the old loop with one emitted item. The generated scanner
now checks the whole page before dispatch and sets page-count and request-time
limits. The regression and both Gateway controller race suites passed.

The `controller` component version 1.3.0 adds `Scan`, `CursorSource`, and shared
cursor page types. The sweep names remain aliases. Both Gateway controllers use
one domain adapter for the private recovery API and canonical, nonzero KSUIDs.
The identity controller now scans retained IDs instead of numbered pages of full
live Gateways. Invalid scan contracts stop the controllers. Provider discovery,
current-state reads, access rules, and cleanup actions remain in the application.
Storage and replay adapter generation remains open; this change does not claim
that the remaining source code is all domain-specific.

The generated scanner tests cover malformed pages before effects, opaque cursor
order, request limits, repeated cursors, cancellation, source and emitter errors,
and request expiry. The full compiler race suite with PostgreSQL and static
checks passed. A local Go 1.26.8 benchmark on Linux amd64, Intel Core Ultra 9 185H,
used three 200-millisecond samples without the race detector. Validation,
request-context setup, and dispatch for 100 empty actions took 4.69–5.14
microseconds, 3,768 bytes, and seven allocations per scan. It excludes transport,
storage, provider calls, and payload construction. It is not production capacity
evidence. Real Gateway identity, restart, and workload checks are running.

All four jobs in variant run `34392649734` passed at
`9254dec8910a6b6705c190ccb238b8b33f85b3b7`. The recovery sweep therefore has
complete remote workflow evidence. Compiler `639b95bb49bc9020b849f5f9ee6180a7b1a1ee09`
adds the cursor scanner and passed remote run `34393311879`.

A host restart stopped the first local scanner transport and workload test
processes. The missing process handles and process list confirmed that they had
stopped. Their incomplete logs are not counted as complete passes. The task test
cluster was removed, PostgreSQL was restarted, and the interrupted checks were
started again. No Playwright command was used. The user has prohibited Playwright
because it causes a kernel panic; do not use it in later goal work.

Regeneration from compiler `639b95bb49bc9020b849f5f9ee6180a7b1a1ee09` produced
identical controller files to the local tested build. Application static checks
passed. The restarted transport suite passed in 38.838 seconds: real Gateway
identity took 33.45 seconds, and 205-ID recovery with denied requests and API
restart took 4.34 seconds. The workload controller race tests passed in 11.214
seconds. The missed-deletion workload test passed in 55.30 seconds, including API
restart and database removal. The complete Gateway workload test is running.

The complete Gateway workload suite passed in 226.455 seconds. The full workload
case took 170.11 seconds and verified database and OIDC setup, owner access,
filtered workspaces, denied writes, removal of access, three service-account
identities, controller restart, Pod and database restart, namespace replacement,
stable keys, and offline deletion. These results use the pinned generated
scanner. They do not establish production recovery capacity.

Variant `b3809721a7b3a2a0814eda9a47a738ddaa0bc36d` commits the scanner migration
and is on remote main. Regeneration after the commit passed. Remote run
`34394176385` is queued for the final variant. The local workload script removed
its test cluster, and the task PostgreSQL container was removed after tests
stopped. This turn made progress. The full enterprise goal remains active.

The next inspected common loop is database deletion replay in
`internal/grpcapi/catalog.go` in the variant. It has a local page loop and a Go
string-order check over rows returned in database order. Before replacing it
with the generated scanner, add a test that uses a database collation whose
ordering differs from Go string order. The current replay test also assumes Go
string order; it does not prove correctness for other database collations.

The previous turn made progress: Gateway recovery moved to the generated cursor
scanner and passed the local workload gate. Compiler documentation run
`34394192646` passed. This turn applied the same scanner to database deletion
replay. A new application test used two fixed IDs and the ICU `und-x-icu`
collation to force an order that differs from Go string order. It compared the
stream with an ordered database query. The old replay loop passed with C ordering
but failed with ICU ordering: `Internal: invalid database replay order`.

The replay server now supplies an authorized page query and the reference event
mapping. STEGO owns the page loop, validation, deadlines, and request limit.
The application retains the capability header and maps internal contract errors
to a bounded gRPC error. No new compiler runtime API was necessary. Retained
storage queries and protocol adapters still need generation; they remain open
common mechanisms, not assumed domain code.

The final real database workflow passed in 74.819 seconds. The workload took
65.21 seconds and verified TLS, persisted data, stable credentials, denied foreign
namespace access, and offline cleanup. Five stable reconciliations took 83.22 ms
in this run; this is not production capacity evidence. The replay cases took
8.56 seconds and cover C and ICU ordering, multiple pages, denied requests,
capability headers, empty authorized results, and API restart. Controller race
tests, application static checks, pinned generation, and regeneration after the
commit passed. No Playwright command was used.

Variant `baacc3ccc242d38eb21ba6bfba0e312e908a21b5` is on remote main. It retains
compiler pin `639b95bb49bc9020b849f5f9ee6180a7b1a1ee09`. Run `34394627677` is
pending for the new variant. The prior variant run `34394176385` passed its
database job but was still running at this check; it is not counted as a full
pass. The task cluster and PostgreSQL container were removed after the local
processes stopped. This turn made progress. The full enterprise goal remains
active.

The next storage work should address unused totals in recovery queries. Current
`ListOptions` has `CountOnly` but no count-free cursor contract. Generated
`Store.List` executes a count before page retrieval. Gateway ID recovery,
database deletion replay, and service-account repair do not use that total.
Use application and generated-storage tests to establish a cursor contract that
preserves filters and access rules, avoids unused counts, and removes repeated
query construction from application sources. Measure the resulting queries.

The user's PR 200 guidance changed the next action before cursor-storage work
began. The review used PR head `a5dbb5c427d461b3988d371d20a02cf5f46088d0`, all
seven changed specifications, and the review discussion. The new
`specs/reconciliation-contract-review.md` maps all eight requirement groups to
current code and tests. It separates restart-safe retained deletion data from
finalization and records missing version checks, status ownership, per-resource
retry coverage, cross-process exclusion, and metrics.

A PostgreSQL probe proved a stale-status defect at variant
`baacc3ccc242d38eb21ba6bfba0e312e908a21b5`. It read a Gateway, changed desired DNS
through the owner update path, then published Healthy through the control-plane
status path using the earlier observation. The API accepted the old success for
the changed desired state. The probe failed its expected-conflict assertion in
0.24 seconds. Its temporary test file was removed after execution. This result
is not an implemented fix and is not counted as a passing check.

The implementation order now starts with generated identity, revision,
generation, conditional writes, and status ownership. The application must then
prove stale-result rejection through real transports and provider work. Durable
cleanup completion, keyed scheduling across resources, fencing, and observability
follow. Unused-count optimization remains open but has lower priority. Common
mechanisms remain STEGO responsibilities. Hypershell retains domain policies and
provider rules.

The user was asked whether public reads should retain a Deleting resource until
cleanup finishes, or preserve immediate 404 with an operator view. Visible
pending deletion is recommended. The answer was still pending at this review;
version-check work does not depend on it. No Playwright command was used. The
probe PostgreSQL container was removed after the probe stopped. The full goal
remains active, and reconciliation is not claimed complete.

The assembler now protects predeclared Go names and generated startup names
when it assigns imports. A generated module first failed to compile because
imports hid `error`, `nil`, and `make`, or conflicted with `main` and `run`.
The allocator also rejects reuse of an already assigned suffix. Fill wiring
now consumes the import allocation record directly; the duplicate allocation
pass was removed. Ambiguous predeclared constructor value names fail before
output. Generated startup tests cover component and fill calls, HTTP and task
wiring, collisions, and repeated assembly. The compiler race suite and static
checks passed. See [symbol bindings](symbol-bindings.md) for measured cost and
limits. Complete typed constructor bindings remain open under C7.

Constructor metadata now has one index-validation step before unused constructors
are removed. A regression test found that invalid middleware indexes could be
ignored, so a declared authentication layer was omitted from handler assembly.
Invalid cleanup, dependency, and collection indexes were also ignored. The
compiler now rejects these records without output, checks all constructor-indexed
fields, and requires one unambiguous primary middleware selection. Tests use valid
control fixtures and check stable diagnostics. An older task-index test now
reaches the intended validation check. See [constructor metadata](constructor-metadata.md).
Complete typed dependency binding and Go type checking remain open.

The constructor metadata change passed the full STEGO race suite and static
checks. Hypershell commit `0ab80d5635a35067ef87b2098433ffc8c8af9f11` pins the
new compiler. All 74 generated and dependency file hashes are unchanged; contract
race tests, the build, and post-commit regeneration passed. Both repositories
were pushed to `main`. This change does not close the remaining reconciliation
or typed compiler contracts.

[Identity reconciliation reads](identity-query-reads.md) now use the generated
cursor contract without unused counts. The full query-change application race
suite passed 116 acceptance tests. Local owner-state latency fell while allocated
memory increased; the measurements and limits are recorded separately.

The real Gateway gate exposed a [generated gRPC header defect](grpc-stream-headers.md).
A nil header was copied into an empty map, which could hide a retryable RPC status
behind a terminal capability error. Compiler `1fe838c21f891ec1ddd09e4df678c00ce126f95a`
preserves the gRPC signal. Compiler race tests, static checks, and CI passed.
Hypershell `2ddb2e4a1f9e8b65eda48d128060eb8d35519d9d` pins that fix and adds a
regression through the generated TLS client. The fixed-compiler Gateway cluster
gate passed in 209.690 seconds. Final controller, contract, query, and static
checks passed; post-commit regeneration preserved all 74 generated and dependency
hashes. Both repositories are on remote main. Durable retries, progress bounds,
fencing, status ownership, and the broader enterprise requirements remain open.

[Registry snapshots](registry-snapshots.md) now bind captured YAML and protobuf
bytes to a plan and record their content digest in applied state. Tests first
showed that the previous compiler accepted registry changes during generation
and after planning. Apply and dependency resolution now reject those changes.
Missing, malformed, and invalid-path slot imports also stop generation. The full
compiler race suite and static checks passed. Local measurements show the added
read and hash cost. Complete compiler build identity, input manifests, and
isolated builds remain open under C3; this digest does not close those contracts.

Hypershell `028c54f32f2ed5c7a3fe6a7100849c623f94e525` pins the snapshot compiler.
Its 73 generated and dependency files are unchanged. Only compiler state gained
the registry digest, which an independent calculation verified. Contract race
tests, the application build, and post-commit regeneration passed. Both feature
commits are on remote `main`. The broad enterprise goal remains active.

[Stream startup contracts](grpc-stream-contracts.md) move initial gRPC header
validation and early RPC error handling into the generated client. Hypershell
keeps the database capability names, replay scope, and controller error policy.
The generated TLS tests check exact headers, duplicate values, empty streams,
early failures, and preservation of the first event. This removes a common
protocol procedure from domain code. Database field ownership and generation
tracking still require the pending application contract decision.

Compiler `da993db0b506ff2ab0084c6c2a817f084efa9a69` passed the full race suite,
static checks, and CI. Hypershell `6e530b71716af805c0284ee51a4c51cc6094b9bf`
uses its generated stream helper. Controller and contract race tests passed.
The real database gate passed in 89.440 seconds; the complete Gateway gate passed
in 206.710 seconds. Post-commit regeneration preserved all 75 generated,
dependency, and state hashes. Both feature commits are on remote `main`.
Validation cost and its measurement limits are recorded with the stream
contract. Durable retries, cross-process fencing, complete database observation
ownership, and the broader client and enterprise requirements remain open.

The CLI port review found unsupported command scaffolding in reference Hypershell
`14256be29bcfe4fff38bcaf4a41511cb394ea8e1`: role create/delete commands and a role
apply mapping exist, but role route registration and OpenAPI expose only reads.
Variant commit `b347af4` corrects the port table. Role mutation and dynamic
permission policy must not be inferred from those unused command stubs.

[Relative timestamp flags](cli-relative-time.md) now supply the supported
service-account `--expires-in` behavior through STEGO. Hypershell selects the
field mapping and retains API lifetime limits. Compiler
`5a2f13eec5e8a2ae633d02e9adf4d82f6dabe02e` passed the full race suite, static
checks, and CI. Variant `d4ea8724b44406e125bca46d06b3b0fe16ce6cb5` passed all
six CLI workflows in 87.367 seconds, including real Keycloak credentials,
expiry checks, access denial, event delivery, and restart. Post-commit generation
preserved all 75 generated, dependency, and state hashes. Both repositories are
on remote `main`. The remaining CLI, controller, and enterprise requirements
remain active.

[CLI identity reporting](cli-identity.md) now uses an authenticated API response
and requires explicit protected output for raw or decoded tokens. STEGO owns
command execution, refresh, transport, validation, and output protection.
Hypershell supplies its current-user route and verified identity fields. The
extension schema is version 1.1.0; strict clients must update that schema.

Compiler `c4a49b3a86e81b5a2190635505492a57cb9644d7` passed the full race suite,
static checks, and CI. Hypershell `0c8092c82d962d5f9ce1c2158beee916ab5c4269`
passed eight CLI and current-user workflows in 103.272 seconds. The checks
include real JWT rejection, protected token export, OIDC refresh, access denial,
event delivery, and restart. Post-commit regeneration preserved all 76 generated,
dependency, and state hashes. Both feature commits are on remote `main`.
The broad goal remains active; this change does not resolve the outstanding
controller ownership, durable progress, build identity, or full client port.

Compiler and CLI build records are implemented in STEGO. See
[`build-identity.md`](build-identity.md). The common record separates the
compiler build from the application build. It reports missing source metadata
as unknown. Artifact verification and complete input manifests remain open.

Hypershell build-report evidence is recorded at commit
`4127c2c11427ce8698cdeea5df38dbd52b6fdc08`, with STEGO compiler pin
`8ae682d5bc3b7fceecc590db0dfbdf21b28f0385`. The variant enables the common
version command with one application setting. Its offline executable test checks
the application metadata, compiler pin, and saved compiler state. Seven selected
CLI acceptance tests passed in 100.494 seconds with PostgreSQL and Keycloak
required. CLI unit tests, contract tests, and vet passed. These tests cover
Gateway access, atomic creation, grants, events, restart, login, apply, catalogs,
and service accounts. The full application suite and Kubernetes provider gates
were not repeated locally for this change.

After the variant commit, pinned regeneration passed with all 78 generated,
state, and dependency file hashes unchanged. STEGO feature CI run 34494759858
passed. Build records remain diagnostic data: artifact digests, signatures,
complete input manifests, and controlled release builds are still open work.

The identity backlog regression exposed a fixed 10,000-reference recovery limit.
Controller 1.8.0 adds [resumable cursor scans](resumable-scans.md). It returns
completed progress across page budgets and cancellation. The existing full-scan
API keeps its limit behavior. Durable progress storage and cross-process
ownership remain open; this addition does not substitute for either contract.

Hypershell cursor recovery is implemented at
`19f021ff13e8489a1a14496fbe24c4b2b228d5a0`, with STEGO compiler pin
`e8afad97deda15136091ab5e203bd96726435ef0`. Its former 10,000-reference
limit failed the backlog regression. The new controller completes 10,100
references across bounded passes. The PostgreSQL and generated gRPC/runtime
test resumes a partial page after API restart and reads 10,106 retained grant
references without loss or repeat. Invalid and denied reads, late insertion
before a cursor, current grants, and cleanup remain covered.

Eight selected acceptance tests passed in 57.873 seconds. The final insertion
and version checks passed in 2.139 seconds. After the duplicate-user check,
the identity workflow passed in 36.270 seconds. Real Gateway login and
independent identity cleanup passed in 38.802 seconds. Unit, contract, race,
and vet checks passed. This does not claim a full application or Kubernetes
suite run for the change. Post-commit regeneration preserved all 78 generated,
state, and dependency file hashes. STEGO feature CI run 34495275642 passed.

The measured late inventory page improved from 3.632–3.876 ms with offsets to
0.764–0.917 ms with a cursor. Each new page uses two reads and no count query.
See the variant's `acceptance/identity-cursors.md` for benchmark conditions and
limits. Durable progress, bounds for incomplete resource cursors, cross-process
fencing, and production recovery targets remain open.

The compiler now records [project input manifests](project-input-manifests.md).
The manifest includes captured project files, declared generator inputs, and
supplied generation options. Input changes remain visible when generated code
is unchanged. Module merging uses the captured bytes. Artifact verification,
external tool identities, undeclared generator reads, and complete application
build inputs remain open C3 work.

The role-binding apply gap requires immutable resource semantics. CLI 1.4.0
adds [immutable apply](immutable-apply.md) with declared identity fields, exact
comparison, and no automatic replacement. Named mutable resources retain their
existing contract. Hypershell supplies the binding field mapping and keeps
creation permissions and uniqueness in the API transaction.

CI history exposed a verification gap: frequent main-branch pushes canceled
complete application runs before they could finish. The
[CI evidence rule](ci-evidence.md) now preserves active push runs in both
repositories. Pull requests can still cancel obsolete runs. This changes how
checks are scheduled; it does not reduce the full application acceptance gate.

The [controller metrics contract](controller-metrics.md) adds fixed queue,
retry, action, scan, and duration metrics to the generated keyed runtime.
Hypershell selects the metrics address and passes the collector to its domain
controllers. The listener and collector remain common STEGO code. Durable
conditions, cleanup age, distributed ownership, and production capacity remain
open reconciliation requirements.

[Cleanup summaries](cleanup-summaries.md) extend controller diagnostics beyond
the admitted queue. A generated aggregate reads pending deleted resources for
one authorized owner and target. An independent runtime sampler reports their
count and oldest deletion time without resource IDs. Per-resource conditions,
history, distributed ownership, durable retries, and production capacity remain
open requirements.

The [CNPG provider gate](cnpg-provider-evidence.md) adds real shared database
creation, SQL, restart, repair, and deletion evidence in the variant. The new
common API discovery contract is generated by `kubernetes-client` 1.1.0. The
variant supplies resource requirements and provider policy. That provider-only
gate did not establish Gateway execution. The later complete CNPG Gateway gate
is recorded in the same evidence file. Database generation tracking and
production deployment remain open.

The complete shared CNPG Gateway workflow now has local and remote evidence for
application execution, separate SQL identities and keys, access, restart, role
repair, and confirmed cleanup. General PostgreSQL connection policy now belongs
to the generated [PostgreSQL read client](postgres-reads.md). Hypershell supplies
SQL queries, identity selection, resource ownership, and repair decisions.
The full Gateway gate passed again with that generated client. This does not
complete production capacity, HA, distributed ownership, or the full enterprise
and Hypershell goals.

The identity grant workflow now uses the existing generated condition writer
and [scan-cycle runtime](scan-cycles.md). Hypershell declares
`identity_users/GrantsSynchronized` for stored Gateway grant references. Failed
passes retain Unknown across restart and resumed tails. Only a full clean cycle
can restore True. Grant writes invalidate the condition and checkpoint in their
existing event transaction. Cycle save checks resource revision, desired
generation, and checkpoint version. No generator change was needed for this
application condition; the compiler pin remains
`635dc4636f1d2ffa808b300a668c2eceaae52ad3`.

Five application workflows passed under race detection in 95.137 seconds.
They cover real Keycloak role changes, offline grant removal, API and controller
restart, stale saves, and condition/event rollback. The upgrade and transaction
checks passed in 6.514 seconds. The upgrade preserves client condition history
and invalidates old observations. It requires the old controller to stop before
the new migration and API start. See the variant's `acceptance/grant-conditions.md`
for the evidence and rollout contract. Distributed ownership, provider fencing,
observation-age policy, durable retries, and production recovery capacity remain
open. This condition does not revoke issued tokens or certify unrelated provider
identities. The broad goal remains active.

The full application race suite then passed with PostgreSQL and Keycloak
required; the acceptance package completed in 923.127 seconds. Static checks
passed. Local Kubernetes and VM gates were not repeated for this condition
change. The preceding scan-cycle commit `0128ef5` passed all six remote CI jobs
in run 34517913592. That earlier run does not cover the new condition.

The grant-condition application commit is
`d0ad397d1d12fbfc99fffabb2bc2fdc280cbc8d5`, pushed to remote `main`.
Final controller compatibility tests passed under race detection in 1.305
seconds, and vet passed again. Pinned regeneration preserved all 90 output,
state, and dependency file hashes. New remote CI results remain pending.

The Hypershell factory probes drove a common component preflight stage. Twelve
components now expose their existing input checks through one compiler contract.
Reconciliation reuses resolved contexts and captured source bytes. The factory
regressions now fail consistently through validate, plan, and apply. Invalid
protobufs and a late source change also stop compilation without output changes.
See [component preflight](compiler-preflight-gap.md) for tests, timing results,
and the next proven namespace defect. C1 and the broad goal remain active.

Compiler preflight is committed at `0b959fb4ffe7842942d2946f82d2e83d093c969b`.
The full compiler race suite passed with PostgreSQL required, and vet passed.
Hypershell `25c09dd53cb36be90ddaf17945e8b3527f50a5e6` uses this pin and the
updated component versions. Regeneration changed only saved compiler state and
the CLI compiler build record among 90 output, state, and dependency files.
Contracts and the offline CLI version check passed under race detection in
1.517 and 1.625 seconds. Vet and post-commit regeneration passed. Repeated
regeneration preserved all 90 hashes. Both commits are on remote main; their
new CI runs were still active at this check. No full application or Kubernetes
suite was repeated locally for the compiler metadata update.

The Go package-name probe now has a common compiler fix. Library generators
check import paths and derived package names before rendering. CLI package
containers retain valid hyphenated paths. The protobuf generator checks its
resolved Go mapping, including the previously accepted main and invalid import
paths. Command tests preserve existing output on failure. Independent build
tests import nested controller and protobuf libraries. Hypershell probes also
pass with unchanged output. See [Go package names](go-package-names.md).
C1 and the broad goal remain active pending the complete acceptance audit.

The package-name compiler change is committed at
`021b2d1986adddeaace234feeae9feda5f72d8d1`. The full compiler race suite and vet
passed with PostgreSQL required. Hypershell
`6d9b6ea77ed8e2b518baf76b45b494ad6034b307` uses that pin. Only saved compiler
state and the CLI compiler build record changed among its generated, state,
and dependency files. Contracts and offline CLI version tests passed in 1.519
and 1.644 seconds. Vet, repeated generation, and post-commit generation passed;
all 90 hashes stayed stable on repeat. Both commits are on remote main.

The prior preflight compiler CI run 34521081985 passed. Package-name compiler
CI run 34521857221 is active, and application run 34521961193 is pending.
GitHub replaced pending metadata run 34521193713; it preserved the active
grant-condition run 34520447297. No success is claimed for these unfinished
runs. The full application and Kubernetes gates were not repeated locally for
this metadata update. C1 and the broad goal remain active.

The assembly source audit now rejects invalid build targets and conflicting
derived slot names before rendering. Project settings and assembly use the
same target check. Regression tests first reproduced both gaps. The full
compiler race suite passed with PostgreSQL required on port 32902; vet passed.
See [component preflight](compiler-preflight-gap.md) for the scope and the open
dependency minimum and wiring checks. C1 remains active.

Remote grant-condition run 34520447297 has now passed all six application jobs,
including acceptance, database, Gateway, CNPG, and sandbox workflows. Compiler
package-name run 34521857221 also passed. Application metadata run 34521961193
is still active. These results do not close the remaining reconciliation gaps.

The next compiler audit adds missing Go target requirements for PostgreSQL
storage and clients, outbox, Kafka, and gRPC. Generated runtime tests fix their
module target during dependency resolution and add static checks. This found
that gRPC needs Go 1.26.0 because of the compiler's x/sys security minimum.
The checks also found unreachable cleanup-reader code for services without
cleanup owners; the template now handles that case directly. See
[generator Go targets](generator-go-targets.md). The full target and wiring
audit remains open. C1, C7, and the broad goal remain active.
