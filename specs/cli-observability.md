`cli-application` 1.6.0 and `otel-tracing` 1.9.0 supply telemetry at the
generated CLI entry point. Application code supplies command definitions.
It does not create a provider or add a logging wrapper.

Each invocation owns one runtime. It starts a `cli.command` span and attaches
the runtime to its context. Generated outbound clients use this context, so
HTTP calls can continue the command trace into a service. The invocation ends
its span and closes the runtime before main returns an exit code. Both interrupt
and `SIGTERM` cancel command work. Existing command errors and stdout remain
unchanged. Diagnostic JSON goes to stderr. Programs that read CLI JSON must
read stdout separately from stderr.

The command emits `cli.command.completed`, `stego.cli.command.duration` in
seconds, and `stego.cli.active_commands`. The fixed outcomes are `success`,
`failure`, `canceled`, `deadline`, and `aborted`. Unknown outcomes become failure.
Cancellation is not an error span. The completion function is idempotent.
Arguments, command names, configuration paths, credentials, output, and raw
errors are absent from telemetry. The span name is fixed to avoid recording
input as a metric or span label. Logs and metrics also cover unsampled calls.
Local JSON remains available without a collector.

Each process receives a distinct service instance ID. This separates producers;
short CLI processes also produce short-lived metric resources. This change does
not provide cross-process aggregation or a delivered-capacity guarantee.
The existing bounded queues, TLS exporter, and three-second runtime close budget
remain in force. Command work must honor cancellation. Blocking application
code or an OS output write can still delay command return. A panic can produce
an aborted completion during unwind, but this change does not replace Go panic
handling. A killed process can lose queued signals.

Generated CLI tests passed with and without a tracing component. They check
clean JSON stdout, command failure, private-data exclusion, distinct build
identity, and `SIGTERM` during a real TLS request. Telemetry tests check sampled
and unsampled export, completion counts, final active counts, error status,
and the parent relationship between command and HTTP spans.

The tests and static checks passed in the OpenShift `jshell` cluster. Job
`cli-telemetry` in namespace `stego-test-20260910` completed with exit code 0.
It used Go 1.26.8, Linux amd64, an Intel Xeon 6975P-C, a one-CPU limit, a 3 GiB
memory limit, and `GOMAXPROCS=1`. The Go image digest was
`sha256:2d54f6c8c6ea532a321e0b4c69553b2ed3637608d4f4357dbed37939fe2620cc`.
The source archive SHA-256 was
`93d1204ceead40b18d0965ebd85d0827fe3eae37cff7d4019d451dd9c2d91ede`.
The job received no service-account token or workstation credentials.

Three 200 ms command-helper samples measured 280.1–296.7 ns for local recording,
168–169 bytes, and four allocations per call. They dropped 99.63%–99.65% of
local records under the synthetic one-CPU load. TLS OTLP recording measured
3.421–3.827 microseconds, 2,168–2,171 bytes, and 20 allocations. Those samples
dropped 95.08%–95.55% of local records; OTLP queue drops were not measured.
The writer discarded JSON. These measurements cover recording calls, not full
CLI startup, delivered throughput, or application latency. They do not establish
production capacity. The generated process tests separately require the
completion records to be delivered at normal command load.

Further performance and heavy tests must run in CI or an approved cluster.
Do not repeat them on the developer workstation. The first local measurement
was interrupted by a host restart. The cause of that restart is unproven.

The Hypershell probe first failed on compiler `f87dfaf`: CLI login succeeded,
but no common command completion was recorded. The variant test covers atomic
Gateway creation, denied reads, server restart, collector loss, and correlated
command, client, and API signals. Its application result must be recorded
separately. Database signals and other independent worker entry points remain
open.
