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
does not identify the earlier recovered CNPG startup failure. The API runner
needs a new result after its collection acknowledgement fix, and the complete
CNPG workflow still needs qualification with this compiler. The full enterprise
goal remains open.
