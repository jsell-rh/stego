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
