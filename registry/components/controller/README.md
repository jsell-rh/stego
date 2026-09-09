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
next watch opens. Each new connection starts a retained-state scan. Action
failures use per-key delays. Transient scan failures retry without overlapping
another scan. Invalid keys, missing receivers, and terminal errors stop the
controller. Watch delivery and scans wait for capacity instead of restarting discovery
when their queue is full. Retry delays reset on transport reconnect. Lifecycle
notices remain serialized.

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
