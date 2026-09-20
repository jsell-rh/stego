# Controller trace boundaries

Status: open. This record identifies a common runtime change. It does not claim
that the change is implemented or tested.

## Evidence

The Hypershell browser workflow at
`7fe2f3d19e86c1ed0ca7f52444ca8a869089ae74` passed in
[run 35487924300](https://github.com/jsell-rh/hypershell-stego/actions/runs/35487924300).
It used STEGO compiler `ee348b819bb43a738a6aad2fc14cb76ce3b1ec21`.

A bounded log observer retained fixed public telemetry fields. During the normal
Gateway cleanup window, it recorded 217 HTTP completions and 28 controller work
completions. All 245 records had one trace ID. Span IDs were distinct. The
observer can miss records, and Pod clock alignment was not independently checked.
These counts do not prove complete coverage or the cause of cleanup time.

The source explains the shared trace. In `watch_keyed.go.tmpl`, the watch span
wraps the whole watch session. That session supplies its context to
`runKeyedWithResult`. Reconciliation, scans, and cleanup samples inherit it.
Compiler `7a674e6c038b9fcf9735de78e66a30956c6feb0b` retains this behavior.

A watch can remain open for a long time. Its repeated work can therefore add
many spans to one trace. The configured trace-ID ratio sampler selects work in
the same trace together. Trace boundaries must follow bounded operations.

## Required behavior

- Each reconciliation attempt must have an independent trace. Expected pending
  work must get another trace on its next attempt.
- Each inventory scan and cleanup sample must have an independent trace.
- Provider HTTP, gRPC, and database calls must remain children of their current
  operation. They must not become unrelated roots within that operation.
- The watch connection must retain its own trace. A fixed, bounded span link can
  preserve an available relationship without making all later work its children.
- Cancellation, deadlines, runtime ownership, and authenticated export must keep
  their current behavior. Detachment must affect trace ancestry only.
- Sampling must apply to each operation. Logs must retain valid correlation IDs
  when an operation is not sampled.
- Attributes and link counts must remain bounded. No resource key, provider
  response, credential, or private error text can enter telemetry.
- The change must apply to the common generated runtime and preserve existing
  error-only and explicit-result controller APIs.

## Acceptance checks

Use generated tests for serial work, concurrent work, rechecks, scans, cleanup
samples, and reconnects. Check distinct trace IDs between operations and correct
parent IDs within each operation. Cover sampled and unsampled work, cancellation,
and error priority. Keep queues, timers, and retained trace state bounded.

Then check the complete Hypershell workflow with the signed compiler. Verify
correlated logs, metrics, and traces for the same worker instances and independent
trace IDs for separate attempts. Keep the cleanup-time measurement separate.
Do not modify an active test source or infer production capacity from this check.
