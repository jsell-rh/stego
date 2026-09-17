# Browser startup diagnostics

The complete Hypershell CNPG workflow passed in run `35176586562`. One browser
backend process restarted three times before it became ready. Its final error
identified `browser.NewBrowserBackend`, but did not identify the failed step.
The cause remains unknown. See the [application record](upstream-dashboard-integration.md).

STEGO now generates an explicit dependency from the browser constructor to the
process telemetry runtime. `NewBrowserBackendWithTelemetry` binds the context
and calls the existing `NewBrowserBackend` entry point. Constructor order and
reverse cleanup order follow the compiler dependency graph. Direct callers can
still use the existing entry point; they must supply a telemetry context to
record startup stages. No global logger or provider is installed.

Each initialization step emits `startup.step.completed`. The fixed
`startup.stage` values are:

- `browser.transport`
- `browser.configuration`
- `browser.session_keys`
- `browser.session_schema`
- `browser.upstream`
- `browser.discovery`
- `browser.authorization`
- `browser.routes`

The event contains a fixed outcome and elapsed time. Outcomes are `success`,
`failure`, `canceled`, `deadline`, and `aborted`. Unknown outcomes become
`failure`. An unknown stage produces no event. Error text, configuration values,
URLs, keys, tokens, and provider response bodies are not accepted as attributes.
A panic records `aborted` and continues to propagate.

The existing process runtime supplies bounded local JSON logging and verified
OTLP export. Startup spans, logs, and the `stego.startup.duration` histogram use
the same stage and outcome. `stego.startup.active_steps` records active steps.
Nested discovery HTTP calls use the startup span context. Sampling can suppress
exported spans while logs and metrics remain available. Queue limits and runtime
shutdown apply to startup records. A completion callback is idempotent and must
finish before runtime close.

Schema verification and identity discovery retain cancellation and deadline
classification. Startup uses the existing TLS, schema, permission, and identity
checks and their current time limits. This change adds no retry or fallback.

## Verification

Focused generator and assembly checks passed locally. The assembly check places
the browser before telemetry in its input and requires generated dependency
ordering and context binding. Generated runtime tests cover fixed stages,
unknown input, duplicate completion, closed runtime, local logging, sampled and
unsampled OTLP correlation, and child HTTP spans. Browser subprocess tests check
configuration and key failures, canceled schema startup, and private-data
exclusion. With PostgreSQL configured, they also check complete startup and a
failed identity-discovery dependency.

Compiler revision `83592be` passed all six jobs in
[35180190820](https://github.com/jsell-rh/stego/actions/runs/35180190820).
The full race suite passed, including the generated browser backend and
telemetry runtime. The PostgreSQL job passed all six startup subprocess modes
and key rotation. Hypershell's candidate pins this compiler in all three
modules. Its complete live qualification is still in progress. This change has
not identified the earlier recovered startup failure.

The first compiler run, `35178696741`, passed the generated telemetry suite but
failed browser tests. The startup subprocess set `OTEL_TRACES_EXPORTER`, which
the strict runtime does not support. It now clears telemetry settings and uses
an empty endpoint to disable export. The pool assembly test now supplies the
required telemetry dependency. The local application address test now supplies
a non-nil database handle so it reaches the address check. These changes do not
relax runtime validation. The PostgreSQL CI job also runs the focused startup
tests so database-backed startup failures are reported before the full suite.

Browser telemetry now includes `stego.relay.service.name` and
`stego.relay.instance.id`. STEGO sets both from the receiving backend runtime
and replaces all browser-supplied resource attributes. The browser service name
remains separate. These fields identify the relay process, not a browser tab
or a user. They let an application test link rendered browser signals to the
backend startup records. They do not make browser-supplied event content
trusted. The common relay test checks all three signal types, forged resource
attributes, and separate relay instances.

Native logout run `35179200926` detected a race in its test fixture. The fixture
changed the backend origin after starting its HTTP server. The origin is now
set from the reserved listener address before the server starts. The test still
uses native form navigation and the race detector. The generated logout policy
is unchanged. Native browser run
[35180190817](https://github.com/jsell-rh/stego/actions/runs/35180190817) passed
at `83592be`. The generated same-origin form returned HTTP 303. The no-referrer
control sent a null Origin and was denied with HTTP 403.

Run `35179358194` passed all five companion jobs and the generated telemetry
suite, including relay identity checks. Its compiler job found one remaining
fixture error in `TestBackendSessionKeyRotation`: a nil database handle stopped
startup before key validation. The test now supplies an unconnected handle and
still requires invalid keys to fail before database access. The focused startup
gate includes that test. The complete run at `83592be` passed after this fixture
correction. That compiler revision was then promoted to remote `main`.

## Complete public Gateway workflow

Hypershell `bf83eef` passed the
[public workflow](https://github.com/jsell-rh/hypershell-stego/actions/runs/35180785681)
with compiler `83592be` in 667.64 seconds. The management console and Gateway
console each exported startup records from three runtime instances. All 48
stage log/span pairs match, duration metrics are present, and active-stage
counts returned to zero. The test requires rendered dashboard signals and
startup signals from the same backend relay instance.

The complete test includes verified RPC, denied access, namespace and database
recovery, account use, durable deletion, and linked worker telemetry. All 415
generation hashes match across regeneration and the workflow. All 1,351 source
hashes match the tested commit. The deployed image layer contains the exact
qualified Gateway console binary. Three saved dashboard images were inspected.
Independent cleanup passed at `2026-09-17T04:30:02Z`.

This is application evidence for the common startup and relay mechanisms. It
does not identify the earlier recovered CNPG startup failure. At that point, the API runner
and complete CNPG workflow still needed qualification with this compiler. The full enterprise
goal remains open.

The next CNPG run, `35182853953` at Hypershell `c0c2d23`, failed during account
creation in the final deletion setup. The test deliberately restarted the
console credential provisioner. Generated RPC logs show six `GetCredentials`
calls with `UNAVAILABLE`, then recovery. The controller had marked the Gateways
unavailable. The test checked provisioner process readiness but did not wait
for Gateway controller recovery before the next account creation. The API
returned HTTP 409 with `gateway_not_ready`.

The failed run retained exact source and generation evidence before the test,
CNPG primary replacement, namespace recovery, and independent cleanup. It did
not reach the final generation snapshot or complete startup signal record.
One test secondary Pod replacement was needed for scheduling. These limits
remain explicit in the application's `startup-cnpg-failure-evidence.json`.

The application test now requires denied account creation during the deliberate
outage, no account state from denied requests, and controller recovery after
restart. It retains the production readiness rule and does not retry account
writes. The complete revised workflow was then awaiting qualification. The cause
of the earlier recovered browser initialization failure remains unknown.

Hypershell run `35184753568` at `fdd11d3` passed all 11 application tests,
including deliberate provisioner outage and controller recovery. Its complete
browser test took 742.70 seconds. The outer CI runner then failed during
evidence collection and saved an empty archive. This remains a failed gate:
final generation and telemetry artifacts cannot be verified. Independent
cleanup at `2026-09-17T05:36:59Z` found no remaining test resources or lease.
No scheduling intervention was required. The consumer collector now retains
available evidence and reports missing required files, while still failing the
gate. The compiler revision remains `83592be`; runtime code is unchanged.

## Verified CNPG workflow

Hypershell `cf232b0` passed all 11 required tests in
[35186648964](https://github.com/jsell-rh/hypershell-stego/actions/runs/35186648964).
The complete browser workflow took 737.75 seconds. All 1,359 source hashes
match the tested commit. All 415 generated-file hashes match the initial and
final snapshots and the saved archive. The console image matches its verified
module binary. Three management console instances and three Gateway console
instances supplied 48 matching startup log/span pairs, complete metrics, and
no failed pairs. Three dashboard screenshots were reviewed.

The deliberate provisioner outage denied account creation for both Gateways.
Controllers recovered without account write retries or changes to SQL and
credential identities. The final artifact matches an independent live capture.
CNPG primary replacement, namespace recovery, denied access, encryption,
three-account cleanup, final deletion, and correlated PostgreSQL telemetry
passed. Independent cleanup at `2026-09-17T06:04:12Z` found no test resources,
allocations, volumes, or held lease. No scheduling intervention was required.

Hypershell remote `main` is `0175b0b` and includes this evidence. The compiler
pin remains `83592be`. The previous empty archive and recovered browser startup
failure remain unexplained. This result qualifies the current CNPG startup
workflow; it does not close production capacity, live Kata isolation, or the
full enterprise goal.

## Main workflow repeat

Hypershell `0175b0b` passed the complete public workflow in
[35188311004](https://github.com/jsell-rh/hypershell-stego/actions/runs/35188311004)
and the complete CNPG workflow in
[35188311598](https://github.com/jsell-rh/hypershell-stego/actions/runs/35188311598).
Both verified all 1,360 source hashes and 415 generated-file hashes. Each
retained six browser runtime instances with 48 matching startup log/span pairs,
complete metrics, and no failed startup pairs. All three screenshots from each
run were reviewed. The separate main API gate passed all 51 required checks;
the core suite passed 307 top-level tests.

The CNPG browser test took 749.98 seconds. Its complete evidence includes
provisioner outage denial and recovery, database and namespace replacement,
denied access, protected credentials, real automation accounts, durable deletion,
and PostgreSQL telemetry. Independent cleanup passed at
`2026-09-17T07:04:39Z`, with both test volumes and all test runtime absent.
The final provisioner record matches an independent live capture.

This repeat required one identity-checked replacement of the test secondary
database Pod to resolve split CPU and memory capacity. Primary, storage, resource
limits, and unrelated workloads were unchanged. The application test later
performed its own deliberate database replacement. This result does not prove
fixture placement without intervention. Hypershell revision `9dabb6c` retains
the complete main CNPG record and this limit. The compiler pin is still
`83592be`. Production capacity, live Kata isolation, and the full goal remain open.
