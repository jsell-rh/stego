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
| C2 | Complete project and fill workflow | Init, apply, fill create, test, build, repeated apply, and drift pass in a fresh directory | Pending |
| C3 | Reproducible and recoverable generation | Compiler and input identities, stable output, dependency ownership, state format, interrupted-write recovery, and concurrent apply tests | Pending |
| C4 | Secure authentication and authorization | Signature, issuer, audience, expiry, key rotation, scope isolation, and denied request tests | Pending |
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
checks, complete schema semantics, and recoverable apply remain open.

Apply now checks symbolic links and special files before writes. Rooted file
operations restrict output writes to their directory. Each file is written to a
temporary file and renamed after a successful sync and close. This prevents
partial file content, but does not yet provide a multi-file transaction or
process locking. Those remain C3 requirements.

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
Dependency resolution, checksum verification, and stale-plan checks remain open.
