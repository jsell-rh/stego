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
| C5 | Correct storage and event behavior | PostgreSQL integration, explicit migrations, concurrency, transactional writes, durable event delivery, and failure tests | Pending |
| C6 | Production runtime support | Health, readiness, tracing, metrics, bounded requests, deadlines, shutdown, and resource limit tests | Pending |
| C7 | Explicit generator contracts | Typed wiring, supported capability checks, typed business extension contracts, and compatibility tests | Pending |
| H1 | Hypershell compatibility baseline | Inventory and executable checks for REST, gRPC, RBAC, watch, SDK, CLI, UI, and deployment contracts | Active |
| H2 | STEGO Hypershell implementation | Clean generation and tests without rh-trex-ai; reviewed domain code remains outside generated output | Pending |
| H3 | System verification | Service integration and local deployment checks, security tests, race tests, measured performance, and regeneration checks in CI | Pending |

Resolve work in small atomic commits. Each behavior change needs a regression
check that can fail when the behavior is wrong. Use runtime tests for runtime
claims. Use explicit compatibility evidence for existing Hypershell behavior.
Do not weaken tests or drop requirements to obtain a passing result.

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
Dependency resolution as a compiler step remains open.

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
and registry identities, dependency resolution, and broader state migration
support remain C3 work.
