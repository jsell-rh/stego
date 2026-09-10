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

Version 1.12.3 preserves the bounded queue across `RunKeyedWatch` connections.
The previous runtime reset all retry delays after a watch failure. A Hypershell
identity regression observed a provider retry after 1.05 seconds instead of its
four-second delay. A watch failure must not bypass provider backoff.

The runtime still cancels and joins all old callbacks before a new watch opens.
It preserves due times and order for queued keys. Interrupted actions have no
confirmed outcome, so they receive the next capped delay. This work remains
inside the same capacity bound. A failed connection attempt does not add delay
to keys already queued. Each successful connection still starts a fresh scan,
and actions still read authoritative state. No application retry map is needed.

Generated tests cover preserved due times, interrupted actions, queue capacity,
independent pending keys, terminal errors, and shutdown before reconnect. These
checks do not establish retry persistence across process restart or distributed
provider ownership. Those contracts remain open.

A local reconnect benchmark used Go 1.26.8 on Linux amd64, Intel Core Ultra 9
185H, on 2026-09-10. Three 200 ms samples measured queue preparation with all
keys pending and no active callbacks. It excluded network and provider work.

| Pending keys | Time per reconnect preparation | Bytes | Allocations |
| --- | --- | --- | --- |
| 1,024 | 11.17–11.93 microseconds | 112 | 1 |
| 10,000 | 123.89–133.15 microseconds | 112 | 1 |
| 65,536 | 0.91–1.06 milliseconds | 112 | 1 |

Preparation visits the bounded queue once. These measurements do not include
rescheduling interrupted workers and do not establish production capacity.

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

Version 1.4.0 restarts discovery on queue overflow. The version 1.5.0 admission
contract below replaces that behavior. Cross-process fencing and provider-side
exclusion remain separate requirements.

A queue contention measurement on 2026-09-09 used the same processor and OS
listed above. Each worker took a key, invalidated it during the action, and
completed it. Three 200 ms samples used a fixed backlog and no race
instrumentation. Application checks ran on the same host during this measurement.

| Keys | Workers | Time per cycle | Bytes per cycle | Allocations per cycle |
| --- | --- | --- | --- | --- |
| 1024 | 1 | 375.1–410.6 ns | 112 | 1 |
| 1024 | 4 | 591.4–646.3 ns | 112 | 1 |
| 10000 | 1 | 504.6–515.8 ns | 112 | 1 |
| 10000 | 4 | 706.4–727.6 ns | 112 | 1 |

Four workers add queue contention. Their application benefit is progress while
another provider call waits; this microbenchmark does not measure that benefit.
It excludes the watch, scan, action, request timers, and external providers.
It does not establish production throughput or a latency guarantee. Repeat with:

```sh
STEGO_BENCH_CONTROLLER=1 go test -count=1 -v ./internal/generator/controller -run '^TestGeneratedController$'
```

Version 1.5.0 applies backpressure to retained scans and keyed watches. The
generated producers each hold at most one waiting key outside the admitted queue.
Admission validates input, preserves existing keys and retry delays, and stops on
cancellation. The scan cursor advances only after admission. Page request
contexts end before this wait. The nonblocking `KeySink.Add` contract is unchanged.

The regression uses a two-key queue, four discovery keys, and one persistent
provider failure. Version 1.4.0 restarted its scan 837 times during the one-second
probe and never reached the last key. With backpressure, one scan reaches that
key while the failed key retains its retry schedule. A watch-only regression
checks the same progression. Further tests check capacity, invalid keys, and
cancellation before and during admission.

This does not evict failed keys. If every slot holds a persistent failure, new
keys can remain blocked. The user was asked whether durable PostgreSQL retry
storage or scan-based overflow recovery should address this case. That choice
and full saturation behavior remain open.

The admission microbenchmark used Go 1.26.8 on Linux amd64 and the Intel Core
Ultra 9 185H on 2026-09-09. Three 200 ms samples measured admission, take, and
successful removal of one key. The queue had free capacity. Nonblocking cycles
took 261.8–288.3 ns. Backpressure cycles took 316.6–339.9 ns. Both used 288 bytes
and three allocations per cycle. These cycles create and remove an entry, unlike
the fixed-backlog benchmark above. They exclude waiting, scans, RPC, and provider
work, and do not establish application throughput. The benchmark command above
now runs both measurements.
