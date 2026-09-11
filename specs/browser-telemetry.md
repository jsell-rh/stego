# Browser telemetry runtime and relay

STEGO owns browser provider setup, batching, transport, limits, and lifecycle.
The application owns domain event names and the mapping from domain probes to
telemetry. The generated package supplies traces, logs, and metrics. It uses
OpenTelemetry SDKs and the official OTLP protobuf serializers. The
[component contract](../registry/components/browser-telemetry/README.md) states
the public interface and limits.

The separate Go browser backend owns the authenticated relay. Its session,
Origin, CSRF, body-size, content-type, and admission checks run before export.
A session has a burst allowance of six requests and gains one request per
second. The admission map has at most 512 entries. The shared Go runtime has
three relay permits, an 8192-node protobuf validation budget, and a two-second
collector deadline. Unknown fields and enum values are rejected. The runtime
replaces browser-supplied resource and scope identity and removes vendor trace
state. It reuses the verified TLS collector connection and sends no browser
cookies or credentials to the collector.

The final common cluster check passed ten Node runtime tests, the strict
TypeScript check, and race checks. These include all three signals, CSRF
transport, partial export, failed sessions, queue loss, deadlines, page hide,
shutdown, zero-entropy rejection, and server rendering. The browser package
completed in 9.152 seconds. The generated Go telemetry package passed its
runtime, TLS collector, input rejection, and deadline checks in 41.880 seconds.
The final source check is `check9` in `/tmp/stego-browser-telemetry-uhil6nq4`.
Earlier checks found an npm cache path error, a Go checksum-cache path error,
a template delimiter error, and the changed log processor constructor. These
were corrected. Failed checks retain separate result files.

The generated browser backend passed with PostgreSQL, including relay session,
CSRF, header, body-size, and admission checks. The package completed in 70.260
seconds. The real Hypershell Gateway workflow then passed in 21.94 seconds.
Its TLS OTLP collector received the browser root span, backend and API parent
chain, a log record with the same trace ID, and a metric with the generated
browser identity. It also retained the required grant, access, REST, gRPC,
event, restart, renewal, and sign-out behavior. This check is in
`/tmp/stego-browser-relay-1orkdb7y`.

This is protocol evidence. Rendered browser acceptance, common deployment
configuration, collector-failure behavior through the complete application,
and deployment remain required. The served UI is still the
scaffold until the rendered Gateway gate passes. Browser exit can lose queued
telemetry; these bounded SDK queues do not provide durable delivery.

UI lint found that the lifecycle declaration did not state that its callbacks
are independent of `this`. The declarations now state this contract. The UI's
application and test types, import checks, and lint passed with the correction.
This change does not change JavaScript output.

Hypershell `910a8ab` adopts compiler `e4ca4dc`. The final pinned Gateway workflow
passed in 12.03 seconds, with the browser root span, log, and metric at the
collector. The input-manifest race check passed. All 162 hashes match both
generation passes and the checkout, including the Node acceptance lockfile.
The UI passed 233 tests, types, import checks, lint, and its production build.
Both generated browser packages match the UI check byte for byte. The final UI
check is `check2` in `/tmp/stego-browser-otel-ui-codwet_4`; its first lint check
failed and remains recorded. The 54-file build was captured in an 852,377-byte
archive. It is not yet the served UI. Full compiler CI passed. Full application
CI is pending. The result files were saved before test namespace removal.
