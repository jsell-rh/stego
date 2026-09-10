Generated process failures must not expose component or driver error text.
The process boundary now writes one JSON record to stderr with these fields:

- `timestamp`: UTC time in RFC 3339 format, with nanoseconds when present.
- `severity`: `ERROR`.
- `event.name`: `service.failed`.
- `message`: `Service failed`.
- `stage`: the failed step from generated code.
- `tasks`: sorted generated task names, when background tasks failed.

Database stages are `database.configure`, `database.open`, and `database.handle`.
Component stages use `component[i].constructor[j]` or `component[i].database[j]`.
The indices refer to the assembly order and the constructor or database call in
that component. The generated main file shows the exact call after each stage
assignment. HTTP stages are `http.listen` and `http.serve`. A service with
background tasks uses `service.run` and the task names. `startup` is the initial
stage. These values do not come from requests, environment values, or error text.

The runtime retains error causes for `errors.Is` and `errors.As`. The process
record does not format or serialize those causes. Task error summaries also omit
cause text. Resource cleanup still runs before the process reports failure.
The process returns exit code 1. Failure output has one attempt and a one-second
wait limit. A blocked writer can retain one worker until process exit. Failed or
blocked output does not cause another output attempt. Delivery is not guaranteed.

The generated GORM connection uses `logger.Discard`. GORM must not print raw SQL,
query parameters, driver errors, or source paths. The compiler does not change
GORM's global logger. Application code that creates its own database connection
or replaces the logger must apply its own data policy. Database server logs are
outside this process boundary.

This change applies with or without the telemetry component. It adds no module
dependency. Before telemetry starts, the process has no telemetry instance to
which it can attach a failure. This bootstrap record is local; it is not an OTLP
event. Complete process lifecycle export, typed fault codes, database signals,
and safe domain event declarations remain open. HTTP listener startup still uses
its existing local message. The later [HTTP diagnostic policy](http-diagnostics.md)
covers server panic output. Other panics and application-owned log calls remain
outside this process failure boundary.

The Hypershell probe first exposed usernames, database names, network addresses,
and source paths during startup. A second probe injected a database error during
Gateway creation. It verified rollback and recovery, then found private database
error text in the process output. Both probes failed on compiler `777d591`.

Compiler tests verify that failure output never calls a cause's `Error` method,
retains error identity, produces structured records, and returns when output
blocks or fails. Task tests verify sorted names, cause identity, and cleanup.
The full compiler race suite passed with PostgreSQL required on port 32914.
Static checks passed. Application adoption is a separate gate.
