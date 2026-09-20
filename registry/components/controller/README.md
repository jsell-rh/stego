This component generates a controller runtime without a transport dependency.
Add `controller` to an archetype. It supplies `reconciliation-runtime` and writes
`out/controller/*.go`. The component has no generation settings.

An application supplies a typed `Source[T]`, a reconciliation function, and
validated `Options`. The source opens and confirms a live watch, then returns a
receiver. The scan emits retained work through a callback. The reconciliation
function reads authoritative state and applies the domain rules.

STEGO owns these mechanisms:

- Open the watch before the first scan, and drain it during scans.
- Bound the queue and process one item at a time.
- Bound watch setup and each action with the configured operation timeout.
- Repeat scans without concurrent scans or a deadline tied to the scan interval.
- Cancel a session on source failure or queue overflow, join its workers, and
  open a new watch before a new recovery scan.
- Stop on errors that the required terminal policy identifies, including denied
  access. Preserve these errors when another worker reports cancellation.
- Emit lifecycle notices without logging private resource data or error text.

A source must respect cancellation. Each external scan request and emitted item
must have a bound. The scan must recover failed work and missed deletions from
retained state. A watch does not provide this guarantee by itself. The runtime
cannot enforce these properties inside arbitrary application callbacks.

The resync interval starts after a scan finishes. It does not impose a total
scan deadline. A transient action failure is retried through the next scan.
Reconnects use the configured delay. One controller process has one worker and
a FIFO queue. `Run` does not coalesce events, schedule individual retries,
or provide distributed leases, fencing, or durable acknowledgments. A successful
callback is not proof of exactly-once effects. Domain actions must tolerate
repeated execution. Run one active process for each ownership scope until a
separate coordination contract is supplied and tested.

Do not use an event or a missing read result as permission to delete resources.
A domain action must use trusted deletion state and verify provider ownership.
The runtime preserves typed items, including retained deletion records; it does
not replace the application's access rules or decide what a deletion means.

The generated tests use resource IDs and typed records. They exercise queue
limits, action deadlines, watch setup deadlines, source failure, reconnect,
retained recovery, denied access, serial actions, slow scans, and cancellation.
These tests are correctness checks. They do not establish production capacity.

`RunKeyed` supplies a second source contract for current-state reconciliation.
`KeyedSource` connects an observer and a retained-state scan. The observer emits
bounded string keys through `KeySink.Add`. It calls `SetReady(false)` before an
incomplete baseline and `SetReady(true)` after a complete replacement. The
runtime starts paused. The observer owns its transport reconnects; an observer
error stops the controller and joins its workers.

The keyed runtime owns duplicate suppression, delayed retries, wakeups, periodic
scans, operation deadlines, and bounded workers. `Workers` defaults to one when
zero. An explicit value must be from 1 to 64 and cannot exceed capacity. Only
one action for each key can run at a time within a call. Other keys can run
concurrently. Actions and the terminal-error policy must support concurrent
calls when more than one worker is selected. Capacity includes queued,
delayed, and active keys. Keys contain 1 to 1,024 UTF-8 bytes. A change during an
active action schedules another pass. Repeated changes cannot bypass a failed
key's delay. Retry delay doubles from `RetryMin` to `RetryMax`; a successful pass
resets it. Due retries precede newer work, so new events cannot starve them.

`RunKeyedWithResult`, `RunKeyedWatchWithResult`, and
`RunKeyedWatchesWithResult` accept an action that returns `(ReconcileResult,
error)`. Set `RecheckAfter` when an authorized operation is still in progress.
The delay must be from one millisecond to one hour. A zero result means that
this pass needs no scheduled recheck. Invalid delays stop the controller.

A pending result releases the worker and stays inside queue capacity. Events
and watch reconnects do not shorten its delay. The next pass reads current
state. A pending action with no error resets previous error backoff. An action
error, deadline expiry, or cancellation discards the result; real failures keep
the existing retry schedule. Do not turn a provider or commit error into a
pending result. Pending work does not establish resource readiness, observation
commit success, or cleanup completion.

Logs, OTEL metrics and traces, and Prometheus action counters use the fixed
`pending` outcome. Pending actions do not increment failed-action retry
counters. `MetricsSnapshot.Pending` counts these actions. Keys remain in the
queued count while they wait. The existing APIs that return only an error keep
their previous scheduling behavior.

A heap orders pending work by due time and insertion order. There is at most one
heap entry for each pending key and no timer for each key. The worker waits for a
change or the next due key. It does not poll the whole queue. The source scan has
separate bounded retry delays. Lifecycle notices are serialized; callbacks must
return promptly and apply a policy before they log private error data.

The action reads current state for each key. A key is an invalidation hint, not a
stored payload. Do not use keyed coalescing for an ordered command log or a
sequence whose intermediate effects must all occur. A pause stops new queue
takes. It cannot retract an external write already in progress. Use domain
version checks or later repair for such writes. Distributed ownership and fencing
remain open work.

`RunKeyedWatch` connects the same keyed scheduler to `Source[string]` or a source
with another string-based key type. `KeyedWatchOptions` contains `KeyedOptions`
and `ReconnectDelay`. The runtime opens the watch before discovery or actions.
The source supplies invalidation keys. Each action must read authoritative state;
this adapter does not maintain a cache or wait for a cached baseline.

A failed subscription cancels and joins actions and source callbacks before the
next watch opens. Each new connection requests a retained-state scan. Action
failures use per-key delays. Transient scan failures retry without overlapping
another scan. Invalid keys, missing receivers, and terminal errors stop the
controller. Watch delivery and scans wait for capacity instead of restarting discovery
when their queue is full. Reconnects preserve the bounded queue and existing
retry due times. After old callbacks stop, interrupted actions receive their
next capped delay. Lifecycle notices remain serialized. A process restart still
loses pending keys and retry history; retained-state scans recover obligations.

Failed scans retain their due time across reconnects. An interrupted scan
receives the next capped delay after its callback stops. A successful scan
resets failure backoff: a later reconnect can discover state immediately, and
the current connection uses `ResyncInterval`. Scan waits permit independent
resource actions and stop on cancellation. The runtime owns this fixed-size
schedule; applications supply no scan retry map or timer.

Capacity bounds admitted keys, including delayed and active keys. The generated
watch and scan each retain at most one additional key while waiting for capacity.
`KeySink.AddWait` waits until a slot is available or its context ends. It validates
the key before waiting and does not discard another key or reset its retry delay.
Callers must bound the number of concurrent emitters. A blocking emitter must
not hold a lock that an action or observer needs. Scan callbacks must release
such locks before emitting. `KeySink.Add` retains its
nonblocking overflow error for observers that must stop on excess input.

A scan can exceed queue capacity when actions continue to release slots. Waiting
for admission does not hold a scan page request open or reset its cursor. It does
not impose a total scan deadline. Cancellation stops waiting emitters. Progress
is not established when persistent failures occupy every slot. Those keys keep
their retry state, and new keys can remain blocked. Durable retry storage and a
complete policy for saturation remain open. Set capacity from workload evidence.

`RunSweep` owns bounded worker pools and cursor-based recovery scans. Applications
supply named groups of streams, a page callback for each stream, and one typed
action. The runtime validates all definitions before it calls a source. It also
validates the whole page before it starts any action. It rejects oversized pages,
empty continuation pages, invalid or duplicate cursors, and an immediate repeat
of the supplied cursor.

A group shares one time budget. Groups rotate after each pass. Streams initially
use their declared order. If a stream uses the remaining budget, the next pass
starts with the next stream. Thus a slow current-state scan cannot indefinitely
prevent retained-history work. A full pass restores the declared order.

`SweepOptions.IntervalMode` selects when the runtime waits. The default,
`SweepIntervalAfterGroup`, applies `Interval` after every group pass, including
an empty group. `SweepIntervalAfterRound` applies it once after all groups have
had one pass. Each group keeps its own `PassTimeout`, cursor, and worker bound.
A full round still waits when all sources are empty. Cancellation interrupts
either wait. Invalid modes fail before any source call.

Round mode reduces the time between retained-work scans but can increase
provider and storage request rates. It does not reserve CPU, bound the total
duration of a round independently of its group count, or establish a capacity
target. Select the mode and interval from complete application measurements.

Each page uses a fixed worker pool. Workers honor cancellation and join before
the next page or group. The cursor advances only through the contiguous prefix
whose actions started. A short page does not reset a partial cursor. A complete
page with no continuation resets the cycle, so failed retained work receives a
later turn. Callback failure does not prove that an external effect did not
occur; actions must tolerate repeated execution.

The runtime holds one page and bounded cursor state. Configuration limits the
worker count, page size, group and stream count, cycle page count, time budget,
and pass interval. Cursors are opaque UTF-8 strings of at most 1,024 bytes. The
source owns their ordering and must retain failed work. The runtime does not
infer database collation or use cursor progress as an acknowledgment. The page
limit bounds a faulty source that continues to return new cursor values.

Cursor state is local to the process. Restart begins each cycle again. A source
must bound each item and return when its context ends. This runtime does not
provide durable claims, distributed exclusion, exactly-once effects, or a global
snapshot across mutable pages. It is a repair mechanism over retained state.

`Scan` supplies one bounded cursor scan for a controller source. It accepts a
typed `CursorSource`, an emitter, and `ScanOptions`. It owns page requests,
request deadlines, complete-page validation, cancellation checks, and cursor
progress. `CursorPage` and `CursorItem` are shared with the sweep runtime; the
existing `SweepPage` and `SweepItem` names remain aliases.

The scanner accepts 1–1,000 items per page, 1–1,000,000 pages per scan, and a
request timeout from 1 millisecond to 1 minute. It uses the same cursor checks
as the sweep. It never sorts opaque cursors. A source must validate its payloads
before it returns a page. A malformed page emits no items. An error stops the
scan and remains available to the controller's terminal-error policy. Set that
policy to stop on `ErrScanContract` when a source contract failure requires
operator action.

Each request context ends before dispatch. The emitter must respect the parent
context; it can apply queue backpressure without holding a request open. A scan
retains one page and does not start helper workers. Earlier emits are not undone
when a later page fails. A new scan starts at the first page. Repeated work must
be safe. Retained state, payload bounds, database ordering, and access checks
remain source obligations. This API does not supply a storage adapter, a durable
cursor, a global snapshot, or exactly-once delivery.

`ScanStream` supplies a finite stream scan. The application opens a stream with
the supplied context and returns a receive function. STEGO limits stream setup,
each receive call, and the number of items. The source must include protocol
setup, such as header validation, in its open function. Each successful receive
passes one value to the emitter. `io.EOF` completes the scan.

`StreamScanOptions` requires 1–1,000,000 items and separate open and receive
timeouts from 1 millisecond to 1 minute. STEGO permits one extra receive to
check for EOF at the item limit. It rejects an excess value before emission.
Invalid options or a missing receiver return `ErrScanContract`. Other errors
remain available to the controller's terminal-error policy.

Each timeout cancels the stream context. Open and receive functions must honor
that cancellation. Calls run synchronously and must return before the scanner
returns; STEGO does not detach blocked callbacks. A timed-out call cannot emit
a late value. The stream context is cancelled on every exit. This contract
requires the source to release its transport resources on cancellation.

No receive timer runs while the emitter waits for queue capacity. The emitter
must honor the parent context. The scanner holds one value and starts no receive
workers. The source must bound each value and validate its contents. There is no
total scan time limit. Earlier emits remain effective if a later receive fails,
and a retry starts from the beginning. Actions must tolerate repeated work.
This API does not supply durable progress, retained storage, or an idle timeout
for a live watch.
RunObservation reserves time to commit a conditional observation after provider
work. Its work timeout cannot consume the commit reserve within a parent action
deadline. Both callbacks run synchronously and retain cancellation. The caller
owns status values, access rules, revision checks, and atomic events. See the
[observation budget contract](../../../specs/observation-budgets.md) for limits,
error behavior, tests, and application evidence.

`ScanCheckpointed` connects `ScanFrom` to durable checkpoint callbacks. It
reserves time for a conditional save after work. Parent cancellation prevents
the save. Completion resets the cursor so that the next pass scans all data.
Initial and page cursors cannot contain NUL bytes. The whole page is checked
before its first effect, so emitted cursors can pass checkpoint validation.
See [scan checkpoints](../../../specs/scan-checkpoints.md).

`ScanCycle` retains action failures across bounded passes and process replacement.
It uses versioned checkpoint callbacks and a bounded record with no provider error
text. A completed cycle with an earlier failure returns `ErrCycleFailed`.
See [scan-cycle outcomes](../../../specs/scan-cycles.md) for invalidation rules.

[Sweep telemetry](../../../specs/sweep-observability.md) covers page reads and
actions in `RunSweep`. It shares providers with keyed controllers and omits
domain values from common logs, metrics, and spans.

[Shared controller telemetry](../../../specs/controller-observability.md) now
records keyed actions, scans, watch sessions, retries, and aggregate queue state.
The generated runtime owns logging, metrics, spans, and provider lifetime.

[Worker startup telemetry](../../../specs/worker-startup-observability.md) starts
before the `Monitor` callback. Provider setup, nested controllers, and deferred
cleanup share one runtime identity. A callback error or abort emits a fixed
failure event before the runtime closes. Probe commands do not start telemetry.

`Main(run)` supplies a controller process entry point. The callback has signature
`func(context.Context, *Metrics) error`. It must stop its work and close its
resources when the context ends. Main handles SIGINT and SIGTERM. A callback
failure exits with code 1 and a fixed error record. A panic or
`runtime.Goexit` in the Run callback returns `ErrRunAborted` after callback defers
finish. The monitor closes its listener. Domain error text and panic values are
not logged. A blocked error-log write has a 100 ms limit before process exit.

The process listens on `STEGO_CONTROLLER_MONITOR_ADDR`, which defaults to
`127.0.0.1:9081`. Only literal loopback IP addresses and explicit nonzero ports
are accepted. `/livez` checks the process context. `/readyz` requires an attached
queue that permits work. It does not claim that all domain resources are ready
or that a scan has completed. `/metrics` retains the existing local diagnostics.

`--stego-probe=live` and `--stego-probe=ready` perform a bounded local probe and
exit. They do not start the application callback or its providers. Probes use
no proxy, do not follow redirects, and require the exact successful response.
The generated Kubernetes worker resources use these commands. Common telemetry
still uses the configured OTLP exporter; the local endpoint is not exposed as
a Kubernetes Service.

[Run callback abort handling](../../../specs/controller-process-abort.md) covers
the goroutine that calls Run, with or without the local monitor. Goroutines
started by the callback, fatal runtime faults, and callbacks that do not stop
on cancellation remain outside this boundary.

`StateProtector` encrypts bounded recovery records with instance, resource,
scope, and version binding. It uses a fresh HKDF salt and an AES-256-GCM key for
each record. Supply an independent random active key and up to three older read
keys. Keep keys outside the state database. Plaintext requires explicit `Reveal`;
formatting is redacted and implicit JSON export fails. The codec does not save
records or grant access. Combine it with `ResourceStateStore` and authorized
resource transactions. Re-encryption requires a new record version.

`CheckStateEnvelope` checks the format and size without a decryption key.
It does not authenticate the record or prove that its contents are encrypted.
A controller must call `Open` before it uses the contents.

`StateJournal` connects that codec to a resource-bound `StatePersistence` pair.
The application adapter supplies access rules and the observed resource revision.
The journal authenticates each load, encrypts each next version, and requires an
exact saved version and envelope in the response. Each storage call has a
ten-second deadline. Storage must support cancellation and atomic version checks.
There is no automatic write retry. After a failed or uncertain save, load again
before another provider effect or save. A whole-database rollback remains outside
this guarantee.

The constructor takes an envelope size limit, so a transport can reserve space
for its metadata. `StateSnapshot` requires explicit plaintext access and belongs
to the journal that loaded it. A new journal after restart must load its own
snapshot. Empty contents retain the record version; they do not delete history.
Journal, snapshot, and sealed-record formatting is redacted; implicit JSON export
fails. The focused tests passed in both generated variants with the race detector
in 6.985 seconds. The production Hypershell provider lifecycle still needs to use
this journal.

`NewStateProtectorFromJSON` accepts a bounded JSON array of one through four
canonical base64 keys. Each decoded key has 32 bytes. The first key is active;
the others remain read keys. Use a common private-file reader before this call,
keep the file outside the database, and clear the input buffer after use. The
parser rejects repeated or zero keys, malformed data, and input over 256 bytes.
It copies accepted keys and clears temporary decoded bytes. The focused codec
checks passed in both generated variants with the race detector in 7.049 seconds.

## Bounded scan windows

Version 1.21.0 adds `ErrScanWindowLimit`. A source can use this error when its
bounded query window ends before it can prove a complete inventory. `ScanCycle`
saves the cycle as complete and failed, and returns both the window error and
`ErrCycleFailed`. The next call starts a new full cycle. Cleanup must still
require a successful cycle; the boundary is never a success result.

Only a source can request this boundary. The same error from an emitter does
not close the source. Parent cancellation prevents the boundary save. A save
conflict remains an error and leaves the persisted cursor unchanged. This lets
bounded cleanup revisit a provider list after earlier deletions move entries
behind an offset. It does not make offset pagination a snapshot.

The public `Run` entry point also uses the common telemetry runtime. It records
watch sessions, scans, reconciliation work, and queue counts. The configured
buffer limit excludes the active action; reported capacity includes that action.
See [public Run telemetry](../../../specs/controller-run-telemetry.md) for the
lifecycle contract and exact-source verification.
