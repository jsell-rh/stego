The generated keyed controller supplies the common scheduler for current-state
reconciliation. An application supplies an observer, a scan, and an action that
reads the latest state. It does not implement a timer loop or retry map.

The contract is documented in the [component guide](../registry/components/controller/README.md).
The generated tests cover duplicate bursts, changes during active work, delayed
retries, retry caps, capacity that includes active keys, baseline resets, source
failure, scan recovery, denied access, action timeouts, cancellation, and worker
shutdown. A due retry must precede newer keys; otherwise sustained new traffic
could prevent repair indefinitely.

Hypershell's Pod count controller is the first application consumer. Its changed
namespace set describes the effects of a cache update. That set is drained into
STEGO after each complete update. Pending work and retries are held only by STEGO.
Hypershell retains Pod classification, count calculation, cluster ownership, and
the current-state read. The existing count workflow must continue to prove REST
and gRPC values, drift repair, restart, zero counts, and denied writes.

A local benchmark ran on 2026-09-09 with Go 1.26.8 on Linux amd64, on an Intel
Core Ultra 9 185H. Each sample ran for 200 ms without race instrumentation. Three
samples measured a take, an invalidation during the action, and completion that
reschedules the key. The queue retained a fixed number of keys.

| Pending keys | Time per cycle | Allocated bytes per cycle | Allocations per cycle |
| --- | --- | --- | --- |
| 1 | 129.2–136.5 ns | 112 | 1 |
| 1,024 | 291.1–320.5 ns | 112 | 1 |
| 10,000 | 369.1–391.2 ns | 112 | 1 |

This is a scheduler microbenchmark. It excludes network calls, locks in the
domain cache, idle waits, startup allocations, and provider latency. It does not
establish application throughput or production memory capacity. The source has
separate bounds on objects and bytes. The key queue bounds both key length and
the total number of pending, delayed, and active keys.

The runtime does not provide distributed fencing or exactly-once effects. A
paused baseline cannot retract an external write already in progress. The count
is advisory and uses later repair. Domains that require stronger write ordering
must supply version checks and test them before they use this pattern.

Component version 1.4.0 adds bounded workers and `RunKeyedWatch`. A worker can
process another key while one action waits on its provider. The queue still
allows only one active action for each key within the runtime call. Duplicate
events during that action request a later pass. Zero workers preserves the
previous single-worker behavior. Explicit concurrency is limited to 64 workers
and cannot exceed the key capacity.

The watch adapter opens the subscription before discovery. Each action reads
authoritative state. Source failure cancels and joins the old session before
reconnect and a new scan. Terminal errors remain visible when another callback
returns cancellation. Tests block one action, complete another key, preserve a
dirty key, and verify shutdown before reconnect.

The adapter reuses the existing bounded queue and retry policy. An overflowing
scan restarts discovery. Progress for a backlog beyond capacity, or a queue full
of permanently failing keys, is not established. Cross-process fencing and
provider-side exclusion remain separate requirements.
