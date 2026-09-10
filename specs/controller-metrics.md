Controller component 1.9.0 supplies optional metrics for `RunKeyed` and
`RunKeyedWatch`. The generated `Metrics` type owns process counters and one
active queue view. Its zero value is usable. `KeyedOptions.Metrics` selects the
collector. Concurrent controller runs cannot share one collector. Counters
survive watch reconnects; queue gauges reset after the queue stops.

The collector exposes these bounded values:

- Queue capacity, queued keys, keys held by workers, queued retries, and callers
  in the waiting admission path.
- Whether a queue is attached and whether the source permits new actions.
- Completed actions classified as success, failure, timeout, or cancellation.
- Scheduled retries, completed scans, and watch reconnect attempts.
- A fixed action-duration histogram, total duration, and completed action count.

Retry keys are a subset of queued keys. They include retries whose delay has
elapsed. Waiting callers are outside the admitted capacity and remain subject
to the source's caller limit. A source pause does not stop an action already in
progress. A canceled action does not schedule a retry in the stopped queue.
Terminal action failures are counted but are not retried. Scans interrupted by
controller cancellation are omitted from completed scan counters.

`Snapshot` copies counters and a constant-time queue view. The collector stores
no keys, payloads, error strings, resource IDs, or configurable label values.
Its size does not increase with resource count. The queue view uses existing
queue sizes and a retry count; it does not walk the queue. Counter updates use
a short mutex section. No metrics callback runs under the queue lock.

The collector is an HTTP handler for `GET /metrics`. It emits fixed metric
names with the `stego_controller_` prefix, fixed outcome labels, and fixed
histogram buckets. The response uses the
[Prometheus text format](https://prometheus.io/docs/instrumenting/exposition_formats/).
Other paths return 404; other methods return 405. The handler does not use the
default HTTP mux. It sets no-store and nosniff headers.

`Monitor(ctx, address, run)` manages the listener and controller together. An
empty address supplies a nil collector and starts no server. A nonempty address
must contain a literal loopback IP and a canonical port from 1 through 65535.
Hostnames, wildcard addresses, interface zones, and non-loopback IPs fail before
the controller starts. A listener failure also prevents controller startup.
The HTTP server has header, read, write, and idle time limits and a configured
4 KiB header limit. On cancellation or either component's exit, the helper
closes the listener, cancels the shared context, and joins both components.
Controller callbacks must honor cancellation.

Loopback is a host or network-namespace boundary. The endpoint has no user
authentication and assumes trusted local access. Remote collection needs an
explicit authenticated proxy or another deployment access policy. The helper
does not create such a proxy or permit a public HTTP bind. Hypershell selects
its address; listener and collector behavior remain in STEGO.

Generated race tests cover blocked work, admission pressure, retry completion,
terminal errors, timeout, cancellation, failed scans, reconnect, collector reuse,
private-data exclusion, invalid routes, invalid addresses, an occupied listener,
HTTP serving, and shutdown. The full compiler race suite and static checks
passed. A parallel counter-and-histogram benchmark on an Intel Core Ultra 9 185H
with Go 1.26.8 measured 83.37–85.02 ns per enabled update with no allocation in
three runs. It excludes HTTP formatting, provider work, queue operations, and
application load. It does not establish a production capacity target.

These metrics do not supply durable resource conditions, cleanup age, historical
observations, distributed ownership, or persistent retries. They do not prove
that all desired resources are known to the queue. Those requirements remain
open. FIFO `Run` and `RunSweep` do not yet use this collector.

Controller 1.10.0 adds an independent sampler for authorized cleanup summaries.
It reports pending resources and the oldest deletion timestamp, with availability
and sample-time gauges. See [cleanup summaries](cleanup-summaries.md) for scope,
failure behavior, query cost, and remaining diagnostic requirements.
