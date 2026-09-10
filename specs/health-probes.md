The `health-check` component generates process liveness and declared dependency
readiness. Version 1.0.0 replaces the empty generator. The earlier component
accepted a health capability but supplied no endpoint.

`GET /livez` reports that the HTTP process responds. It does not query a database
or external provider. `GET /readyz` returns 503 until the monitor completes a
successful sample. It also returns 503 after cancellation, a failed sample, or
expiration of the last successful observation. Successful responses contain
`ok` and failed responses contain `unavailable`. Both end with a newline.
Responses prevent caching and contain no dependency errors or credentials.
HEAD requests have the same status without a body. Other methods are rejected.

The routes are outside application authentication. All other routes retain their
existing authentication and authorization. The endpoints use the generated
HTTP server and its header, request, response, and shutdown limits. They do not
add a listener or a public diagnostic report. Protect detailed operational data
through a separate access policy.

The monitor runs one sampling loop. A cycle has a 500 ms shared deadline and at
most 16 read-only checks. After each cycle, the loop waits one second. Readiness
expires two seconds after the start of the last successful cycle. HTTP probes
only read a saved result; probe traffic cannot increase dependency traffic.
Cancellation makes readiness fail immediately. Checks must honor cancellation.
A stalled callback cannot start another callback or retain readiness forever;
the runtime still waits for that callback during shutdown.

Set the component's `database` override to true to sample `PingContext` on the
compiler-owned SQL pool. The connection remains owned by the service runtime.
A successful ping proves database connectivity. It does not prove schema
compatibility, write permission, external provider availability, gRPC readiness,
or completed event delivery. With `database` absent or false, the generated
monitor has no external dependency checks. Its readiness reports only that the
monitor started and remains active. Applications can use the generated
`NewMonitor` contract with additional checks outside generated code.

This distinction follows the Kubernetes
[probe contract](https://kubernetes.io/docs/concepts/workloads/pods/probes/):
liveness can cause a restart, while readiness controls traffic eligibility.
Do not make a transient dependency failure alone cause liveness failure.

Generated runtime tests cover failures, recovery, deadlines, stale observations,
cancellation, one active check, and repeated HTTP requests. A separate generated
service verifies public probe routes, protected application routes, and clean
shutdown. The Hypershell application test delays database traffic and checks
readiness loss, independent liveness, recovery, retained Gateway access, and
API restart. The original application baseline failed because `/livez` did not
exist. Adding the routes then exposed a compiler assembly defect: a temporary
`handler` variable forced the application constructor to be renamed, while its
route retained the old name. The assembler now passes the wrapped handler
directly to the top-level mux. The standalone generated service reproduces this
name conflict and verifies both denied and permitted application requests.
The fixed application workflow passed under race detection in 7.462 seconds
with PostgreSQL required. Final pinned adoption is recorded separately.
The full compiler race suite passed with PostgreSQL required on port 32906.
Static checks also passed.

This change does not complete C6. Tracing, complete dependency readiness,
controller health endpoints, gRPC health, and deployment probe configuration
still need application evidence.
