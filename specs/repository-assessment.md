STEGO has a sound purpose and a substantial implementation. It can support a
controlled trial for Go REST services. The repository does not yet support its
claim that it generates production-ready services from trusted components.

This assessment covers commit `f844f8b` on 2026-09-08. Checks used Go 1.24.4 on
Linux. Findings marked as observed came from local command runs. Other findings
come from source inspection. This review did not run PostgreSQL, Kafka, or an
identity provider. It did not change application code.

The intended workflow is useful: a team defines its service in YAML, STEGO
generates the common code, and developers supply business logic through fills.
The output has no STEGO runtime dependency. The project deliberately excludes
LLM integration. That boundary is consistent with the purpose in
[the main specification](spec.md).

The repository has the following structure:

| Area | Purpose | Assessment |
| --- | --- | --- |
| `cmd/stego` | CLI commands and generator registration | Simple entry point. Generator selection is fixed in the binary. |
| `internal/types`, `parser`, `registry`, `ports` | Read declarations and resolve components | Clear package roles. Some validation rules also occur in later stages. |
| `internal/compiler` | Validate, plan, write files, record state, and assemble shared code | Useful separation of stages. The assembler contains substantial Go and GORM knowledge. |
| `internal/gen`, `generator` | Generator contract and component implementations | Good package separation. Component contracts still depend heavily on code strings. |
| `internal/slot` | Read a protobuf subset and generate interfaces and operators | Works for the supplied contracts. It is a custom parser, not a complete protobuf toolchain. |
| `registry` | Archetype, component, mixin, and slot definitions | Describes more capabilities than the binary implements. |
| `examples` | Two service modules, fills, and generated output | Useful demonstrations. Both examples use the same basic domain and architecture. |
| `specs`, `scripts` | Specifications, task records, reviews, and automated development scripts | Detailed history. Product acceptance needs a separate, executable check. |

The tracked core contains 14,423 lines of Go source and 33,099 lines of Go tests.
The REST generator alone has 4,313 source lines. The assembler has 1,788 source
lines. Generated example output occupies another 10,253 lines. These counts
include comments and blank lines. They show substantial work, but they do not
measure reliability.

The entity and collection distinction is a strong design choice. One entity can
have several access paths with different scopes, operations, and fills. This
supports real API requirements without duplicate entity declarations. The
generated constructor wiring is also easy to inspect. Keeping fills outside
`out/` gives developers a clear ownership boundary.

The delivered scope differs from the stated scope as follows:

| Intended capability | Delivered capability | Assessment |
| --- | --- | --- |
| One complete Go REST archetype for the MVP | CRUD, scoped routes, patch, upsert, search, OpenAPI, validation, and auth choices | Substantial functional coverage. |
| Plain Go output | Both checked-in example modules compile during their test runs | Delivered for the supplied examples. |
| Typed business extension points | Generated Go interfaces with gate, chain, and fan-out operators | Delivered at the interface level. Entity fields pass through string maps. |
| Full project lifecycle | CLI commands exist | Fresh build and fill creation have observed failures. |
| Plan, apply, and drift | File hashes, entity change summaries, file removal, and drift checks | Useful foundation. Disk state and dependency ownership are inconsistent. |
| Trusted, reusable registry components | YAML registry plus generators compiled into the CLI | A fixed component set, with incomplete records of the code used to generate output. |
| Operations support | Health and tracing declarations; empty generators | No generated health or tracing behavior. |
| Event publishing | Working event slot composition; Kafka metadata | No Kafka producer generator. Example notifications write logs. |
| Database evolution | GORM AutoMigrate at startup | Matches the detailed REST specification. It is not a reviewed sequence of migration changes. |

The main specification explicitly defers multiple archetypes, multiple
registries, per-component SHA pins, and other output languages. Their absence is
not an MVP defect. Mixins have some implementation despite their deferred status.
The more serious gaps affect promises made for the current workflow.

1. **The default authentication boundary is incomplete for a standalone service.**
   The [JWT generator](../internal/generator/jwtauth/generator.go) reads the token
   payload without signature verification. It assumes that a gateway or proxy
   verifies the token. It also does not enforce expiration, issuer, or audience.
   A caller who can reach this middleware directly can supply identity and role
   claims. The supplied RH SSO generator adds signature verification and key
   discovery, which is a useful alternative. The default and the README still
   need a clear deployment contract. A production claim must state and enforce
   this boundary.

2. **The declaration is not yet a strict boundary for human or LLM input.**
   Observed: a string field with `min_lenght: 1` passed validation. The parser
   ignored the unknown key, so the intended constraint was lost. Observed:
   `min_length: 10, max_length: 1` failed `validate` but passed `plan`.
   [Validation](../internal/compiler/validate.go) and
   [reconciliation](../internal/compiler/reconciler.go) use different sets of
   checks. The latter also skips components without registered generators.
   These are direct problems for the stated purpose: accepted declarations must
   have precise meaning, and missing behavior must cause an error.

3. **The first-use workflow is not complete.**
   Observed: the README Todo declaration passed `validate` and `apply`. The
   documented `cd out && go build` then failed because `go.sum` entries were
   missing. Running `go mod tidy` at the project root made the build pass.
   Observed: `fill create admin-policy -slot before_create -collection todos`
   wrote an `interface.go` with undefined `CreateRequest`, `Identity`, and
   `SlotResult` types. `go test ./fills/...` then failed. The
   [fill scaffold code](../cmd/stego/main.go) calls the interface generator
   without resolved common message definitions. The hand-written example fills
   work, but they do not prove the scaffold workflow.

4. **Dependency ownership conflicts with reconciliation.**
   STEGO owns the root `go.mod`, including the module that contains human fills.
   It derives requirements only from component wiring. Dependencies added by
   fills have no equivalent input in that process. Normal `go mod tidy` also
   changes this generated file. Observed: both examples reported no changes from
   `plan`, while `drift` reported a modified `go.mod`. The fresh Todo project
   showed the same result after `go mod tidy`.
   [Plan computation](../internal/compiler/reconciler.go) compares desired
   hashes with saved hashes. When they match, it checks file existence but does
   not compare disk content. If another change causes an apply, the writer
   writes all generated files, including the module file. The project needs an
   explicit ownership rule for dependencies and a consistent drift policy.

5. **Registry records do not identify all executable generation inputs.**
   [Generator registration](../cmd/stego/main.go) is fixed in the binary.
   Checking out a registry SHA does not change that binary's generator code.
   State assigns the same registry reference to each component and does not
   record a compiler build identity. Local registry paths are used directly.
   The documented environment override records `env-override` as the SHA, as
   both examples show. Thus, a state file does not establish the complete code
   origin claimed by the specification. Deterministic generation needs a record
   of the compiler build, registry content, configuration, and dependencies.

6. **Declared capabilities can have no implementation.**
   The [health generator](../internal/generator/healthcheck/generator.go) and
   [tracing generator](../internal/generator/oteltracing/generator.go) return no
   files or wiring. `event-publisher` adds `kafka-producer`, but the CLI has no
   generator for it. Reconciliation skips it. Event fills still execute, so the
   mixin is partly useful, but it does not provide Kafka delivery. The search
   component also describes a `resolve_field` slot; its generator does not
   consume slot bindings. A component listing must distinguish implemented,
   partial, and unsupported behavior.

7. **Runtime failure behavior needs more proof.**
   In the generated user create handler, storage succeeds before event and
   after-create fills run. A fill error then returns HTTP 500. The caller cannot
   infer from that response whether the write succeeded. A retry can repeat the
   action or cause a conflict. The code has no durable event handoff for this
   path. The generated server also uses `http.ListenAndServe` without explicit
   server timeouts or graceful shutdown. These findings come from
   [the generated handler](../examples/user-management/out/internal/api/handler_org_users.go)
   and [main](../examples/user-management/out/main.go). They require a documented
   runtime contract and tests before broad deployment.

The main structural concern is that the compiler depends on the supplied
components more than the directory layout suggests. `Wiring` contains Go
expressions, constructor indexes, variable names, and formatting strings. The
assembler must rename identifiers, inject arguments, and order constructors.
It also emits GORM and PostgreSQL setup. The generated storage package imports
API and search packages. A replacement component must satisfy these code
conventions as well as its declared ports. The generator interface itself lives
under `internal/`, which limits direct use by separate Go modules.

These choices can serve a fixed platform. They impose a high cost on a general
component ecosystem. A typed intermediate representation for constructors,
dependencies, routes, and middleware would make those contracts explicit.
Separate rendering from validation and route resolution. Do not split large
files only to reduce their length; split them where responsibilities differ.

The fill boundary also needs a precise claim. The Go interface is typed, but
`CreateRequest.Fields` is `map[string]string`. Field names and entity value
types are not checked at the fill boundary. This is a deliberate tradeoff for
generic slots, but it is weaker than entity-specific typed contracts. Fill
qualification fields are metadata. The compiler does not establish that the
current fill code was tested or approved by the named person.

The test work is a strength. The REST generator tests include actual `go build`
calls, not only string comparisons. All 15 root packages passed `go test ./...`.
Separate test runs passed in both example modules. Those runs compiled the
generated packages, which contain no runtime tests. The root test command does
not include the nested example modules. The inspected tests provide no HTTP to
PostgreSQL service acceptance suite. No tracked CI workflow was present.

The observed checks were:

| Check | Result |
| --- | --- |
| Build CLI from current source | Pass |
| Root `go test ./...` | Pass |
| `go test ./...` in each example module | Pass |
| Validate both examples using the local registry | Pass |
| Plan both examples | No changes |
| Drift check both examples | Modified `go.mod` |
| Fresh README declaration: validate and apply | Pass |
| Fresh README declaration: documented build | Fail: missing dependency checksums |
| Fresh project: tidy dependencies, then build | Pass |
| Fresh project: create fill, then test fills | Fail: undefined common types |
| Unknown constraint key | Accepted and ignored |
| Impossible string constraint | Validation rejects it; plan accepts it |

All write probes used temporary directories. The example commands only read
project files or ran tests. The pre-existing untracked `.claude/` directory was
not changed.

The development process has extensive records: 65 task files and 62 review
files. All task files have complete status. The
[development loop](../scripts/loop.sh) repeatedly runs planning, implementation,
verification, and process review. Those records help explain changes. They do
not replace product acceptance: the fresh-user failures remain despite complete
task status. The next release gate should run user workflows and service
behavior, with expected results independent of implementation details.

The vision is most credible as a standard Go REST platform for one organization.
Its value is consistent APIs, controlled upgrades, and a small amount of custom
code. The two examples demonstrate this use case. They do not establish general
fitness for unrelated architectures or domain workflows. The project has not
yet measured the effort needed to create a real service, upgrade its generator,
or diagnose a generated runtime failure.

Priorities should follow the trust claim:

1. Make the declared support boundary accurate. Document auth requirements and
   reject missing generators. Mark empty and partial components clearly.
2. Complete one fresh-project workflow, including dependency resolution, fill
   creation, tests, build, a second apply, and a clean drift result.
3. Use one validation stage for validate, plan, and apply. Reject unknown YAML
   keys and invalid component configuration. Preserve clear error locations.
4. Define module ownership and complete generation records. Record the compiler
   identity as well as registry content. Make file and state updates recoverable.
5. Add service acceptance tests for auth, scope isolation, concurrent updates,
   database evolution, and failures after a committed write. Run all three Go
   modules in CI.
6. Use the generator on a real service with a different domain. Measure manual
   work, unsupported requirements, and upgrade effort. Use that evidence before
   adding languages or a broad plugin system.

My fitness assessment is: a strong architectural prototype; a candidate for a
controlled internal trial after the workflow and auth gaps are resolved; and an
unproven general service platform. Its best next milestone is one service that
can be created, tested, deployed, changed, and regenerated with clear guarantees.
