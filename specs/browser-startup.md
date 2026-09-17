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

The generated runtime tests require CI qualification. This change has not yet
identified the earlier live failure and is not yet adopted by Hypershell.
