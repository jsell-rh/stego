Generated HTTP servers must not expose raw server diagnostics. Go's default
logger can print panic values, stack traces, source paths, and peer addresses.
A Hypershell test build with a temporary panic route reproduced a private panic
value in stderr. The test created a Gateway and owner grant, received its event,
triggered the panic, and read the retained Gateway after the failed request.
The privacy assertion failed on compiler `6277272`.

The compiler now selects one HTTP diagnostic logger. `HTTPErrorLogger` wiring
names its constructor index. That constructor must provide
`HTTPErrorLog() *log.Logger`. Invalid indices and multiple declarations fail
before generation. HTTP routes consume the selected constructor and its
dependencies. A service without HTTP routes does not consume it for this role.
The assembler uses the constructor's resolved name, including import and local
name changes. HTTP services with background tasks use the same rule.

OpenTelemetry component version 1.6.0 supplies this method automatically. Its
writer discards all raw diagnostic bytes and emits one fixed service event:

- Event: `http.server.diagnostic`.
- Message: `HTTP server reported a diagnostic`.
- Severity: `ERROR`.
- Time, service name, and instance ID from the existing telemetry runtime.

The event uses the existing local queue and OTLP log provider. It has no custom
attributes. The standard server logger supplies no request context, so this
diagnostic has no trace or span ID. The separate HTTP request log, metric, and
span still report `error.type=panic` and retain their own correlation. They do
not invent an HTTP response status for an aborted request. Disabling OTLP log
export does not remove the local diagnostic.

Without a selected component logger, the compiler provides a local fallback.
It queues only event times. Each record contains the time and the three fixed
fields above. It does not invent a service or instance identity. One worker and
a queue of 256 events start on the first diagnostic. Admission never waits for
the output writer. Queue pressure drops events. Internal counters record queue
drops and write failures. Output failures are not logged or retried. A server
that emits no diagnostic starts no worker.

The server closes only its fallback logger. It allows one second for that queue
to drain after HTTP shutdown. A blocked OS writer can retain one worker until
the writer returns or the process exits. Component loggers retain their own
resource owner; the server does not close the telemetry runtime. Custom component
loggers must also provide safe, bounded admission.

Go still handles the panic and aborts the request. `http.ErrAbortHandler` keeps
its normal behavior and does not produce a server diagnostic. The server remains
available for later requests. This change does not install a second panic
recovery layer, change response behavior, or alter request instrumentation.
Go still constructs its diagnostic before the writer discards it. The queue
bound does not describe that formatting cost or application panic behavior.

Compiler tests cover a real HTTP panic and later successful request without the
telemetry component, explicit aborts, lazy worker creation, queue pressure,
blocked output, write failure, and worker release. Generated program checks
cover logger selection with and without background tasks and conflicting names.
Telemetry tests cover fixed local and OTLP output, shared queue bounds, and
runtime close. The full compiler race suite passed with PostgreSQL required on
port 32915. Static checks passed. Application adoption is a separate gate.

Panics outside the HTTP server, application-owned logging, complete process
lifecycle export, database signals, and outbound client signals remain open.
This change does not complete the full observability or enterprise requirement.
