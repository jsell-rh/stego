The `otel-tracing` component version 1.5.0 gives each telemetry runtime a random
UUID v4 in `service.instance.id`. OpenTelemetry defines this field to distinguish
instances of the same service and recommends opaque UUIDs. See the
[service resource conventions](https://opentelemetry.io/docs/specs/semconv/resource/service/).

The runtime reads 16 bytes from Go's cryptographic random source, sets the UUID
version and variant bits, and formats a canonical lowercase value. The ID uses
no host name, pod name, process ID, file path, credential, or domain data. A short
read or reader error prevents identity creation and returns fixed error text.
There is no deployment override for the ID.

All three providers in one runtime share one immutable resource containing
`service.name` and `service.instance.id`. Local service and controller JSON logs
carry the same instance ID. IDs are resource identity, not request fields or
metric point labels. The existing metric series limit is unchanged.

The ID lasts for one runtime lifetime. Repeated requests, exports, and watch
reconnects keep it. Overlapping keyed controllers that share one provider set
also share its ID. A replacement runtime gets a new ID, including when it starts
in the same process. Independent runtimes cannot accidentally share metric
identity merely because they use the same service name. A process restart also
creates a new ID. Randomness is used at runtime construction, not per request.

The compiler emits the algorithm, not a generated instance value. Generation
therefore remains deterministic. No new Go module dependency or deployment
setting is required. The ID is also present in local logs when export is disabled.

Generated tests check UUID encoding, entropy read failures and safe error text,
32 concurrent runtime identities, identity stability through close, and shared
identity across OTLP logs, metrics, traces, and local JSON output. Existing request
and controller tests still pass. The full compiler race suite passed with
PostgreSQL required on port 32913, and static checks passed.

The Gateway test uses two simultaneous API processes with the same service name
and database. It creates through REST, verifies the atomic owner grant, reads
through gRPC on the other process, denies an unauthenticated request, and receives
the durable event. It restarts one API while the other remains active and reads
the retained Gateway. The test then requires three distinct runtime resources,
separate cumulative request counts, matching local log identities, and matching
request log, span, and metric exemplar ownership. Compiler `86b436c` completed
the application actions but failed identity validation because it exported only
the shared service name. Pinned results are recorded in the application repo.

This change supplies distinct OTLP resource identity. It does not prove correct
storage or aggregation in every telemetry backend. Backends must preserve the
instance dimension when they process cumulative streams. Distributed controller
ownership, provider fencing, full process logging, non-keyed controller telemetry,
database and outbound spans, and production capacity remain open. See the
[full observability requirement](shared-observability.md).
