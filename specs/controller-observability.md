The `controller` component version 1.13.0 connects keyed controllers to
`otel-tracing` version 1.4.0 when both components are present. `RunKeyed` and
`RunKeyedWatch` start and own common telemetry. Consumers do not need exporter
setup, log callbacks, metric registries, or span wrappers. A controller generated
without the telemetry component keeps its existing standalone behavior.

Overlapping controllers in one process share a private provider set. The first
starts it; the last flushes and closes it. A new controller waits for an ongoing
bounded close to finish. Each queue registers once and unregisters when its run
ends. Registration is limited to 64 concurrent queues. Reconnects retain the
same queue registration and providers. Metrics aggregate all registered queues
in the process. They do not create overlapping cumulative streams from separate
providers with the same identity. Deployment must set `OTEL_SERVICE_NAME` to
identify each controller service. Per-controller domain labels are not supplied.

Common work operations are `reconcile`, `scan`, and `watch`. Each creates an
internal span and one completion log with operation, outcome, retry status, and
duration. The log shares the span's trace and span IDs. Watch work covers one
session, including its child scans and actions. A failed session ends before
reconnect. Canceled work is recorded when the callback returns. Callbacks must
still obey their context. A panic propagates; its text is never recorded.
Abrupt process failure has no flush guarantee.

Outcomes are success, failure, timeout, and canceled. Timeout and cancellation
use the Go context error contracts. Success and cancellation have INFO severity.
A retrying failure has WARN severity. A failure without retry has ERROR severity.
An error cannot become a message or field. Resource keys, provider values,
credentials, request metadata, and domain state are excluded.

The runtime also emits fixed watch-start, reconnect, and cleanup-summary failure
events. Existing application observation callbacks remain supported. Hypershell
can remove callbacks that only translate these common events into log messages.
Domain callbacks are still responsible for their own data policy.

The generated OTLP metrics are:

| Name | Meaning |
| --- | --- |
| `stego.controller.work.duration` | Duration histogram in seconds; fixed operation, outcome, and retry fields |
| `stego.controller.retries` | Retry counter by fixed operation |
| `stego.controller.events` | Counter by fixed common event name |
| `stego.controller.queue.running` | Number of registered queues |
| `stego.controller.queue.capacity` | Sum of queue capacities |
| `stego.controller.queue.queued` | Pending admitted keys |
| `stego.controller.queue.active` | Keys with active actions |
| `stego.controller.queue.retrying` | Pending keys with retry history |
| `stego.controller.queue.waiting` | Callers waiting for admission |
| `stego.controller.queue.ready` | Number of queues ready to start work |

Queue gauges have no labels. Instrument state keeps the shared 128-series bound.
Metrics and logs cover unsampled work. Local JSON logging remains enabled
without a collector. Local and OTLP queues retain their existing separate
256-record limits. Full queues can drop logs. Recording does not wait for export
or local writes. The shared three-second close budget and fixed failure counters
still apply. See [service logging](service-logging.md) for blocked stderr behavior.

Generated tests cover controller operation with and without the telemetry peer,
overlapping provider ownership, queue aggregation, registration limits and
release, fixed outcomes and severity, duplicate finish calls, unknown operations,
private-data exclusion, and sampled and unsampled work. Existing keyed queue,
scan, retry, cancellation, and reconnect tests pass through both generated forms.
The full compiler race suite passed with PostgreSQL required on port 32912.
Static checks passed. The new Gateway test first failed with compiler `47a0e0a`
because controller telemetry was absent after creation and a provider failure.

A local recording measurement used Go 1.26.8, Linux amd64, and an Intel Core
Ultra 9 185H. Three 200 ms samples measured 5.810–5.889 microseconds per successful
action with TLS OTLP enabled, 6,738–6,784 bytes, and 72–74 allocations. No local
logs were dropped in those samples; OTLP queue drops were not measured. Local
logging alone measured 301.9–326.5 ns, 382–386 bytes, and four allocations, but
dropped 67.21%–68.71% of records under overload. These results measure recording
cost and queue pressure, not delivered throughput or production capacity. The
writer discarded local JSON. Repeat with `STEGO_BENCH_TRACING=1 go test -v
-count=1 ./internal/generator/oteltracing`.

This change does not complete telemetry for non-keyed sweeps and cycles, cleanup
backlog export through OTLP, outbound client spans and propagation, database
spans, process resource metrics, or domain event declarations. Existing loopback
Prometheus diagnostics remain available. Cross-process trace continuation from
API requests into reconciliation is not claimed. The full
[observability requirement](shared-observability.md) remains open.

Compiler CI run 34532475046 failed an existing reconnect test. A retained action
returned its terminal error before the second scan started. The test assumed an
order that the queue contract does not require. The corrected test waits for
that scan before returning the terminal error. It still verifies that old
callbacks stop before reconnect and that discovery repeats. Four hundred race
runs passed across both generated forms and GOMAXPROCS values 1 and 4. Run
`STEGO_STRESS_CONTROLLER=1 go test -v -count=1 ./internal/generator/controller`
to repeat this check. Production runtime code did not change in this correction.
The failed CI result is not a pass; the corrected revision needs its own CI.

Resource identity currently contains only `service.name`. Separate replicas do
not yet have a unique `service.instance.id` in their resource records. Metric
attribution across replicas therefore remains open, as does the full production
observability requirement.
