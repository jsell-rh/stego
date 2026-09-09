This component generates a controller runtime without a transport dependency.
Add `controller` to an archetype. It supplies `reconciliation-runtime` and writes
`out/controller/runtime.go`. The component has no generation settings.

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
a FIFO queue. This version does not coalesce events, schedule individual retries,
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
