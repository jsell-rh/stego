# Browser telemetry

This component generates `@stego/browser-telemetry`. Add its output directory
to the consumer's package workspace. The package pins its OpenTelemetry SDK
and wire-format dependencies. It does not change global providers.

The common `browser-service` archetype includes this runtime and the backend
relay. Both use the service declaration's name by default. Set `service_name`
or the backend's `telemetry_service_name` to select another identity. When both
are supplied, they must agree. A conflict fails validation before output writes.
Use one through 64 lowercase letters, digits, dots, underscores, or hyphens.
The first character is a letter. Invalid explicit values cannot select a default.

The component can also generate a client package without a backend in the same
module. That client uses its explicit `service_name` or the service name. Its
separate backend must enable the relay with the same identity.

`createBrowserTelemetry` returns a tracer, logger, meter, and lifecycle methods.
The generated backend puts public deployment settings in the HTML head. The
runtime uses these settings to enable traces, logs, and metrics. It uses the
backend's trace sample ratio. Invalid or duplicate metadata disables export.
An optional `sampleRatio` can reduce the deployment ratio. It cannot increase
that ratio or enable a disabled signal. Without metadata, export is disabled
unless the caller supplies `sampleRatio`. This explicit option enables all
three signals for standalone use; the ratio must be between zero and one. `beginTrace` lets a domain workflow supply its
root trace ID. `RootTraceIdGenerator` uses the platform's cryptographic random
source and rejects zero IDs. Domain code supplies event names and attributes.
Do not put credentials or private user data in telemetry records.

The runtime sends OTLP protobuf to the fixed same-origin paths
`/telemetry/v1/traces`, `/telemetry/v1/logs`, and `/telemetry/v1/metrics`.
It gets a session CSRF token before each batch. It does not accept a collector
URL, OAuth token, arbitrary header, or redirect. Each export has a five-second
deadline, a 256 KiB request limit, and a 16 KiB response limit. Each signal has
at most one active export. Logs and traces have queues of 256 records and
batches of 16. Metric instruments have 32-series limits, and a meter can create
at most 32 instruments. Log bodies are strings of at most 512 characters.
Log and metric attributes use at most 16 primitive values with bounded names
and strings. The runtime reports queue loss and export failure with finite
public categories. It does not expose collector error text.

Observable metric callbacks use the same value and attribute limits as direct
metric writes. A meter permits at most 32 registered callbacks across single
instruments and batches. Each callback can record at most 32 valid observations
per collection. These checks run before the SDK buffers observations. Batch
callbacks can observe only their selected instruments from the same meter.
Duplicate registrations do not consume more capacity. Removing a callback
releases its capacity. Asynchronous callbacks retain the SDK collection deadline.

Call `forceFlush` at a workflow boundary when delivery evidence is required.
Call `shutdown` when the application releases the runtime. Hidden-page events
also start a flush. Shutdown removes these event listeners. Browser page exit
can still prevent delivery; it is not a durable queue. Server rendering gets
providers that do not export and does not create browser timers.

The generated Go backend checks the session, Origin, CSRF token, content type,
body size, and admission limits. The shared Go telemetry runtime validates the
protobuf data, replaces resource and scope identity, removes vendor trace
state, and uses its existing verified TLS collector connection. The collector
address and credentials do not enter browser code.
