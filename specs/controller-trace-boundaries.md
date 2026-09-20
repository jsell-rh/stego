# Controller trace boundaries

Status: the common compiler is qualified and released. The real Hypershell
workflow with this compiler remains required before application adoption is
qualified. See the [compiler evidence](controller-trace-evidence.json).

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

## Released change

The common telemetry entry point starts an independent root for each fixed
operation. It retains the supplied context. It does not retain links or increase
span limits. Provider calls inherit the operation context and keep their parent.
Cleanup samples use the same entry point. Their result includes sample validation;
failed samples keep their existing native counters and do not count as work retries.

Generated trace tests cover serial and concurrent work, pending attempts, scans,
cleanup samples, and successive watch contexts. They check exported parent IDs,
HTTP, gRPC, and PostgreSQL child spans, and sampled and unsampled log correlation.
The controller integration test checks cleanup failure, recovery, invalid samples,
and native counters. Existing queue and watch tests remain required. A live
Hypershell run must still prove trace separation through actual scheduling.


## Compiler qualification

Compiler `f6ebd0ba93983bfdcaefa15d8c3344b299af245e` passed all six
[main compiler jobs](https://github.com/jsell-rh/stego/actions/runs/35490158790).
The race check passed 34 packages. Both examples matched their generated code,
and their state snapshots contained only the expected compiler identity changes.
The full log does not report every test skip. The separate focused checks supply
named results for the changed controller behavior.

The [controller check](https://github.com/jsell-rh/stego/actions/runs/35490158808)
passed 21 outer cases. Each of its two trace cases checked 14 independent roots
and 36 provider children, with sampled and unsampled log correlation. Two cleanup
cases checked failure, recovery, invalid samples, and native counters. Existing
error-only and explicit-result controller APIs remain covered. The
[authentication check](https://github.com/jsell-rh/stego/actions/runs/35490158802)
passed eight cases at the same source.

The [signed build](https://github.com/jsell-rh/stego/actions/runs/35490158799)
produced identical bytes in two isolated builds. Independent review checked
1,292 source files, 28 module records, the build policy, signatures, four rejection
cases, and the common installer. The
[immutable release](https://github.com/jsell-rh/stego/releases/tag/compiler-f6ebd0ba93983bfdcaefa15d8c3344b299af245e)
was downloaded and verified independently. Its installation record matches the
verified build artifact. The downloaded compiler did not execute locally.

Hypershell candidate `e026fb00159953615110fd7c7022ee6d53526db0` selects this
release. Hosted regeneration and the committed Gateway console module update
passed independent review. Its full consumer checks and live trace workflow are
separate acceptance gates. No live trace, capacity, or cleanup-time result is
claimed for this candidate. Keep the pending-result cleanup measurement on its
separate frozen source and compiler.

## Consumer verification

Source `e026fb0` passed the [full hosted check](https://github.com/jsell-rh/hypershell-stego/actions/runs/35490842878)
with 1,019 core cases. Its live test found an incorrect expected instance count:
the deliberate cleanup restart creates a third allocator instance. The corrected
focused tests require all three allocator instances and retain the exact identity
and workload counts. They do not accept extra instances.

The corrected source `fdb06e2` failed its [next live test](https://github.com/jsell-rh/hypershell-stego/actions/runs/35493398031)
at the fixture Pod guard before the database restart. The guard had one combined
failure message, so the cause remains unknown. Independent cleanup found both
test fixtures absent, a free shared lease, and all 32 installation resources
unchanged. The new guard reports fixed categories and HTTP status. It retains
the same read deadline, single request, UID, labels, and database status checks.
See the [failed live result](https://github.com/jsell-rh/hypershell-stego/blob/c06bd3c3e435e46e0b3ee6ba5662a19364e38a5e/acceptance/database-restart-failure-evidence.json).

The [full check for fdb06e2](https://github.com/jsell-rh/hypershell-stego/actions/runs/35493201216)
also failed the late Keycloak creation recovery test. A separate
[deterministic regression](https://github.com/jsell-rh/hypershell-stego/actions/runs/35494601331)
failed all ten candidate-removal cases on the original code. Denied requests,
invalid responses, failed lists, and current ownership checks passed. This proves
the inventory defect; it does not qualify the failed workflow.

Hypershell fix `a5bf5717448086b093f5cac4cab0b516b706353a` skips only the common
typed not-found error from a current candidate read. A failed list or another
read error still stops the scan. Page bounds and cleanup journals remain required.
The original recovery test is unchanged. See the
[fix and failure evidence](https://github.com/jsell-rh/hypershell-stego/blob/a5bf5717448086b093f5cac4cab0b516b706353a/acceptance/keycloak-inventory-removal.md).

Fresh hosted checks are pending on that exact source. A new bounded live test
requires all of them to pass and requires a fresh cluster cleanup audit. Complete
live trace proof, the separate API gate, the cleanup-time target, and production
capacity remain unproved for this candidate. The signed compiler is unchanged.
